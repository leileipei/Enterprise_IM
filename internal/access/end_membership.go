package access

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// EndMembership closes exactly one organization membership and its current
// department assignments. It retains the group user and other memberships.
func (s Service) EndMembership(ctx context.Context, id TrustedIdentity, membershipID string) error {
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" || membershipID == "" {
		return ErrInvalidIdentity
	}
	at := s.currentTime()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	g, err := s.resolve(ctx, tx, id, at)
	if errors.Is(err, ErrInvalidIdentity) {
		return deny(ctx, tx, id, "membership_end", "membership", membershipID, "invalid_identity", at, ErrInvalidIdentity)
	}
	if err != nil {
		return err
	}
	var organizationID, status string
	var from time.Time
	var to *time.Time
	err = tx.QueryRow(ctx, `
SELECT organization_id,status,effective_from,effective_to FROM user_organizations
WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, id.TenantID, membershipID).Scan(&organizationID, &status, &from, &to)
	if errors.Is(err, pgx.ErrNoRows) {
		return deny(ctx, tx, id, "membership_end", "membership", membershipID, "not_visible", at, ErrNotFound)
	}
	if err != nil {
		return err
	}
	if !g.allows(organizationID) {
		return deny(ctx, tx, id, "membership_end", "membership", membershipID, "not_visible", at, ErrNotFound)
	}
	if status != "active" || !at.After(from) || (to != nil && !at.Before(*to)) {
		return deny(ctx, tx, id, "membership_end", "membership", membershipID, "not_active", at, ErrConflict)
	}
	var futureDepartment bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM user_departments
WHERE tenant_id=$1 AND user_organization_id=$2 AND effective_from >= $3)`,
		id.TenantID, membershipID, at).Scan(&futureDepartment); err != nil {
		return err
	}
	if futureDepartment {
		return deny(ctx, tx, id, "membership_end", "membership", membershipID, "future_department", at, ErrConflict)
	}
	if _, err := tx.Exec(ctx, `
UPDATE user_departments SET effective_to=$3,status='ended'
WHERE tenant_id=$1 AND user_organization_id=$2 AND effective_from < $3
  AND (effective_to IS NULL OR effective_to > $3)`, id.TenantID, membershipID, at); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
UPDATE user_organizations SET effective_to=$3,status='ended'
WHERE tenant_id=$1 AND id=$2`, id.TenantID, membershipID, at); err != nil {
		return err
	}
	if err := audit(ctx, tx, id, "membership_end", "membership", membershipID, "allow", "scope_granted", at); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return tx.Commit(ctx)
}
