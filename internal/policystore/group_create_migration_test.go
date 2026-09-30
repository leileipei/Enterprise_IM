package policystore_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

const groupRequestID = "00000000-0000-4000-8000-000000000851"

func TestGroupCreateRequestSchemaRejectsReuseAndMutation(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, `INSERT INTO conversations
 (id,tenant_id,kind,group_name,group_creator_membership_id,group_creator_organization_id,
  group_creator_legal_entity_id,created_by_user_id,group_create_request_id,group_create_request_digest)
 VALUES ($1,$2,'group','项目群',$3,$4,$5,$6,$7,decode(repeat('ab',32),'hex'))`,
		groupA, tenantA, adminM, orgA, legalA, adminA, groupRequestID)
	reject(t, conn, `INSERT INTO conversations
 (tenant_id,kind,group_name,group_creator_membership_id,group_creator_organization_id,
  group_creator_legal_entity_id,created_by_user_id,group_create_request_id,group_create_request_digest)
 VALUES ($1,'group','重复群',$2,$3,$4,$5,$6,decode(repeat('cd',32),'hex'))`,
		tenantA, adminM, orgA, legalA, adminA, groupRequestID)
	reject(t, conn, "UPDATE conversations SET group_create_request_id=$1 WHERE id=$2",
		"00000000-0000-4000-8000-000000000852", groupA)
	reject(t, conn, `INSERT INTO conversations
 (tenant_id,kind,direct_user_low_id,direct_user_high_id,direct_low_membership_id,direct_high_membership_id,
  created_by_user_id,group_create_request_id,group_create_request_digest)
 VALUES ($1,'direct',$2,$3,$4,$5,$2,$6,decode(repeat('ab',32),'hex'))`,
		tenantA, adminA, personA, adminM, targetM2, groupRequestID)
}

func TestGroupCreateRequestMigrationDownProtectsData(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, `INSERT INTO conversations
 (id,tenant_id,kind,group_name,group_creator_membership_id,group_creator_organization_id,
  group_creator_legal_entity_id,created_by_user_id,group_create_request_id,group_create_request_digest)
 VALUES ($1,$2,'group','项目群',$3,$4,$5,$6,$7,decode(repeat('ab',32),'hex'))`,
		groupA, tenantA, adminM, orgA, legalA, adminA, groupRequestID)
	transferDown, err := os.ReadFile("../../db/migrations/000012_group_owner_transfer.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(transferDown)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	invitationDown, err := os.ReadFile("../../db/migrations/000011_group_invitation.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(invitationDown)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile("../../db/migrations/000010_group_create_request.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(down)).ReadAll(); err == nil {
		t.Fatal("down removed group idempotency data")
	}
	if _, err := conn.Exec(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	run(t, conn, "DELETE FROM conversations WHERE id=$1", groupA)
	if _, err := conn.PgConn().Exec(context.Background(), string(down)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	var col *string
	if err := conn.QueryRow(context.Background(), `SELECT column_name FROM information_schema.columns
	 WHERE table_schema=current_schema() AND table_name='conversations' AND column_name='group_create_request_id'`).Scan(&col); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("group request ID after down: %v %v", col, err)
	}
	up, err := os.ReadFile("../../db/migrations/000010_group_create_request.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(up)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	invitationUp, err := os.ReadFile("../../db/migrations/000011_group_invitation.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(invitationUp)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	transferUp, err := os.ReadFile("../../db/migrations/000012_group_owner_transfer.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(transferUp)).ReadAll(); err != nil {
		t.Fatal(err)
	}
}
