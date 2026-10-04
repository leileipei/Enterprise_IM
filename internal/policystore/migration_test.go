package policystore_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	tenantA  = "00000000-0000-4000-8000-000000000201"
	tenantB  = "00000000-0000-4000-8000-000000000202"
	legalA   = "00000000-0000-4000-8000-000000000211"
	legalB   = "00000000-0000-4000-8000-000000000212"
	orgA     = "00000000-0000-4000-8000-000000000221"
	orgA2    = "00000000-0000-4000-8000-000000000222"
	orgB     = "00000000-0000-4000-8000-000000000223"
	adminA   = "00000000-0000-4000-8000-000000000231"
	personA  = "00000000-0000-4000-8000-000000000232"
	personB  = "00000000-0000-4000-8000-000000000233"
	adminM   = "00000000-0000-4000-8000-000000000241"
	targetM  = "00000000-0000-4000-8000-000000000242"
	targetM2 = "00000000-0000-4000-8000-000000000243"
	otherM   = "00000000-0000-4000-8000-000000000244"
)

var at = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

func db(t *testing.T) *pgx.Conn {
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
	schema := fmt.Sprintf("im_policy_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); conn.Close(ctx) })
	if _, err := conn.Exec(ctx, "SET search_path TO "+schema+", public"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"../../db/migrations/000001_group_foundation.up.sql",
		"../../db/migrations/000002_admin_access.up.sql",
		"../../db/migrations/000003_policy_store.up.sql",
		"../../db/migrations/000005_direct_conversations.up.sql",
		"../../db/migrations/000006_message_write.up.sql",
		"../../db/migrations/000007_message_recipient.up.sql",
		"../../db/migrations/000008_conversation_inbox.up.sql",
		"../../db/migrations/000009_group_membership.up.sql",
		"../../db/migrations/000010_group_create_request.up.sql",
		"../../db/migrations/000011_group_invitation.up.sql",
		"../../db/migrations/000012_group_owner_transfer.up.sql",
		"../../db/migrations/000013_tenant_retention.up.sql",
		"../../db/migrations/000014_conversation_legal_hold.up.sql",
		"../../db/migrations/000015_message_body_clear.up.sql",
		"../../db/migrations/000016_message_digest_retirement.up.sql",
		"../../db/migrations/000017_cross_message_search_indexes.up.sql",
		"../../db/migrations/000018_file_foundation.up.sql",
		"../../db/migrations/000019_file_upload_scan.up.sql",
		"../../db/migrations/000020_file_message.up.sql",
	} {
		sql, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.PgConn().Exec(ctx, string(sql)).ReadAll(); err != nil {
			t.Fatal(err)
		}
	}
	return conn
}

func TestConversationInboxMigrationAddsParticipantIndexes(t *testing.T) {
	conn := db(t)
	for _, index := range []string{"conversations_direct_low_inbox", "conversations_direct_high_inbox"} {
		var exists bool
		if err := conn.QueryRow(context.Background(), `SELECT EXISTS (
SELECT 1 FROM pg_indexes WHERE schemaname=current_schema() AND indexname=$1)`, index).Scan(&exists); err != nil || !exists {
			t.Fatalf("missing inbox index %s: %v", index, err)
		}
	}
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
		t.Fatal("expected invalid policy write to be rejected")
	}
}

func seed(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	run(t, conn, "INSERT INTO tenants (id,code,name) VALUES ($1,'a','集团 A'),($2,'b','集团 B')", tenantA, tenantB)
	run(t, conn, "INSERT INTO legal_entities (id,tenant_id,code,name) VALUES ($1,$2,'a','法人 A'),($3,$4,'b','法人 B')", legalA, tenantA, legalB, tenantB)
	run(t, conn, "INSERT INTO organizations (id,tenant_id,legal_entity_id,org_type,code,name) VALUES ($1,$2,$3,'company','a','公司 A'),($4,$2,$3,'company','a2','公司 A2'),($5,$6,$7,'company','b','公司 B')", orgA, tenantA, legalA, orgA2, orgB, tenantB, legalB)
	run(t, conn, "INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,'A001','管理员'),($3,$2,'A002','用户 A'),($4,$5,'B001','用户 B')", adminA, tenantA, personA, personB, tenantB)
	run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from,is_primary) VALUES ($1,$2,$3,$4,'2020-01-01',true),($5,$2,$6,$7,'2020-01-01',true),($8,$2,$6,$4,'2020-01-01',false),($9,$10,$11,$12,'2020-01-01',true)", adminM, tenantA, adminA, orgA, targetM, personA, orgA2, targetM2, otherM, tenantB, personB, orgB)
}

func TestPolicySchemaTenantAndImmutability(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "INSERT INTO policy_versions (tenant_id,version,status,published_by_user_id,reason) VALUES ($1,1,'draft',$2,'首版')", tenantA, adminA)
	run(t, conn, "INSERT INTO policy_rules (tenant_id,version,rule_id,effect,action,source_organization_id,target_organization_id,reason,effective_from) VALUES ($1,1,'isolate-1','isolate','start_chat',$2,$3,'组织隔离','2020-01-01')", tenantA, orgA, orgA2)
	reject(t, conn, "INSERT INTO policy_rules (tenant_id,version,rule_id,effect,action,source_organization_id,target_organization_id,reason,effective_from) VALUES ($1,1,'foreign','isolate','start_chat',$2,$3,'跨租户','2020-01-01')", tenantA, orgA, orgB)
	reject(t, conn, "INSERT INTO policy_rules (tenant_id,version,rule_id,effect,action,source_organization_id,target_organization_id,source_membership_id,reason,effective_from) VALUES ($1,1,'wrong-member','isolate','start_chat',$2,$3,$4,'任职错配','2020-01-01')", tenantA, orgA, orgA2, targetM)
	run(t, conn, "UPDATE policy_versions SET status='published',published_at=$3 WHERE tenant_id=$1 AND version=$2", tenantA, int64(1), at)
	run(t, conn, "INSERT INTO policy_current (tenant_id,current_version) VALUES ($1,1)", tenantA)
	reject(t, conn, "INSERT INTO policy_rules (tenant_id,version,rule_id,effect,action,reason,effective_from) VALUES ($1,1,'late','hard_deny','start_chat','晚到','2020-01-01')", tenantA)
	reject(t, conn, "UPDATE policy_rules SET reason='changed' WHERE tenant_id=$1 AND version=1 AND rule_id='isolate-1'", tenantA)
	reject(t, conn, "DELETE FROM policy_versions WHERE tenant_id=$1 AND version=1", tenantA)
}

func TestPolicyMigrationRollsBackAndReapplies(t *testing.T) {
	conn := db(t)
	ctx := context.Background()
	down, err := os.ReadFile("../../db/migrations/000003_policy_store.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(down)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	var name *string
	if err := conn.QueryRow(ctx, "SELECT to_regclass('policy_versions')::text").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != nil {
		t.Fatalf("policy_versions remains: %s", *name)
	}
	up, err := os.ReadFile("../../db/migrations/000003_policy_store.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(up)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	seed(t, conn)
}

func TestPolicyCurrentTenantCannotChange(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "INSERT INTO policy_versions (tenant_id,version,status,published_by_user_id,reason) VALUES ($1,1,'draft',$2,'A 首版'),($3,2,'draft',$4,'B 二版')", tenantA, adminA, tenantB, personB)
	run(t, conn, "UPDATE policy_versions SET status='published',published_at=$3 WHERE tenant_id=$1 AND version=$2", tenantA, int64(1), at)
	run(t, conn, "UPDATE policy_versions SET status='published',published_at=$3 WHERE tenant_id=$1 AND version=$2", tenantB, int64(2), at)
	reject(t, conn, "INSERT INTO policy_current (tenant_id,current_version) VALUES ($1,2)", tenantB)
	run(t, conn, "INSERT INTO policy_current (tenant_id,current_version) VALUES ($1,1)", tenantA)
	reject(t, conn, "UPDATE policy_current SET tenant_id=$2,current_version=2 WHERE tenant_id=$1", tenantA, tenantB)
}

func TestPolicyExceptionRequiresMatchingIsolationAtPublication(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "INSERT INTO policy_versions (tenant_id,version,status,published_by_user_id,reason) VALUES ($1,1,'draft',$2,'错误例外')", tenantA, adminA)
	run(t, conn, "INSERT INTO policy_rules (tenant_id,version,rule_id,effect,action,source_organization_id,target_organization_id,reason,effective_from) VALUES ($1,1,'isolate-1','isolate','send_message',$2,$3,'消息隔离','2020-01-01')", tenantA, orgA, orgA2)
	run(t, conn, "INSERT INTO policy_rules (tenant_id,version,rule_id,effect,action,source_organization_id,target_organization_id,source_membership_id,target_membership_id,override_rule_id,requested_by_user_id,approved_by_user_id,reason,effective_from,effective_to) VALUES ($1,1,'exception-1','exception_allow','start_chat',$2,$3,$4,$5,'isolate-1',$6,$6,'错误覆盖','2020-01-01','2027-01-01')", tenantA, orgA, orgA2, adminM, targetM, adminA)
	reject(t, conn, "UPDATE policy_versions SET status='published',published_at=$2 WHERE tenant_id=$1 AND version=1", tenantA, at)
}

func TestPolicyExceptionCannotCoverAllowRule(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "INSERT INTO policy_versions (tenant_id,version,status,published_by_user_id,reason) VALUES ($1,1,'draft',$2,'错误覆盖')", tenantA, adminA)
	run(t, conn, "INSERT INTO policy_rules (tenant_id,version,rule_id,effect,action,source_organization_id,target_organization_id,requested_by_user_id,approved_by_user_id,reason,effective_from,effective_to) VALUES ($1,1,'allow-1','allow','start_chat',$2,$3,$4,$4,'白名单','2020-01-01','2027-01-01')", tenantA, orgA, orgA2, adminA)
	run(t, conn, "INSERT INTO policy_rules (tenant_id,version,rule_id,effect,action,source_organization_id,target_organization_id,source_membership_id,target_membership_id,override_rule_id,requested_by_user_id,approved_by_user_id,reason,effective_from,effective_to) VALUES ($1,1,'exception-1','exception_allow','start_chat',$2,$3,$4,$5,'allow-1',$6,$6,'错误覆盖','2020-01-01','2027-01-01')", tenantA, orgA, orgA2, adminM, targetM, adminA)
	reject(t, conn, "UPDATE policy_versions SET status='published',published_at=$2 WHERE tenant_id=$1 AND version=1", tenantA, at)
}
