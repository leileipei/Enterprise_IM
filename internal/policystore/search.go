package policystore

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
)

var (
	ErrInvalidDirectorySearch  = errors.New("invalid directory name search")
	ErrDirectorySearchTooBroad = errors.New("directory name search too broad")
)

type DirectorySearchPage struct {
	People  []DirectoryPerson
	HasMore bool
}

type nameCandidate struct {
	id         string
	employeeNo string
	name       string
	active     bool
}

func auditNameSearch(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `
INSERT INTO audit_events (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
VALUES ($1,$2,$3,'directory_name_search','user',NULL,$4,$5,$6)`,
		id.TenantID, id.UserID, id.ActingMembershipID, outcome, reason, at)
	return err
}

func finishNameSearch(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, outcome, reason string, at time.Time) error {
	if err := auditNameSearch(ctx, tx, id, outcome, reason, at); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return tx.Commit(ctx)
}

// SearchVisiblePeople filters all bounded name candidates by current directory
// policy before calculating the visible result page.
func (s Service) SearchVisiblePeople(ctx context.Context, id access.TrustedIdentity, query string, limit int) (DirectorySearchPage, error) {
	query = strings.TrimSpace(query)
	if !utf8.ValidString(query) || strings.ContainsRune(query, 0) ||
		utf8.RuneCountInString(query) < 2 || utf8.RuneCountInString(query) > 100 ||
		limit < 1 || limit > 20 {
		return DirectorySearchPage{}, ErrInvalidDirectorySearch
	}
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return DirectorySearchPage{}, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return DirectorySearchPage{}, err
	}
	defer tx.Rollback(ctx)
	at := s.now()
	actor, found, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return DirectorySearchPage{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !found || !memberActiveAt(actor, at) {
		if err := finishNameSearch(ctx, tx, id, "deny", "invalid_identity", at); err != nil {
			return DirectorySearchPage{}, err
		}
		return DirectorySearchPage{}, ErrForbidden
	}
	version, err := currentVersion(ctx, tx, id.TenantID)
	if err != nil {
		return DirectorySearchPage{}, err
	}
	rules, err := loadRules(ctx, tx, id.TenantID, version)
	if err != nil {
		return DirectorySearchPage{}, err
	}
	pattern := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query) + "%"
	rows, err := tx.Query(ctx, `
SELECT id::text FROM users
WHERE tenant_id=$1 AND status='active' AND display_name ILIKE $2 ESCAPE '\'
ORDER BY global_employee_no,id LIMIT 501`, id.TenantID, pattern)
	if err != nil {
		return DirectorySearchPage{}, err
	}
	ids := make([]string, 0, 501)
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			rows.Close()
			return DirectorySearchPage{}, err
		}
		ids = append(ids, userID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return DirectorySearchPage{}, err
	}
	if len(ids) > 500 {
		if err := finishNameSearch(ctx, tx, id, "deny", "refine_search", at); err != nil {
			return DirectorySearchPage{}, err
		}
		return DirectorySearchPage{}, ErrDirectorySearchTooBroad
	}
	orderedIDs := slices.Clone(ids)
	slices.Sort(orderedIDs)
	candidates := make(map[string]nameCandidate, len(ids))
	for _, userID := range orderedIDs {
		var candidate nameCandidate
		var matches bool
		err := tx.QueryRow(ctx, `
SELECT id::text,global_employee_no,display_name,status='active',display_name ILIKE $3 ESCAPE '\'
FROM users WHERE tenant_id=$1 AND id=$2 FOR UPDATE NOWAIT`, id.TenantID, userID, pattern).
			Scan(&candidate.id, &candidate.employeeNo, &candidate.name, &candidate.active, &matches)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return DirectorySearchPage{}, err
		}
		if candidate.active && matches {
			candidates[userID] = candidate
		}
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !memberActiveAt(actor, at) {
		if err := finishNameSearch(ctx, tx, id, "deny", "invalid_identity", at); err != nil {
			return DirectorySearchPage{}, err
		}
		return DirectorySearchPage{}, ErrForbidden
	}
	visibleIDs := make([]string, 0, len(candidates))
	for userID := range candidates {
		visibleIDs = append(visibleIDs, userID)
	}
	slices.SortFunc(visibleIDs, func(a, b string) int {
		if byNumber := cmp.Compare(candidates[a].employeeNo, candidates[b].employeeNo); byNumber != 0 {
			return byNumber
		}
		return cmp.Compare(a, b)
	})
	page := DirectorySearchPage{People: make([]DirectoryPerson, 0, limit)}
	for _, userID := range visibleIDs {
		candidate := candidates[userID]
		membershipIDs, err := activeNameMembershipIDs(ctx, tx, id.TenantID, userID, at)
		if err != nil {
			return DirectorySearchPage{}, err
		}
		person := DirectoryPerson{ID: userID, DisplayName: candidate.name, EmployeeNo: candidate.employeeNo,
			Memberships: make([]DirectoryMembership, 0)}
		for _, membershipID := range membershipIDs {
			var currentUserID string
			err := tx.QueryRow(ctx, `SELECT user_id::text FROM user_organizations WHERE tenant_id=$1 AND id=$2 FOR SHARE NOWAIT`,
				id.TenantID, membershipID).Scan(&currentUserID)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return DirectorySearchPage{}, err
			}
			if currentUserID != userID {
				continue
			}
			target, found, err := loadMembership(ctx, tx, id.TenantID, membershipID, "")
			if err != nil {
				return DirectorySearchPage{}, err
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
				return DirectorySearchPage{}, errors.Join(ErrAuditUnavailable, err)
			}
			if !decision.Allowed {
				continue
			}
			profile, err := readDirectoryMembership(ctx, tx, id.TenantID, membershipID, at)
			if err != nil {
				return DirectorySearchPage{}, err
			}
			if profile.UserID == userID && profile.EmployeeNo == candidate.employeeNo {
				person.Memberships = append(person.Memberships, profile)
			}
		}
		if len(person.Memberships) > 0 {
			page.People = append(page.People, person)
		}
	}
	if len(page.People) > limit {
		page.People = page.People[:limit]
		page.HasMore = true
	}
	if err := finishNameSearch(ctx, tx, id, "allow", "policy_visible", at); err != nil {
		return DirectorySearchPage{}, err
	}
	return page, nil
}

func activeNameMembershipIDs(ctx context.Context, tx pgx.Tx, tenantID, userID string, at time.Time) ([]string, error) {
	rows, err := tx.Query(ctx, `
SELECT id::text FROM user_organizations
WHERE tenant_id=$1 AND user_id=$2 AND status='active'
  AND effective_from <= $3 AND (effective_to IS NULL OR $3 < effective_to)
ORDER BY id`, tenantID, userID, at)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	return ids, err
}
