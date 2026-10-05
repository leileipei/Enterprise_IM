// Package access provides tenant-scoped administrative access to group membership data.
// Its identity input must come from a verified authentication adapter, never from request fields.
package access

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrInvalidIdentity  = errors.New("invalid acting identity")
	ErrNotFound         = errors.New("resource not found")
	ErrConflict         = errors.New("membership state conflict")
	ErrAuditUnavailable = errors.New("audit unavailable")
)

type TrustedIdentity struct {
	TenantID           string
	UserID             string
	ActingMembershipID string
}

type Beginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

type Service struct {
	DB  Beginner
	Now func() time.Time
}

type Department struct {
	ID   string
	Name string
}

type Membership struct {
	ID               string
	OrganizationID   string
	OrganizationName string
	Title            string
	IsPrimary        bool
	Departments      []Department
}

type Person struct {
	ID          string
	DisplayName string
	Memberships []Membership
}

type grants struct {
	all           bool
	organizations map[string]bool
}

func (g grants) allows(organizationID string) bool {
	return g.all || g.organizations[organizationID]
}

func (g grants) empty() bool {
	return !g.all && len(g.organizations) == 0
}

func (s Service) currentTime() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s Service) resolve(ctx context.Context, tx pgx.Tx, id TrustedIdentity, at time.Time) (grants, error) {
	var actorID string
	err := tx.QueryRow(ctx, `
SELECT u.id FROM tenants t
JOIN users u ON u.tenant_id=t.id
JOIN user_organizations m ON m.tenant_id=u.tenant_id AND m.user_id=u.id
JOIN organizations o ON o.tenant_id=m.tenant_id AND o.id=m.organization_id
JOIN legal_entities l ON l.tenant_id=o.tenant_id AND l.id=o.legal_entity_id
WHERE t.id=$1 AND u.id=$2 AND m.id=$3
  AND t.status='active' AND u.status='active' AND m.status='active'
  AND o.status='active' AND l.status='active'
  AND m.effective_from <= $4 AND (m.effective_to IS NULL OR $4 < m.effective_to)
FOR SHARE OF t,u,m,o,l`, id.TenantID, id.UserID, id.ActingMembershipID, at).Scan(&actorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return grants{}, ErrInvalidIdentity
	}
	if err != nil {
		return grants{}, err
	}
	rows, err := tx.Query(ctx, `
SELECT role, COALESCE(scope_organization_id::text,'') FROM admin_grants
WHERE tenant_id=$1 AND membership_id=$2 AND status='active'
  AND effective_from <= $3 AND (effective_to IS NULL OR $3 < effective_to)
FOR SHARE`, id.TenantID, id.ActingMembershipID, at)
	if err != nil {
		return grants{}, err
	}
	g := grants{organizations: make(map[string]bool)}
	for rows.Next() {
		var role, scope string
		if err := rows.Scan(&role, &scope); err != nil {
			rows.Close()
			return grants{}, err
		}
		if role == "group_admin" {
			g.all = true
		} else if role == "organization_admin" {
			g.organizations[scope] = true
		}
	}
	err = rows.Err()
	rows.Close()
	return g, err
}

func audit(ctx context.Context, tx pgx.Tx, id TrustedIdentity, action, resourceType string, resourceID any, outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `
INSERT INTO audit_events (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id.TenantID, id.UserID, id.ActingMembershipID,
		action, resourceType, resourceID, outcome, reason, at)
	return err
}

func deny(ctx context.Context, tx pgx.Tx, id TrustedIdentity, action, resourceType string, resourceID any, reason string, at time.Time, result error) error {
	if err := audit(ctx, tx, id, action, resourceType, resourceID, "deny", reason, at); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return result
}

// GetManagedPerson returns only current memberships in the administrator's grant scope.
// It is an administrative query; ordinary directory visibility is a separate policy decision.
func (s Service) GetManagedPerson(ctx context.Context, id TrustedIdentity, targetUserID string) (Person, error) {
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" || targetUserID == "" {
		return Person{}, ErrInvalidIdentity
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Person{}, err
	}
	defer tx.Rollback(ctx)
	at := s.currentTime()
	g, err := s.resolve(ctx, tx, id, at)
	if errors.Is(err, ErrInvalidIdentity) {
		return Person{}, deny(ctx, tx, id, "directory_view", "user", targetUserID, "invalid_identity", at, ErrInvalidIdentity)
	}
	if err != nil {
		return Person{}, err
	}
	if fresh := s.currentTime(); fresh.After(at) {
		at = fresh
		g, err = s.resolve(ctx, tx, id, at)
		if errors.Is(err, ErrInvalidIdentity) {
			return Person{}, deny(ctx, tx, id, "directory_view", "user", targetUserID, "invalid_identity", at, ErrInvalidIdentity)
		}
		if err != nil {
			return Person{}, err
		}
	}
	if g.empty() {
		return Person{}, deny(ctx, tx, id, "directory_view", "user", targetUserID, "not_visible", at, ErrNotFound)
	}
	rows, err := tx.Query(ctx, `
SELECT u.display_name, m.id, m.organization_id, o.name, COALESCE(m.title,''), m.is_primary
FROM users u
JOIN user_organizations m ON m.tenant_id=u.tenant_id AND m.user_id=u.id
JOIN organizations o ON o.tenant_id=m.tenant_id AND o.id=m.organization_id
WHERE u.tenant_id=$1 AND u.id=$2 AND m.status='active' AND o.status='active'
  AND m.effective_from <= $3 AND (m.effective_to IS NULL OR $3 < m.effective_to)
ORDER BY m.organization_id, m.id`, id.TenantID, targetUserID, at)
	if err != nil {
		return Person{}, err
	}
	person := Person{ID: targetUserID}
	for rows.Next() {
		var membership Membership
		if err := rows.Scan(&person.DisplayName, &membership.ID, &membership.OrganizationID,
			&membership.OrganizationName, &membership.Title, &membership.IsPrimary); err != nil {
			rows.Close()
			return Person{}, err
		}
		if g.allows(membership.OrganizationID) {
			person.Memberships = append(person.Memberships, membership)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Person{}, err
	}
	if len(person.Memberships) == 0 {
		return Person{}, deny(ctx, tx, id, "directory_view", "user", targetUserID, "not_visible", at, ErrNotFound)
	}
	for i := range person.Memberships {
		m := &person.Memberships[i]
		deptRows, err := tx.Query(ctx, `
SELECT d.id, d.name FROM user_departments ud
JOIN departments d ON d.tenant_id=ud.tenant_id AND d.organization_id=ud.organization_id AND d.id=ud.department_id
WHERE ud.tenant_id=$1 AND ud.user_organization_id=$2 AND ud.status='active' AND d.status='active'
  AND ud.effective_from <= $3 AND (ud.effective_to IS NULL OR $3 < ud.effective_to)
ORDER BY d.id`, id.TenantID, m.ID, at)
		if err != nil {
			return Person{}, err
		}
		for deptRows.Next() {
			var d Department
			if err := deptRows.Scan(&d.ID, &d.Name); err != nil {
				deptRows.Close()
				return Person{}, err
			}
			m.Departments = append(m.Departments, d)
		}
		err = deptRows.Err()
		deptRows.Close()
		if err != nil {
			return Person{}, err
		}
	}
	if err := audit(ctx, tx, id, "directory_view", "user", targetUserID, "allow", "scope_granted", at); err != nil {
		return Person{}, errors.Join(ErrAuditUnavailable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Person{}, err
	}
	return person, nil
}
