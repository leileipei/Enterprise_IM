package importapply

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type appendFixture struct {
	Admin  *pgx.Conn
	Pool   *pgxpool.Pool
	Schema string
}

func appendDB(t *testing.T, last int) *appendFixture {
	t.Helper()
	adminURL := os.Getenv("IM_IMPORT_APPLY_TEST_ADMIN_URL")
	writerURL := os.Getenv("IM_IMPORT_APPLY_TEST_DATABASE_URL")
	if adminURL == "" || writerURL == "" {
		t.Skip("dedicated append PostgreSQL fixture required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal("admin fixture unavailable")
	}
	schema := fmt.Sprintf("im_append_%d", time.Now().UnixNano())
	q := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+q); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(ctx, "DROP SCHEMA "+q+" CASCADE"); admin.Close(ctx) })
	if _, err = admin.Exec(ctx, "SET search_path TO "+q+", public"); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob("../../db/migrations/*.up.sql")
	for _, path := range paths {
		var n int
		fmt.Sscanf(filepath.Base(path), "%d_", &n)
		if n > last {
			continue
		}
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = admin.PgConn().Exec(ctx, string(b)).ReadAll(); e != nil {
			t.Fatalf("migration %s: %v", filepath.Base(path), e)
		}
	}
	cfg, err := pgxpool.ParseConfig(writerURL)
	if err != nil {
		t.Fatal("writer config invalid")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var role string
	if err = pool.QueryRow(ctx, "SELECT current_user").Scan(&role); err != nil {
		t.Fatal(err)
	}
	quotedRole := pgx.Identifier{role}.Sanitize()
	for _, sql := range []string{"GRANT USAGE ON SCHEMA " + q + " TO " + quotedRole, "GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA " + q + " TO " + quotedRole, "GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA " + q + " TO " + quotedRole} {
		if _, err = admin.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	return &appendFixture{admin, pool, schema}
}

func seedApplyActor(t *testing.T, f *appendFixture) access.ImportPrincipal {
	t.Helper()
	ctx := context.Background()
	rows := []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO tenants(id,code,name) VALUES($1,'fixture','Fixture')", []any{fixtureTenant}},
		{"INSERT INTO legal_entities(id,tenant_id,code,name) VALUES('95000000-0000-4000-8000-000000000010',$1,'actorlegal','Actor legal')", []any{fixtureTenant}},
		{"INSERT INTO organizations(id,tenant_id,legal_entity_id,org_type,code,name) VALUES('95000000-0000-4000-8000-000000000011',$1,'95000000-0000-4000-8000-000000000010','company','actororg','Actor org')", []any{fixtureTenant}},
		{"INSERT INTO users(id,tenant_id,global_employee_no,display_name) VALUES($1,$2,'actor','Actor')", []any{fixtureActor, fixtureTenant}},
		{"INSERT INTO user_organizations(id,tenant_id,user_id,organization_id,effective_from) VALUES($1,$2,$3,'95000000-0000-4000-8000-000000000011','2020-01-01Z')", []any{fixtureMembership, fixtureTenant, fixtureActor}},
		{"INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES('95000000-0000-4000-8000-000000000012',$1,$2,'95000000-0000-4000-8000-000000000011','group_admin','2020-01-01Z')", []any{fixtureTenant, fixtureMembership}},
		{"INSERT INTO external_identities(issuer,subject,tenant_id,user_id) VALUES('https://sso.test','actor',$1,$2)", []any{fixtureTenant, fixtureActor}},
	}
	for _, r := range rows {
		if _, e := f.Admin.Exec(ctx, r.sql, r.args...); e != nil {
			t.Fatal(e)
		}
	}
	return access.ImportPrincipal{Identity: access.TrustedIdentity{TenantID: fixtureTenant, UserID: fixtureActor, ActingMembershipID: fixtureMembership}, Issuer: "https://sso.test", Subject: "actor", ExpiresAt: time.Now().Add(time.Hour)}
}
func applyInput(t *testing.T) []byte {
	t.Helper()
	base := map[string]any{"format_version": 1, "baseline_commit": "7f5868aa11a3d4d8b758f0e332813502efe7eb6e", "data_origin": "entirely_synthetic", "reference_time": "2026-10-07T00:00:00Z", "identity_source_selected": false}
	tables := map[string]any{}
	for _, name := range []string{"tenants", "legal_entities", "organizations", "departments", "users", "user_organizations", "user_departments", "external_identities", "admin_grants"} {
		tables[name] = []any{}
	}
	tables["tenants"] = []any{map[string]any{"id": fixtureTenant, "code": "fixture", "name": "Fixture", "status": "active"}}
	tables["legal_entities"] = []any{map[string]any{"id": "96000000-0000-4000-8000-000000000001", "tenant_id": fixtureTenant, "code": "newlegal", "name": "New legal"}}
	tables["organizations"] = []any{map[string]any{"id": "96000000-0000-4000-8000-000000000002", "tenant_id": fixtureTenant, "legal_entity_id": "96000000-0000-4000-8000-000000000001", "parent_id": nil, "org_type": "company", "code": "neworg", "name": "New org"}}
	tables["departments"] = []any{map[string]any{"id": "96000000-0000-4000-8000-000000000003", "tenant_id": fixtureTenant, "organization_id": "96000000-0000-4000-8000-000000000002", "parent_id": nil, "code": "newdept", "name": "New dept"}}
	tables["users"] = []any{map[string]any{"id": "96000000-0000-4000-8000-000000000004", "tenant_id": fixtureTenant, "global_employee_no": "newperson", "display_name": "New person"}}
	tables["user_organizations"] = []any{map[string]any{"id": "96000000-0000-4000-8000-000000000005", "tenant_id": fixtureTenant, "user_id": "96000000-0000-4000-8000-000000000004", "organization_id": "96000000-0000-4000-8000-000000000002", "effective_from": "2026-01-01T00:00:00Z", "effective_to": nil, "employee_no": nil, "title": nil}}
	tables["user_departments"] = []any{map[string]any{"id": "96000000-0000-4000-8000-000000000006", "tenant_id": fixtureTenant, "user_organization_id": "96000000-0000-4000-8000-000000000005", "organization_id": "96000000-0000-4000-8000-000000000002", "department_id": "96000000-0000-4000-8000-000000000003", "effective_from": "2026-01-01T00:00:00Z", "effective_to": nil}}
	base["tables"] = tables
	b, e := json.Marshal(base)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func databaseCounts(t *testing.T, f *appendFixture) [8]int {
	t.Helper()
	out := [8]int{}
	for i, name := range []string{"legal_entities", "organizations", "departments", "users", "user_organizations", "user_departments", "import_batches", "audit_events"} {
		sql := "SELECT count(*) FROM " + tableName(f.Schema, name)
		if name == "audit_events" {
			sql += " WHERE action='controlled_import.apply'"
		}
		if e := f.Pool.QueryRow(context.Background(), sql).Scan(&out[i]); e != nil {
			t.Fatal(e)
		}
	}
	return out
}
