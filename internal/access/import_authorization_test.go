package access

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"testing"
	"time"
)

const iaTenant = "91000000-0000-4000-8000-000000000001"
const iaUser = "91000000-0000-4000-8000-000000000002"
const iaMembership = "91000000-0000-4000-8000-000000000003"
const iaOrg = "91000000-0000-4000-8000-000000000004"
const iaLegal = "91000000-0000-4000-8000-000000000005"
const iaGrant = "91000000-0000-4000-8000-000000000006"

func iaDB(t *testing.T) (*pgx.Conn, *pgxpool.Pool, string, ImportPrincipal) {
	t.Helper()
	a, w := os.Getenv("IM_IMPORT_APPLY_TEST_ADMIN_URL"), os.Getenv("IM_IMPORT_APPLY_TEST_DATABASE_URL")
	if a == "" || w == "" {
		t.Skip("dedicated import auth fixture required")
	}
	ctx := context.Background()
	admin, e := pgx.Connect(ctx, a)
	if e != nil {
		t.Fatal(e)
	}
	schema := fmt.Sprintf("im_import_auth_%d", time.Now().UnixNano())
	q := pgx.Identifier{schema}.Sanitize()
	admin.Exec(ctx, "CREATE SCHEMA "+q)
	admin.Exec(ctx, "SET search_path TO "+q+", public")
	t.Cleanup(func() { admin.Exec(ctx, "DROP SCHEMA "+q+" CASCADE"); admin.Close(ctx) })
	for _, f := range []string{"000001_group_foundation", "000002_admin_access", "000004_external_identities"} {
		b, e := os.ReadFile("../../db/migrations/" + f + ".up.sql")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = admin.PgConn().Exec(ctx, string(b)).ReadAll(); e != nil {
			t.Fatal(e)
		}
	}
	cmds := []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO tenants(id,code,name) VALUES($1,'t','T')", []any{iaTenant}},
		{"INSERT INTO legal_entities(id,tenant_id,code,name) VALUES($1,$2,'l','L')", []any{iaLegal, iaTenant}},
		{"INSERT INTO organizations(id,tenant_id,legal_entity_id,org_type,code,name) VALUES($1,$2,$3,'company','o','O')", []any{iaOrg, iaTenant, iaLegal}},
		{"INSERT INTO users(id,tenant_id,global_employee_no,display_name) VALUES($1,$2,'u','U')", []any{iaUser, iaTenant}},
		{"INSERT INTO user_organizations(id,tenant_id,user_id,organization_id,effective_from) VALUES($1,$2,$3,$4,'2020-01-01Z')", []any{iaMembership, iaTenant, iaUser, iaOrg}},
		{"INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES($1,$2,$3,$4,'group_admin','2020-01-01Z')", []any{iaGrant, iaTenant, iaMembership, iaOrg}},
		{"INSERT INTO external_identities(issuer,subject,tenant_id,user_id) VALUES('https://sso.test','actor',$1,$2)", []any{iaTenant, iaUser}},
	}
	for _, c := range cmds {
		if _, e = admin.Exec(ctx, c.sql, c.args...); e != nil {
			t.Fatal(e)
		}
	}
	cfg, e := pgxpool.ParseConfig(w)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(pool.Close)
	var role string
	pool.QueryRow(ctx, "SELECT current_user").Scan(&role)
	role = pgx.Identifier{role}.Sanitize()
	for _, s := range []string{"GRANT USAGE ON SCHEMA " + q + " TO " + role, "GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA " + q + " TO " + role, "GRANT USAGE ON ALL SEQUENCES IN SCHEMA " + q + " TO " + role} {
		if _, e = admin.Exec(ctx, s); e != nil {
			t.Fatal(e)
		}
	}
	return admin, pool, schema, ImportPrincipal{Identity: TrustedIdentity{TenantID: iaTenant, UserID: iaUser, ActingMembershipID: iaMembership}, Issuer: "https://sso.test", Subject: "actor", ExpiresAt: time.Now().Add(time.Hour)}
}
func TestAppendPGAuthorization(t *testing.T) {
	for _, mutation := range []string{"", "UPDATE admin_grants SET role='organization_admin',scope_organization_id=membership_organization_id", "UPDATE users SET status='frozen'", "UPDATE tenants SET status='suspended'", "UPDATE user_organizations SET status='ended'", "UPDATE organizations SET status='disabled'", "UPDATE legal_entities SET status='disabled'", "UPDATE external_identities SET status='disabled'", "UPDATE admin_grants SET status='revoked'"} {
		t.Run(mutation, func(t *testing.T) {
			admin, pool, schema, p := iaDB(t)
			ctx := context.Background()
			if mutation != "" {
				if _, e := admin.Exec(ctx, mutation); e != nil {
					t.Fatal(e)
				}
			}
			tx, e := pool.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(ctx)
			e = AuthorizeImport(ctx, tx, schema, p, time.Now().UTC())
			if mutation == "" && e != nil {
				t.Fatal(e)
			}
			if mutation != "" && e == nil {
				t.Fatal("invalid authority accepted")
			}
		})
	}
}
func TestAppendPGAuthorizationExpiry(t *testing.T) {
	admin, pool, schema, p := iaDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	end := now.Add(time.Second)
	if _, e := admin.Exec(ctx, "UPDATE admin_grants SET effective_to=$1", end); e != nil {
		t.Fatal(e)
	}
	for _, test := range []struct {
		p  ImportPrincipal
		at time.Time
	}{{p, end}, {func() ImportPrincipal { x := p; x.ExpiresAt = now; return x }(), now}, {func() ImportPrincipal { x := p; x.Issuer = ""; return x }(), now}, {func() ImportPrincipal { x := p; x.ExpiresAt = time.Time{}; return x }(), now}} {
		tx, e := pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		e = AuthorizeImport(ctx, tx, schema, test.p, test.at)
		tx.Rollback(ctx)
		if e == nil {
			t.Fatal("expired/missing source accepted")
		}
	}
}
func TestAppendPGAuthNoCommit(t *testing.T) {
	_, pool, schema, p := iaDB(t)
	ctx := context.Background()
	tx, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if e = AuthorizeImport(ctx, tx, schema, p, time.Now().UTC()); e != nil {
		t.Fatal(e)
	}
	if e = AuditImportTerminal(ctx, tx, schema, p, iaGrant, "applied", "NONE", time.Now().UTC()); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, "SELECT 1"); e != nil {
		t.Fatal("helper committed tx")
	}
	if e = tx.Rollback(ctx); e != nil {
		t.Fatal(e)
	}
	var n int
	if e = pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, "audit_events"}.Sanitize()).Scan(&n); e != nil || n != 0 {
		t.Fatal("audit survived rollback")
	}
}
