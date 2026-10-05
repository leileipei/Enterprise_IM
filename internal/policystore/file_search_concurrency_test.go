package policystore_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"strings"
	"testing"
	"time"
)

type fileSearchLateDB struct {
	conn     *pgx.Conn
	boundary time.Time
}

func (d fileSearchLateDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, e := d.conn.Begin(ctx)
	if e != nil {
		return nil, e
	}
	return &fileSearchLateTx{Tx: tx, boundary: d.boundary}, nil
}

type fileSearchLateTx struct {
	pgx.Tx
	boundary time.Time
	waited   bool
}

func (d *fileSearchLateTx) QueryRow(ctx context.Context, q string, args ...any) pgx.Row {
	if !d.waited && strings.Contains(q, "SELECT EXISTS(SELECT 1 FROM conversation_membership_intervals") {
		d.waited = true
		d.Tx.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp())))+0.01)`, d.boundary)
	}
	return d.Tx.QueryRow(ctx, q, args...)
}
func TestCrossFileSearchFinalBoundary(t *testing.T) {
	for _, change := range []string{"future deny", "ttl"} {
		t.Run(change, func(t *testing.T) {
			c, s, cid, m := fileHistoryFixture(t, "group")
			ctx := context.Background()
			grantPublisher(t, c)
			var boundary time.Time
			if e := c.QueryRow(ctx, "SELECT clock_timestamp()+interval '600 milliseconds'").Scan(&boundary); e != nil {
				t.Fatal(e)
			}
			if change == "ttl" {
				setDownloadRetention(t, c, 1)
				run(t, c, "UPDATE messages SET accepted_at=$2::timestamptz-interval '1 day' WHERE conversation_id=$1 AND seq=2", cid, boundary)
			} else {
				r := policy.Rule{ID: "final-search-deny", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, SourceMembershipID: targetM2, TargetMembershipID: groupMemberC, EffectiveFrom: boundary, Reason: "future"}
				if _, e := s.Publish(ctx, publisher(), 0, []policy.Rule{r}, "test"); e != nil {
					t.Fatal(e)
				}
			}
			s.DB = fileSearchLateDB{c, boundary}
			p, e := s.SearchAllFileMessages(ctx, groupMemberIdentity(), m.OriginalFilename, "group", "", 20)
			if e != nil || len(p.Matches) != 0 {
				t.Fatal("late boundary name leak", p, e)
			}
		})
	}
}
func TestFileSearchConcurrencyInvalidIdentityAudit(t *testing.T) {
	c, s, cid, m := fileHistoryFixture(t, "direct")
	run(t, c, "UPDATE user_organizations SET status='suspended' WHERE id=$1", targetM2)
	p, e := s.SearchFileMessages(context.Background(), groupMemberIdentity(), cid, "direct", m.OriginalFilename, "", 20)
	if !errors.Is(e, policystore.ErrForbidden) || len(p.Matches) != 0 {
		t.Fatal(p, e)
	}
	var n int
	if e = c.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='file_name_search' AND outcome='deny' AND reason='invalid_identity'").Scan(&n); e != nil || n != 1 {
		t.Fatal("missing denied identity audit", n, e)
	}
}
func TestFileSearchConcurrencyLockTimeout(t *testing.T) {
	c, s, cid, m := fileHistoryFixture(t, "group")
	ctx := context.Background()
	peer := filePeer(t, c)
	tx, e := peer.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "UPDATE conversations SET group_name='locked' WHERE id=$1", cid); e != nil {
		t.Fatal(e)
	}
	started := time.Now()
	p, e := s.SearchAllFileMessages(ctx, publisher(), m.OriginalFilename, "group", "", 20)
	if !errors.Is(e, policystore.ErrFileSearchUnavailable) || len(p.Matches) != 0 || time.Since(started) > 6*time.Second {
		t.Fatal("partial or unbounded result", p, e, time.Since(started))
	}
}

type fileSearchIsolationDB struct {
	conn     *pgx.Conn
	observed *string
}

func (d fileSearchIsolationDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, e := d.conn.Begin(ctx)
	if e != nil {
		return nil, e
	}
	return fileSearchIsolationTx{Tx: tx, observed: d.observed}, nil
}

type fileSearchIsolationTx struct {
	pgx.Tx
	observed *string
}

func (d fileSearchIsolationTx) Query(ctx context.Context, q string, args ...any) (pgx.Rows, error) {
	if strings.Contains(q, "SELECT m.id FROM user_organizations m JOIN users") {
		if e := d.Tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(d.observed); e != nil {
			return nil, e
		}
	}
	return d.Tx.Query(ctx, q, args...)
}
func TestFileSearchConcurrencyIsolation(t *testing.T) {
	c, s, cid, m := fileHistoryFixture(t, "direct")
	run(t, c, "SET default_transaction_isolation='repeatable read'")
	var observed string
	s.DB = fileSearchIsolationDB{c, &observed}
	p, e := s.SearchFileMessages(context.Background(), publisher(), cid, "direct", m.OriginalFilename, "", 20)
	if e != nil || len(p.Matches) != 1 || observed != "read committed" {
		t.Fatal("inherited stale isolation", observed, p, e)
	}
}

func TestFileSearchConcurrencyFinalClockWait(t *testing.T) {
	c, s, cid, m := fileHistoryFixture(t, "group")
	ctx := context.Background()
	grantPublisher(t, c)
	peer := filePeer(t, c)
	var boundary time.Time
	if e := c.QueryRow(ctx, "SELECT clock_timestamp()+interval '800 milliseconds'").Scan(&boundary); e != nil {
		t.Fatal(e)
	}
	r := policy.Rule{ID: "search-wait-deny", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, SourceMembershipID: targetM2, TargetMembershipID: groupMemberC, EffectiveFrom: boundary, Reason: "wait"}
	if _, e := s.Publish(ctx, publisher(), 0, []policy.Rule{r}, "wait"); e != nil {
		t.Fatal(e)
	}
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT tenant_id FROM tenant_file_retention_policy WHERE tenant_id=$1 FOR UPDATE", tenantA); e != nil {
		t.Fatal(e)
	}
	type result struct {
		page policystore.FileSearchPage
		err  error
	}
	done := make(chan result, 1)
	go func() {
		p, e := (policystore.Service{DB: peer}).SearchAllFileMessages(ctx, groupMemberIdentity(), m.OriginalFilename, "group", "", 20)
		done <- result{p, e}
	}()
	waitFileLock(t, c, peer, "", func() { tx.Rollback(ctx) })
	if _, e = tx.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp())))+0.01)`, boundary); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	got := <-done
	if got.err != nil || len(got.page.Matches) != 0 {
		t.Fatal("wait released stale name", cid, got.page, got.err)
	}
}
