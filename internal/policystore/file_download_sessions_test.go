package policystore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func downloadDeadline(t *testing.T, c *pgx.Conn, d time.Duration) time.Time {
	t.Helper()
	var at time.Time
	if e := c.QueryRow(context.Background(), "SELECT clock_timestamp()+$1::bigint*interval '1 microsecond'", d.Microseconds()).Scan(&at); e != nil {
		t.Fatal(e)
	}
	return at
}
func beginDownload(t *testing.T, c *pgx.Conn, m files.Metadata, id access.TrustedIdentity) filedownload.Ticket {
	t.Helper()
	v, e := (policystore.Service{DB: c}).BeginFileDownload(context.Background(), id, m.ID, uploadOwner, downloadDeadline(t, c, 45*time.Second))
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func anotherDownloadFile(t *testing.T, c *pgx.Conn, s policystore.Service, seq int) files.Metadata {
	t.Helper()
	m := fileMessageFixture(t, c, directA, adminA, adminM)
	if _, e := s.SendMessage(context.Background(), publisher(), directA, policystore.MessageSendRequest{ClientMessageID: clientUUIDv7(at, 9000+seq), MessageType: "file", FileID: m.ID}); e != nil {
		t.Fatal(e)
	}
	return m
}
func TestFileDownloadSessionLimits(t *testing.T) {
	c, s, _, m := fileHistoryFixture(t, "direct")
	peer := filePeer(t, c)
	ctx := context.Background()
	deadline := downloadDeadline(t, c, 45*time.Second)
	out := make(chan error, 2)
	for _, db := range []*pgx.Conn{c, peer} {
		go func(db *pgx.Conn) {
			_, e := (policystore.Service{DB: db}).BeginFileDownload(ctx, publisher(), m.ID, uploadOwner, deadline)
			out <- e
		}(db)
	}
	wins := 0
	for i := 0; i < 2; i++ {
		e := <-out
		if e == nil {
			wins++
		} else if !errors.Is(e, filedownload.ErrBusy) {
			t.Fatal(e)
		}
	}
	if wins != 1 {
		t.Fatal("multiple starts", wins)
	}
	m2 := anotherDownloadFile(t, c, s, 4)
	_ = beginDownload(t, c, m2, publisher())
	m3 := anotherDownloadFile(t, c, s, 5)
	if _, e := s.BeginFileDownload(ctx, publisher(), m3.ID, uploadOwner, deadline); !errors.Is(e, filedownload.ErrLimit) {
		t.Fatal("third", e)
	}
	if _, e := s.BeginFileDownload(ctx, groupMemberIdentity(), m.ID, uploadOwner, deadline); e != nil {
		t.Fatal("other user", e)
	}
	var n int
	if e := c.QueryRow(ctx, "SELECT count(*) FROM file_download_sessions").Scan(&n); e != nil || n != 3 {
		t.Fatal(n, e)
	}
}
func TestFileDownloadTerminalCAS(t *testing.T) {
	c, s, _, m := fileHistoryFixture(t, "direct")
	ctx := context.Background()
	ticket := beginDownload(t, c, m, publisher())
	if e := s.AuthorizeFileDownload(ctx, publisher(), ticket); e != nil {
		t.Fatal(e)
	}
	if e := s.CheckFileDownload(ctx, publisher(), ticket); e != nil {
		t.Fatal(e)
	}
	completed := filedownload.Result{Outcome: "completed", Reason: "completed", BytesWritten: 1}
	bad := ticket
	bad.OwnerID = freshFile().ID
	if e := s.FinishFileDownload(ctx, bad, completed); e == nil {
		t.Fatal("foreign owner finished")
	}
	bad = ticket
	bad.LeaseToken = freshFile().ID
	if e := s.FinishFileDownload(ctx, bad, completed); e == nil {
		t.Fatal("foreign token finished")
	}
	if e := s.FinishFileDownload(ctx, ticket, filedownload.Result{Outcome: "completed", Reason: "completed", BytesWritten: 0}); e == nil {
		t.Fatal("partial complete")
	}
	if e := s.FinishFileDownload(ctx, ticket, completed); e != nil {
		t.Fatal(e)
	}
	if e := s.FinishFileDownload(ctx, ticket, completed); e != nil {
		t.Fatal("same replay", e)
	}
	if e := s.FinishFileDownload(ctx, ticket, filedownload.Result{Outcome: "unknown", Reason: "process_lost"}); e == nil {
		t.Fatal("rewrote terminal")
	}
	var phase, reason string
	var bytes, facts int64
	if e := c.QueryRow(ctx, `SELECT phase,reason_code,bytes_written,(SELECT count(*) FROM file_download_terminal_events WHERE session_id=$1) FROM file_download_sessions WHERE id=$1`, ticket.SessionID).Scan(&phase, &reason, &bytes, &facts); e != nil || phase != "completed" || reason != "completed" || bytes != 1 || facts != 1 {
		t.Fatal(phase, reason, bytes, facts, e)
	}
	if e := s.CheckFileDownload(ctx, publisher(), ticket); e == nil {
		t.Fatal("terminal streamed")
	}
}
func TestFileDownloadCrashUnknown(t *testing.T) {
	c, s, _, m := fileHistoryFixture(t, "direct")
	ctx := context.Background()
	deadline := downloadDeadline(t, c, 250*time.Millisecond)
	ticket, e := s.BeginFileDownload(ctx, publisher(), m.ID, uploadOwner, deadline)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.AuthorizeFileDownload(ctx, publisher(), ticket); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp())))+0.01)`, deadline); e != nil {
		t.Fatal(e)
	}
	if n, e := s.RepairFileDownloadAudit(ctx, freshFile().ID, 20); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if e = s.FinishFileDownload(ctx, ticket, filedownload.Result{Outcome: "completed", Reason: "completed", BytesWritten: 1}); e == nil {
		t.Fatal("late completed replaced crash")
	}
	var phase string
	var bytes int64
	var ack bool
	if e = c.QueryRow(ctx, "SELECT phase,bytes_written,audit_acked FROM file_download_sessions WHERE id=$1", ticket.SessionID).Scan(&phase, &bytes, &ack); e != nil || phase != "unknown" || bytes != 0 || !ack {
		t.Fatal(phase, bytes, ack, e)
	}
	_ = beginDownload(t, c, m, publisher())
}

type delayedDownloadSessionTx struct {
	pgx.Tx
	expires time.Time
}

func (d delayedDownloadSessionTx) QueryRow(ctx context.Context, q string, args ...any) pgx.Row {
	if strings.Contains(q, "FROM file_download_sessions WHERE id=$1") {
		d.Tx.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp())))+0.01)`, d.expires)
	}
	return d.Tx.QueryRow(ctx, q, args...)
}

type delayedDownloadSessionDB struct {
	access.Beginner
	expires time.Time
}

func (d delayedDownloadSessionDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, e := d.Beginner.Begin(ctx)
	if e != nil {
		return nil, e
	}
	return delayedDownloadSessionTx{Tx: tx, expires: d.expires}, nil
}
func TestFileDownloadSessionCheckLateIdentity(t *testing.T) {
	c, s, _, m := fileHistoryFixture(t, "direct")
	ctx := context.Background()
	ticket := beginDownload(t, c, m, publisher())
	if e := s.AuthorizeFileDownload(ctx, publisher(), ticket); e != nil {
		t.Fatal(e)
	}
	expires := downloadDeadline(t, c, time.Second)
	run(t, c, "UPDATE user_organizations SET effective_to=$2 WHERE id=$1", adminM, expires)
	s.DB = delayedDownloadSessionDB{Beginner: c, expires: expires}
	if e := s.CheckFileDownload(ctx, publisher(), ticket); !errors.Is(e, filedownload.ErrInvalidIdentity) {
		t.Fatal("late stage check identity", e)
	}
}
