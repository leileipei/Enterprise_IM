package policystore_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"github.com/leileipei/Enterprise_IM/internal/retention"
)

type crossGateDB struct {
	conn            *pgx.Conn
	needle          string
	reached, resume chan struct{}
}

func (d crossGateDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := d.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &crossGateTx{Tx: tx, db: d}, nil
}

type crossGateTx struct {
	pgx.Tx
	db   crossGateDB
	once sync.Once
}

func (t *crossGateTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, t.db.needle) {
		t.once.Do(func() {
			close(t.db.reached)
			select {
			case <-t.db.resume:
			case <-ctx.Done():
			}
		})
	}
	return t.Tx.Query(ctx, sql, args...)
}
func crossUpdater(t *testing.T, conn *pgx.Conn) *pgx.Conn {
	t.Helper()
	var searchPath string
	if err := conn.QueryRow(context.Background(), "SHOW search_path").Scan(&searchPath); err != nil {
		t.Fatal(err)
	}
	other, err := pgx.Connect(context.Background(), os.Getenv("IM_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close(context.Background()) })
	run(t, other, "SET search_path TO "+searchPath)
	return other
}
func crossWaitSignal(t *testing.T, reached chan struct{}) {
	t.Helper()
	select {
	case <-reached:
	case <-time.After(3 * time.Second):
		t.Fatal("gate not reached")
	}
}

func TestCrossMessageSearchRechecksIdentityAfterWait(t *testing.T) {
	for _, change := range []string{"account", "membership"} {
		t.Run(change, func(t *testing.T) {
			conn := db(t)
			seedDirectConversation(t, conn)
			other := crossUpdater(t, conn)
			run(t, conn, "SET default_transaction_isolation='repeatable read'")
			reached, resume := make(chan struct{}), make(chan struct{})
			svc := policystore.Service{DB: searchSnapshotGateDB{conn, reached, resume}, Now: func() time.Time { return at }}
			done := make(chan error, 1)
			go func() {
				p, err := svc.SearchAllTextMessages(context.Background(), publisher(), "工单", "all", "", 20)
				if len(p.Messages) != 0 || p.NextCursor != "" {
					err = errors.New("identity returned data")
				}
				done <- err
			}()
			crossWaitSignal(t, reached)
			if change == "account" {
				run(t, other, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
			} else {
				run(t, other, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at, adminM)
			}
			close(resume)
			if err := <-done; !errors.Is(err, policystore.ErrForbidden) {
				t.Fatal(err)
			}
			var n int
			if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='message_search_all' AND outcome='allow'").Scan(&n); err != nil || n != 0 {
				t.Fatalf("allow %d %v", n, err)
			}
		})
	}
}

func TestCrossMessageSearchFirstPolicyPublication(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	grantPublisher(t, conn)
	writer := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if _, err := writer.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 1720), "工单secret"); err != nil {
		t.Fatal(err)
	}
	other := crossUpdater(t, conn)
	observer := crossUpdater(t, conn)
	reached, resume := make(chan struct{}), make(chan struct{})
	svc := policystore.Service{DB: crossGateDB{conn, "SELECT c.id::text FROM conversations c", reached, resume}, Now: func() time.Time { return at }}
	type result struct {
		page policystore.CrossConversationSearchPage
		err  error
	}
	done := make(chan result, 1)
	go func() {
		p, e := svc.SearchAllTextMessages(context.Background(), publisher(), "工单", "all", "", 20)
		done <- result{p, e}
	}()
	crossWaitSignal(t, reached)
	pub := policystore.Service{DB: other, Now: func() time.Time { return at }}
	pubDone := make(chan error, 1)
	pubCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	go func() {
		_, e := pub.Publish(pubCtx, publisher(), 0, []policy.Rule{{ID: "cross-hard", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, EffectiveFrom: at.Add(-time.Hour), Reason: "blocked"}}, "first publication")
		pubDone <- e
	}()
	waited := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		e := observer.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')`, other.PgConn().PID()).Scan(&waiting)
		if e != nil {
			t.Fatal(e)
		}
		if waiting {
			waited = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(resume)
	got := <-done
	err := <-pubDone
	if !waited {
		t.Fatal("publication did not wait behind shared tenant lock")
	}
	if err != nil {
		t.Fatal(err)
	}
	if got.err != nil || len(got.page.Messages) != 1 {
		t.Fatalf("serialized original page %+v %v", got.page, got.err)
	}
	page, e := writer.SearchAllTextMessages(context.Background(), publisher(), "工单", "all", "", 20)
	if e != nil || len(page.Messages) != 0 {
		t.Fatalf("first policy missed on new page %+v %v", page, e)
	}

}

type crossRetryDB struct {
	conn       *pgx.Conn
	failures   int
	code       string
	deadlines  []time.Time
	commitFail bool
}

func (d *crossRetryDB) Begin(ctx context.Context) (pgx.Tx, error) {
	deadline, _ := ctx.Deadline()
	d.deadlines = append(d.deadlines, deadline)
	if d.failures > 0 {
		d.failures--
		return nil, &pgconn.PgError{Code: d.code, Message: "retry"}
	}
	tx, err := d.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if d.commitFail {
		return crossCommitTx{tx}, nil
	}
	return tx, nil
}

type crossCommitTx struct{ pgx.Tx }

func (t crossCommitTx) Commit(context.Context) error {
	return errors.New("commit failed before commit")
}
func TestCrossMessageSearchDeadlineRetryAndCommit(t *testing.T) {
	for _, code := range []string{"40P01", "40001"} {
		t.Run(code, func(t *testing.T) {
			conn := db(t)
			seedDirectConversation(t, conn)
			d := &crossRetryDB{conn: conn, failures: 2, code: code}
			svc := policystore.Service{DB: d, Now: func() time.Time { return at }}
			p, err := svc.SearchAllTextMessages(context.Background(), publisher(), "工单", "all", "", 20)
			if err != nil || p.HasMore || len(d.deadlines) != 3 {
				t.Fatalf("retry %+v %v %d", p, err, len(d.deadlines))
			}
			for _, deadline := range d.deadlines {
				if deadline != d.deadlines[0] {
					t.Fatal("retry reset deadline")
				}
			}
			if time.Until(d.deadlines[0]) > 5*time.Second {
				t.Fatal("deadline unbounded")
			}
			d.failures = 3
			d.deadlines = nil
			p, err = svc.SearchAllTextMessages(context.Background(), publisher(), "工单", "all", "", 20)
			if err == nil || p.NextCursor != "" || len(d.deadlines) != 3 {
				t.Fatalf("retry bound %+v %v %d", p, err, len(d.deadlines))
			}
		})
	}
	t.Run("commit", func(t *testing.T) {
		conn := db(t)
		seedDirectConversation(t, conn)
		svc := policystore.Service{DB: &crossRetryDB{conn: conn, commitFail: true}, Now: func() time.Time { return at }}
		p, err := svc.SearchAllTextMessages(context.Background(), publisher(), "工单", "all", "", 20)
		if err == nil || len(p.Messages) != 0 || p.NextCursor != "" {
			t.Fatalf("commit result %+v %v", p, err)
		}
		var n int
		if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='message_search_all'").Scan(&n); err != nil || n != 0 {
			t.Fatalf("commit audit %d %v", n, err)
		}
	})
}

func TestCrossMessageSearchLockTimeoutIsLocal(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	other := crossUpdater(t, conn)
	lock, err := other.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	if _, err := lock.Exec(context.Background(), "SELECT id FROM conversations WHERE id=$1 FOR UPDATE", directA); err != nil {
		t.Fatal(err)
	}
	var beforeStatement, beforeLock string
	if err := conn.QueryRow(context.Background(), "SHOW statement_timeout").Scan(&beforeStatement); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(context.Background(), "SHOW lock_timeout").Scan(&beforeLock); err != nil {
		t.Fatal(err)
	}
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	p, err := svc.SearchAllTextMessages(context.Background(), publisher(), "工单", "all", "", 20)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "55P03" || p.NextCursor != "" || len(p.Messages) != 0 {
		t.Fatalf("timeout %+v %v", p, err)
	}
	var afterStatement, afterLock string
	if err := conn.QueryRow(context.Background(), "SHOW statement_timeout").Scan(&afterStatement); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(context.Background(), "SHOW lock_timeout").Scan(&afterLock); err != nil {
		t.Fatal(err)
	}
	if beforeStatement != afterStatement || beforeLock != afterLock {
		t.Fatal("timeout leaked into connection")
	}
}

// The query waits behind an actual PostgreSQL conversation lock, not a fake response.
func TestCrossMessageSearchFinalExpiry(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	g1, g2 := crossID(9501), crossID(9502)
	for _, g := range []string{g1, g2} {
		seedCrossGroup(t, conn, g)
		insertGroupHistoryMessage(t, conn, g, 1, adminA, adminM, "工单正文")
	}
	run(t, conn, "UPDATE messages SET accepted_at=$1 WHERE conversation_id=$2", at.Add(time.Hour), g2)
	var days int
	if err := conn.QueryRow(context.Background(), "SELECT message_body_retention_days FROM tenants WHERE id=$1", tenantA).Scan(&days); err != nil {
		t.Fatal(err)
	}
	other := crossUpdater(t, conn)
	lock, err := other.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	if _, err := lock.Exec(context.Background(), "SELECT id FROM conversations WHERE id=$1 FOR UPDATE", g2); err != nil {
		t.Fatal(err)
	}
	var advanced atomic.Bool
	svc := policystore.Service{DB: conn, Now: func() time.Time {
		if advanced.Load() {
			return at.Add(time.Duration(days) * 24 * time.Hour)
		}
		return at
	}}
	type result struct {
		page policystore.CrossConversationSearchPage
		err  error
	}
	done := make(chan result, 1)
	go func() {
		p, e := svc.SearchAllTextMessages(context.Background(), publisher(), "工单", "all", "", 20)
		done <- result{p, e}
	}()
	deadline := time.Now().Add(800 * time.Millisecond)
	waited := false
	for time.Now().Before(deadline) {
		var waiting bool
		if err := lock.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')`, conn.PgConn().PID()).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			waited = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waited {
		t.Fatal("search did not wait on real lock")
	}
	advanced.Store(true)
	if err := lock.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || len(got.page.Messages) != 1 || got.page.Messages[0].ConversationID != g2 {
		t.Fatalf("final expiry %+v %v", got.page, got.err)
	}
}

type crossQueryFailureDB struct {
	conn         *pgx.Conn
	conversation string
}

func (d crossQueryFailureDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, e := d.conn.Begin(ctx)
	return crossQueryFailureTx{tx, d.conversation}, e
}

type crossQueryFailureTx struct {
	pgx.Tx
	conversation string
}

func (t crossQueryFailureTx) Query(ctx context.Context, q string, args ...any) (pgx.Rows, error) {
	if strings.Contains(q, "FROM messages") && len(args) > 1 && args[1] == t.conversation {
		return nil, errors.New("later conversation unavailable")
	}
	return t.Tx.Query(ctx, q, args...)
}
func TestCrossMessageSearchLaterConversationFailure(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	g1, g2 := crossID(9951), crossID(9952)
	for _, g := range []string{g1, g2} {
		seedCrossGroup(t, conn, g)
		insertGroupHistoryMessage(t, conn, g, 1, adminA, adminM, "工单正文")
	}
	svc := policystore.Service{DB: crossQueryFailureDB{conn, g2}, Now: func() time.Time { return at }}
	p, e := svc.SearchAllTextMessages(context.Background(), publisher(), "工单", "all", "", 20)
	if e == nil || len(p.Messages) != 0 || p.NextCursor != "" || p.HasMore {
		t.Fatalf("partial %+v %v", p, e)
	}
	var n int
	if e := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='message_search_all'").Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}

func TestCrossMessageSearchConcurrentCleanupAndLeave(t *testing.T) {
	for _, change := range []string{"cleanup", "leave"} {
		t.Run(change, func(t *testing.T) {
			conn := db(t)
			seed(t, conn)
			g := crossID(9971)
			seedCrossGroup(t, conn, g)
			insertGroupHistoryMessage(t, conn, g, 1, adminA, adminM, "工单正文")
			if change == "cleanup" {
				run(t, conn, "UPDATE messages SET accepted_at=$1 WHERE conversation_id=$2", time.Now().Add(-400*24*time.Hour), g)
			}
			other := crossUpdater(t, conn)
			reached, resume := make(chan struct{}), make(chan struct{})
			svc := policystore.Service{DB: crossGateDB{conn, "SELECT c.id::text FROM conversations c", reached, resume}}
			type result struct {
				p policystore.CrossConversationSearchPage
				e error
			}
			done := make(chan result, 1)
			go func() {
				p, e := svc.SearchAllTextMessages(context.Background(), groupMemberIdentity(), "工单", "all", "", 20)
				done <- result{p, e}
			}()
			crossWaitSignal(t, reached)
			if change == "cleanup" {
				r, e := (retention.Worker{DB: other}).ProcessTenant(context.Background(), tenantA)
				if e != nil || r.ClearedCount != 1 {
					t.Fatal(r, e)
				}
			} else {
				var interval string
				if e := other.QueryRow(context.Background(), "SELECT id::text FROM conversation_membership_intervals WHERE conversation_id=$1 AND user_id=$2", g, personA).Scan(&interval); e != nil {
					t.Fatal(e)
				}
				if _, e := (policystore.Service{DB: other}).LeaveGroup(context.Background(), groupMemberIdentity(), g, interval); e != nil {
					t.Fatal(e)
				}
				insertGroupHistoryMessage(t, other, g, 2, adminA, adminM, "工单退群后")
			}
			close(resume)
			r := <-done
			if r.e != nil {
				t.Fatal(r.e)
			}
			want := 0
			if change == "leave" {
				want = 1
			}
			if len(r.p.Messages) != want {
				t.Fatalf("%s %+v", change, r.p)
			}
		})
	}
}
