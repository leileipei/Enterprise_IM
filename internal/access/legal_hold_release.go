package access

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s Service) ReleaseLegalHold(ctx context.Context, id TrustedIdentity,
	conversationID, holdID, requestID, approvalReference string) (LegalHold, error) {
	if !legalHoldUUIDPattern.MatchString(conversationID) ||
		!legalHoldUUIDPattern.MatchString(holdID) ||
		!legalHoldUUIDPattern.MatchString(requestID) ||
		!validLegalHoldReference(approvalReference) {
		return LegalHold{}, ErrInvalidLegalHold
	}
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return LegalHold{}, ErrInvalidIdentity
	}
	conversationID, holdID, requestID = strings.ToLower(conversationID),
		strings.ToLower(holdID), strings.ToLower(requestID)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return LegalHold{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockLegalHoldRequest(ctx, tx, id.TenantID, requestID); err != nil {
		return LegalHold{}, err
	}
	if err := lockLegalHoldActor(ctx, tx, id); err != nil {
		return LegalHold{}, err
	}
	at := s.currentTime()
	if err := s.legalHoldGrant(ctx, tx, id, conversationID, "legal_hold_release", at); err != nil {
		return LegalHold{}, err
	}
	err = lockLegalHoldConversation(ctx, tx, id.TenantID, conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return LegalHold{}, deny(ctx, tx, id, "legal_hold_release", "conversation",
			conversationID, "conversation_unavailable", at, ErrNotFound)
	}
	if err != nil {
		return LegalHold{}, err
	}
	if fresh := s.currentTime(); fresh.After(at) {
		at = fresh
	}
	if err := s.legalHoldGrant(ctx, tx, id, conversationID, "legal_hold_release", at); err != nil {
		return LegalHold{}, err
	}
	var eventType, eventConversation, eventHold, eventReference, eventActor, eventMembership string
	err = tx.QueryRow(ctx, `SELECT event_type,conversation_id::text,hold_id::text,
 reference,actor_user_id::text,acting_membership_id::text
 FROM conversation_legal_hold_events WHERE tenant_id=$1 AND request_id=$2`,
		id.TenantID, requestID).Scan(&eventType, &eventConversation, &eventHold,
		&eventReference, &eventActor, &eventMembership)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return LegalHold{}, err
	}
	if err == nil {
		if eventType != "released" || eventConversation != conversationID ||
			eventHold != holdID || eventReference != approvalReference ||
			!strings.EqualFold(eventActor, id.UserID) ||
			!strings.EqualFold(eventMembership, id.ActingMembershipID) {
			return LegalHold{}, deny(ctx, tx, id, "legal_hold_release", "conversation",
				conversationID, "request_conflict", at, ErrConflict)
		}
		hold, err := loadLegalHold(ctx, tx, id.TenantID, conversationID, holdID)
		if err != nil {
			return LegalHold{}, err
		}
		if err := audit(ctx, tx, id, "legal_hold_release", "conversation", conversationID,
			"allow", "replay", at); err != nil {
			return LegalHold{}, errors.Join(ErrAuditUnavailable, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return LegalHold{}, err
		}
		return hold, nil
	}
	var releasedAt *string
	err = tx.QueryRow(ctx, `SELECT released_at::text FROM conversation_legal_holds
 WHERE tenant_id=$1 AND conversation_id=$2 AND id=$3 FOR UPDATE`,
		id.TenantID, conversationID, holdID).Scan(&releasedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LegalHold{}, deny(ctx, tx, id, "legal_hold_release", "legal_hold",
			holdID, "hold_unavailable", at, ErrNotFound)
	}
	if err != nil {
		return LegalHold{}, err
	}
	if releasedAt != nil {
		return LegalHold{}, deny(ctx, tx, id, "legal_hold_release", "legal_hold",
			holdID, "already_released", at, ErrConflict)
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT legal_hold_write`); err != nil {
		return LegalHold{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE conversation_legal_holds SET
	 release_approval_reference=$4,release_request_id=$5,released_by_user_id=$6,
	 released_by_membership_id=$7,released_at=$8
	 WHERE tenant_id=$1 AND conversation_id=$2 AND id=$3 AND released_at IS NULL`,
		id.TenantID, conversationID, holdID, approvalReference, requestID, id.UserID,
		id.ActingMembershipID, at)
	if err != nil {
		return LegalHold{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO conversation_legal_hold_events
 (tenant_id,conversation_id,hold_id,event_type,request_id,reference,
 actor_user_id,acting_membership_id,occurred_at)
 VALUES ($1,$2,$3,'released',$4,$5,$6,$7,$8)`,
		id.TenantID, conversationID, holdID, requestID, approvalReference,
		id.UserID, id.ActingMembershipID, at)
	if err != nil {
		return LegalHold{}, err
	}
	if err := audit(ctx, tx, id, "legal_hold_release", "conversation",
		conversationID, "allow", "case_released", at); err != nil {
		return LegalHold{}, errors.Join(ErrAuditUnavailable, err)
	}
	if fresh := s.currentTime(); fresh.After(at) {
		grant, err := s.resolve(ctx, tx, id, fresh)
		if errors.Is(err, ErrInvalidIdentity) {
			return LegalHold{}, denyLegalHoldAfterWrite(ctx, tx, id, "legal_hold_release",
				conversationID, "invalid_identity", fresh, ErrInvalidIdentity)
		}
		if err != nil {
			return LegalHold{}, err
		}
		if !grant.all {
			return LegalHold{}, denyLegalHoldAfterWrite(ctx, tx, id, "legal_hold_release",
				conversationID, "not_group_admin", fresh, ErrNotFound)
		}
	}
	hold, err := loadLegalHold(ctx, tx, id.TenantID, conversationID, holdID)
	if err != nil {
		return LegalHold{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LegalHold{}, err
	}
	return hold, nil
}
