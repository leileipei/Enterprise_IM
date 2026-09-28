package policystore

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
)

var (
	ErrInvalidDirectoryPage = errors.New("invalid directory organization page")
	directoryUUIDPattern    = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type DirectoryMemberPage struct {
	People    []DirectoryPerson
	HasMore   bool
	NextAfter string
}

type organizationMemberKey struct {
	employeeNo   string
	userID       string
	membershipID string
}

func auditOrganizationMembers(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `
INSERT INTO audit_events (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
VALUES ($1,$2,$3,'directory_organization_members','organization',NULL,$4,$5,$6)`,
		id.TenantID, id.UserID, id.ActingMembershipID, outcome, reason, at)
	return err
}

func finishOrganizationMembers(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, outcome, reason string, at time.Time) error {
	if err := auditOrganizationMembers(ctx, tx, id, outcome, reason, at); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return tx.Commit(ctx)
}

// ListVisibleOrganizationMembers lists only the target organization's current
// policy-visible memberships. An after anchor is reauthorized on every page.
func (s Service) ListVisibleOrganizationMembers(ctx context.Context, id access.TrustedIdentity, orgID, afterMembershipID string, limit int) (DirectoryMemberPage, error) {
	if !directoryUUIDPattern.MatchString(orgID) ||
		(afterMembershipID != "" && !directoryUUIDPattern.MatchString(afterMembershipID)) ||
		limit < 1 || limit > 20 {
		return DirectoryMemberPage{}, ErrInvalidDirectoryPage
	}
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return DirectoryMemberPage{}, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return DirectoryMemberPage{}, err
	}
	defer tx.Rollback(ctx)
	at := s.now()
	actor, found, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return DirectoryMemberPage{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !found || !memberActiveAt(actor, at) {
		if err := finishOrganizationMembers(ctx, tx, id, "deny", "invalid_identity", at); err != nil {
			return DirectoryMemberPage{}, err
		}
		return DirectoryMemberPage{}, ErrForbidden
	}
	var currentOrgID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM organizations WHERE tenant_id=$1 AND id=$2 AND status='active' FOR SHARE NOWAIT`,
		id.TenantID, orgID).Scan(&currentOrgID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := finishOrganizationMembers(ctx, tx, id, "deny", "not_visible", at); err != nil {
			return DirectoryMemberPage{}, err
		}
		return DirectoryMemberPage{}, ErrDirectoryNotVisible
	}
	if err != nil {
		return DirectoryMemberPage{}, err
	}
	version, err := currentVersion(ctx, tx, id.TenantID)
	if err != nil {
		return DirectoryMemberPage{}, err
	}
	rules, err := loadRules(ctx, tx, id.TenantID, version)
	if err != nil {
		return DirectoryMemberPage{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !memberActiveAt(actor, at) {
		if err := finishOrganizationMembers(ctx, tx, id, "deny", "invalid_identity", at); err != nil {
			return DirectoryMemberPage{}, err
		}
		return DirectoryMemberPage{}, ErrForbidden
	}
	var after *organizationMemberKey
	if afterMembershipID != "" {
		anchor, err := loadOrganizationAnchor(ctx, tx, id.TenantID, orgID, afterMembershipID, at)
		if errors.Is(err, pgx.ErrNoRows) {
			if err := finishOrganizationMembers(ctx, tx, id, "deny", "not_visible", at); err != nil {
				return DirectoryMemberPage{}, err
			}
			return DirectoryMemberPage{}, ErrDirectoryNotVisible
		}
		if err != nil {
			return DirectoryMemberPage{}, err
		}
		_, visible, err := evaluateOrganizationMember(ctx, tx, id, actor, anchor, orgID, version, rules, at)
		if err != nil {
			return DirectoryMemberPage{}, err
		}
		if !visible {
			if err := finishOrganizationMembers(ctx, tx, id, "deny", "not_visible", at); err != nil {
				return DirectoryMemberPage{}, err
			}
			return DirectoryMemberPage{}, ErrDirectoryNotVisible
		}
		after = &anchor
	}
	page := DirectoryMemberPage{People: make([]DirectoryPerson, 0, limit)}
	for {
		batch, err := organizationMemberBatch(ctx, tx, id.TenantID, orgID, after, at)
		if err != nil {
			return DirectoryMemberPage{}, err
		}
		if len(batch) == 0 {
			break
		}
		for _, member := range batch {
			current := member
			after = &current
			profile, visible, err := evaluateOrganizationMember(ctx, tx, id, actor, member, orgID, version, rules, at)
			if err != nil {
				return DirectoryMemberPage{}, err
			}
			if !visible {
				continue
			}
			page.People = append(page.People, DirectoryPerson{ID: profile.UserID,
				DisplayName: profile.DisplayName, EmployeeNo: profile.EmployeeNo,
				Memberships: []DirectoryMembership{profile}})
			if len(page.People) > limit {
				page.People = page.People[:limit]
				page.HasMore = true
				page.NextAfter = page.People[len(page.People)-1].Memberships[0].MembershipID
				if err := finishOrganizationMembers(ctx, tx, id, "allow", "policy_visible", at); err != nil {
					return DirectoryMemberPage{}, err
				}
				return page, nil
			}
		}
		if len(batch) < 50 {
			break
		}
	}
	if len(page.People) == 0 && afterMembershipID == "" {
		if err := finishOrganizationMembers(ctx, tx, id, "deny", "not_visible", at); err != nil {
			return DirectoryMemberPage{}, err
		}
		return DirectoryMemberPage{}, ErrDirectoryNotVisible
	}
	if err := finishOrganizationMembers(ctx, tx, id, "allow", "policy_visible", at); err != nil {
		return DirectoryMemberPage{}, err
	}
	return page, nil
}

func loadOrganizationAnchor(ctx context.Context, tx pgx.Tx, tenantID, orgID, membershipID string, at time.Time) (organizationMemberKey, error) {
	var anchor organizationMemberKey
	err := tx.QueryRow(ctx, `
SELECT u.global_employee_no,u.id::text,m.id::text FROM user_organizations m
JOIN users u ON u.tenant_id=m.tenant_id AND u.id=m.user_id
WHERE m.tenant_id=$1 AND m.organization_id=$2 AND m.id=$3
  AND m.status='active' AND u.status='active'
  AND m.effective_from <= $4 AND (m.effective_to IS NULL OR $4 < m.effective_to)
FOR SHARE OF u,m NOWAIT`, tenantID, orgID, membershipID, at).
		Scan(&anchor.employeeNo, &anchor.userID, &anchor.membershipID)
	return anchor, err
}

func organizationMemberBatch(ctx context.Context, tx pgx.Tx, tenantID, orgID string, after *organizationMemberKey, at time.Time) ([]organizationMemberKey, error) {
	var number, userID, membershipID any
	if after != nil {
		number, userID, membershipID = after.employeeNo, after.userID, after.membershipID
	}
	rows, err := tx.Query(ctx, `
SELECT u.global_employee_no,u.id::text,m.id::text FROM user_organizations m
JOIN users u ON u.tenant_id=m.tenant_id AND u.id=m.user_id
WHERE m.tenant_id=$1 AND m.organization_id=$2
  AND m.status='active' AND u.status='active'
  AND m.effective_from <= $3 AND (m.effective_to IS NULL OR $3 < m.effective_to)
  AND ($4::text IS NULL OR (u.global_employee_no,u.id,m.id) > ($4::text,$5::uuid,$6::uuid))
ORDER BY u.global_employee_no,u.id,m.id LIMIT 50 FOR SHARE OF u,m NOWAIT`,
		tenantID, orgID, at, number, userID, membershipID)
	if err != nil {
		return nil, err
	}
	batch := make([]organizationMemberKey, 0, 50)
	for rows.Next() {
		var member organizationMemberKey
		if err := rows.Scan(&member.employeeNo, &member.userID, &member.membershipID); err != nil {
			rows.Close()
			return nil, err
		}
		batch = append(batch, member)
	}
	err = rows.Err()
	rows.Close()
	return batch, err
}

func evaluateOrganizationMember(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, actor policy.Membership,
	member organizationMemberKey, orgID string, version int64, rules []policy.Rule, at time.Time) (DirectoryMembership, bool, error) {
	target, found, err := loadMembership(ctx, tx, id.TenantID, member.membershipID, "")
	if err != nil {
		return DirectoryMembership{}, false, err
	}
	decision := policy.Decision{PolicyVersion: version, Reason: policy.ReasonInvalidContext}
	if found && target.OrganizationID == orgID {
		decision = policy.Evaluate(policy.Input{Action: policy.ActionDirectoryView,
			Actor: actor, Target: target, At: at, ScopeAllowed: true, ResourceActive: true,
			PolicyVersion: version, Rules: rules})
	}
	req := Request{Identity: id, TargetMembershipID: member.membershipID, Action: policy.ActionDirectoryView,
		ScopeAllowed: true, ResourceActive: true}
	if err := auditDecision(ctx, tx, req, decision, at); err != nil {
		return DirectoryMembership{}, false, errors.Join(ErrAuditUnavailable, err)
	}
	if !decision.Allowed {
		return DirectoryMembership{}, false, nil
	}
	profile, err := readDirectoryMembership(ctx, tx, id.TenantID, member.membershipID, at)
	if err != nil {
		return DirectoryMembership{}, false, err
	}
	if profile.UserID != member.userID || profile.OrganizationID != orgID ||
		profile.EmployeeNo != member.employeeNo || profile.MembershipID != member.membershipID {
		return DirectoryMembership{}, false, nil
	}
	return profile, true, nil
}
