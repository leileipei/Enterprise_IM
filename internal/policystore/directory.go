package policystore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
)

var ErrDirectoryNotVisible = errors.New("directory membership not visible")

type DirectoryMembership struct {
	UserID           string
	DisplayName      string
	EmployeeNo       string
	MembershipID     string
	OrganizationID   string
	OrganizationName string
	Title            string
	IsPrimary        bool
	Departments      []access.Department
}

func memberActiveAt(member policy.Membership, at time.Time) bool {
	return member.AccountStatus == "active" && member.Status == "active" &&
		!member.EffectiveFrom.IsZero() && !at.Before(member.EffectiveFrom) &&
		(member.EffectiveTo.IsZero() || at.Before(member.EffectiveTo))
}

// GetVisibleMembership evaluates ordinary directory visibility and reads only
// the selected target membership in the same transaction as its decision audit.
func (s Service) GetVisibleMembership(ctx context.Context, id access.TrustedIdentity, targetMembershipID string) (DirectoryMembership, error) {
	if s.DB == nil || id.TenantID == "" || id.UserID == "" ||
		id.ActingMembershipID == "" || targetMembershipID == "" {
		return DirectoryMembership{}, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return DirectoryMembership{}, err
	}
	defer tx.Rollback(ctx)
	locked, err := tx.Query(ctx, `
SELECT id FROM user_organizations WHERE tenant_id=$1 AND id IN ($2,$3)
ORDER BY id FOR SHARE`, id.TenantID, id.ActingMembershipID, targetMembershipID)
	if err != nil {
		return DirectoryMembership{}, err
	}
	for locked.Next() {
	}
	err = locked.Err()
	locked.Close()
	if err != nil {
		return DirectoryMembership{}, err
	}
	at := s.now()
	actor, actorFound, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return DirectoryMembership{}, err
	}
	target, targetFound, err := loadMembership(ctx, tx, id.TenantID, targetMembershipID, "")
	if err != nil {
		return DirectoryMembership{}, err
	}
	version, err := currentVersion(ctx, tx, id.TenantID)
	if err != nil {
		return DirectoryMembership{}, err
	}
	rules, err := loadRules(ctx, tx, id.TenantID, version)
	if err != nil {
		return DirectoryMembership{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	decision := policy.Decision{PolicyVersion: version, Reason: policy.ReasonInvalidContext}
	if actorFound && targetFound {
		decision = policy.Evaluate(policy.Input{Action: policy.ActionDirectoryView,
			Actor: actor, Target: target, At: at, ScopeAllowed: true, ResourceActive: true,
			PolicyVersion: version, Rules: rules})
	}
	var profile DirectoryMembership
	if decision.Allowed {
		profile, err = readDirectoryMembership(ctx, tx, id.TenantID, targetMembershipID, at)
		if err != nil {
			return DirectoryMembership{}, err
		}
	}
	req := Request{Identity: id, TargetMembershipID: targetMembershipID, Action: policy.ActionDirectoryView,
		ScopeAllowed: true, ResourceActive: true}
	if err := auditDecision(ctx, tx, req, decision, at); err != nil {
		return DirectoryMembership{}, errors.Join(ErrAuditUnavailable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return DirectoryMembership{}, err
	}
	if !actorFound || !memberActiveAt(actor, at) {
		return DirectoryMembership{}, ErrForbidden
	}
	if !decision.Allowed {
		return DirectoryMembership{}, ErrDirectoryNotVisible
	}
	return profile, nil
}

func readDirectoryMembership(ctx context.Context, tx pgx.Tx, tenantID, membershipID string, at time.Time) (DirectoryMembership, error) {
	var profile DirectoryMembership
	err := tx.QueryRow(ctx, `
SELECT u.id,u.display_name,u.global_employee_no,m.id,m.organization_id,o.name,
 COALESCE(m.title,''),m.is_primary
FROM user_organizations m
JOIN users u ON u.tenant_id=m.tenant_id AND u.id=m.user_id
JOIN organizations o ON o.tenant_id=m.tenant_id AND o.id=m.organization_id
WHERE m.tenant_id=$1 AND m.id=$2`, tenantID, membershipID).
		Scan(&profile.UserID, &profile.DisplayName, &profile.EmployeeNo,
			&profile.MembershipID, &profile.OrganizationID, &profile.OrganizationName,
			&profile.Title, &profile.IsPrimary)
	if err != nil {
		return DirectoryMembership{}, err
	}
	rows, err := tx.Query(ctx, `
SELECT d.id,d.name FROM user_departments ud
JOIN departments d ON d.tenant_id=ud.tenant_id AND d.organization_id=ud.organization_id AND d.id=ud.department_id
WHERE ud.tenant_id=$1 AND ud.user_organization_id=$2
  AND ud.status='active' AND d.status='active'
  AND ud.effective_from <= $3 AND (ud.effective_to IS NULL OR $3 < ud.effective_to)
	ORDER BY d.id FOR SHARE OF ud,d`, tenantID, membershipID, at)
	if err != nil {
		return DirectoryMembership{}, err
	}
	profile.Departments = make([]access.Department, 0)
	for rows.Next() {
		var department access.Department
		if err := rows.Scan(&department.ID, &department.Name); err != nil {
			rows.Close()
			return DirectoryMembership{}, err
		}
		profile.Departments = append(profile.Departments, department)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return DirectoryMembership{}, err
	}
	return profile, nil
}
