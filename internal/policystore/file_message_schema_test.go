package policystore_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestFileMessageMigrationPreservesText(t *testing.T) {
	for _, state := range []string{"active", "cleared", "retired"} {
		t.Run(state, func(t *testing.T) {
			c := fileMessageDB(t)
			seedDigestMessage(t, c)
			if state != "active" {
				run(t, c, "UPDATE messages SET text_body=NULL,body_cleared_at=$1", at)
			}
			if state == "retired" {
				retireDigestPair(t, c, messageA, at.Add(31*24*time.Hour))
			}
			var beforeBody *string
			var beforeDigest []byte
			var beforeCleared, beforeRetired *time.Time
			query := "SELECT text_body,content_digest,body_cleared_at,digest_retired_at FROM messages WHERE id=$1"
			if e := c.QueryRow(context.Background(), query, messageA).Scan(&beforeBody, &beforeDigest, &beforeCleared, &beforeRetired); e != nil {
				t.Fatal(e)
			}
			if e := fileMessageMigration(t, c, "down"); e != nil {
				t.Fatal(e)
			}
			if e := fileMessageMigration(t, c, "up"); e != nil {
				t.Fatal(e)
			}
			var kind string
			var body *string
			var digest []byte
			var cleared, retired *time.Time
			if e := c.QueryRow(context.Background(), "SELECT message_type,text_body,content_digest,body_cleared_at,digest_retired_at FROM messages WHERE id=$1", messageA).Scan(&kind, &body, &digest, &cleared, &retired); e != nil {
				t.Fatal(e)
			}
			sameBody := beforeBody == nil && body == nil || beforeBody != nil && body != nil && *beforeBody == *body
			sameTime := func(a, b *time.Time) bool { return a == nil && b == nil || a != nil && b != nil && a.Equal(*b) }
			if kind != "text" || !sameBody || !bytes.Equal(digest, beforeDigest) || !sameTime(cleared, beforeCleared) || !sameTime(retired, beforeRetired) {
				t.Fatal("migration changed historical text evidence", kind, body, digest, cleared, retired)
			}
		})
	}
}
func TestFileMessageSchemaSources(t *testing.T) {
	for _, which := range []string{"tenant", "conversation", "user", "membership"} {
		t.Run(which, func(t *testing.T) {
			c := fileMessageDB(t)
			seedDirectConversation(t, c)
			m := fileMessageFixture(t, c, directA, adminA, adminM)
			tx, e := c.Begin(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(context.Background())
			id := rawFileMessage(t, tx, m, 1, "file", "")
			bad := m
			switch which {
			case "tenant":
				bad.TenantID = tenantB
			case "conversation":
				bad.ConversationID = directB
			case "user":
				bad.UploaderUserID = personA
			case "membership":
				bad.UploaderMembershipID = targetM2
			}
			if e = rawFileBinding(context.Background(), tx, bad, id, m.SHA256); e == nil {
				e = tx.Commit(context.Background())
			}
			if e == nil {
				t.Fatal("cross-source binding committed", which)
			}
		})
	}
}
func TestFileMessageSchemaCardinality(t *testing.T) {
	for _, which := range []string{"file without link", "text with link", "same file twice", "caption too long", "kind mutation", "link mutation", "link delete", "empty caption"} {
		t.Run(which, func(t *testing.T) {
			c := fileMessageDB(t)
			seedDirectConversation(t, c)
			m := fileMessageFixture(t, c, directA, adminA, adminM)
			ctx := context.Background()
			if which == "file without link" || which == "text with link" || which == "caption too long" {
				tx, e := c.Begin(ctx)
				if e != nil {
					t.Fatal(e)
				}
				defer tx.Rollback(ctx)
				if which == "caption too long" {
					_, e = tx.Exec(ctx, `INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,message_type,text_body,content_digest) VALUES($1,$2,1,$3,$4,$5,'file',$6,decode(repeat('ab',32),'hex'))`, tenantA, directA, adminA, adminM, clientUUIDv7(at, 7111), strings.Repeat("a", 16385))
					expectFileSQLState(t, e, "23514")
					return
				}
				kind := "file"
				caption := ""
				if which == "text with link" {
					kind = "text"
					caption = "text"
				}
				id := rawFileMessage(t, tx, m, 1, kind, caption)
				if which == "text with link" {
					e = rawFileBinding(ctx, tx, m, id, m.SHA256)
				}
				if e == nil {
					e = tx.Commit(ctx)
				}
				expectFileSQLState(t, e, "23514")
				return
			}
			id := bindFixtureMessage(t, c, m, 1, "")
			switch which {
			case "same file twice":
				tx, e := c.Begin(ctx)
				if e != nil {
					t.Fatal(e)
				}
				defer tx.Rollback(ctx)
				second := rawFileMessage(t, tx, m, 2, "file", "")
				e = rawFileBinding(ctx, tx, m, second, m.SHA256)
				expectFileSQLState(t, e, "23505")
			case "kind mutation":
				reject(t, c, "UPDATE messages SET message_type='text',text_body='changed' WHERE id=$1", id)
			case "link mutation":
				reject(t, c, "UPDATE message_attachments SET file_id=$2 WHERE message_id=$1", id, freshFile().ID)
			case "link delete":
				reject(t, c, "DELETE FROM message_attachments WHERE message_id=$1", id)
			case "empty caption":
				var body *string
				if e := c.QueryRow(ctx, "SELECT text_body FROM messages WHERE id=$1", id).Scan(&body); e != nil || body == nil || *body != "" {
					t.Fatal(body, e)
				}
				run(t, c, "UPDATE messages SET text_body=NULL,body_cleared_at=$2 WHERE id=$1", id, at)
				reject(t, c, "UPDATE messages SET text_body='',body_cleared_at=NULL WHERE id=$1", id)
			}
		})
	}
}
func TestFileMessageSchemaReadyBinding(t *testing.T) {
	for _, state := range []string{"allocated", "uploaded", "scanning", "scan_failed", "rejected", "delete_pending", "deleted", "wrong SHA"} {
		t.Run(state, func(t *testing.T) {
			c := fileMessageDB(t)
			seedDirectConversation(t, c)
			s := state
			if s == "wrong SHA" {
				s = "ready"
			}
			m := storedFile(t, c, s)
			tx, e := c.Begin(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(context.Background())
			id := rawFileMessage(t, tx, m, 1, "file", "")
			sha := m.SHA256
			if state == "wrong SHA" {
				sha = bytes.Repeat([]byte{1}, 32)
			}
			if e = rawFileBinding(context.Background(), tx, m, id, sha); e == nil {
				e = tx.Commit(context.Background())
			}
			expectFileSQLState(t, e, "23514")
		})
	}
}
func TestFileMessageSchemaFingerprintRetirement(t *testing.T) {
	c := fileMessageDB(t)
	seedDirectConversation(t, c)
	m := fileMessageFixture(t, c, directA, adminA, adminM)
	id := bindFixtureMessage(t, c, m, 1, "caption")
	run(t, c, "UPDATE messages SET text_body=NULL,body_cleared_at=$2 WHERE id=$1", id, at)
	stamp := at.Add(31 * 24 * time.Hour)
	for _, which := range []string{"message", "key", "pair", "fingerprint", "mismatch"} {
		t.Run(which, func(t *testing.T) {
			tx, e := c.Begin(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(context.Background())
			if which == "message" || which == "pair" || which == "mismatch" {
				_, e = tx.Exec(context.Background(), "UPDATE messages SET content_digest=NULL,digest_retired_at=$2 WHERE id=$1", id, stamp)
				if e != nil {
					t.Fatal(e)
				}
			}
			if which == "key" || which == "pair" || which == "mismatch" {
				_, e = tx.Exec(context.Background(), "UPDATE message_idempotency SET content_digest=NULL,digest_retired_at=$2 WHERE message_id=$1", id, stamp)
				if e != nil {
					t.Fatal(e)
				}
			}
			if which == "fingerprint" || which == "mismatch" {
				when := stamp
				if which == "mismatch" {
					when = when.Add(time.Second)
				}
				_, e = tx.Exec(context.Background(), "UPDATE message_attachments SET sealed_sha256=NULL,fingerprint_retired_at=$2 WHERE message_id=$1", id, when)
				if e != nil {
					t.Fatal(e)
				}
			}
			expectFileSQLState(t, tx.Commit(context.Background()), "23514")
		})
	}
	run(t, c, "BEGIN")
	run(t, c, "UPDATE messages SET content_digest=NULL,digest_retired_at=$2 WHERE id=$1", id, stamp)
	run(t, c, "UPDATE message_idempotency SET content_digest=NULL,digest_retired_at=$2 WHERE message_id=$1", id, stamp)
	run(t, c, "UPDATE message_attachments SET sealed_sha256=NULL,fingerprint_retired_at=$2 WHERE message_id=$1", id, stamp)
	run(t, c, "COMMIT")
	reject(t, c, "UPDATE message_attachments SET sealed_sha256=$2,fingerprint_retired_at=NULL WHERE message_id=$1", id, m.SHA256)
	reject(t, c, "DELETE FROM message_attachments WHERE message_id=$1", id)
	var sha []byte
	var when time.Time
	if e := c.QueryRow(context.Background(), "SELECT sealed_sha256,fingerprint_retired_at FROM message_attachments WHERE message_id=$1", id).Scan(&sha, &when); e != nil || sha != nil || !when.Equal(stamp) {
		t.Fatal(sha, when, e)
	}
}
func TestFileMessageDownGuards(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		c := fileMessageDB(t)
		seedBodyMessage(t, c)
		if e := fileMessageMigration(t, c, "down"); e != nil {
			t.Fatal(e)
		}
		if e := fileMessageMigration(t, c, "up"); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("bound", func(t *testing.T) {
		c := fileMessageDB(t)
		seedDirectConversation(t, c)
		m := fileMessageFixture(t, c, directA, adminA, adminM)
		id := bindFixtureMessage(t, c, m, 1, "")
		expectFileSQLState(t, fileMessageMigration(t, c, "down"), "23514")
		run(t, c, "ROLLBACK")
		var n int
		if e := c.QueryRow(context.Background(), "SELECT count(*) FROM message_attachments WHERE message_id=$1", id).Scan(&n); e != nil || n != 1 {
			t.Fatal(n, e)
		}
	})
}
func TestFileMessageDownConcurrentInsert(t *testing.T) {
	c := fileMessageDB(t)
	seedDirectConversation(t, c)
	m := fileMessageFixture(t, c, directA, adminA, adminM)
	peer := filePeer(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	id := rawFileMessage(t, tx, m, 1, "file", "")
	if e = rawFileBinding(ctx, tx, m, id, m.SHA256); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- fileMessageMigration(t, peer, "down") }()
	for {
		var wait *string
		if e = tx.QueryRow(ctx, "SELECT wait_event_type FROM pg_stat_activity WHERE pid=$1", peer.PgConn().PID()).Scan(&wait); e != nil {
			t.Fatal(e)
		}
		if wait != nil && *wait == "Lock" {
			break
		}
		select {
		case e = <-done:
			t.Fatalf("down did not wait: %v", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	expectFileSQLState(t, <-done, "23514")
	run(t, peer, "ROLLBACK")
	var n int
	if e = c.QueryRow(ctx, "SELECT count(*) FROM message_attachments WHERE message_id=$1", id).Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
}
