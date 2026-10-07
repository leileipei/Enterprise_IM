package access

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ImportPrincipal is created by verified authentication, never from request data.
type ImportPrincipal struct {
	Identity        TrustedIdentity
	Issuer, Subject string
	ExpiresAt       time.Time
}

func (ImportPrincipal) MarshalJSON() ([]byte, error) { return nil, ErrInvalidIdentity }
func (ImportPrincipal) String() string               { return "<private import principal>" }
func (p ImportPrincipal) GoString() string           { return p.String() }

var importSchemaPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func importTable(schema, table string) string { return pgx.Identifier{schema, table}.Sanitize() }
func validImportPrincipal(p ImportPrincipal, at time.Time) bool {
	id := p.Identity
	if !legalHoldUUIDPattern.MatchString(id.TenantID) || !legalHoldUUIDPattern.MatchString(id.UserID) || !legalHoldUUIDPattern.MatchString(id.ActingMembershipID) || p.ExpiresAt.IsZero() || !at.Before(p.ExpiresAt) {
		return false
	}
	for _, v := range []string{p.Issuer, p.Subject} {
		if strings.TrimSpace(v) == "" || len(v) > 4096 || strings.ContainsRune(v, 0) || !utf8.ValidString(v) {
			return false
		}
	}
	return true
}

// AuthorizeImport locks authority in the caller's transaction; it never commits or rolls it back.
func AuthorizeImport(ctx context.Context, tx pgx.Tx, schema string, p ImportPrincipal, at time.Time) error {
	if tx == nil || !importSchemaPattern.MatchString(schema) || !validImportPrincipal(p, at) {
		return ErrInvalidIdentity
	}
	id := p.Identity
	var found string
	lock := func(sql string, args ...any) error {
		e := tx.QueryRow(ctx, sql, args...).Scan(&found)
		if errors.Is(e, pgx.ErrNoRows) {
			return ErrInvalidIdentity
		}
		return e
	}
	if e := lock("SELECT id FROM "+importTable(schema, "user_organizations")+" WHERE tenant_id=$1 AND user_id=$2 AND id=$3 FOR SHARE", id.TenantID, id.UserID, id.ActingMembershipID); e != nil {
		return e
	}
	if e := lock("SELECT id FROM "+importTable(schema, "tenants")+" WHERE id=$1 AND status='active' FOR SHARE", id.TenantID); e != nil {
		return e
	}
	sql := "SELECT u.id FROM " + importTable(schema, "users") + " u JOIN " + importTable(schema, "user_organizations") + " m ON m.tenant_id=u.tenant_id AND m.user_id=u.id JOIN " + importTable(schema, "organizations") + " o ON o.tenant_id=m.tenant_id AND o.id=m.organization_id JOIN " + importTable(schema, "legal_entities") + " l ON l.tenant_id=o.tenant_id AND l.id=o.legal_entity_id WHERE u.tenant_id=$1 AND u.id=$2 AND m.id=$3 AND u.status='active' AND m.status='active' AND o.status='active' AND l.status='active' AND m.effective_from<=$4 AND (m.effective_to IS NULL OR $4<m.effective_to) FOR SHARE OF u,o,l"
	if e := lock(sql, id.TenantID, id.UserID, id.ActingMembershipID, at.UTC()); e != nil {
		return e
	}
	if e := lock("SELECT user_id FROM "+importTable(schema, "external_identities")+" WHERE issuer=$1 AND subject=$2 AND tenant_id=$3 AND user_id=$4 AND status='active' FOR SHARE", p.Issuer, p.Subject, id.TenantID, id.UserID); e != nil {
		return e
	}
	if e := lock("SELECT id FROM "+importTable(schema, "admin_grants")+" WHERE tenant_id=$1 AND membership_id=$2 AND role='group_admin' AND status='active' AND effective_from<=$3 AND (effective_to IS NULL OR $3<effective_to) ORDER BY id LIMIT 1 FOR SHARE", id.TenantID, id.ActingMembershipID, at.UTC()); e != nil {
		return e
	}
	return nil
}
func AuditImportTerminal(ctx context.Context, tx pgx.Tx, schema string, p ImportPrincipal, requestID, state, reason string, at time.Time) error {
	if tx == nil || !importSchemaPattern.MatchString(schema) || !legalHoldUUIDPattern.MatchString(requestID) || !validImportPrincipal(p, at) {
		return ErrInvalidIdentity
	}
	outcome := "allow"
	if state == "applied" {
		if reason != "NONE" {
			return ErrInvalidIdentity
		}
	} else if state == "rejected" {
		outcome = "deny"
		if reason != "INPUT_CONFLICT" && reason != "DATABASE_CONSTRAINT_CONFLICT" {
			return ErrInvalidIdentity
		}
	} else {
		return ErrInvalidIdentity
	}
	id := p.Identity
	tag, e := tx.Exec(ctx, "INSERT INTO "+importTable(schema, "audit_events")+" (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at) VALUES($1,$2,$3,'controlled_import.apply','import_batch',$4,$5,$6,$7)", id.TenantID, id.UserID, id.ActingMembershipID, requestID, outcome, reason, at.UTC())
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return errors.New("import audit not recorded")
	}
	return nil
}
