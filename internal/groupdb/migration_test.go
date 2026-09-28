package groupdb_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	tenantA = "00000000-0000-4000-8000-000000000001"
	tenantB = "00000000-0000-4000-8000-000000000002"
	legalA  = "00000000-0000-4000-8000-000000000011"
	legalB  = "00000000-0000-4000-8000-000000000012"
	orgA    = "00000000-0000-4000-8000-000000000021"
	orgA2   = "00000000-0000-4000-8000-000000000022"
	orgB    = "00000000-0000-4000-8000-000000000023"
	virtual = "00000000-0000-4000-8000-000000000024"
	userA   = "00000000-0000-4000-8000-000000000031"
	depA    = "00000000-0000-4000-8000-000000000041"
	depA2   = "00000000-0000-4000-8000-000000000042"
)

func migratedDB(t *testing.T) *pgx.Conn {
	t.Helper()
	url := os.Getenv("IM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set IM_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("im_test_%d", time.Now().UnixNano())
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
	migration, err := os.ReadFile("../../db/migrations/000001_group_foundation.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(migration)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	return conn
}

func exec(t *testing.T, conn *pgx.Conn, query string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func mustReject(t *testing.T, conn *pgx.Conn, query string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), query, args...); err == nil {
		t.Fatal("expected PostgreSQL to reject invalid relationship")
	}
}

func seed(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	exec(t, conn, "INSERT INTO tenants (id, code, name) VALUES ($1, 'group-a', '集团 A'), ($2, 'group-b', '集团 B')", tenantA, tenantB)
	exec(t, conn, "INSERT INTO legal_entities (id, tenant_id, code, name) VALUES ($1,$2,'a','法人 A'), ($3,$4,'b','法人 B')", legalA, tenantA, legalB, tenantB)
	exec(t, conn, "INSERT INTO organizations (id, tenant_id, legal_entity_id, org_type, code, name) VALUES ($1,$2,$3,'company','a','公司 A'), ($4,$2,$3,'company','a2','公司 A2'), ($5,$6,$7,'company','b','公司 B'), ($8,$2,NULL,'virtual_group','v','汇总')", orgA, tenantA, legalA, orgA2, orgB, tenantB, legalB, virtual)
	exec(t, conn, "INSERT INTO users (id, tenant_id, global_employee_no, display_name) VALUES ($1,$2,'E001','张某')", userA, tenantA)
	exec(t, conn, "INSERT INTO departments (id, tenant_id, organization_id, code, name) VALUES ($1,$2,$3,'d1','部门一'), ($4,$2,$5,'d2','部门二')", depA, tenantA, orgA, depA2, orgA2)
}

func TestAllowsParallelOrganizationsAndAdjacentIntervals(t *testing.T) {
	conn := migratedDB(t)
	seed(t, conn)
	exec(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from,effective_to,is_primary) VALUES ($1,$2,$3,$4,'2026-01-01','2026-02-01',true)", "00000000-0000-4000-8000-000000000051", tenantA, userA, orgA)
	exec(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from,is_primary) VALUES ($1,$2,$3,$4,'2026-02-01',true)", "00000000-0000-4000-8000-000000000052", tenantA, userA, orgA)
	exec(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from,is_primary) VALUES ($1,$2,$3,$4,'2026-02-01',false)", "00000000-0000-4000-8000-000000000053", tenantA, userA, orgA2)
}

func TestRejectsOverlappingMembershipInSameOrganization(t *testing.T) {
	conn := migratedDB(t)
	seed(t, conn)
	exec(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from,effective_to) VALUES ($1,$2,$3,$4,'2026-01-01','2026-03-01')", "00000000-0000-4000-8000-000000000051", tenantA, userA, orgA)
	mustReject(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2026-02-01')", "00000000-0000-4000-8000-000000000052", tenantA, userA, orgA)
}

func TestRejectsSimultaneousPrimaryMemberships(t *testing.T) {
	conn := migratedDB(t)
	seed(t, conn)
	exec(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from,is_primary) VALUES ($1,$2,$3,$4,'2026-01-01',true)", "00000000-0000-4000-8000-000000000051", tenantA, userA, orgA)
	mustReject(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from,is_primary) VALUES ($1,$2,$3,$4,'2026-02-01',true)", "00000000-0000-4000-8000-000000000052", tenantA, userA, orgA2)
}

func TestRejectsCrossTenantLegalEntityAndVirtualMembership(t *testing.T) {
	conn := migratedDB(t)
	seed(t, conn)
	mustReject(t, conn, "INSERT INTO organizations (id, tenant_id, legal_entity_id, org_type, code, name) VALUES ($1,$2,$3,'company','bad','错误法人')", "00000000-0000-4000-8000-000000000025", tenantA, legalB)
	mustReject(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2026-01-01')", "00000000-0000-4000-8000-000000000051", tenantA, userA, virtual)
}

func TestRejectsDepartmentFromDifferentOrganizationOrOutsideMembershipTime(t *testing.T) {
	conn := migratedDB(t)
	seed(t, conn)
	membership := "00000000-0000-4000-8000-000000000051"
	exec(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from,effective_to) VALUES ($1,$2,$3,$4,'2026-01-01','2026-03-01')", membership, tenantA, userA, orgA)
	mustReject(t, conn, "INSERT INTO user_departments (id,tenant_id,user_organization_id,organization_id,department_id,effective_from,effective_to) VALUES ($1,$2,$3,$4,$5,'2026-01-01','2026-02-01')", "00000000-0000-4000-8000-000000000061", tenantA, membership, orgA, depA2)
	mustReject(t, conn, "INSERT INTO user_departments (id,tenant_id,user_organization_id,organization_id,department_id,effective_from,effective_to) VALUES ($1,$2,$3,$4,$5,'2026-02-01','2026-04-01')", "00000000-0000-4000-8000-000000000062", tenantA, membership, orgA, depA)
}

func TestRejectsShorteningMembershipPastDepartmentAssignment(t *testing.T) {
	conn := migratedDB(t)
	seed(t, conn)
	membership := "00000000-0000-4000-8000-000000000051"
	exec(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from,effective_to) VALUES ($1,$2,$3,$4,'2026-01-01','2026-04-01')", membership, tenantA, userA, orgA)
	exec(t, conn, "INSERT INTO user_departments (id,tenant_id,user_organization_id,organization_id,department_id,effective_from,effective_to) VALUES ($1,$2,$3,$4,$5,'2026-02-01','2026-03-01')", "00000000-0000-4000-8000-000000000061", tenantA, membership, orgA, depA)
	mustReject(t, conn, "UPDATE user_organizations SET effective_to='2026-02-15' WHERE id=$1", membership)
}

func TestRejectsInvalidMembershipInterval(t *testing.T) {
	conn := migratedDB(t)
	seed(t, conn)
	mustReject(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from,effective_to) VALUES ($1,$2,$3,$4,'2026-02-01','2026-01-01')", "00000000-0000-4000-8000-000000000051", tenantA, userA, orgA)
}

func TestMigrationCanRollBackAndReapply(t *testing.T) {
	conn := migratedDB(t)
	ctx := context.Background()
	down, err := os.ReadFile("../../db/migrations/000001_group_foundation.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(down)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	var tableName *string
	if err := conn.QueryRow(ctx, "SELECT to_regclass('tenants')::text").Scan(&tableName); err != nil {
		t.Fatal(err)
	}
	if tableName != nil {
		t.Fatalf("tenants table remains after down migration: %s", *tableName)
	}
	up, err := os.ReadFile("../../db/migrations/000001_group_foundation.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(up)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	seed(t, conn)
}
