package access_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	tenantA  = "00000000-0000-4000-8000-000000000101"
	tenantB  = "00000000-0000-4000-8000-000000000102"
	legalA   = "00000000-0000-4000-8000-000000000111"
	legalB   = "00000000-0000-4000-8000-000000000112"
	orgA     = "00000000-0000-4000-8000-000000000121"
	orgA2    = "00000000-0000-4000-8000-000000000122"
	orgB     = "00000000-0000-4000-8000-000000000123"
	depA     = "00000000-0000-4000-8000-000000000131"
	adminA   = "00000000-0000-4000-8000-000000000141"
	personA  = "00000000-0000-4000-8000-000000000142"
	personB  = "00000000-0000-4000-8000-000000000143"
	adminM   = "00000000-0000-4000-8000-000000000151"
	personM  = "00000000-0000-4000-8000-000000000152"
	personM2 = "00000000-0000-4000-8000-000000000153"
	personBM = "00000000-0000-4000-8000-000000000154"
)

func testDB(t *testing.T) *pgx.Conn {
	t.Helper()
	dsn := os.Getenv("IM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set IM_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("im_access_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		conn.Close(ctx)
	})
	if _, err := conn.Exec(ctx, "SET search_path TO "+schema+", public"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../../db/migrations/000001_group_foundation.up.sql", "../../db/migrations/000002_admin_access.up.sql"} {
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.PgConn().Exec(ctx, string(migration)).ReadAll(); err != nil {
			t.Fatal(err)
		}
	}
	return conn
}

func run(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func reject(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err == nil {
		t.Fatal("expected invalid grant to be rejected")
	}
}

func seedAccess(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	run(t, conn, "INSERT INTO tenants (id,code,name) VALUES ($1,'a','集团 A'),($2,'b','集团 B')", tenantA, tenantB)
	run(t, conn, "INSERT INTO legal_entities (id,tenant_id,code,name) VALUES ($1,$2,'a','法人 A'),($3,$4,'b','法人 B')", legalA, tenantA, legalB, tenantB)
	run(t, conn, "INSERT INTO organizations (id,tenant_id,legal_entity_id,org_type,code,name) VALUES ($1,$2,$3,'company','a','公司 A'),($4,$2,$3,'company','a2','公司 A2'),($5,$6,$7,'company','b','公司 B')", orgA, tenantA, legalA, orgA2, orgB, tenantB, legalB)
	run(t, conn, "INSERT INTO departments (id,tenant_id,organization_id,code,name) VALUES ($1,$2,$3,'d','部门 A')", depA, tenantA, orgA)
	run(t, conn, "INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,'A001','管理员'),($3,$2,'A002','双任职人员'),($4,$5,'B001','其他租户人员')", adminA, tenantA, personA, personB, tenantB)
	run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from,is_primary) VALUES ($1,$2,$3,$4,'2020-01-01',true),($5,$2,$6,$4,'2020-01-01',true),($7,$2,$6,$8,'2020-01-01',false),($9,$10,$11,$12,'2020-01-01',true)", adminM, tenantA, adminA, orgA, personM, personA, personM2, orgA2, personBM, tenantB, personB, orgB)
}

func TestAdminGrantDatabaseBoundaries(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')", "00000000-0000-4000-8000-000000000161", tenantA, adminM, orgA)
	run(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,scope_organization_id,effective_from) VALUES ($1,$2,$3,$4,'organization_admin',$5,'2020-01-01')", "00000000-0000-4000-8000-000000000162", tenantA, adminM, orgA, orgA)
	reject(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,scope_organization_id,effective_from) VALUES ($1,$2,$3,$4,'organization_admin',$5,'2020-01-01')", "00000000-0000-4000-8000-000000000163", tenantA, adminM, orgA, orgB)
	reject(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES ($1,$2,$3,$4,'organization_admin','2020-01-01')", "00000000-0000-4000-8000-000000000164", tenantA, adminM, orgA)
	reject(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,scope_organization_id,effective_from) VALUES ($1,$2,$3,$4,'group_admin',$5,'2020-01-01')", "00000000-0000-4000-8000-000000000165", tenantA, adminM, orgA, orgA)
	reject(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')", "00000000-0000-4000-8000-000000000166", tenantA, adminM, orgA2)
}

func TestAdminAccessMigrationRollsBackAndReapplies(t *testing.T) {
	conn := testDB(t)
	ctx := context.Background()
	down, err := os.ReadFile("../../db/migrations/000002_admin_access.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(down)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	var name *string
	if err := conn.QueryRow(ctx, "SELECT to_regclass('admin_grants')::text").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != nil {
		t.Fatalf("admin_grants remains after down migration: %s", *name)
	}
	up, err := os.ReadFile("../../db/migrations/000002_admin_access.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(up)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	seedAccess(t, conn)
}
