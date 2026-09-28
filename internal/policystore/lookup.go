package policystore

import (
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

var ErrInvalidEmployeeNo = errors.New("invalid employee number")

type DirectoryPerson struct {
	ID          string
	DisplayName string
	EmployeeNo  string
	Memberships []DirectoryMembership
}

func auditLookup(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `
INSERT INTO audit_events (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
VALUES ($1,$2,$3,'directory_lookup','user',NULL,$4,$5,$6)`,
		id.TenantID, id.UserID, id.ActingMembershipID, outcome, reason, at)
	return err
}

func finishLookup(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, outcome, reason string, at time.Time) error {
	if err := auditLookup(ctx, tx, id, outcome, reason, at); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return tx.Commit(ctx)
}

// FindVisiblePersonByEmployeeNo resolves one tenant-unique employee number and
// returns only memberships visible under the selected acting membership.
func (s Service) FindVisiblePersonByEmployeeNo(ctx context.Context, id access.TrustedIdentity, employeeNo string) (DirectoryPerson, error) {
	employeeNo = strings.TrimSpace(employeeNo)
	if employeeNo == "" || !utf8.ValidString(employeeNo) || strings.ContainsRune(employeeNo, 0) ||
		utf8.RuneCountInString(employeeNo) > 128 {
		return DirectoryPerson{}, ErrInvalidEmployeeNo
	}
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return DirectoryPerson{}, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return DirectoryPerson{}, err
	}
	defer tx.Rollback(ctx)
	at := s.now()
	var targetUserID string
	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE tenant_id=$1 AND global_employee_no=$2`, id.TenantID, employeeNo).Scan(&targetUserID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return DirectoryPerson{}, err
	}
	var candidates []string
	if err == nil {
		rows, err := tx.Query(ctx, `
SELECT id FROM user_organizations
WHERE tenant_id=$1 AND user_id=$2 AND status='active'
  AND effective_from <= $3 AND (effective_to IS NULL OR $3 < effective_to)
ORDER BY id`, id.TenantID, targetUserID, at)
		if err != nil {
			return DirectoryPerson{}, err
		}
		for rows.Next() {
			var membershipID string
			if err := rows.Scan(&membershipID); err != nil {
				rows.Close()
				return DirectoryPerson{}, err
			}
			candidates = append(candidates, membershipID)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return DirectoryPerson{}, err
		}
	}
	lockIDs := append([]string{id.ActingMembershipID}, candidates...)
	slices.Sort(lockIDs)
	lockIDs = slices.Compact(lockIDs)
	for _, membershipID := range lockIDs {
		var locked string
		err := tx.QueryRow(ctx, `SELECT id FROM user_organizations WHERE tenant_id=$1 AND id=$2 FOR SHARE`, id.TenantID, membershipID).Scan(&locked)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return DirectoryPerson{}, err
		}
	}
	actor, actorFound, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return DirectoryPerson{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !actorFound || !memberActiveAt(actor, at) {
		if err := finishLookup(ctx, tx, id, "deny", "invalid_identity", at); err != nil {
			return DirectoryPerson{}, err
		}
		return DirectoryPerson{}, ErrForbidden
	}
	if len(candidates) == 0 {
		if err := finishLookup(ctx, tx, id, "deny", "not_visible", at); err != nil {
			return DirectoryPerson{}, err
		}
		return DirectoryPerson{}, ErrDirectoryNotVisible
	}
	version, err := currentVersion(ctx, tx, id.TenantID)
	if err != nil {
		return DirectoryPerson{}, err
	}
	rules, err := loadRules(ctx, tx, id.TenantID, version)
	if err != nil {
		return DirectoryPerson{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !memberActiveAt(actor, at) {
		if err := finishLookup(ctx, tx, id, "deny", "invalid_identity", at); err != nil {
			return DirectoryPerson{}, err
		}
		return DirectoryPerson{}, ErrForbidden
	}
	person := DirectoryPerson{ID: targetUserID, Memberships: make([]DirectoryMembership, 0)}
	for _, membershipID := range candidates {
		target, found, err := loadMembership(ctx, tx, id.TenantID, membershipID, "")
		if err != nil {
			return DirectoryPerson{}, err
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
			return DirectoryPerson{}, errors.Join(ErrAuditUnavailable, err)
		}
		if !decision.Allowed {
			continue
		}
		profile, err := readDirectoryMembership(ctx, tx, id.TenantID, membershipID, at)
		if err != nil {
			return DirectoryPerson{}, err
		}
		if profile.EmployeeNo != employeeNo {
			continue
		}
		person.DisplayName = profile.DisplayName
		person.EmployeeNo = profile.EmployeeNo
		person.Memberships = append(person.Memberships, profile)
	}
	if len(person.Memberships) == 0 {
		if err := finishLookup(ctx, tx, id, "deny", "not_visible", at); err != nil {
			return DirectoryPerson{}, err
		}
		return DirectoryPerson{}, ErrDirectoryNotVisible
	}
	if err := finishLookup(ctx, tx, id, "allow", "policy_visible", at); err != nil {
		return DirectoryPerson{}, err
	}
	return person, nil
}
