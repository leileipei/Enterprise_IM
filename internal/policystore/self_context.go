package policystore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// SelfContext is the account's own identity and currently selectable roles.
// It is informational; protected operations still verify the chosen role.
type SelfContext struct {
	TenantID         string           `json:"tenant_id"`
	UserID           string           `json:"user_id"`
	DisplayName      string           `json:"display_name"`
	GlobalEmployeeNo string           `json:"global_employee_no"`
	Memberships      []SelfMembership `json:"memberships"`
}

type SelfMembership struct {
	ID               string `json:"id"`
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	LegalEntityID    string `json:"legal_entity_id"`
	LegalEntityName  string `json:"legal_entity_name"`
	Title            string `json:"title"`
	IsPrimary        bool   `json:"is_primary"`
}

// GetSelfContext reads only the identity mapped from a verified access token.
func (s Service) GetSelfContext(ctx context.Context, tenantID, userID string) (SelfContext, error) {
	if !directoryUUIDPattern.MatchString(tenantID) || !directoryUUIDPattern.MatchString(userID) {
		return SelfContext{}, ErrForbidden
	}
	if s.DB == nil {
		return SelfContext{}, errors.New("self context database unavailable")
	}
	tenantID, userID = strings.ToLower(tenantID), strings.ToLower(userID)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return SelfContext{}, err
	}
	defer tx.Rollback(ctx)
	self := SelfContext{TenantID: tenantID, UserID: userID, Memberships: make([]SelfMembership, 0)}
	var userStatus, tenantStatus string
	err = tx.QueryRow(ctx, `
SELECT u.display_name,u.global_employee_no,u.status,t.status
FROM users u JOIN tenants t ON t.id=u.tenant_id
WHERE u.tenant_id=$1 AND u.id=$2 FOR SHARE OF u,t`, tenantID, userID).
		Scan(&self.DisplayName, &self.GlobalEmployeeNo, &userStatus, &tenantStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return SelfContext{}, ErrForbidden
	}
	if err != nil {
		return SelfContext{}, err
	}
	if userStatus != "active" || tenantStatus != "active" {
		return SelfContext{}, ErrForbidden
	}
	at := s.now()
	rows, err := tx.Query(ctx, `
SELECT m.id::text,m.organization_id::text,o.name,
 COALESCE(l.id::text,''),COALESCE(l.name,''),COALESCE(m.title,''),m.is_primary,
 m.effective_to
FROM user_organizations m
JOIN organizations o ON o.tenant_id=m.tenant_id AND o.id=m.organization_id
JOIN legal_entities l ON l.tenant_id=o.tenant_id AND l.id=o.legal_entity_id
WHERE m.tenant_id=$1 AND m.user_id=$2
 AND m.status='active' AND m.effective_from <= $3
 AND (m.effective_to IS NULL OR $3 < m.effective_to)
 AND o.status='active' AND l.status='active'
ORDER BY m.is_primary DESC,o.name,m.id
FOR SHARE OF m,o,l`, tenantID, userID, at)
	if err != nil {
		return SelfContext{}, err
	}
	type selectedMembership struct {
		membership  SelfMembership
		effectiveTo *time.Time
	}
	selected := make([]selectedMembership, 0)
	for rows.Next() {
		var item selectedMembership
		if err := rows.Scan(&item.membership.ID, &item.membership.OrganizationID,
			&item.membership.OrganizationName, &item.membership.LegalEntityID,
			&item.membership.LegalEntityName, &item.membership.Title,
			&item.membership.IsPrimary, &item.effectiveTo); err != nil {
			rows.Close()
			return SelfContext{}, err
		}
		selected = append(selected, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return SelfContext{}, err
	}
	// A membership can expire while the query runs even though its row is locked.
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	for _, item := range selected {
		if item.effectiveTo == nil || at.Before(*item.effectiveTo) {
			self.Memberships = append(self.Memberships, item.membership)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SelfContext{}, err
	}
	return self, nil
}
