package policystore_test

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"os"
	"testing"
	"time"
)

func fileMessageMigration(t *testing.T, c *pgx.Conn, direction string) error {
	t.Helper()
	if direction == "down" && fileDownloadMigrationPresent(t, c) {
		if e := fileDownloadMigration(t, c, "down"); e != nil {
			return e
		}
	}
	b, e := os.ReadFile("../../db/migrations/000020_file_message." + direction + ".sql")
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.PgConn().Exec(context.Background(), string(b)).ReadAll()
	if e == nil && direction == "up" && !fileDownloadMigrationPresent(t, c) {
		return fileDownloadMigration(t, c, "up")
	}
	return e
}
func fileMessageMigrationPresent(t *testing.T, c *pgx.Conn) bool {
	t.Helper()
	var present bool
	if e := c.QueryRow(context.Background(), "SELECT to_regclass('message_attachments') IS NOT NULL").Scan(&present); e != nil {
		t.Fatal(e)
	}
	return present
}
func fileMessageDB(t *testing.T) *pgx.Conn {
	t.Helper()
	c := db(t)
	if !fileMessageMigrationPresent(t, c) {
		if e := fileMessageMigration(t, c, "up"); e != nil {
			t.Fatal(e)
		}
	}
	return c
}

// Only a database-boundary fixture. Real scan evidence is tested separately.
func fileMessageFixture(t *testing.T, c *pgx.Conn, conversationID, userID, membershipID string) files.Metadata {
	t.Helper()
	m := freshFile()
	m.ConversationID = conversationID
	m.UploaderUserID = userID
	m.UploaderMembershipID = membershipID
	d, e := files.CreationDigest(m.CreateParams)
	if e != nil {
		t.Fatal(e)
	}
	m.RequestDigest = d[:]
	if e = writeFile(c, m, true); e != nil {
		t.Fatal(e)
	}
	for _, state := range []string{"uploaded", "scanning", "ready"} {
		m = fileNext(m, state)
		if e = writeFile(c, m, false); e != nil {
			t.Fatal(e)
		}
	}
	return m
}
func rawFileMessage(t *testing.T, tx pgx.Tx, m files.Metadata, seq int64, kind, caption string) string {
	t.Helper()
	var id string
	client := clientUUIDv7(at, int(seq)+7000)
	e := tx.QueryRow(context.Background(), `INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,message_type,text_body,content_digest,accepted_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,decode(repeat('ab',32),'hex'),$9) RETURNING id::text`, m.TenantID, m.ConversationID, seq, m.UploaderUserID, m.UploaderMembershipID, client, kind, caption, at).Scan(&id)
	if e != nil {
		t.Fatal(e)
	}
	_, e = tx.Exec(context.Background(), `INSERT INTO message_idempotency(tenant_id,conversation_id,sender_user_id,client_msg_id,message_id,content_digest,accepted_at,expires_at)
 VALUES($1,$2,$3,$4,$5,decode(repeat('ab',32),'hex'),$6,$7)`, m.TenantID, m.ConversationID, m.UploaderUserID, client, id, at, at.Add(30*24*time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func rawFileBinding(ctx context.Context, tx pgx.Tx, m files.Metadata, messageID string, sha []byte) error {
	_, e := tx.Exec(ctx, `INSERT INTO message_attachments(tenant_id,conversation_id,message_id,sender_user_id,sender_membership_id,file_id,sealed_sha256)
 VALUES($1,$2,$3,$4,$5,$6,$7)`, m.TenantID, m.ConversationID, messageID, m.UploaderUserID, m.UploaderMembershipID, m.ID, sha)
	return e
}
func bindFixtureMessage(t *testing.T, c *pgx.Conn, m files.Metadata, seq int64, caption string) string {
	t.Helper()
	tx, e := c.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	id := rawFileMessage(t, tx, m, seq, "file", caption)
	if e = rawFileBinding(context.Background(), tx, m, id, m.SHA256); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(context.Background()); e != nil {
		t.Fatal(e)
	}
	return id
}
