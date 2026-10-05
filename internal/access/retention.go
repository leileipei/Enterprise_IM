package access

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidRetentionPolicy = errors.New("invalid retention policy")

type RetentionPolicy struct {
	MessageBodyDays   int
	Version           int64
	ApprovalReference string
	ApprovedByUserID  string
	ApprovedAt        *time.Time
}

func validRetentionChange(expectedVersion int64, days int, reference string) bool {
	if expectedVersion < 0 || days < 1 || days > 3650 || reference == "" ||
		!utf8.ValidString(reference) || utf8.RuneCountInString(reference) > 128 {
		return false
	}
	return strings.TrimSpace(reference) == reference &&
		strings.IndexFunc(reference, unicode.IsControl) < 0
}

func (s Service) GetRetentionPolicy(ctx context.Context, id TrustedIdentity) (RetentionPolicy, error) {
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return RetentionPolicy{}, ErrInvalidIdentity
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return RetentionPolicy{}, err
	}
	defer tx.Rollback(ctx)
	at := s.currentTime()
	grant, err := s.resolve(ctx, tx, id, at)
	if err != nil {
		return RetentionPolicy{}, err
	}
	if !grant.all {
		return RetentionPolicy{}, deny(ctx, tx, id, "retention_policy_read", "tenant", id.TenantID,
			"not_group_admin", at, ErrNotFound)
	}
	var policy RetentionPolicy
	err = tx.QueryRow(ctx, `SELECT message_body_retention_days,retention_version,
 COALESCE(retention_approval_reference,''),COALESCE(retention_approved_by_user_id::text,''),
 retention_approved_at FROM tenants WHERE id=$1 FOR SHARE`, id.TenantID).
		Scan(&policy.MessageBodyDays, &policy.Version, &policy.ApprovalReference,
			&policy.ApprovedByUserID, &policy.ApprovedAt)
	if err != nil {
		return RetentionPolicy{}, err
	}
	if fresh := s.currentTime(); fresh.After(at) {
		at = fresh
		grant, err = s.resolve(ctx, tx, id, at)
		if err != nil {
			return RetentionPolicy{}, err
		}
		if !grant.all {
			return RetentionPolicy{}, deny(ctx, tx, id, "retention_policy_read", "tenant", id.TenantID,
				"not_group_admin", at, ErrNotFound)
		}
	}
	if err := audit(ctx, tx, id, "retention_policy_read", "tenant", id.TenantID,
		"allow", "retention_policy", at); err != nil {
		return RetentionPolicy{}, errors.Join(ErrAuditUnavailable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RetentionPolicy{}, err
	}
	return policy, nil
}

func (s Service) SetRetentionPolicy(ctx context.Context, id TrustedIdentity,
	expectedVersion int64, days int, approvalReference string) (RetentionPolicy, error) {
	if !validRetentionChange(expectedVersion, days, approvalReference) {
		return RetentionPolicy{}, ErrInvalidRetentionPolicy
	}
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return RetentionPolicy{}, ErrInvalidIdentity
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return RetentionPolicy{}, err
	}
	defer tx.Rollback(ctx)
	// Serialize policy writers without locking the tenant row ahead of the
	// actor membership: EndMembership takes those row locks in that order.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('im_retention:' || $1::text, 0))`,
		id.TenantID); err != nil {
		return RetentionPolicy{}, err
	}
	var actorMembershipID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM user_organizations
 WHERE tenant_id=$1 AND id=$2 AND user_id=$3 FOR SHARE`, id.TenantID,
		id.ActingMembershipID, id.UserID).Scan(&actorMembershipID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RetentionPolicy{}, ErrInvalidIdentity
	}
	if err != nil {
		return RetentionPolicy{}, err
	}
	var version int64
	var currentDays int
	err = tx.QueryRow(ctx, `SELECT retention_version,message_body_retention_days
 FROM tenants WHERE id=$1 FOR UPDATE`, id.TenantID).Scan(&version, &currentDays)
	if errors.Is(err, pgx.ErrNoRows) {
		return RetentionPolicy{}, ErrInvalidIdentity
	}
	if err != nil {
		return RetentionPolicy{}, err
	}
	at := s.currentTime()
	grant, err := s.resolve(ctx, tx, id, at)
	if err != nil {
		return RetentionPolicy{}, err
	}
	if !grant.all {
		return RetentionPolicy{}, deny(ctx, tx, id, "retention_policy_update", "tenant", id.TenantID,
			"not_group_admin", at, ErrNotFound)
	}
	if version != expectedVersion {
		return RetentionPolicy{}, deny(ctx, tx, id, "retention_policy_update", "tenant", id.TenantID,
			"version_conflict", at, ErrConflict)
	}
	if days > currentDays {
		var hasMessages bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM messages WHERE tenant_id=$1)`, id.TenantID).Scan(&hasMessages); err != nil {
			return RetentionPolicy{}, err
		}
		if hasMessages {
			return RetentionPolicy{}, deny(ctx, tx, id, "retention_policy_update", "tenant", id.TenantID,
				"retention_extension_requires_empty_history", at, ErrConflict)
		}
	}
	policy := RetentionPolicy{MessageBodyDays: days, Version: version + 1,
		ApprovalReference: approvalReference, ApprovedByUserID: id.UserID}
	at = s.currentTime()
	grant, err = s.resolve(ctx, tx, id, at)
	if err != nil {
		return RetentionPolicy{}, err
	}
	if !grant.all {
		return RetentionPolicy{}, deny(ctx, tx, id, "retention_policy_update", "tenant", id.TenantID,
			"not_group_admin", at, ErrNotFound)
	}
	policy.ApprovedAt = &at
	_, err = tx.Exec(ctx, `UPDATE tenants SET message_body_retention_days=$2,
 retention_version=$3,retention_approval_reference=$4,
 retention_approved_by_user_id=$5,retention_approved_at=$6 WHERE id=$1`,
		id.TenantID, days, policy.Version, approvalReference, id.UserID, at)
	if err != nil {
		return RetentionPolicy{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO tenant_retention_policy_history
 (tenant_id,version,message_body_retention_days,approval_reference,approved_by_user_id,approved_at)
 VALUES ($1,$2,$3,$4,$5,$6)`, id.TenantID, policy.Version, days,
		approvalReference, id.UserID, at)
	if err != nil {
		return RetentionPolicy{}, err
	}
	if err := audit(ctx, tx, id, "retention_policy_update", "tenant", id.TenantID,
		"allow", "approved_retention_change", at); err != nil {
		return RetentionPolicy{}, errors.Join(ErrAuditUnavailable, err)
	}
	if fresh := s.currentTime(); fresh.After(at) {
		grant, err = s.resolve(ctx, tx, id, fresh)
		if err != nil || !grant.all {
			if err != nil {
				return RetentionPolicy{}, err
			}
			return RetentionPolicy{}, ErrNotFound
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return RetentionPolicy{}, err
	}
	return policy, nil
}
