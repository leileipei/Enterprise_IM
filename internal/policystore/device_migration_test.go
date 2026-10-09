package policystore_test

import (
	"context"
	"os"
	"testing"
)

const migrationDeviceID = "00000000-0000-4000-8000-000000000251"
const migrationRegistrationID = "019a13c0-0000-7000-8000-000000000251"

func TestDeviceMigrationTenantBoundariesAndLifecycle(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	insert := `INSERT INTO user_devices(id,tenant_id,user_id,registration_id,platform,name,credential_digest,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`
	digest := make([]byte, 32)
	run(t, conn, insert, migrationDeviceID, tenantA, personA, migrationRegistrationID, "web", "浏览器", digest, at, at.AddDate(0, 0, 30))
	bad := []struct {
		name                                        string
		tenant, user, registration, platform, label string
		digest                                      []byte
	}{
		{"tenant user mismatch", tenantA, personB, "019a13c0-0000-7000-8000-000000000252", "web", "浏览器", digest},
		{"cross tenant", tenantB, personA, "019a13c0-0000-7000-8000-000000000252", "web", "浏览器", digest},
		{"short digest", tenantA, personA, "019a13c0-0000-7000-8000-000000000252", "web", "浏览器", digest[:31]},
		{"long digest", tenantA, personA, "019a13c0-0000-7000-8000-000000000252", "web", "浏览器", make([]byte, 33)},
		{"registration reused", tenantA, personA, migrationRegistrationID, "web", "浏览器", digest},
		{"bad platform", tenantA, personA, "019a13c0-0000-7000-8000-000000000252", "unknown", "浏览器", digest},
		{"untrimmed name", tenantA, personA, "019a13c0-0000-7000-8000-000000000252", "web", " 浏览器", digest},
		{"control name", tenantA, personA, "019a13c0-0000-7000-8000-000000000252", "web", "浏览器\n", digest},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			reject(t, conn, insert, "00000000-0000-4000-8000-000000000252", tc.tenant, tc.user, tc.registration, tc.platform, tc.label, tc.digest, at, at.AddDate(0, 0, 30))
		})
	}
	for _, column := range []string{"tenant_id", "user_id", "id", "registration_id", "platform", "credential_digest", "created_at"} {
		values := map[string]any{"tenant_id": tenantB, "user_id": adminA, "id": "00000000-0000-4000-8000-000000000253", "registration_id": "019a13c0-0000-7000-8000-000000000253", "platform": "ios", "credential_digest": make([]byte, 32), "created_at": at.AddDate(0, 0, -1)}
		if column == "credential_digest" {
			values[column] = []byte("01234567890123456789012345678901")
		}
		reject(t, conn, "UPDATE user_devices SET "+column+"=$2 WHERE id=$1", migrationDeviceID, values[column])
	}
	reject(t, conn, "UPDATE user_devices SET expires_at=created_at WHERE id=$1", migrationDeviceID)
	reject(t, conn, "UPDATE user_devices SET revoked_at=created_at-interval '1 second' WHERE id=$1", migrationDeviceID)
	run(t, conn, "UPDATE user_devices SET expires_at=expires_at+interval '1 day',last_seen_at=$2 WHERE id=$1", migrationDeviceID, at)
	run(t, conn, "UPDATE user_devices SET revoked_at=$2 WHERE id=$1", migrationDeviceID, at.AddDate(0, 0, 1))
	reject(t, conn, "UPDATE user_devices SET revoked_at=NULL WHERE id=$1", migrationDeviceID)
	reject(t, conn, "UPDATE user_devices SET revoked_at=revoked_at+interval '1 day' WHERE id=$1", migrationDeviceID)
	run(t, conn, "INSERT INTO audit_events(tenant_id,actor_user_id,acting_membership_id,action,resource_type,outcome,reason) VALUES($1,$2,NULL,'device_list','user','allow','owner_active')", tenantA, personA)
	reject(t, conn, "INSERT INTO audit_events(tenant_id,actor_user_id,acting_membership_id,action,resource_type,outcome,reason) VALUES($1,$2,NULL,'message_send','user','allow','invalid')", tenantA, personA)
	run(t, conn, "INSERT INTO tenant_client_policies(tenant_id) VALUES($1)", tenantA)
	for _, set := range []string{"version=-1", "device_lease_days=0", "device_lease_days=91", "max_active_devices=0", "max_active_devices=101", "offline_cache_seconds=-1", "offline_cache_seconds=3601", "group_receipt_visibility='all'"} {
		reject(t, conn, "UPDATE tenant_client_policies SET "+set+" WHERE tenant_id=$1", tenantA)
	}
	run(t, conn, "UPDATE tenant_client_policies SET device_lease_days=90,max_active_devices=100,offline_cache_seconds=0,group_receipt_visibility='sender_aggregate' WHERE tenant_id=$1", tenantA)
	reject(t, conn, "INSERT INTO device_registration_windows(tenant_id,user_id,window_start,created_count) VALUES($1,$2,$3,1)", tenantA, personB, at)
	reject(t, conn, "INSERT INTO device_registration_windows(tenant_id,user_id,window_start,created_count) VALUES($1,$2,$3,-1)", tenantA, personA, at)
	run(t, conn, "INSERT INTO device_registration_windows(tenant_id,user_id,window_start,created_count) VALUES($1,$2,$3,10)", tenantA, personA, at)
}

func TestDeviceMigrationEmptyRollbackAndReapply(t *testing.T) {
	conn := db(t)
	for _, direction := range []string{"down", "up"} {
		sql, err := os.ReadFile("../../db/migrations/000023_devices." + direction + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = conn.PgConn().Exec(context.Background(), string(sql)).ReadAll(); err != nil {
			t.Fatal(err)
		}
		var exists bool
		if err = conn.QueryRow(context.Background(), "SELECT to_regclass('user_devices') IS NOT NULL").Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists != (direction == "up") {
			t.Fatalf("table after %s: %v", direction, exists)
		}
	}
	seed(t, conn)
	run(t, conn, `INSERT INTO user_devices(id,tenant_id,user_id,registration_id,platform,name,credential_digest,created_at,expires_at) VALUES($1,$2,$3,$4,'web','浏览器',$5,$6,$7)`, migrationDeviceID, tenantA, personA, migrationRegistrationID, make([]byte, 32), at, at.AddDate(0, 0, 30))
}
