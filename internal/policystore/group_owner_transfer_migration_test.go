package policystore_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

const ownerTransferRequestID = "00000000-0000-4000-8000-000000000901"

func TestGroupOwnerTransferRequestSchemaAndProtectedRollback(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	source := groupIntervalFor(t, conn, group.ID, adminA)
	target := groupIntervalFor(t, conn, group.ID, personA)
	run(t, conn, `INSERT INTO group_owner_transfer_requests
 (tenant_id,group_id,actor_user_id,request_id,acting_membership_id,source_interval_id,target_interval_id)
 VALUES ($1,$2,$3,$4,$5,$6,$7)`, tenantA, group.ID, adminA, ownerTransferRequestID, adminM, source, target)
	reject(t, conn, `INSERT INTO group_owner_transfer_requests
 (tenant_id,group_id,actor_user_id,request_id,acting_membership_id,source_interval_id,target_interval_id)
 VALUES ($1,$2,$3,$4,$5,$6,$7)`, tenantA, group.ID, adminA, ownerTransferRequestID, adminM, source, target)
	otherReq := createGroupRequest(targetM2)
	otherReq.ClientRequestID = "00000000-0000-4000-8000-000000000902"
	otherGroup, err := svc.CreateGroup(context.Background(), publisher(), otherReq)
	if err != nil {
		t.Fatal(err)
	}
	otherTarget := groupIntervalFor(t, conn, otherGroup.ID, personA)
	reject(t, conn, `INSERT INTO group_owner_transfer_requests
 (tenant_id,group_id,actor_user_id,request_id,acting_membership_id,source_interval_id,target_interval_id)
 VALUES ($1,$2,$3,$4,$5,$6,$7)`, tenantA, group.ID, adminA,
		"00000000-0000-4000-8000-000000000903", adminM, source, otherTarget)
	down, err := os.ReadFile("../../db/migrations/000012_group_owner_transfer.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(down)).ReadAll(); err == nil {
		t.Fatal("down removed recorded ownership transfer")
	}
	if _, err := conn.Exec(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	run(t, conn, "DELETE FROM group_owner_transfer_requests")
	if _, err := conn.PgConn().Exec(context.Background(), string(down)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile("../../db/migrations/000012_group_owner_transfer.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(up)).ReadAll(); err != nil {
		t.Fatal(err)
	}
}
