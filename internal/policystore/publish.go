package policystore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
)

var (
	ErrForbidden        = errors.New("policy publication forbidden")
	ErrInvalidRules     = errors.New("invalid policy snapshot")
	ErrVersionConflict  = errors.New("policy version conflict")
	ErrAuditUnavailable = errors.New("policy audit unavailable")
)

type Service struct {
	DB                   access.Beginner
	Now                  func() time.Time
	MessageRatePerSecond int
}

func (s Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func nullableID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

func nullableTime(at time.Time) any {
	if at.IsZero() {
		return nil
	}
	return at
}

func validAction(action policy.Action) bool {
	switch action {
	case policy.ActionDirectoryView, policy.ActionStartChat, policy.ActionSendMessage,
		policy.ActionCreateGroup, policy.ActionInviteGroup:
		return true
	default:
		return false
	}
}

func validateRules(tenantID string, rules []policy.Rule) bool {
	byID := make(map[string]policy.Rule, len(rules))
	for _, r := range rules {
		if strings.TrimSpace(r.ID) == "" || r.TenantID != tenantID || !validAction(r.Action) ||
			strings.TrimSpace(r.Reason) == "" || r.EffectiveFrom.IsZero() ||
			(!r.EffectiveTo.IsZero() && !r.EffectiveTo.After(r.EffectiveFrom)) ||
			(r.SourceMembershipID != "" && r.SourceOrganizationID == "") ||
			(r.TargetMembershipID != "" && r.TargetOrganizationID == "") {
			return false
		}
		if _, exists := byID[r.ID]; exists {
			return false
		}
		switch r.Effect {
		case policy.EffectHardDeny, policy.EffectIsolate:
			if r.OverrideRuleID != "" || r.CrossLegalApproved {
				return false
			}
		case policy.EffectAllow:
			if r.SourceOrganizationID == "" || r.TargetOrganizationID == "" ||
				r.RequestedBy == "" || r.ApprovedBy == "" || r.EffectiveTo.IsZero() ||
				r.OverrideRuleID != "" {
				return false
			}
		case policy.EffectExceptionAllow:
			if r.SourceMembershipID == "" || r.TargetMembershipID == "" ||
				r.RequestedBy == "" || r.ApprovedBy == "" || r.OverrideRuleID == "" ||
				r.EffectiveTo.IsZero() {
				return false
			}
		default:
			return false
		}
		byID[r.ID] = r
	}
	for _, r := range rules {
		if r.Effect == policy.EffectExceptionAllow {
			covered, ok := byID[r.OverrideRuleID]
			if !ok || covered.Effect != policy.EffectIsolate || covered.Action != r.Action {
				return false
			}
		}
	}
	return true
}

func authorizedPublisher(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, at time.Time) (bool, error) {
	var grantID string
	err := tx.QueryRow(ctx, `
SELECT g.id FROM tenants t
 JOIN users u ON u.tenant_id=t.id
 JOIN user_organizations m ON m.tenant_id=u.tenant_id AND m.user_id=u.id
 JOIN organizations o ON o.tenant_id=m.tenant_id AND o.id=m.organization_id
 JOIN legal_entities l ON l.tenant_id=o.tenant_id AND l.id=o.legal_entity_id
 JOIN admin_grants g ON g.tenant_id=m.tenant_id AND g.membership_id=m.id
 WHERE t.id=$1 AND u.id=$2 AND m.id=$3 AND t.status='active' AND u.status='active'
   AND m.status='active' AND o.status='active' AND l.status='active'
   AND m.effective_from <= $4 AND (m.effective_to IS NULL OR $4 < m.effective_to)
   AND g.role='group_admin' AND g.status='active'
   AND g.effective_from <= $4 AND (g.effective_to IS NULL OR $4 < g.effective_to)
LIMIT 1 FOR SHARE OF u,m,o,l,g`, id.TenantID, id.UserID, id.ActingMembershipID, at).Scan(&grantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func auditPublication(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `
INSERT INTO audit_events (tenant_id,actor_user_id,acting_membership_id,action,resource_type,outcome,reason,occurred_at)
VALUES ($1,$2,$3,'policy_publish','policy_version',$4,$5,$6)`,
		id.TenantID, id.UserID, id.ActingMembershipID, outcome, reason, at)
	return err
}

func denyPublication(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, reason string, at time.Time, result error) error {
	if err := auditPublication(ctx, tx, id, "deny", reason, at); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return result
}

// Publish stores a complete immutable snapshot and atomically advances the
// tenant's current version. A conflict returns the actual current version.
func (s Service) Publish(ctx context.Context, id access.TrustedIdentity, expectedVersion int64, rules []policy.Rule, reason string) (int64, error) {
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" ||
		expectedVersion < 0 || strings.TrimSpace(reason) == "" {
		return 0, ErrInvalidRules
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	// Match the membership-first lock order used by access.EndMembership.
	var actorMembership string
	err = tx.QueryRow(ctx, `SELECT id FROM user_organizations
WHERE tenant_id=$1 AND id=$2 FOR SHARE`, id.TenantID, id.ActingMembershipID).Scan(&actorMembership)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	var tenantID string
	err = tx.QueryRow(ctx, "SELECT id FROM tenants WHERE id=$1 FOR UPDATE", id.TenantID).Scan(&tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrForbidden
	}
	if err != nil {
		return 0, err
	}
	at := s.now()
	authorized, err := authorizedPublisher(ctx, tx, id, at)
	if err != nil {
		return 0, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
		authorized, err = authorizedPublisher(ctx, tx, id, at)
		if err != nil {
			return 0, err
		}
	}
	if !authorized {
		return 0, denyPublication(ctx, tx, id, "not_group_admin", at, ErrForbidden)
	}
	var current int64
	err = tx.QueryRow(ctx, "SELECT current_version FROM policy_current WHERE tenant_id=$1", id.TenantID).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if current != expectedVersion {
		return current, denyPublication(ctx, tx, id, "version_conflict", at, ErrVersionConflict)
	}
	if !validateRules(id.TenantID, rules) {
		return current, denyPublication(ctx, tx, id, "invalid_rules", at, ErrInvalidRules)
	}
	next := current + 1
	if _, err := tx.Exec(ctx, `
INSERT INTO policy_versions (tenant_id,version,status,published_by_user_id,reason)
VALUES ($1,$2,'draft',$3,$4)`, id.TenantID, next, id.UserID, reason); err != nil {
		return 0, err
	}
	for _, r := range rules {
		if _, err := tx.Exec(ctx, `
INSERT INTO policy_rules (
 tenant_id,version,rule_id,effect,action,source_organization_id,target_organization_id,
 source_membership_id,target_membership_id,bidirectional,override_rule_id,
 requested_by_user_id,approved_by_user_id,cross_legal_approved,reason,effective_from,effective_to)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
			id.TenantID, next, r.ID, r.Effect, r.Action, nullableID(r.SourceOrganizationID),
			nullableID(r.TargetOrganizationID), nullableID(r.SourceMembershipID), nullableID(r.TargetMembershipID),
			r.Bidirectional, nullableID(r.OverrideRuleID), nullableID(r.RequestedBy), nullableID(r.ApprovedBy),
			r.CrossLegalApproved, r.Reason, r.EffectiveFrom, nullableTime(r.EffectiveTo)); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE policy_versions SET status='published',published_at=$3
WHERE tenant_id=$1 AND version=$2`, id.TenantID, next, at); err != nil {
		return 0, err
	}
	if current == 0 {
		_, err = tx.Exec(ctx, `INSERT INTO policy_current (tenant_id,current_version,updated_at) VALUES ($1,$2,$3)`, id.TenantID, next, at)
	} else {
		_, err = tx.Exec(ctx, `UPDATE policy_current SET current_version=$2,updated_at=$3 WHERE tenant_id=$1`, id.TenantID, next, at)
	}
	if err != nil {
		return 0, err
	}
	if err := auditPublication(ctx, tx, id, "allow", fmt.Sprintf("published_v%d", next), at); err != nil {
		return 0, errors.Join(ErrAuditUnavailable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return next, nil
}
