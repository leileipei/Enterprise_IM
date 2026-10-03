package policystore_test

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"os"
	"testing"
	"time"
)

func TestFileFoundationConcurrentScanCAS(t *testing.T) {
	c := fileFoundationDB(t)
	seedDirectConversation(t, c)
	m := storedFile(t, c, "scanning")
	p := filePeer(t, c)
	q := `UPDATE file_objects SET state='ready',state_version=state_version+1,updated_at=$4,scan_engine='engine',scan_definition_version='definitions-1',scanned_at=$4,scan_sha256=sha256 WHERE id=$1 AND state_version=$2 AND scan_job_id=$3`
	run(t, c, "BEGIN")
	tag, e := c.Exec(context.Background(), q, m.ID, m.StateVersion, m.ScanJobID, m.UpdatedAt.Add(time.Second))
	if e != nil || tag.RowsAffected() != 1 {
		t.Fatal(tag, e)
	}
	result := make(chan struct {
		tag pgconn.CommandTag
		err error
	}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go func() {
		tag, err := p.Exec(ctx, `UPDATE file_objects SET state='scan_failed',state_version=state_version+1,updated_at=$4 WHERE id=$1 AND state_version=$2 AND scan_job_id=$3`, m.ID, m.StateVersion, m.ScanJobID, m.UpdatedAt.Add(time.Second))
		result <- struct {
			tag pgconn.CommandTag
			err error
		}{tag, err}
	}()
	waitFileLock(t, c, p, "", func() { run(t, c, "ROLLBACK") })
	run(t, c, "COMMIT")
	got := <-result
	if got.err != nil || got.tag.RowsAffected() != 0 {
		t.Fatal(got.tag, got.err)
	}

	var state string
	var v int64
	if e = c.QueryRow(context.Background(), "SELECT state,state_version FROM file_objects WHERE id=$1", m.ID).Scan(&state, &v); e != nil || state != "ready" || v != m.StateVersion+1 {
		t.Fatal(state, v, e)
	}
}
func TestFileFoundationMigrationDownWaitsForConcurrentEvidence(t *testing.T) {
	c := fileFoundationDB(t)
	seedDirectConversation(t, c)
	p := filePeer(t, c)
	run(t, c, "BEGIN")
	if e := writeFile(c, freshFile(), true); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile("../../db/migrations/000018_file_foundation.down.sql")
	if e != nil {
		t.Fatal(e)
	}
	result := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go func() { _, e := p.PgConn().Exec(ctx, string(b)).ReadAll(); result <- e }()
	waitFileLock(t, c, p, "AccessExclusiveLock", func() { run(t, c, "ROLLBACK") })
	run(t, c, "COMMIT")
	select {
	case e := <-result:
		expectFileSQLState(t, e, "23514")
	case <-time.After(5 * time.Second):
		t.Fatal("Down stuck")
	}
	run(t, p, "ROLLBACK")
	var n int
	if e := c.QueryRow(context.Background(), "SELECT count(*) FROM file_objects").Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
}

func waitFileLock(t *testing.T, c, p *pgx.Conn, mode string, rollback func()) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if e := c.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND NOT granted AND ($2='' OR mode=$2))`, int32(p.PgConn().PID()), mode).Scan(&waiting); e != nil {
			rollback()
			t.Fatal(e)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	rollback()
	t.Fatal("second connection never waited for writer lock")
}
