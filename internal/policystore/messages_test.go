package policystore_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

const (
	messageA = "00000000-0000-4000-8000-000000000501"
	messageB = "00000000-0000-4000-8000-000000000502"
	clientA  = "0199f04a-0000-7000-8000-000000000501"
	clientB  = "0199f04a-0000-7000-8000-000000000502"
)

func seedDirectConversation(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	seed(t, conn)
	run(t, conn, `INSERT INTO conversations
 (id,tenant_id,direct_user_low_id,direct_user_high_id,direct_low_membership_id,direct_high_membership_id,created_by_user_id)
 VALUES ($1,$2,$3,$4,$5,$6,$3)`, directA, tenantA, adminA, personA, adminM, targetM2)
}

func TestMessageSchemaEnforcesTenantSequenceAndOutboxAssociation(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	ctx := context.Background()
	insert := `INSERT INTO messages
 (id,tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest)
 VALUES ($1,$2,$3,$4,$5,$6,$7,'你好',decode(repeat('ab',32),'hex'))`
	run(t, conn, insert, messageA, tenantA, directA, 1, adminA, adminM, clientA)
	for _, args := range [][]any{
		{messageB, tenantA, directA, 1, personA, targetM2, clientB},
		{messageB, tenantB, directA, 2, personB, otherM, clientB},
		{messageB, tenantA, directA, 2, adminA, targetM2, clientB},
		{messageB, tenantA, directA, 2, adminA, adminM, clientA},
	} {
		if _, err := conn.Exec(ctx, insert, args...); err == nil {
			t.Fatalf("invalid message accepted: %v", args)
		}
	}
	run(t, conn, `INSERT INTO message_idempotency
 (tenant_id,conversation_id,sender_user_id,client_msg_id,message_id,content_digest,accepted_at,expires_at)
 VALUES ($1,$2,$3,$4,$5,decode(repeat('ab',32),'hex'),'2026-09-28','2026-10-29')`,
		tenantA, directA, adminA, clientA, messageA)
	if _, err := conn.Exec(ctx, `INSERT INTO message_idempotency
 (tenant_id,conversation_id,sender_user_id,client_msg_id,message_id,content_digest,accepted_at,expires_at)
 VALUES ($1,$2,$3,$4,$5,decode(repeat('ab',32),'hex'),'2026-09-28','2026-10-29')`,
		tenantA, directA, personA, clientB, messageA); err == nil {
		t.Fatal("idempotency record linked to another sender's message")
	}
	run(t, conn, `INSERT INTO outbox_events
 (tenant_id,conversation_id,seq,message_id,event_type)
 VALUES ($1,$2,1,$3,'message_created')`, tenantA, directA, messageA)
	if _, err := conn.Exec(ctx, `INSERT INTO outbox_events
 (tenant_id,conversation_id,seq,message_id,event_type)
 VALUES ($1,$2,2,$3,'message_created')`, tenantA, directA, messageA); err == nil {
		t.Fatal("outbox event linked to wrong sequence")
	}
	var messages, events int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM messages WHERE tenant_id=$1", tenantA).Scan(&messages); err != nil || messages != 1 {
		t.Fatalf("message count: %d %v", messages, err)
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE tenant_id=$1", tenantA).Scan(&events); err != nil || events != 1 {
		t.Fatalf("event count: %d %v", events, err)
	}
}

func TestMessageMigrationRollsBackAndReapplies(t *testing.T) {
	conn := db(t)
	ctx := context.Background()
	down, err := os.ReadFile("../../db/migrations/000006_message_write.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(down)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	var relation *string
	if err := conn.QueryRow(ctx, "SELECT to_regclass('messages')::text").Scan(&relation); err != nil || relation != nil {
		t.Fatalf("messages after down: %v %v", relation, err)
	}
	up, err := os.ReadFile("../../db/migrations/000006_message_write.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(up)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	seedDirectConversation(t, conn)
}
