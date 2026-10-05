package access

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

// ValidateFileIdentity verifies the current employee identity without requiring
// an administrator grant or inspecting any tenant file configuration.
func (s Service) ValidateFileIdentity(ctx context.Context, id TrustedIdentity) error {
	if s.DB == nil || !legalHoldUUIDPattern.MatchString(id.TenantID) || !legalHoldUUIDPattern.MatchString(id.UserID) || !legalHoldUUIDPattern.MatchString(id.ActingMembershipID) {
		return ErrInvalidIdentity
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer done()
		_ = tx.Rollback(cleanup)
	}()
	if _, err = tx.Exec(ctx, `SELECT set_config('statement_timeout','4s',true),set_config('lock_timeout','1s',true)`); err != nil {
		return err
	}
	var from time.Time
	var until *time.Time
	err = tx.QueryRow(ctx, `SELECT m.effective_from,m.effective_to FROM tenants t
 JOIN users u ON u.tenant_id=t.id
 JOIN user_organizations m ON m.tenant_id=u.tenant_id AND m.user_id=u.id
 JOIN organizations o ON o.tenant_id=m.tenant_id AND o.id=m.organization_id
 JOIN legal_entities l ON l.tenant_id=o.tenant_id AND l.id=o.legal_entity_id
 WHERE t.id=$1 AND u.id=$2 AND m.id=$3 AND t.status='active'
 AND u.status='active' AND m.status='active' AND o.status='active' AND l.status='active'
 FOR SHARE OF t,u,m,o,l`, id.TenantID, id.UserID, id.ActingMembershipID).Scan(&from, &until)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidIdentity
	}
	if err != nil {
		return err
	}
	var fresh time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&fresh); err != nil {
		return err
	}
	if fresh.Before(from) || (until != nil && !fresh.Before(*until)) {
		return ErrInvalidIdentity
	}
	return tx.Commit(ctx)
}
