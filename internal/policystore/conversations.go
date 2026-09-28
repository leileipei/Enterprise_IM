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

var (
	ErrInvalidChatTarget = errors.New("invalid chat target")
	ErrChatNotAvailable  = errors.New("direct chat not available")
)

type DirectConversation struct {
	ID             string
	LastSeq        int64
	PolicyVersion  int64
	CrossLegal     bool
	DecisionReason policy.Reason
}

func auditConversationStart(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, conversationID,
	outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `
INSERT INTO audit_events (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
VALUES ($1,$2,$3,'conversation_start','conversation',$4,$5,$6,$7)`,
		id.TenantID, id.UserID, id.ActingMembershipID, nullableID(conversationID), outcome, reason, at)
	return err
}

func finishConversationStart(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, conversationID,
	outcome, reason string, at time.Time) error {
	if err := auditConversationStart(ctx, tx, id, conversationID, outcome, reason, at); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return tx.Commit(ctx)
}

// StartDirectConversation rechecks the current start_chat policy before
// creating or reusing the tenant's single direct conversation for a user pair.
func (s Service) StartDirectConversation(ctx context.Context, id access.TrustedIdentity, targetMembershipID string) (DirectConversation, error) {
	if !directoryUUIDPattern.MatchString(targetMembershipID) {
		return DirectConversation{}, ErrInvalidChatTarget
	}
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return DirectConversation{}, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return DirectConversation{}, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
SELECT id FROM user_organizations WHERE tenant_id=$1 AND id IN ($2,$3)
ORDER BY id FOR SHARE NOWAIT`, id.TenantID, id.ActingMembershipID, targetMembershipID)
	if err != nil {
		return DirectConversation{}, err
	}
	for rows.Next() {
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return DirectConversation{}, err
	}
	at := s.now()
	actor, actorFound, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return DirectConversation{}, err
	}
	target, targetFound, err := loadMembership(ctx, tx, id.TenantID, targetMembershipID, "")
	if err != nil {
		return DirectConversation{}, err
	}
	version, err := currentVersion(ctx, tx, id.TenantID)
	if err != nil {
		return DirectConversation{}, err
	}
	rules, err := loadRules(ctx, tx, id.TenantID, version)
	if err != nil {
		return DirectConversation{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	var actorUserID, targetUserID string
	if actorFound {
		actorUserID, err = conversationMembershipUser(ctx, tx, id.TenantID, id.ActingMembershipID)
		if err != nil {
			return DirectConversation{}, err
		}
	}
	if targetFound {
		targetUserID, err = conversationMembershipUser(ctx, tx, id.TenantID, targetMembershipID)
		if err != nil {
			return DirectConversation{}, err
		}
	}
	decision := policy.Decision{PolicyVersion: version, Reason: policy.ReasonInvalidContext}
	if actorFound && targetFound && actorUserID != targetUserID {
		decision = policy.Evaluate(policy.Input{Action: policy.ActionStartChat,
			Actor: actor, Target: target, At: at, ScopeAllowed: true, ResourceActive: true,
			PolicyVersion: version, Rules: rules})
	}
	req := Request{Identity: id, TargetMembershipID: targetMembershipID,
		Action: policy.ActionStartChat, ScopeAllowed: true, ResourceActive: true}
	if err := auditDecision(ctx, tx, req, decision, at); err != nil {
		return DirectConversation{}, errors.Join(ErrAuditUnavailable, err)
	}
	if !actorFound || !memberActiveAt(actor, at) {
		if err := finishConversationStart(ctx, tx, id, "", "deny", "invalid_identity", at); err != nil {
			return DirectConversation{}, err
		}
		return DirectConversation{}, ErrForbidden
	}
	if !decision.Allowed {
		if err := finishConversationStart(ctx, tx, id, "", "deny", string(decision.Reason), at); err != nil {
			return DirectConversation{}, err
		}
		return DirectConversation{}, ErrChatNotAvailable
	}
	lowUserID, highUserID := actorUserID, targetUserID
	lowMembershipID, highMembershipID := id.ActingMembershipID, targetMembershipID
	if strings.Compare(lowUserID, highUserID) > 0 {
		lowUserID, highUserID = highUserID, lowUserID
		lowMembershipID, highMembershipID = highMembershipID, lowMembershipID
	}
	conversation := DirectConversation{PolicyVersion: version,
		CrossLegal: actor.LegalEntityID != target.LegalEntityID, DecisionReason: decision.Reason}
	err = tx.QueryRow(ctx, `
INSERT INTO conversations (tenant_id,kind,status,direct_user_low_id,direct_user_high_id,
 direct_low_membership_id,direct_high_membership_id,last_policy_version,created_by_user_id,updated_at)
VALUES ($1,'direct','active',$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (tenant_id,direct_user_low_id,direct_user_high_id) WHERE kind='direct'
DO UPDATE SET direct_low_membership_id=EXCLUDED.direct_low_membership_id,
 direct_high_membership_id=EXCLUDED.direct_high_membership_id,
 last_policy_version=EXCLUDED.last_policy_version,updated_at=EXCLUDED.updated_at
WHERE conversations.status='active'
RETURNING id::text,last_seq`, id.TenantID, lowUserID, highUserID, lowMembershipID,
		highMembershipID, version, actorUserID, at).Scan(&conversation.ID, &conversation.LastSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := finishConversationStart(ctx, tx, id, "", "deny", "conversation_unavailable", at); err != nil {
			return DirectConversation{}, err
		}
		return DirectConversation{}, ErrChatNotAvailable
	}
	if err != nil {
		return DirectConversation{}, err
	}
	if err := finishConversationStart(ctx, tx, id, conversation.ID, "allow", string(decision.Reason), at); err != nil {
		return DirectConversation{}, err
	}
	return conversation, nil
}

func conversationMembershipUser(ctx context.Context, tx pgx.Tx, tenantID, membershipID string) (string, error) {
	var userID string
	err := tx.QueryRow(ctx, `SELECT user_id::text FROM user_organizations
WHERE tenant_id=$1 AND id=$2`, tenantID, membershipID).Scan(&userID)
	return userID, err
}
