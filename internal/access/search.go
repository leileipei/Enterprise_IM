package access

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

var ErrInvalidSearch = errors.New("invalid directory search")

type SearchPerson struct {
	ID          string
	EmployeeNo  string
	DisplayName string
}

type SearchPage struct {
	People  []SearchPerson
	HasMore bool
}

// SearchManagedPeople finds active users through active memberships in the
// administrator's current grant scope. It does not confer ordinary directory
// visibility or return membership details.
func (s Service) SearchManagedPeople(ctx context.Context, id TrustedIdentity, query string, limit int) (SearchPage, error) {
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return SearchPage{}, ErrInvalidIdentity
	}
	query = strings.TrimSpace(query)
	if query == "" || !utf8.ValidString(query) || strings.ContainsRune(query, 0) ||
		utf8.RuneCountInString(query) > 100 || limit < 1 || limit > 50 {
		return SearchPage{}, ErrInvalidSearch
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return SearchPage{}, err
	}
	defer tx.Rollback(ctx)
	at := s.currentTime()
	g, err := s.resolve(ctx, tx, id, at)
	if errors.Is(err, ErrInvalidIdentity) {
		return SearchPage{}, deny(ctx, tx, id, "directory_search", "user", nil, "invalid_identity", at, ErrInvalidIdentity)
	}
	if err != nil {
		return SearchPage{}, err
	}
	if fresh := s.currentTime(); fresh.After(at) {
		at = fresh
		g, err = s.resolve(ctx, tx, id, at)
		if errors.Is(err, ErrInvalidIdentity) {
			return SearchPage{}, deny(ctx, tx, id, "directory_search", "user", nil, "invalid_identity", at, ErrInvalidIdentity)
		}
		if err != nil {
			return SearchPage{}, err
		}
	}
	if g.empty() {
		return SearchPage{}, deny(ctx, tx, id, "directory_search", "user", nil, "not_visible", at, ErrNotFound)
	}
	scopes := make([]string, 0, len(g.organizations))
	for organizationID := range g.organizations {
		scopes = append(scopes, organizationID)
	}
	pattern := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query) + "%"
	rows, err := tx.Query(ctx, `
SELECT u.id,u.global_employee_no,u.display_name
FROM users u
WHERE u.tenant_id=$1 AND u.status='active'
  AND (u.display_name ILIKE $2 ESCAPE '\' OR u.global_employee_no ILIKE $2 ESCAPE '\')
  AND EXISTS (
    SELECT 1 FROM user_organizations m
    JOIN organizations o ON o.tenant_id=m.tenant_id AND o.id=m.organization_id
    JOIN legal_entities l ON l.tenant_id=o.tenant_id AND l.id=o.legal_entity_id
    WHERE m.tenant_id=u.tenant_id AND m.user_id=u.id
      AND m.status='active' AND o.status='active' AND l.status='active'
      AND m.effective_from <= $3 AND (m.effective_to IS NULL OR $3 < m.effective_to)
      AND ($4 OR m.organization_id::text = ANY($5::text[]))
  )
ORDER BY u.global_employee_no,u.id
LIMIT $6`, id.TenantID, pattern, at, g.all, scopes, limit+1)
	if err != nil {
		return SearchPage{}, err
	}
	page := SearchPage{People: make([]SearchPerson, 0, limit)}
	for rows.Next() {
		var person SearchPerson
		if err := rows.Scan(&person.ID, &person.EmployeeNo, &person.DisplayName); err != nil {
			rows.Close()
			return SearchPage{}, err
		}
		page.People = append(page.People, person)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return SearchPage{}, err
	}
	if len(page.People) > limit {
		page.People = page.People[:limit]
		page.HasMore = true
	}
	if err := audit(ctx, tx, id, "directory_search", "user", nil, "allow", "scope_granted", at); err != nil {
		return SearchPage{}, errors.Join(ErrAuditUnavailable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return SearchPage{}, err
	}
	return page, nil
}
