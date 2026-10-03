package access_test

import (
	"context"
	"os"
	"testing"
	"time"
)

const (
	legalHoldConversation = "00000000-0000-4000-8000-000000000191"
	legalHoldOne          = "00000000-0000-4000-8000-000000000192"
	legalHoldTwo          = "00000000-0000-4000-8000-000000000193"
	legalHoldRequestOne   = "00000000-0000-4000-8000-000000000194"
	legalHoldRequestTwo   = "00000000-0000-4000-8000-000000000195"
	legalHoldRelease      = "00000000-0000-4000-8000-000000000196"
)

func TestLegalHoldMigrationConstraintsAndRollback(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, `INSERT INTO conversations
 (id,tenant_id,direct_user_low_id,direct_user_high_id,direct_low_membership_id,
 direct_high_membership_id,created_by_user_id)
 VALUES ($1,$2,$3,$4,$5,$6,$3)`, legalHoldConversation, tenantA, adminA, personA,
		adminM, personM)
	run(t, conn, `INSERT INTO conversation_legal_holds
 (id,tenant_id,conversation_id,case_reference,create_request_id,
 placed_by_user_id,placed_by_membership_id)
 VALUES ($1,$2,$3,'CASE-A',$4,$5,$6)`, legalHoldOne, tenantA, legalHoldConversation,
		legalHoldRequestOne, adminA, adminM)
	reject(t, conn, `INSERT INTO conversation_legal_holds
 (id,tenant_id,conversation_id,case_reference,create_request_id,
 placed_by_user_id,placed_by_membership_id)
 VALUES ($1,$2,$3,'CASE-X',$4,$5,$6)`, legalHoldTwo, tenantB, legalHoldConversation,
		legalHoldRequestTwo, personB, personBM)
	reject(t, conn, `INSERT INTO conversation_legal_holds
 (id,tenant_id,conversation_id,case_reference,create_request_id,
 placed_by_user_id,placed_by_membership_id)
 VALUES ($1,$2,$3,'CASE-X',$4,$5,$6)`, legalHoldTwo, tenantA, legalHoldConversation,
		legalHoldRequestTwo, personB, personBM)
	reject(t, conn, `INSERT INTO conversation_legal_holds
 (id,tenant_id,conversation_id,case_reference,create_request_id,
 placed_by_user_id,placed_by_membership_id)
 VALUES ($1,$2,$3,'CASE-A',$4,$5,$6)`, legalHoldTwo, tenantA, legalHoldConversation,
		legalHoldRequestTwo, adminA, adminM)
	run(t, conn, `INSERT INTO conversation_legal_holds
 (id,tenant_id,conversation_id,case_reference,create_request_id,
 placed_by_user_id,placed_by_membership_id)
 VALUES ($1,$2,$3,'CASE-B',$4,$5,$6)`, legalHoldTwo, tenantA, legalHoldConversation,
		legalHoldRequestTwo, adminA, adminM)
	reject(t, conn, `UPDATE conversation_legal_holds SET released_at=now()
 WHERE tenant_id=$1 AND id=$2`, tenantA, legalHoldOne)
	run(t, conn, `INSERT INTO conversation_legal_hold_events
 (tenant_id,conversation_id,hold_id,event_type,request_id,reference,
 actor_user_id,acting_membership_id)
 VALUES ($1,$2,$3,'placed',$4,'CASE-A',$5,$6)`, tenantA, legalHoldConversation,
		legalHoldOne, legalHoldRequestOne, adminA, adminM)
	reject(t, conn, `UPDATE conversation_legal_hold_events SET reference='rewritten'
 WHERE tenant_id=$1 AND hold_id=$2`, tenantA, legalHoldOne)
	reject(t, conn, `DELETE FROM conversation_legal_hold_events
 WHERE tenant_id=$1 AND hold_id=$2`, tenantA, legalHoldOne)
	reject(t, conn, `INSERT INTO conversation_legal_hold_events
 (tenant_id,conversation_id,hold_id,event_type,request_id,reference,
 actor_user_id,acting_membership_id)
 VALUES ($1,$2,$3,'placed',$4,'CASE-B',$5,$6)`, tenantA, legalHoldConversation,
		legalHoldTwo, legalHoldRequestOne, adminA, adminM)
	run(t, conn, `UPDATE conversation_legal_holds SET release_approval_reference='CAB-1',
 release_request_id=$3,released_by_user_id=$4,released_by_membership_id=$5,
 released_at=now() WHERE tenant_id=$1 AND id=$2`, tenantA, legalHoldOne,
		legalHoldRelease, adminA, adminM)
	reject(t, conn, `UPDATE conversation_legal_holds SET release_approval_reference='rewritten'
 WHERE tenant_id=$1 AND id=$2`, tenantA, legalHoldOne)
	down, err := os.ReadFile("../../db/migrations/000014_conversation_legal_hold.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(down)).ReadAll(); err == nil {
		t.Fatal("rollback discarded legal hold evidence")
	}
	_, _ = conn.Exec(context.Background(), "ROLLBACK")
}

func TestLegalHoldMigrationEmptyRollbackReapplies(t *testing.T) {
	conn := testDB(t)
	ctx := context.Background()
	down, err := os.ReadFile("../../db/migrations/000014_conversation_legal_hold.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(down)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	var table *string
	if err := conn.QueryRow(ctx, "SELECT to_regclass('conversation_legal_holds')::text").Scan(&table); err != nil || table != nil {
		t.Fatalf("hold table survived empty rollback: %v %v", table, err)
	}
	up, err := os.ReadFile("../../db/migrations/000014_conversation_legal_hold.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(up)).ReadAll(); err != nil {
		t.Fatal(err)
	}
}

func TestLegalHoldMigrationRollbackWaitsForConcurrentEvidence(t *testing.T) {
	first := testDB(t)
	seedAccess(t, first)
	run(t, first, `INSERT INTO conversations
 (id,tenant_id,direct_user_low_id,direct_user_high_id,direct_low_membership_id,
 direct_high_membership_id,created_by_user_id)
 VALUES ($1,$2,$3,$4,$5,$6,$3)`, legalHoldConversation, tenantA, adminA, personA,
		adminM, personM)
	second := secondConnection(t, first)
	writer, err := first.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(context.Background(), `INSERT INTO conversation_legal_holds
 (id,tenant_id,conversation_id,case_reference,create_request_id,
 placed_by_user_id,placed_by_membership_id)
 VALUES ($1,$2,$3,'CASE-CONCURRENT',$4,$5,$6)`,
		legalHoldOne, tenantA, legalHoldConversation, legalHoldRequestOne, adminA, adminM); err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile("../../db/migrations/000014_conversation_legal_hold.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		_, err := second.PgConn().Exec(context.Background(), string(down)).ReadAll()
		result <- err
	}()
	<-started
	time.Sleep(50 * time.Millisecond)
	if err := writer.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil {
		t.Fatal("rollback discarded evidence committed while it waited")
	}
	_, _ = second.Exec(context.Background(), "ROLLBACK")
	var count int
	if err := first.QueryRow(context.Background(), `SELECT count(*) FROM conversation_legal_holds
 WHERE id=$1`, legalHoldOne).Scan(&count); err != nil || count != 1 {
		t.Fatalf("concurrent evidence lost: count=%d err=%v", count, err)
	}
}
