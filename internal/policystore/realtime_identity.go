package policystore

import (
	"context"
	"errors"
	"strings"

	"github.com/leileipei/Enterprise_IM/internal/access"
)

type RealtimeAction string

const (
	RealtimeTicket  RealtimeAction = "realtime_ticket"
	RealtimeConnect RealtimeAction = "realtime_connect"
)

func canonicalRealtimeIdentity(id access.TrustedIdentity) (access.TrustedIdentity, bool) {
	if !directoryUUIDPattern.MatchString(id.TenantID) || !directoryUUIDPattern.MatchString(id.UserID) ||
		!directoryUUIDPattern.MatchString(id.ActingMembershipID) {
		return access.TrustedIdentity{}, false
	}
	id.TenantID = strings.ToLower(id.TenantID)
	id.UserID = strings.ToLower(id.UserID)
	id.ActingMembershipID = strings.ToLower(id.ActingMembershipID)
	return id, true
}

// AuthorizeRealtime checks the selected membership again at ticket issuance
// and at ticket consumption. Its audit and decision commit together.
func (s Service) AuthorizeRealtime(ctx context.Context, id access.TrustedIdentity, action RealtimeAction) error {
	if s.DB == nil || (action != RealtimeTicket && action != RealtimeConnect) {
		return ErrForbidden
	}
	id, valid := canonicalRealtimeIdentity(id)
	if !valid {
		return ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	at := s.now()
	member, found, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	allowed := found && memberActiveAt(member, at)
	outcome, reason := "allow", "active_identity"
	if !allowed {
		outcome, reason = "deny", "invalid_identity"
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events
 (tenant_id,actor_user_id,acting_membership_id,action,resource_type,outcome,reason,occurred_at)
VALUES ($1,$2,$3,$4,'realtime_connection',$5,$6,$7)`, id.TenantID, id.UserID,
		id.ActingMembershipID, action, outcome, reason, at)
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}

// RealtimeIdentityActive rechecks an existing socket without creating an
// audit row on every heartbeat. The connection is closed when false or on DB
// failure; messages are never sent based on a stale cached identity.
func (s Service) RealtimeIdentityActive(ctx context.Context, id access.TrustedIdentity) (bool, error) {
	if s.DB == nil {
		return false, ErrForbidden
	}
	id, valid := canonicalRealtimeIdentity(id)
	if !valid {
		return false, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	member, found, err := loadMembershipSnapshot(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return false, err
	}
	active := found && memberActiveAt(member, s.now())
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return active, nil
}
