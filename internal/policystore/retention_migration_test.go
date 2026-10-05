package policystore_test

import (
	"context"
	"os"
	"testing"
)

func TestTenantRetentionMigrationDefaultsAndConstraints(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	var days int
	var version int64
	var reference, approver *string
	if err := conn.QueryRow(context.Background(), `SELECT message_body_retention_days,
 retention_version,retention_approval_reference,retention_approved_by_user_id::text
 FROM tenants WHERE id=$1`, tenantA).Scan(&days, &version, &reference, &approver); err != nil ||
		days != 365 || version != 0 || reference != nil || approver != nil {
		t.Fatalf("default retention: %d %d %v %v %v", days, version, reference, approver, err)
	}
	reject(t, conn, "UPDATE tenants SET message_body_retention_days=0 WHERE id=$1", tenantA)
	reject(t, conn, "UPDATE tenants SET message_body_retention_days=3651 WHERE id=$1", tenantA)
	reject(t, conn, "UPDATE tenants SET retention_approval_reference='CAB-1' WHERE id=$1", tenantA)
	reject(t, conn, `UPDATE tenants SET retention_approval_reference='CAB-1',
 retention_approved_by_user_id=$2,retention_approved_at=$3 WHERE id=$1`, tenantA, personB, at)
}

func TestTenantRetentionMigrationProtectsApprovalOnRollback(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	down, err := os.ReadFile("../../db/migrations/000013_tenant_retention.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, `UPDATE tenants SET message_body_retention_days=730,
 retention_version=1,retention_approval_reference='CAB-1',
 retention_approved_by_user_id=$2,retention_approved_at=$3 WHERE id=$1`, tenantA, adminA, at)
	if _, err := conn.PgConn().Exec(context.Background(), string(down)).ReadAll(); err == nil {
		t.Fatal("rollback discarded approved retention policy")
	}
	run(t, conn, "ROLLBACK")
	run(t, conn, `UPDATE tenants SET message_body_retention_days=365,
 retention_version=0,retention_approval_reference=NULL,
 retention_approved_by_user_id=NULL,retention_approved_at=NULL WHERE id=$1`, tenantA)
	if _, err := conn.PgConn().Exec(context.Background(), string(down)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := conn.QueryRow(context.Background(), `SELECT EXISTS (
 SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema()
 AND table_name='tenants' AND column_name='message_body_retention_days')`).Scan(&exists); err != nil || exists {
		t.Fatalf("down left retention column: %v %v", exists, err)
	}
	up, err := os.ReadFile("../../db/migrations/000013_tenant_retention.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(up)).ReadAll(); err != nil {
		t.Fatal(err)
	}
}
