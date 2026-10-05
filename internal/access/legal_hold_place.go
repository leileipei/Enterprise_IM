package access

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s Service) PlaceLegalHold(ctx context.Context, id TrustedIdentity,
	conversationID, requestID, caseReference string) (LegalHold, bool, error) {
	if !legalHoldUUIDPattern.MatchString(conversationID) ||
		!legalHoldUUIDPattern.MatchString(requestID) || !validLegalHoldReference(caseReference) {
		return LegalHold{}, false, ErrInvalidLegalHold
	}
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return LegalHold{}, false, ErrInvalidIdentity
	}
	conversationID, requestID = strings.ToLower(conversationID), strings.ToLower(requestID)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return LegalHold{}, false, err
	}
	defer tx.Rollback(ctx)
	if err := lockLegalHoldRequest(ctx, tx, id.TenantID, requestID); err != nil {
		return LegalHold{}, false, err
	}
	if err := lockLegalHoldActor(ctx, tx, id); err != nil {
		return LegalHold{}, false, err
	}
	at := s.currentTime()
	if err := s.legalHoldGrant(ctx, tx, id, conversationID, "legal_hold_place", at); err != nil {
		return LegalHold{}, false, err
	}
	err = lockLegalHoldConversation(ctx, tx, id.TenantID, conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return LegalHold{}, false, deny(ctx, tx, id, "legal_hold_place", "conversation",
			conversationID, "conversation_unavailable", at, ErrNotFound)
	}
	if err != nil {
		return LegalHold{}, false, err
	}
	if fresh := s.currentTime(); fresh.After(at) {
		at = fresh
	}
	if err := s.legalHoldGrant(ctx, tx, id, conversationID, "legal_hold_place", at); err != nil {
		return LegalHold{}, false, err
	}
	var eventType, eventConversation, eventHold, eventReference, eventActor, eventMembership string
	err = tx.QueryRow(ctx, `SELECT event_type,conversation_id::text,hold_id::text,
 reference,actor_user_id::text,acting_membership_id::text
 FROM conversation_legal_hold_events WHERE tenant_id=$1 AND request_id=$2`,
		id.TenantID, requestID).Scan(&eventType, &eventConversation, &eventHold,
		&eventReference, &eventActor, &eventMembership)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return LegalHold{}, false, err
	}
	if err == nil {
		if eventType != "placed" || eventConversation != conversationID ||
			eventReference != caseReference || !strings.EqualFold(eventActor, id.UserID) ||
			!strings.EqualFold(eventMembership, id.ActingMembershipID) {
			return LegalHold{}, false, deny(ctx, tx, id, "legal_hold_place", "conversation",
				conversationID, "request_conflict", at, ErrConflict)
		}
		hold, err := loadLegalHold(ctx, tx, id.TenantID, conversationID, eventHold)
		if err != nil {
			return LegalHold{}, false, err
		}
		if err := audit(ctx, tx, id, "legal_hold_place", "conversation", conversationID,
			"allow", "replay", at); err != nil {
			return LegalHold{}, false, errors.Join(ErrAuditUnavailable, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return LegalHold{}, false, err
		}
		return hold, false, nil
	}
	var cleanupInProgress bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM file_delete_versions WHERE tenant_id=$1 AND conversation_id=$2 AND phase IN ('committed','uncertain'))`, id.TenantID, conversationID).Scan(&cleanupInProgress)
	if err != nil {
		return LegalHold{}, false, err
	}
	if cleanupInProgress {
		return LegalHold{}, false, deny(ctx, tx, id, "legal_hold_place", "conversation", conversationID, "file_cleanup_in_progress", at, ErrFileCleanupInProgress)
	}
	var activeHold string
	err = tx.QueryRow(ctx, `SELECT id::text FROM conversation_legal_holds
 WHERE tenant_id=$1 AND conversation_id=$2 AND case_reference=$3 AND released_at IS NULL`,
		id.TenantID, conversationID, caseReference).Scan(&activeHold)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return LegalHold{}, false, err
	}
	if err == nil {
		return LegalHold{}, false, deny(ctx, tx, id, "legal_hold_place", "conversation",
			conversationID, "active_case_exists", at, ErrConflict)
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT legal_hold_write`); err != nil {
		return LegalHold{}, false, err
	}
	var holdID string
	err = tx.QueryRow(ctx, `INSERT INTO conversation_legal_holds
 (tenant_id,conversation_id,case_reference,create_request_id,
 placed_by_user_id,placed_by_membership_id,placed_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id::text`, id.TenantID, conversationID,
		caseReference, requestID, id.UserID, id.ActingMembershipID, at).Scan(&holdID)
	if err != nil {
		return LegalHold{}, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO conversation_legal_hold_events
 (tenant_id,conversation_id,hold_id,event_type,request_id,reference,
 actor_user_id,acting_membership_id,occurred_at)
 VALUES ($1,$2,$3,'placed',$4,$5,$6,$7,$8)`, id.TenantID, conversationID,
		holdID, requestID, caseReference, id.UserID, id.ActingMembershipID, at)
	if err != nil {
		return LegalHold{}, false, err
	}
	if err := audit(ctx, tx, id, "legal_hold_place", "conversation", conversationID,
		"allow", "case_placed", at); err != nil {
		return LegalHold{}, false, errors.Join(ErrAuditUnavailable, err)
	}
	if fresh := s.currentTime(); fresh.After(at) {
		grant, err := s.resolve(ctx, tx, id, fresh)
		if errors.Is(err, ErrInvalidIdentity) {
			return LegalHold{}, false, denyLegalHoldAfterWrite(ctx, tx, id, "legal_hold_place",
				conversationID, "invalid_identity", fresh, ErrInvalidIdentity)
		}
		if err != nil {
			return LegalHold{}, false, err
		}
		if !grant.all {
			return LegalHold{}, false, denyLegalHoldAfterWrite(ctx, tx, id, "legal_hold_place",
				conversationID, "not_group_admin", fresh, ErrNotFound)
		}
	}
	hold, err := loadLegalHold(ctx, tx, id.TenantID, conversationID, holdID)
	if err != nil {
		return LegalHold{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LegalHold{}, false, err
	}
	return hold, true, nil
}
