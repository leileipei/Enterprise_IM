package policystore

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
)

type DirectoryOrganization struct {
	ID                string
	ParentID          string
	Name              string
	OrgType           string
	HasVisibleMembers bool
}

type directoryOrgRow struct {
	DirectoryOrganization
	status string
}

func auditOrganizationList(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `
INSERT INTO audit_events (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
VALUES ($1,$2,$3,'directory_organization_list','organization',NULL,$4,$5,$6)`,
		id.TenantID, id.UserID, id.ActingMembershipID, outcome, reason, at)
	return err
}

func finishOrganizationList(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, outcome, reason string, at time.Time) error {
	if err := auditOrganizationList(ctx, tx, id, outcome, reason, at); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return tx.Commit(ctx)
}

// ListVisibleOrganizations returns only organizations with a visible current
// membership, plus active ancestors needed to build the tree.
func (s Service) ListVisibleOrganizations(ctx context.Context, id access.TrustedIdentity) ([]DirectoryOrganization, error) {
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return nil, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	at := s.now()
	actor, found, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return nil, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !found || !memberActiveAt(actor, at) {
		if err := finishOrganizationList(ctx, tx, id, "deny", "invalid_identity", at); err != nil {
			return nil, err
		}
		return nil, ErrForbidden
	}
	version, err := currentVersion(ctx, tx, id.TenantID)
	if err != nil {
		return nil, err
	}
	rules, err := loadRules(ctx, tx, id.TenantID, version)
	if err != nil {
		return nil, err
	}
	organizations, err := loadDirectoryOrganizations(ctx, tx, id.TenantID)
	if err != nil {
		return nil, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !memberActiveAt(actor, at) {
		if err := finishOrganizationList(ctx, tx, id, "deny", "invalid_identity", at); err != nil {
			return nil, err
		}
		return nil, ErrForbidden
	}
	possible, all := possibleDirectoryOrganizations(actor.OrganizationID, rules)
	orgIDs := make([]string, 0, len(organizations))
	for orgID, org := range organizations {
		if org.status != "active" || org.OrgType == "virtual_group" {
			continue
		}
		if all || possible[orgID] {
			orgIDs = append(orgIDs, orgID)
		}
	}
	slices.Sort(orgIDs)
	visible := make(map[string]bool)
	for _, orgID := range orgIDs {
		membershipIDs, err := activeOrganizationMembershipIDs(ctx, tx, id.TenantID, orgID, at)
		if err != nil {
			return nil, err
		}
		for _, membershipID := range membershipIDs {
			var currentOrgID string
			err := tx.QueryRow(ctx, `SELECT organization_id::text FROM user_organizations WHERE tenant_id=$1 AND id=$2 FOR SHARE NOWAIT`,
				id.TenantID, membershipID).Scan(&currentOrgID)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if currentOrgID != orgID {
				continue
			}
			target, found, err := loadMembership(ctx, tx, id.TenantID, membershipID, "")
			if err != nil {
				return nil, err
			}
			decision := policy.Decision{PolicyVersion: version, Reason: policy.ReasonInvalidContext}
			if found {
				decision = policy.Evaluate(policy.Input{Action: policy.ActionDirectoryView,
					Actor: actor, Target: target, At: at, ScopeAllowed: true, ResourceActive: true,
					PolicyVersion: version, Rules: rules})
			}
			req := Request{Identity: id, TargetMembershipID: membershipID, Action: policy.ActionDirectoryView,
				ScopeAllowed: true, ResourceActive: true}
			if err := auditDecision(ctx, tx, req, decision, at); err != nil {
				return nil, errors.Join(ErrAuditUnavailable, err)
			}
			if decision.Allowed {
				visible[orgID] = true
				break
			}
		}
	}
	nodes := projectVisibleOrganizationTree(organizations, visible)
	if err := finishOrganizationList(ctx, tx, id, "allow", "policy_visible", at); err != nil {
		return nil, err
	}
	return nodes, nil
}

func possibleDirectoryOrganizations(actorOrgID string, rules []policy.Rule) (map[string]bool, bool) {
	possible := map[string]bool{actorOrgID: true}
	all := false
	for _, rule := range rules {
		if rule.Action != policy.ActionDirectoryView ||
			(rule.Effect != policy.EffectAllow && rule.Effect != policy.EffectExceptionAllow) {
			continue
		}
		if rule.TargetOrganizationID == "" {
			all = true
		} else {
			possible[rule.TargetOrganizationID] = true
		}
		if rule.Bidirectional {
			if rule.SourceOrganizationID == "" {
				all = true
			} else {
				possible[rule.SourceOrganizationID] = true
			}
		}
	}
	return possible, all
}

func loadDirectoryOrganizations(ctx context.Context, tx pgx.Tx, tenantID string) (map[string]directoryOrgRow, error) {
	// Pin ancestor status and parent links until the tree response commits.
	rows, err := tx.Query(ctx, `
SELECT id::text,COALESCE(parent_id::text,''),name,org_type,status
FROM organizations WHERE tenant_id=$1
ORDER BY id FOR SHARE NOWAIT`, tenantID)
	if err != nil {
		return nil, err
	}
	organizations := make(map[string]directoryOrgRow)
	for rows.Next() {
		var org directoryOrgRow
		if err := rows.Scan(&org.ID, &org.ParentID, &org.Name, &org.OrgType, &org.status); err != nil {
			rows.Close()
			return nil, err
		}
		organizations[org.ID] = org
	}
	err = rows.Err()
	rows.Close()
	return organizations, err
}

func activeOrganizationMembershipIDs(ctx context.Context, tx pgx.Tx, tenantID, orgID string, at time.Time) ([]string, error) {
	rows, err := tx.Query(ctx, `
SELECT id::text FROM user_organizations
WHERE tenant_id=$1 AND organization_id=$2 AND status='active'
  AND effective_from <= $3 AND (effective_to IS NULL OR $3 < effective_to)
ORDER BY id`, tenantID, orgID, at)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var membershipID string
		if err := rows.Scan(&membershipID); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, membershipID)
	}
	err = rows.Err()
	rows.Close()
	return ids, err
}

func projectVisibleOrganizationTree(organizations map[string]directoryOrgRow, visible map[string]bool) []DirectoryOrganization {
	included := make(map[string]bool)
	for id := range visible {
		for current := id; current != ""; {
			org, ok := organizations[current]
			if !ok || org.status != "active" || included[current] {
				break
			}
			included[current] = true
			current = org.ParentID
		}
	}
	nodes := make([]DirectoryOrganization, 0, len(included))
	for id := range included {
		node := organizations[id].DirectoryOrganization
		if !included[node.ParentID] {
			node.ParentID = ""
		}
		node.HasVisibleMembers = visible[id]
		nodes = append(nodes, node)
	}
	depth := func(id string) int {
		count := 0
		for current := organizations[id].ParentID; included[current] && count < len(included); current = organizations[current].ParentID {
			count++
		}
		return count
	}
	slices.SortFunc(nodes, func(a, b DirectoryOrganization) int {
		if byDepth := cmp.Compare(depth(a.ID), depth(b.ID)); byDepth != 0 {
			return byDepth
		}
		if byName := cmp.Compare(a.Name, b.Name); byName != 0 {
			return byName
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return nodes
}
