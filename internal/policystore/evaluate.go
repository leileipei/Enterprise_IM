package policystore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
)

var ErrPolicyUnavailable = errors.New("current policy unavailable")

type Request struct {
	Identity           access.TrustedIdentity
	TargetMembershipID string
	Action             policy.Action
	ScopeAllowed       bool
	ResourceActive     bool
}

func loadMembership(ctx context.Context, tx pgx.Tx, tenantID, membershipID, actorUserID string) (policy.Membership, bool, error) {
	return loadMembershipWithLock(ctx, tx, tenantID, membershipID, actorUserID, true)
}

func loadMembershipSnapshot(ctx context.Context, tx pgx.Tx, tenantID, membershipID, actorUserID string) (policy.Membership, bool, error) {
	return loadMembershipWithLock(ctx, tx, tenantID, membershipID, actorUserID, false)
}

func loadMembershipWithLock(ctx context.Context, tx pgx.Tx, tenantID, membershipID, actorUserID string, lock bool) (policy.Membership, bool, error) {
	query := `
SELECT m.id,m.tenant_id,m.organization_id,o.legal_entity_id,u.status,
 CASE WHEN t.status='active' AND o.status='active' AND l.status='active' THEN m.status ELSE 'suspended' END,
 m.effective_from,m.effective_to
FROM user_organizations m
JOIN users u ON u.tenant_id=m.tenant_id AND u.id=m.user_id
JOIN organizations o ON o.tenant_id=m.tenant_id AND o.id=m.organization_id
JOIN legal_entities l ON l.tenant_id=o.tenant_id AND l.id=o.legal_entity_id
JOIN tenants t ON t.id=m.tenant_id
WHERE m.tenant_id=$1 AND m.id=$2`
	args := []any{tenantID, membershipID}
	if actorUserID != "" {
		query += " AND u.id=$3"
		args = append(args, actorUserID)
	}
	if lock {
		query += " FOR SHARE OF t,u,m,o,l"
	}
	var member policy.Membership
	var end *time.Time
	err := tx.QueryRow(ctx, query, args...).Scan(&member.ID, &member.TenantID, &member.OrganizationID,
		&member.LegalEntityID, &member.AccountStatus, &member.Status, &member.EffectiveFrom, &end)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy.Membership{}, false, nil
	}
	if err != nil {
		return policy.Membership{}, false, err
	}
	if end != nil {
		member.EffectiveTo = *end
	}
	return member, true, nil
}

func currentVersion(ctx context.Context, tx pgx.Tx, tenantID string) (int64, error) {
	var version int64
	err := tx.QueryRow(ctx, `
SELECT pc.current_version FROM policy_current pc
JOIN policy_versions pv ON pv.tenant_id=pc.tenant_id AND pv.version=pc.current_version
WHERE pc.tenant_id=$1 AND pv.status='published'`, tenantID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM policy_current WHERE tenant_id=$1)", tenantID).Scan(&exists); err != nil {
			return 0, err
		}
		if exists {
			return 0, ErrPolicyUnavailable
		}
		return 0, nil
	}
	return version, err
}

func loadRules(ctx context.Context, tx pgx.Tx, tenantID string, version int64) ([]policy.Rule, error) {
	if version == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
SELECT rule_id,effect,action,
 COALESCE(source_organization_id::text,''),COALESCE(target_organization_id::text,''),
 COALESCE(source_membership_id::text,''),COALESCE(target_membership_id::text,''),
 bidirectional,COALESCE(override_rule_id,''),COALESCE(requested_by_user_id::text,''),
 COALESCE(approved_by_user_id::text,''),cross_legal_approved,reason,effective_from,effective_to
FROM policy_rules WHERE tenant_id=$1 AND version=$2 ORDER BY rule_id`, tenantID, version)
	if err != nil {
		return nil, err
	}
	var rules []policy.Rule
	for rows.Next() {
		var r policy.Rule
		var end *time.Time
		r.TenantID = tenantID
		if err := rows.Scan(&r.ID, &r.Effect, &r.Action, &r.SourceOrganizationID,
			&r.TargetOrganizationID, &r.SourceMembershipID, &r.TargetMembershipID,
			&r.Bidirectional, &r.OverrideRuleID, &r.RequestedBy, &r.ApprovedBy,
			&r.CrossLegalApproved, &r.Reason, &r.EffectiveFrom, &end); err != nil {
			rows.Close()
			return nil, err
		}
		if end != nil {
			r.EffectiveTo = *end
		}
		rules = append(rules, r)
	}
	err = rows.Err()
	rows.Close()
	return rules, err
}

func auditDecision(ctx context.Context, tx pgx.Tx, req Request, decision policy.Decision, at time.Time) error {
	var version any
	if decision.PolicyVersion > 0 {
		version = decision.PolicyVersion
	}
	matched := decision.MatchedRuleIDs
	if matched == nil {
		matched = []string{}
	}
	overridden := decision.OverriddenRuleIDs
	if overridden == nil {
		overridden = []string{}
	}
	_, err := tx.Exec(ctx, `
INSERT INTO policy_decision_events (
 tenant_id,policy_version,actor_user_id,actor_membership_id,target_membership_id,
 action,allowed,reason,matched_rule_ids,overridden_rule_ids,occurred_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		req.Identity.TenantID, version, req.Identity.UserID, req.Identity.ActingMembershipID,
		req.TargetMembershipID, req.Action, decision.Allowed, decision.Reason,
		matched, overridden, at)
	return err
}

// EvaluateCurrent loads validated memberships and an immutable policy snapshot.
// ScopeAllowed and ResourceActive must come from the authenticated caller's own checks.
func (s Service) EvaluateCurrent(ctx context.Context, req Request) (policy.Decision, error) {
	if s.DB == nil || req.Identity.TenantID == "" || req.Identity.UserID == "" ||
		req.Identity.ActingMembershipID == "" || req.TargetMembershipID == "" {
		return policy.Decision{}, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return policy.Decision{}, err
	}
	defer tx.Rollback(ctx)
	locked, err := tx.Query(ctx, `
SELECT id FROM user_organizations WHERE tenant_id=$1 AND id IN ($2,$3)
ORDER BY id FOR SHARE`, req.Identity.TenantID, req.Identity.ActingMembershipID, req.TargetMembershipID)
	if err != nil {
		return policy.Decision{}, err
	}
	for locked.Next() {
	}
	err = locked.Err()
	locked.Close()
	if err != nil {
		return policy.Decision{}, err
	}
	at := s.now()
	actor, actorFound, err := loadMembership(ctx, tx, req.Identity.TenantID, req.Identity.ActingMembershipID, req.Identity.UserID)
	if err != nil {
		return policy.Decision{}, err
	}
	target, targetFound, err := loadMembership(ctx, tx, req.Identity.TenantID, req.TargetMembershipID, "")
	if err != nil {
		return policy.Decision{}, err
	}
	version, err := currentVersion(ctx, tx, req.Identity.TenantID)
	if err != nil {
		return policy.Decision{}, err
	}
	rules, err := loadRules(ctx, tx, req.Identity.TenantID, version)
	if err != nil {
		return policy.Decision{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	decision := policy.Decision{PolicyVersion: version, Reason: policy.ReasonInvalidContext}
	if actorFound && targetFound {
		decision = policy.Evaluate(policy.Input{
			Action: req.Action, Actor: actor, Target: target, At: at,
			ScopeAllowed: req.ScopeAllowed, ResourceActive: req.ResourceActive,
			PolicyVersion: version, Rules: rules,
		})
	}
	if err := auditDecision(ctx, tx, req, decision, at); err != nil {
		return policy.Decision{}, errors.Join(ErrAuditUnavailable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return policy.Decision{}, err
	}
	return decision, nil
}
