package policystore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
)

type PulledMessage struct {
	MessageID    string
	Seq          int64
	SenderUserID string
	Text         string
	ServerTime   time.Time
	Redacted     bool
}

type MessagePage struct {
	ConversationID string
	Messages       []PulledMessage
	NextAfterSeq   int64
	HasMore        bool
}

func auditMessagePull(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity,
	conversationID, outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `
INSERT INTO audit_events (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
VALUES ($1,$2,$3,'message_pull','conversation',$4,$5,$6,$7)`, id.TenantID, id.UserID,
		id.ActingMembershipID, nullableID(conversationID), outcome, reason, at)
	return err
}

func finishMessagePull(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity,
	conversationID, outcome, reason string, at time.Time) error {
	if err := auditMessagePull(ctx, tx, id, conversationID, outcome, reason, at); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return tx.Commit(ctx)
}

// PullTextMessages returns one bounded page of direct messages. Historical
// recipient identity, not the conversation's current selection, establishes
// who was authorized at the time of each send.
func (s Service) PullTextMessages(ctx context.Context, id access.TrustedIdentity,
	conversationID string, afterSeq int64, limit int) (MessagePage, error) {
	if !directoryUUIDPattern.MatchString(conversationID) || afterSeq < 0 || limit < 1 || limit > 500 {
		return MessagePage{}, ErrInvalidMessageRequest
	}
	conversationID = strings.ToLower(conversationID)
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return MessagePage{}, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return MessagePage{}, err
	}
	defer tx.Rollback(ctx)
	at := s.now()
	actor, found, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return MessagePage{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !found || !memberActiveAt(actor, at) {
		if err := finishMessagePull(ctx, tx, id, "", "deny", "invalid_identity", at); err != nil {
			return MessagePage{}, err
		}
		return MessagePage{}, ErrForbidden
	}
	var lowUser, highUser string
	err = tx.QueryRow(ctx, `SELECT direct_user_low_id::text,direct_user_high_id::text
FROM conversations WHERE tenant_id=$1 AND id=$2 AND kind='direct' FOR SHARE`,
		id.TenantID, conversationID).Scan(&lowUser, &highUser)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !strings.EqualFold(id.UserID, lowUser) && !strings.EqualFold(id.UserID, highUser)) {
		if err := finishMessagePull(ctx, tx, id, "", "deny", "conversation_unavailable", at); err != nil {
			return MessagePage{}, err
		}
		return MessagePage{}, ErrMessageNotAvailable
	}
	if err != nil {
		return MessagePage{}, err
	}
	// Hold the policy pointer during the read, so a new hard deny cannot be
	// published between policy selection and message filtering.
	var currentPolicyVersion int64
	err = tx.QueryRow(ctx, `SELECT current_version FROM policy_current WHERE tenant_id=$1 FOR SHARE`,
		id.TenantID).Scan(&currentPolicyVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return MessagePage{}, err
	}
	version, err := currentVersion(ctx, tx, id.TenantID)
	if err != nil {
		return MessagePage{}, err
	}
	rules, err := loadRules(ctx, tx, id.TenantID, version)
	if err != nil {
		return MessagePage{}, err
	}
	page := MessagePage{ConversationID: conversationID,
		Messages: make([]PulledMessage, 0, min(limit, 100)), NextAfterSeq: afterSeq}
	type historicalPair struct{ reader, peer policy.Membership }
	histories := make([]historicalPair, 0, min(limit, 100))
	rows, err := tx.Query(ctx, `
SELECT m.id::text,m.seq,m.sender_user_id::text,m.sender_membership_id::text,
 COALESCE(m.recipient_user_id::text,''),COALESCE(m.recipient_membership_id::text,''),
 m.text_body,m.accepted_at,
 COALESCE(m.sender_organization_id::text,''),COALESCE(m.recipient_organization_id::text,'')
FROM messages m
WHERE m.tenant_id=$1 AND m.conversation_id=$2 AND m.seq>$3
ORDER BY m.seq LIMIT $4`, id.TenantID, conversationID, afterSeq, limit+1)
	if err != nil {
		return MessagePage{}, err
	}
	for rows.Next() {
		var messageID, senderUser, senderMember, recipientUser, recipientMember string
		var senderOrg, recipientOrg, body string
		var seq int64
		var acceptedAt time.Time
		if err := rows.Scan(&messageID, &seq, &senderUser, &senderMember,
			&recipientUser, &recipientMember, &body, &acceptedAt, &senderOrg, &recipientOrg); err != nil {
			rows.Close()
			return MessagePage{}, err
		}
		if len(page.Messages) == limit {
			page.HasMore = true
			break
		}
		item := PulledMessage{Seq: seq, Redacted: true}
		history := historicalPair{}
		validPair := (senderUser == lowUser && recipientUser == highUser) ||
			(senderUser == highUser && recipientUser == lowUser)
		if validPair && senderMember != "" && recipientMember != "" && senderOrg != "" && recipientOrg != "" {
			sender := policy.Membership{ID: senderMember, TenantID: id.TenantID, OrganizationID: senderOrg}
			recipient := policy.Membership{ID: recipientMember, TenantID: id.TenantID, OrganizationID: recipientOrg}
			historicalReader, peer := sender, recipient
			if strings.EqualFold(id.UserID, recipientUser) {
				historicalReader, peer = recipient, sender
			}
			item = PulledMessage{MessageID: messageID, Seq: seq, SenderUserID: senderUser,
				Text: body, ServerTime: acceptedAt, Redacted: false}
			history = historicalPair{reader: historicalReader, peer: peer}
		}
		page.Messages = append(page.Messages, item)
		histories = append(histories, history)
		page.NextAfterSeq = seq
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return MessagePage{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !memberActiveAt(actor, at) {
		if err := finishMessagePull(ctx, tx, id, "", "deny", "invalid_identity", at); err != nil {
			return MessagePage{}, err
		}
		return MessagePage{}, ErrForbidden
	}
	for i := range page.Messages {
		if !page.Messages[i].Redacted && policy.HistoryHardDeny(actor, histories[i].reader, histories[i].peer, at, rules) {
			page.Messages[i] = PulledMessage{Seq: page.Messages[i].Seq, Redacted: true}
		}
	}
	if err := finishMessagePull(ctx, tx, id, conversationID, "allow", "history_page", at); err != nil {
		return MessagePage{}, err
	}
	return page, nil
}
