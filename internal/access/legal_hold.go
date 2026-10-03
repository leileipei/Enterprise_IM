package access

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidLegalHold = errors.New("invalid legal hold request")

var legalHoldUUIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type LegalHold struct {
	ID                       string
	ConversationID           string
	CaseReference            string
	PlacedByUserID           string
	PlacedByMembershipID     string
	PlacedAt                 time.Time
	ReleaseApprovalReference string
	ReleasedByUserID         string
	ReleasedByMembershipID   string
	ReleasedAt               *time.Time
}

func validLegalHoldReference(reference string) bool {
	return utf8.ValidString(reference) && utf8.RuneCountInString(reference) >= 1 &&
		utf8.RuneCountInString(reference) <= 128 && strings.TrimSpace(reference) == reference &&
		strings.IndexFunc(reference, unicode.IsControl) < 0
}

func scanLegalHold(row pgx.Row) (LegalHold, error) {
	var hold LegalHold
	var approval, releasedBy, releasedMembership *string
	err := row.Scan(&hold.ID, &hold.ConversationID, &hold.CaseReference,
		&hold.PlacedByUserID, &hold.PlacedByMembershipID, &hold.PlacedAt,
		&approval, &releasedBy, &releasedMembership, &hold.ReleasedAt)
	if err != nil {
		return LegalHold{}, err
	}
	if approval != nil {
		hold.ReleaseApprovalReference = *approval
	}
	if releasedBy != nil {
		hold.ReleasedByUserID = *releasedBy
	}
	if releasedMembership != nil {
		hold.ReleasedByMembershipID = *releasedMembership
	}
	return hold, nil
}

func loadLegalHold(ctx context.Context, tx pgx.Tx, tenantID, conversationID, holdID string) (LegalHold, error) {
	return scanLegalHold(tx.QueryRow(ctx, `SELECT id::text,conversation_id::text,case_reference,
 placed_by_user_id::text,placed_by_membership_id::text,placed_at,
 release_approval_reference,released_by_user_id::text,released_by_membership_id::text,released_at
 FROM conversation_legal_holds WHERE tenant_id=$1 AND conversation_id=$2 AND id=$3`,
		tenantID, conversationID, holdID))
}

func lockLegalHoldActor(ctx context.Context, tx pgx.Tx, id TrustedIdentity) error {
	var membershipID string
	err := tx.QueryRow(ctx, `SELECT id::text FROM user_organizations
 WHERE tenant_id=$1 AND id=$2 AND user_id=$3 FOR SHARE`, id.TenantID,
		id.ActingMembershipID, id.UserID).Scan(&membershipID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidIdentity
	}
	return err
}

func lockLegalHoldRequest(ctx context.Context, tx pgx.Tx, tenantID, requestID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(
 hashtextextended('im_legal_hold_req:' || $1::text || ':' || $2::text, 0))`, tenantID, requestID)
	return err
}

func (s Service) legalHoldGrant(ctx context.Context, tx pgx.Tx, id TrustedIdentity,
	conversationID, action string, at time.Time) error {
	grant, err := s.resolve(ctx, tx, id, at)
	if errors.Is(err, ErrInvalidIdentity) {
		return deny(ctx, tx, id, action, "conversation", conversationID,
			"invalid_identity", at, ErrInvalidIdentity)
	}
	if err != nil {
		return err
	}
	if !grant.all {
		return deny(ctx, tx, id, action, "conversation", conversationID,
			"not_group_admin", at, ErrNotFound)
	}
	return nil
}

func lockLegalHoldConversation(ctx context.Context, tx pgx.Tx, tenantID, conversationID string) error {
	var found string
	err := tx.QueryRow(ctx, `SELECT id::text FROM conversations
 WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantID, conversationID).Scan(&found)
	return err
}

func denyLegalHoldAfterWrite(ctx context.Context, tx pgx.Tx, id TrustedIdentity,
	action, conversationID, reason string, at time.Time, result error) error {
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT legal_hold_write`); err != nil {
		return err
	}
	return deny(ctx, tx, id, action, "conversation", conversationID, reason, at, result)
}
