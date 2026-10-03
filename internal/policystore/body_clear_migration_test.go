package policystore_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func seedBodyMessage(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	seedDirectConversation(t, conn)
	run(t, conn, `INSERT INTO messages (id,tenant_id,conversation_id,seq,
 sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest)
 VALUES ($1,$2,$3,1,$4,$5,$6,'original',decode(repeat('ab',32),'hex'))`,
		messageA, tenantA, directA, adminA, adminM, clientA)
}

func bodyMigration(t *testing.T, conn *pgx.Conn, direction string) error {
	t.Helper()
	if direction == "down" {
		if err := digestMigration(t, conn, "down"); err != nil {
			return err
		}
	}
	data, err := os.ReadFile("../../db/migrations/000015_message_body_clear." + direction + ".sql")
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.PgConn().Exec(context.Background(), string(data)).ReadAll()
	if err == nil && direction == "up" {
		return digestMigration(t, conn, "up")
	}
	return err
}

const insertBatch = `INSERT INTO message_body_clear_batches
 (tenant_id,conversation_id,retention_days,cutoff_at,cleared_at,first_seq,last_seq,cleared_count)
 VALUES ($1,$2,365,$3::timestamptz-INTERVAL '8760 hours',$3,1,1,$4)`

func TestBodyClearMigrationConstraints(t *testing.T) {
	conn := db(t)
	seedBodyMessage(t, conn)
	var body *string
	var cleared *time.Time
	if err := conn.QueryRow(context.Background(), "SELECT text_body,body_cleared_at FROM messages WHERE id=$1", messageA).Scan(&body, &cleared); err != nil || body == nil || *body != "original" || cleared != nil {
		t.Fatalf("old message not preserved: %v %v %v", body, cleared, err)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("x", 16385)} {
		reject(t, conn, "UPDATE messages SET text_body=$2 WHERE id=$1", messageA, bad)
	}
	reject(t, conn, "UPDATE messages SET text_body=NULL WHERE id=$1", messageA)
	reject(t, conn, "UPDATE messages SET body_cleared_at=$2 WHERE id=$1", messageA, at)
	run(t, conn, "UPDATE messages SET text_body=NULL,body_cleared_at=$2 WHERE id=$1", messageA, at)
	reject(t, conn, "UPDATE messages SET text_body='restored',body_cleared_at=NULL WHERE id=$1", messageA)
	reject(t, conn, "UPDATE messages SET body_cleared_at=NULL WHERE id=$1", messageA)
	reject(t, conn, "UPDATE messages SET body_cleared_at=$2 WHERE id=$1", messageA, at.Add(time.Second))
	reject(t, conn, insertBatch, tenantB, directA, at, 1)
	reject(t, conn, insertBatch, tenantA, directA, at, 0)
	reject(t, conn, insertBatch, tenantA, directA, at, 1001)
	run(t, conn, insertBatch, tenantA, directA, at, 1)
	reject(t, conn, "UPDATE message_body_clear_batches SET cleared_count=2")
	reject(t, conn, "DELETE FROM message_body_clear_batches")
	reject(t, conn, `INSERT INTO message_body_clear_batches
 (tenant_id,conversation_id,retention_days,cutoff_at,cleared_at,first_seq,last_seq,cleared_count)
 VALUES ($1,$2,365,$3,$3,1,1,1)`, tenantA, directA, at)
	var count int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname='messages_body_clear_candidates'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("candidate index: %d %v", count, err)
	}
}

func TestBodyClearMigrationRollback(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		conn := db(t)
		seedBodyMessage(t, conn)
		if err := bodyMigration(t, conn, "down"); err != nil {
			t.Fatal(err)
		}
		if err := bodyMigration(t, conn, "up"); err != nil {
			t.Fatal(err)
		}
	})
	for _, history := range []string{"message", "batch"} {
		t.Run(history, func(t *testing.T) {
			conn := db(t)
			seedBodyMessage(t, conn)
			if history == "message" {
				run(t, conn, "UPDATE messages SET text_body=NULL,body_cleared_at=$2 WHERE id=$1", messageA, at)
			} else {
				run(t, conn, insertBatch, tenantA, directA, at, 1)
			}
			if err := bodyMigration(t, conn, "down"); err == nil {
				t.Fatal("down discarded cleaning history")
			}
			run(t, conn, "ROLLBACK")
			var exists bool
			if err := conn.QueryRow(context.Background(), "SELECT to_regclass('message_body_clear_batches') IS NOT NULL").Scan(&exists); err != nil || !exists {
				t.Fatalf("rejected down changed schema: %v %v", exists, err)
			}
		})
	}
	t.Run("concurrent clearing", func(t *testing.T) {
		first := db(t)
		seedBodyMessage(t, first)
		second := secondDB(t, first)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tx, err := first.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(ctx, "UPDATE messages SET text_body=NULL,body_cleared_at=$2 WHERE id=$1", messageA, at); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile("../../db/migrations/000015_message_body_clear.down.sql")
		prefix, prefixErr := os.ReadFile("../../db/migrations/000016_message_digest_retirement.down.sql")
		if prefixErr != nil {
			t.Fatal(prefixErr)
		}
		data = append(prefix, data...)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := second.PgConn().Exec(ctx, string(data)).ReadAll(); done <- err }()
		for {
			var wait *string
			if err := tx.QueryRow(ctx, "SELECT wait_event_type FROM pg_stat_activity WHERE pid=$1", second.PgConn().PID()).Scan(&wait); err != nil {
				t.Fatal(err)
			}
			if wait != nil && *wait == "Lock" {
				break
			}
			select {
			case err := <-done:
				t.Fatalf("down did not wait: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err == nil {
			t.Fatal("down crossed concurrent clearing")
		}
		run(t, second, "ROLLBACK")
		var count int
		if err := first.QueryRow(ctx, "SELECT count(*) FROM messages WHERE body_cleared_at IS NOT NULL").Scan(&count); err != nil || count != 1 {
			t.Fatalf("lost clearing history: %d %v", count, err)
		}
	})
}
