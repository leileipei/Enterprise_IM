package policystore_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func seedDigestMessage(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	seedBodyMessage(t, conn)
	run(t, conn, `UPDATE messages SET accepted_at=$2 WHERE id=$1`, messageA, at)
	run(t, conn, `INSERT INTO message_idempotency
 (tenant_id,conversation_id,sender_user_id,client_msg_id,message_id,content_digest,accepted_at,expires_at)
 SELECT tenant_id,conversation_id,sender_user_id,client_msg_id,id,content_digest,accepted_at,$2 FROM messages WHERE id=$1`, messageA, at.Add(30*24*time.Hour))
}

func digestMigration(t *testing.T, conn *pgx.Conn, direction string) error {
	t.Helper()
	if direction == "down" && fileMessageMigrationPresent(t, conn) {
		if e := fileMessageMigration(t, conn, "down"); e != nil {
			return e
		}
	}
	data, err := os.ReadFile("../../db/migrations/000016_message_digest_retirement." + direction + ".sql")
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.PgConn().Exec(context.Background(), string(data)).ReadAll()
	if err == nil && direction == "up" && !fileMessageMigrationPresent(t, conn) {
		return fileMessageMigration(t, conn, "up")
	}
	return err
}

func retireDigestPair(t *testing.T, conn *pgx.Conn, id string, when time.Time) {
	t.Helper()
	run(t, conn, "BEGIN")
	run(t, conn, `UPDATE messages SET content_digest=NULL,digest_retired_at=$2 WHERE id=$1`, id, when)
	run(t, conn, `UPDATE message_idempotency SET content_digest=NULL,digest_retired_at=$2 WHERE message_id=$1`, id, when)
	run(t, conn, "COMMIT")
}

const insertDigestBatch = `INSERT INTO message_digest_retirement_batches
 (tenant_id,conversation_id,retired_at,retired_count,first_seq,last_seq,min_expires_at,max_expires_at)
 VALUES ($1,$2,$3,$4,1,1,$3::timestamptz-INTERVAL '24 hours',$3)`

func TestDigestRetirementMigrationConstraints(t *testing.T) {
	conn := db(t)
	seedDigestMessage(t, conn)
	retired := at.Add(31 * 24 * time.Hour)
	var digest []byte
	var stamp *time.Time
	if err := conn.QueryRow(context.Background(), `SELECT content_digest,digest_retired_at FROM messages WHERE id=$1`, messageA).Scan(&digest, &stamp); err != nil || len(digest) != 32 || stamp != nil {
		t.Fatalf("old digest: %d %v %v", len(digest), stamp, err)
	}
	for _, table := range []string{"messages", "message_idempotency"} {
		reject(t, conn, "UPDATE "+table+" SET content_digest=decode('aa','hex')")
		reject(t, conn, "UPDATE "+table+" SET content_digest=NULL")
	}
	reject(t, conn, `UPDATE messages SET content_digest=NULL,digest_retired_at=$1`, retired)
	run(t, conn, `UPDATE messages SET text_body=NULL,body_cleared_at=$2 WHERE id=$1`, messageA, at.Add(24*time.Hour))
	reject(t, conn, `UPDATE message_idempotency SET content_digest=NULL,digest_retired_at=$1`, at.Add(29*24*time.Hour))
	reject(t, conn, `UPDATE messages SET content_digest=NULL,digest_retired_at=$1`, at)
	for _, which := range []string{"message", "idempotency", "mismatch"} {
		run(t, conn, "BEGIN")
		if which != "idempotency" {
			run(t, conn, `UPDATE messages SET content_digest=NULL,digest_retired_at=$1`, retired)
		}
		if which != "message" {
			when := retired
			if which == "mismatch" {
				when = when.Add(time.Second)
			}
			run(t, conn, `UPDATE message_idempotency SET content_digest=NULL,digest_retired_at=$1`, when)
		}
		reject(t, conn, "SET CONSTRAINTS ALL IMMEDIATE")
		run(t, conn, "ROLLBACK")
	}
	reject(t, conn, `INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,body_cleared_at,content_digest,digest_retired_at)
 SELECT tenant_id,conversation_id,2,sender_user_id,sender_membership_id,$2,NULL,body_cleared_at,NULL,$3 FROM messages WHERE id=$1`, messageA, clientUUIDv7(at, 999), retired)
	retireDigestPair(t, conn, messageA, retired)
	for _, sql := range []string{
		"UPDATE messages SET content_digest=decode(repeat('ab',32),'hex'),digest_retired_at=NULL",
		"UPDATE messages SET digest_retired_at=digest_retired_at+INTERVAL '1 second'",
		"UPDATE messages SET accepted_at=accepted_at+INTERVAL '1 second'",
		"UPDATE messages SET seq=2", "DELETE FROM message_idempotency",
		"UPDATE message_idempotency SET content_digest=decode(repeat('ab',32),'hex'),digest_retired_at=NULL",
		"UPDATE message_idempotency SET digest_retired_at=digest_retired_at+INTERVAL '1 second'",
		"UPDATE message_idempotency SET expires_at=expires_at+INTERVAL '1 second'",
		"UPDATE message_idempotency SET accepted_at=accepted_at-INTERVAL '1 second'",
	} {
		reject(t, conn, sql)
	}
	var count int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM messages m JOIN message_idempotency i ON i.message_id=m.id AND i.tenant_id=m.tenant_id WHERE m.content_digest IS NULL AND i.content_digest IS NULL AND m.digest_retired_at=i.digest_retired_at`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("pair: %d %v", count, err)
	}
}

func TestDigestRetirementMigrationRollbackAndEvidence(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		conn := db(t)
		seedDigestMessage(t, conn)
		if err := digestMigration(t, conn, "down"); err != nil {
			t.Fatal(err)
		}
		if err := digestMigration(t, conn, "up"); err != nil {
			t.Fatal(err)
		}
	})
	for _, history := range []string{"pair", "batch"} {
		t.Run(history, func(t *testing.T) {
			conn := db(t)
			seedDigestMessage(t, conn)
			when := at.Add(31 * 24 * time.Hour)
			if history == "pair" {
				run(t, conn, `UPDATE messages SET text_body=NULL,body_cleared_at=$1`, at)
				retireDigestPair(t, conn, messageA, when)
			} else {
				reject(t, conn, insertDigestBatch, tenantB, directA, when, 1)
				reject(t, conn, insertDigestBatch, tenantA, directA, when, 0)
				reject(t, conn, insertDigestBatch, tenantA, directA, when, 1001)
				reject(t, conn, `INSERT INTO message_digest_retirement_batches(tenant_id,conversation_id,retired_at,retired_count,first_seq,last_seq,min_expires_at,max_expires_at) VALUES ($1,$2,$3,1,2,1,$3,$3)`, tenantA, directA, when)
				reject(t, conn, `INSERT INTO message_digest_retirement_batches(tenant_id,conversation_id,retired_at,retired_count,first_seq,last_seq,min_expires_at,max_expires_at) VALUES ($1,$2,$3,1,1,1,$3,$3::timestamptz+INTERVAL '1 second')`, tenantA, directA, when)
				run(t, conn, insertDigestBatch, tenantA, directA, when, 1)
				reject(t, conn, "UPDATE message_digest_retirement_batches SET retired_count=2")
				reject(t, conn, "DELETE FROM message_digest_retirement_batches")
			}
			if err := digestMigration(t, conn, "down"); err == nil {
				t.Fatal("discarded history")
			}
			run(t, conn, "ROLLBACK")
			var exists bool
			if err := conn.QueryRow(context.Background(), `SELECT to_regclass('message_digest_retirement_batches') IS NOT NULL`).Scan(&exists); err != nil || !exists {
				t.Fatalf("schema: %v %v", exists, err)
			}
		})
	}
	t.Run("concurrent", func(t *testing.T) {
		first := db(t)
		seedDigestMessage(t, first)
		second := secondDB(t, first)
		run(t, first, `UPDATE messages SET text_body=NULL,body_cleared_at=$1`, at)
		run(t, first, "BEGIN")
		when := at.Add(31 * 24 * time.Hour)
		run(t, first, `UPDATE messages SET content_digest=NULL,digest_retired_at=$1`, when)
		run(t, first, `UPDATE message_idempotency SET content_digest=NULL,digest_retired_at=$1`, when)
		data, err := os.ReadFile("../../db/migrations/000016_message_digest_retirement.down.sql")
		prefix, pe := os.ReadFile("../../db/migrations/000020_file_message.down.sql")
		if pe != nil {
			t.Fatal(pe)
		}
		data = append(prefix, data...)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := second.PgConn().Exec(ctx, string(data)).ReadAll(); done <- err }()
		for {
			var wait *string
			if err := first.QueryRow(ctx, "SELECT wait_event_type FROM pg_stat_activity WHERE pid=$1", second.PgConn().PID()).Scan(&wait); err != nil {
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
		run(t, first, "COMMIT")
		if err := <-done; err == nil {
			t.Fatal("down crossed retirement")
		}
		run(t, second, "ROLLBACK")
	})
}
