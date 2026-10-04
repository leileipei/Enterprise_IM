package policystore_test

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"os"
	"testing"
	"time"
)

func fileDownloadMigration(t *testing.T, c *pgx.Conn, direction string) error {
	t.Helper()
	b, e := os.ReadFile("../../db/migrations/000021_file_download_retention." + direction + ".sql")
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.PgConn().Exec(context.Background(), string(b)).ReadAll()
	return e
}
func fileDownloadDB(t *testing.T) *pgx.Conn {
	t.Helper()
	c := fileMessageDB(t)
	var exists bool
	if e := c.QueryRow(context.Background(), "SELECT to_regclass('file_download_sessions') IS NOT NULL").Scan(&exists); e != nil {
		t.Fatal(e)
	}
	if !exists {
		if e := fileDownloadMigration(t, c, "up"); e != nil {
			t.Fatal(e)
		}
	}
	return c
}
func downloadFixture(t *testing.T, c *pgx.Conn) (files.Metadata, string) {
	t.Helper()
	seedDirectConversation(t, c)
	m := fileMessageFixture(t, c, directA, adminA, adminM)
	return m, bindFixtureMessage(t, c, m, 1, "")
}
func insertDownloadSession(c *pgx.Conn, m files.Metadata, mid string, phase string) (string, error) {
	id := freshFile().ID
	_, e := c.Exec(context.Background(), `INSERT INTO file_download_sessions(id,tenant_id,file_id,conversation_id,message_id,requester_user_id,source_membership_id,owner_id,lease_token,phase,expected_bytes,created_at,updated_at,deadline,lease_expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$1,$1,$8,$9,$10,$10,$11,$11)`, id, m.TenantID, m.ID, m.ConversationID, mid, adminA, adminM, phase, m.DeclaredSizeBytes, at.Add(time.Minute), at.Add(2*time.Minute))
	return id, e
}
func insertDeleteJob(t *testing.T, c *pgx.Conn, m files.Metadata) string {
	t.Helper()
	id := freshFile().ID
	run(t, c, `INSERT INTO file_delete_jobs(id,tenant_id,file_id,conversation_id,policy_version,expected_state_version,owner_id,lease_token,lease_expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,0,$5,$1,$1,$6,$7,$7)`, id, m.TenantID, m.ID, m.ConversationID, m.StateVersion, at.Add(time.Minute), at)
	return id
}
func rawSchemaHold(c *pgx.Conn, convo string) error {
	_, e := c.Exec(context.Background(), `INSERT INTO conversation_legal_holds(tenant_id,conversation_id,case_reference,create_request_id,placed_by_user_id,placed_by_membership_id,placed_at) VALUES($1,$2,'case-test',$3,$4,$5,$6)`, tenantA, convo, freshFile().ID, adminA, adminM, at)
	return e
}

func fileDownloadMigrationPresent(t *testing.T, c *pgx.Conn) bool {
	t.Helper()
	var exists bool
	if e := c.QueryRow(context.Background(), "SELECT to_regclass('file_download_sessions') IS NOT NULL").Scan(&exists); e != nil {
		t.Fatal(e)
	}
	return exists
}
