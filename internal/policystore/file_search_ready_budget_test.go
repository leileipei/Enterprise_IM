package policystore_test

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"sort"
	"testing"
	"time"
)

// Count actual transaction round trips for the specified normal-ready fixture.
// This is not a bound for arbitrary denied or malformed candidate populations.
type fileSearchBudgetDB struct {
	conn  *pgx.Conn
	calls *int
}

func (d fileSearchBudgetDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, e := d.conn.Begin(ctx)
	if e != nil {
		return nil, e
	}
	return &fileSearchBudgetTx{Tx: tx, calls: d.calls}, nil
}

type fileSearchBudgetTx struct {
	pgx.Tx
	calls *int
}

func (d *fileSearchBudgetTx) QueryRow(ctx context.Context, q string, args ...any) pgx.Row {
	*d.calls++
	return d.Tx.QueryRow(ctx, q, args...)
}
func (d *fileSearchBudgetTx) Query(ctx context.Context, q string, args ...any) (pgx.Rows, error) {
	*d.calls++
	return d.Tx.Query(ctx, q, args...)
}
func (d *fileSearchBudgetTx) Exec(ctx context.Context, q string, args ...any) (pgconn.CommandTag, error) {
	*d.calls++
	return d.Tx.Exec(ctx, q, args...)
}

func TestFileSearchReadyCandidateBudget(t *testing.T)      { readyFileSearchBudget(t, false) }
func TestCrossFileSearchReadyCandidateBudget(t *testing.T) { readyFileSearchBudget(t, true) }

// DB-boundary workload: normal guarded service binding and schema-valid scan hashes.
// No missing links or disabled triggers; actual trusted scanner is covered independently.
func readyFileSearchBudget(t *testing.T, cross bool) {
	c, s, _, _ := directFileFixture(t)
	ctx := context.Background()
	ids := []string{directA}
	kind := "direct"
	if cross {
		kind = "group"
		ids = nil
		for i := 0; i < 21; i++ {
			req := createGroupRequest(targetM2)
			req.ClientRequestID = clientUUIDv7(at, 22000+i)
			g, e := s.CreateGroup(ctx, publisher(), req)
			if e != nil {
				t.Fatal(e)
			}
			ids = append(ids, g.ID)
		}
		sort.Strings(ids)
	}
	n := 0
	appendFile := func(cid, name string) {
		n++
		m := namedSearchFile(t, c, cid, name)
		s.Now = func() time.Time { return at.Add(time.Duration(n) * time.Minute) }
		req := policystore.MessageSendRequest{ClientMessageID: clientUUIDv7(at, 23000+n), MessageType: "file", FileID: m.ID}
		var e error
		if cross {
			_, e = s.SendGroupMessage(ctx, publisher(), cid, req)
		} else {
			_, e = s.SendMessage(ctx, publisher(), cid, req)
		}
		if e != nil {
			t.Fatal("normal binding", n, e)
		}
	}
	if cross {
		for _, cid := range ids[:20] {
			for i := 0; i < 25; i++ {
				appendFile(cid, "ordinary.pdf")
			}
		}
		appendFile(ids[20], "needle.pdf")
	} else {
		for i := 0; i < 500; i++ {
			appendFile(directA, "ordinary.pdf")
		}
		appendFile(directA, "needle.pdf")
	}
	var count int
	if e := c.QueryRow(ctx, `SELECT count(*) FROM message_attachments a JOIN file_objects f ON f.tenant_id=a.tenant_id AND f.id=a.file_id WHERE f.state='ready' AND f.scan_sha256=f.sha256 AND a.sealed_sha256=f.sha256 AND f.scan_engine IS NOT NULL AND f.scan_definition_version IS NOT NULL`).Scan(&count); e != nil || count != 501 {
		t.Fatal("normal ready proofs", count, e)
	}
	call := func(q, cursor string, limit int) policystore.FileSearchPage {
		calls := 0
		s.DB = fileSearchBudgetDB{conn: c, calls: &calls}
		start := time.Now()
		var p policystore.FileSearchPage
		var e error
		if cross {
			p, e = s.SearchAllFileMessages(ctx, publisher(), q, kind, cursor, limit)
		} else {
			p, e = s.SearchFileMessages(ctx, publisher(), directA, kind, q, cursor, limit)
		}
		elapsed := time.Since(start)
		t.Logf("ready-search cross=%t candidates=500 limit=%d elapsed-ms=%d transaction-queries=%d", cross, limit, elapsed.Milliseconds(), calls)
		if calls >= 200 {
			t.Fatal("normal-ready search repeatedly loaded per-file proof beyond required results", calls)
		}
		if e != nil || elapsed >= 5*time.Second {
			t.Fatal("original 5s budget", e, elapsed)
		}
		return p
	}
	first := call("needle", "", 20)
	if len(first.Matches) != 0 || !first.HasMore || first.NextCursor == "" {
		t.Fatal("normal empty bounded page", first)
	}
	next := call("needle", first.NextCursor, 20)
	if len(next.Matches) != 1 || next.HasMore || next.NextCursor != "" {
		t.Fatal("normal next page progress", next)
	}
	one := call("ordinary", "", 1)
	if len(one.Matches) != 1 || !one.HasMore {
		t.Fatal("normal limit1 visibility", one)
	}
	t.Log(fmt.Sprintf("normal-ready-boundary cross=%t normal-bindings=%d proof-hashes-valid=true", cross, count))
}
