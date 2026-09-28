package policystore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
)

var (
	ErrInvalidMessageRequest      = errors.New("invalid message request")
	ErrInvalidTextMessage         = errors.New("invalid text message")
	ErrMessageNotAvailable        = errors.New("conversation or recipient not available")
	ErrIdempotencyConflict        = errors.New("idempotency content conflict")
	ErrConversationContextChanged = errors.New("conversation membership context changed")
	ErrMessageRateLimited         = errors.New("message rate limit exceeded")
)

type MessageACK struct {
	MessageID      string
	ConversationID string
	Seq            int64
	ServerTime     time.Time
}

type directMessageContext struct {
	status                string
	lowUserID, highUserID string
	lowMember, highMember string
}

func auditMessageSend(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, conversationID,
	outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `
INSERT INTO audit_events (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
VALUES ($1,$2,$3,'message_send','conversation',$4,$5,$6,$7)`, id.TenantID, id.UserID,
		id.ActingMembershipID, nullableID(conversationID), outcome, reason, at)
	return err
}

func finishMessageSend(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, conversationID,
	outcome, reason string, at time.Time) error {
	if err := auditMessageSend(ctx, tx, id, conversationID, outcome, reason, at); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return tx.Commit(ctx)
}

func loadDirectMessageContext(ctx context.Context, tx pgx.Tx, tenantID, conversationID string, lock bool) (directMessageContext, error) {
	query := `SELECT status,direct_user_low_id::text,direct_user_high_id::text,
 direct_low_membership_id::text,direct_high_membership_id::text
FROM conversations WHERE tenant_id=$1 AND id=$2 AND kind='direct'`
	if lock {
		query += " FOR UPDATE"
	}
	var context directMessageContext
	err := tx.QueryRow(ctx, query, tenantID, conversationID).Scan(&context.status,
		&context.lowUserID, &context.highUserID, &context.lowMember, &context.highMember)
	return context, err
}

func (c directMessageContext) selectedMembership(userID string) (string, string, bool) {
	switch {
	case strings.EqualFold(userID, c.lowUserID):
		return c.lowMember, c.highMember, true
	case strings.EqualFold(userID, c.highUserID):
		return c.highMember, c.lowMember, true
	default:
		return "", "", false
	}
}

func (c directMessageContext) sameSelection(other directMessageContext) bool {
	return c.lowUserID == other.lowUserID && c.highUserID == other.highUserID &&
		c.lowMember == other.lowMember && c.highMember == other.highMember
}

func existingMessageACK(ctx context.Context, tx pgx.Tx, tenantID, conversationID, senderUserID,
	clientMessageID string, digest [32]byte) (MessageACK, bool, bool, error) {
	var ack MessageACK
	var storedDigest []byte
	err := tx.QueryRow(ctx, `
SELECT i.content_digest,m.id::text,m.seq,m.accepted_at
FROM message_idempotency i JOIN messages m
  ON m.tenant_id=i.tenant_id AND m.id=i.message_id
WHERE i.tenant_id=$1 AND i.conversation_id=$2 AND i.sender_user_id=$3 AND i.client_msg_id=$4`,
		tenantID, conversationID, senderUserID, clientMessageID).
		Scan(&storedDigest, &ack.MessageID, &ack.Seq, &ack.ServerTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return MessageACK{}, false, false, nil
	}
	if err != nil {
		return MessageACK{}, false, false, err
	}
	ack.ConversationID = conversationID
	return ack, true, bytes.Equal(storedDigest, digest[:]), nil
}

func reserveMessageRate(ctx context.Context, tx pgx.Tx, tenantID, senderUserID string, at time.Time, maxPerSecond int) (bool, error) {
	var count int
	err := tx.QueryRow(ctx, `
INSERT INTO message_rate_windows (tenant_id,sender_user_id,window_start,sent_count)
VALUES ($1,$2,date_trunc('second',$3::timestamptz),1)
ON CONFLICT (tenant_id,sender_user_id) DO UPDATE
SET window_start=EXCLUDED.window_start,
 sent_count=CASE WHEN message_rate_windows.window_start=EXCLUDED.window_start
   THEN message_rate_windows.sent_count+1 ELSE 1 END
WHERE message_rate_windows.window_start<>EXCLUDED.window_start
   OR message_rate_windows.sent_count<$4
RETURNING sent_count`, tenantID, senderUserID, at, maxPerSecond).Scan(&count)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// SendTextMessage commits the message, deduplication key, outbox event and
// sequence together. The returned ACK is valid only after transaction commit.
func (s Service) SendTextMessage(ctx context.Context, id access.TrustedIdentity, conversationID,
	clientMessageID, body string) (MessageACK, error) {
	if !directoryUUIDPattern.MatchString(conversationID) {
		return MessageACK{}, ErrInvalidMessageRequest
	}
	conversationID = strings.ToLower(conversationID)
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return MessageACK{}, ErrForbidden
	}
	maxRate := s.MessageRatePerSecond
	if maxRate == 0 {
		maxRate = 10
	}
	if maxRate < 0 {
		return MessageACK{}, ErrInvalidMessageRequest
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return MessageACK{}, err
	}
	defer tx.Rollback(ctx)
	at := s.now()
	preflightActor, found, err := loadMembershipSnapshot(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return MessageACK{}, err
	}
	if !found || !memberActiveAt(preflightActor, at) {
		if err := finishMessageSend(ctx, tx, id, "", "deny", "invalid_identity", at); err != nil {
			return MessageACK{}, err
		}
		return MessageACK{}, ErrForbidden
	}
	snapshot, err := loadDirectMessageContext(ctx, tx, id.TenantID, conversationID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return MessageACK{}, ErrMessageNotAvailable
	}
	if err != nil {
		return MessageACK{}, err
	}
	_, targetMembershipID, participant := snapshot.selectedMembership(id.UserID)
	if !participant {
		return MessageACK{}, ErrMessageNotAvailable
	}
	locked, err := tx.Query(ctx, `SELECT id FROM user_organizations
WHERE tenant_id=$1 AND id IN ($2,$3) ORDER BY id FOR SHARE NOWAIT`,
		id.TenantID, id.ActingMembershipID, targetMembershipID)
	if err != nil {
		return MessageACK{}, err
	}
	for locked.Next() {
	}
	err = locked.Err()
	locked.Close()
	if err != nil {
		return MessageACK{}, err
	}
	at = s.now()
	actor, found, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return MessageACK{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !found || !memberActiveAt(actor, at) {
		if err := finishMessageSend(ctx, tx, id, "", "deny", "invalid_identity", at); err != nil {
			return MessageACK{}, err
		}
		return MessageACK{}, ErrForbidden
	}
	if err := validateClientMessageID(clientMessageID, at); err != nil {
		return MessageACK{}, err
	}
	if !utf8.ValidString(body) || strings.TrimSpace(body) == "" || strings.ContainsRune(body, 0) || len(body) > 16*1024 {
		return MessageACK{}, ErrInvalidTextMessage
	}
	digest := sha256.Sum256([]byte(body))
	if ack, exists, same, err := existingMessageACK(ctx, tx, id.TenantID, conversationID, id.UserID, clientMessageID, digest); err != nil {
		return MessageACK{}, err
	} else if exists {
		return finishExistingMessage(ctx, tx, id, ack, same, at)
	}
	if snapshot.status != "active" {
		if err := finishMessageSend(ctx, tx, id, "", "deny", "conversation_unavailable", at); err != nil {
			return MessageACK{}, err
		}
		return MessageACK{}, ErrMessageNotAvailable
	}
	current, err := loadDirectMessageContext(ctx, tx, id.TenantID, conversationID, true)
	if err != nil {
		return MessageACK{}, err
	}
	selectedActorMembership, _, _ := current.selectedMembership(id.UserID)
	if !snapshot.sameSelection(current) || current.status != "active" ||
		!strings.EqualFold(id.ActingMembershipID, selectedActorMembership) {
		if err := finishMessageSend(ctx, tx, id, "", "deny", "conversation_context_changed", at); err != nil {
			return MessageACK{}, err
		}
		return MessageACK{}, ErrConversationContextChanged
	}
	if ack, exists, same, err := existingMessageACK(ctx, tx, id.TenantID, conversationID, id.UserID, clientMessageID, digest); err != nil {
		return MessageACK{}, err
	} else if exists {
		return finishExistingMessage(ctx, tx, id, ack, same, at)
	}
	target, targetFound, err := loadMembership(ctx, tx, id.TenantID, targetMembershipID, "")
	if err != nil {
		return MessageACK{}, err
	}
	version, err := currentVersion(ctx, tx, id.TenantID)
	if err != nil {
		return MessageACK{}, err
	}
	rules, err := loadRules(ctx, tx, id.TenantID, version)
	if err != nil {
		return MessageACK{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	decision := policy.Decision{PolicyVersion: version, Reason: policy.ReasonInvalidContext}
	if targetFound {
		decision = policy.Evaluate(policy.Input{Action: policy.ActionSendMessage,
			Actor: actor, Target: target, At: at, ScopeAllowed: true, ResourceActive: true,
			PolicyVersion: version, Rules: rules})
	}
	req := Request{Identity: id, TargetMembershipID: targetMembershipID,
		Action: policy.ActionSendMessage, ScopeAllowed: true, ResourceActive: true}
	if err := auditDecision(ctx, tx, req, decision, at); err != nil {
		return MessageACK{}, errors.Join(ErrAuditUnavailable, err)
	}
	if !decision.Allowed {
		if err := finishMessageSend(ctx, tx, id, "", "deny", string(decision.Reason), at); err != nil {
			return MessageACK{}, err
		}
		return MessageACK{}, ErrMessageNotAvailable
	}
	targetUserID := current.lowUserID
	if strings.EqualFold(targetUserID, id.UserID) {
		targetUserID = current.highUserID
	}
	allowed, err := reserveMessageRate(ctx, tx, id.TenantID, id.UserID, at, maxRate)
	if err != nil {
		return MessageACK{}, err
	}
	if !allowed {
		if err := finishMessageSend(ctx, tx, id, "", "deny", "rate_limited", at); err != nil {
			return MessageACK{}, err
		}
		return MessageACK{}, ErrMessageRateLimited
	}
	ack := MessageACK{ConversationID: conversationID, ServerTime: at}
	err = tx.QueryRow(ctx, `UPDATE conversations SET last_seq=last_seq+1,updated_at=$3
WHERE tenant_id=$1 AND id=$2 RETURNING last_seq`, id.TenantID, conversationID, at).Scan(&ack.Seq)
	if err != nil {
		return MessageACK{}, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO messages
 (tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,
  recipient_user_id,recipient_membership_id,sender_organization_id,recipient_organization_id,
  client_msg_id,text_body,content_digest,accepted_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id::text`, id.TenantID, conversationID,
		ack.Seq, id.UserID, id.ActingMembershipID, targetUserID, targetMembershipID,
		actor.OrganizationID, target.OrganizationID, clientMessageID, body, digest[:], at).Scan(&ack.MessageID)
	if err != nil {
		return MessageACK{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO message_idempotency
 (tenant_id,conversation_id,sender_user_id,client_msg_id,message_id,content_digest,accepted_at,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id.TenantID, conversationID,
		id.UserID, clientMessageID, ack.MessageID, digest[:], at, at.Add(30*24*time.Hour))
	if err != nil {
		return MessageACK{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events
 (tenant_id,conversation_id,seq,message_id,event_type,next_retry_at,created_at)
VALUES ($1,$2,$3,$4,'message_created',$5,$5)`, id.TenantID, conversationID,
		ack.Seq, ack.MessageID, at)
	if err != nil {
		return MessageACK{}, err
	}
	if err := finishMessageSend(ctx, tx, id, conversationID, "allow", string(decision.Reason), at); err != nil {
		return MessageACK{}, err
	}
	return ack, nil
}

func finishExistingMessage(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, ack MessageACK,
	same bool, at time.Time) (MessageACK, error) {
	if !same {
		if err := finishMessageSend(ctx, tx, id, "", "deny", "idempotency_conflict", at); err != nil {
			return MessageACK{}, err
		}
		return MessageACK{}, ErrIdempotencyConflict
	}
	if err := finishMessageSend(ctx, tx, id, ack.ConversationID, "allow", "idempotent_replay", at); err != nil {
		return MessageACK{}, err
	}
	return ack, nil
}
