package retention

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

func holdService(pool *pgxpool.Pool) access.Service {
	return access.Service{DB: pool, Now: func() time.Time { return fixedTime }}
}
func placeHold(t *testing.T, pool *pgxpool.Pool, conversation string, n int) access.LegalHold {
	t.Helper()
	hold, _, err := holdService(pool).PlaceLegalHold(context.Background(), identity(), conversation, fmt.Sprintf("00000000-0000-4000-8000-%012x", 2000+n), fmt.Sprintf("CASE-%d", n))
	if err != nil {
		t.Fatal(err)
	}
	return hold
}
func releaseHold(t *testing.T, pool *pgxpool.Pool, conversation string, hold access.LegalHold, n int) {
	t.Helper()
	if _, err := holdService(pool).ReleaseLegalHold(context.Background(), identity(), conversation, hold.ID, fmt.Sprintf("00000000-0000-4000-8000-%012x", 3000+n), "APPROVED"); err != nil {
		t.Fatal(err)
	}
}

func waitForLock(t *testing.T, pool *pgxpool.Pool, pid uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		var wait *string
		if err := pool.QueryRow(ctx, "SELECT wait_event_type FROM pg_stat_activity WHERE pid=$1", pid).Scan(&wait); err != nil {
			t.Fatal(err)
		}
		if wait != nil && *wait == "Lock" {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("expected database lock wait", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

type wrappedDB struct {
	access.Beginner
	wrap func(pgx.Tx) pgx.Tx
}

func (db wrappedDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := db.Beginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return db.wrap(tx), nil
}

type failedRow struct{ err error }

func (r failedRow) Scan(...any) error { return r.err }

type hookedTx struct {
	pgx.Tx
	before     func(context.Context, string) error
	after      func(string) error
	rolledBack func()
}

func (tx hookedTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx.before != nil {
		if err := tx.before(ctx, sql); err != nil {
			return failedRow{err}
		}
	}
	return hookedRow{Row: tx.Tx.QueryRow(ctx, sql, args...), sql: sql, after: tx.after}
}
func (tx hookedTx) Rollback(ctx context.Context) error {
	err := tx.Tx.Rollback(ctx)
	if tx.rolledBack != nil {
		tx.rolledBack()
	}
	return err
}

type hookedRow struct {
	pgx.Row
	sql   string
	after func(string) error
}

func (row hookedRow) Scan(dest ...any) error {
	if err := row.Row.Scan(dest...); err != nil {
		return err
	}
	if row.after != nil {
		return row.after(row.sql)
	}
	return nil
}

func pausedDB(pool *pgxpool.Pool, sqlPart string, reached chan<- struct{}, proceed <-chan struct{}) access.Beginner {
	return wrappedDB{Beginner: pool, wrap: func(tx pgx.Tx) pgx.Tx {
		return hookedTx{Tx: tx, before: func(ctx context.Context, sql string) error {
			if strings.Contains(sql, sqlPart) {
				select {
				case reached <- struct{}{}:
				case <-ctx.Done():
					return ctx.Err()
				}
				select {
				case <-proceed:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}}
	}}
}

func TestProcessTenantLegalHoldsAndLockedCandidates(t *testing.T) {
	pool := database(t)
	seedMessage(t, pool, conversationA, 1, fixedTime.Add(-366*24*time.Hour))
	seedMessage(t, pool, conversationA2, 1, fixedTime.Add(-366*24*time.Hour))
	a, b := placeHold(t, pool, conversationA, 1), placeHold(t, pool, conversationA, 2)
	w := testWorker(pool, 0)
	batch, err := w.ProcessTenant(context.Background(), tenantA)
	if err != nil || batch.ConversationID != conversationA2 || batch.ClearedCount != 1 {
		t.Fatalf("held conversation blocked next: %+v %v", batch, err)
	}
	releaseHold(t, pool, conversationA, a, 1)
	if batch, err := w.ProcessTenant(context.Background(), tenantA); err != nil || batch.ClearedCount != 0 {
		t.Fatalf("one remaining hold ignored: %+v %v", batch, err)
	}
	releaseHold(t, pool, conversationA, b, 2)
	if batch, err := w.ProcessTenant(context.Background(), tenantA); err != nil || batch.ClearedCount != 1 {
		t.Fatalf("released holds block forever: %+v %v", batch, err)
	}
	assertCounts(t, pool, 2, 2)
	t.Run("skip locked", func(t *testing.T) {
		pool := database(t)
		seedMessage(t, pool, conversationA, 1, fixedTime.Add(-366*24*time.Hour))
		seedMessage(t, pool, conversationA2, 1, fixedTime.Add(-366*24*time.Hour))
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(context.Background(), "SELECT id FROM conversations WHERE id=$1 FOR UPDATE", conversationA); err != nil {
			t.Fatal(err)
		}
		batch, err := testWorker(pool, 0).ProcessTenant(context.Background(), tenantA)
		if err != nil || batch.ConversationID != conversationA2 {
			t.Fatalf("did not skip locked: %+v %v", batch, err)
		}
		if err := tx.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		if batch, err := testWorker(pool, 0).ProcessTenant(context.Background(), tenantA); err != nil || batch.ConversationID != conversationA {
			t.Fatalf("skipped forever: %+v %v", batch, err)
		}
	})
}

func TestProcessTenantRollbackAndCancellation(t *testing.T) {
	t.Run("evidence failure", func(t *testing.T) {
		pool := database(t)
		seedMessage(t, pool, conversationA, 1, fixedTime.Add(-366*24*time.Hour))
		exec(t, pool, `CREATE FUNCTION fail_batch() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'failure'; END $$;
 CREATE TRIGGER fail_batch BEFORE INSERT ON message_body_clear_batches FOR EACH ROW EXECUTE FUNCTION fail_batch()`)
		if result, err := testWorker(pool, 0).ProcessTenant(context.Background(), tenantA); err == nil || result.ClearedCount != 0 {
			t.Fatalf("partial result: %+v %v", result, err)
		}
		assertCounts(t, pool, 0, 0)
	})
	t.Run("tenant lock cancellation", func(t *testing.T) {
		pool := database(t)
		seedMessage(t, pool, conversationA, 1, fixedTime.Add(-366*24*time.Hour))
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(context.Background(), "SELECT id FROM tenants WHERE id=$1 FOR UPDATE", tenantA); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pid := make(chan uint32, 1)
		w := testWorker(pool, 0)
		w.DB = wrappedDB{Beginner: pool, wrap: func(tx pgx.Tx) pgx.Tx { pid <- tx.Conn().PgConn().PID(); return tx }}
		done := make(chan error, 1)
		go func() { _, err := w.ProcessTenant(ctx, tenantA); done <- err }()
		waitForLock(t, pool, <-pid)
		cancel()
		if err := <-done; err == nil {
			t.Fatal("cancelled transaction succeeded")
		}
		tx.Rollback(context.Background())
		assertCounts(t, pool, 0, 0)
	})
	t.Run("after update cancellation", func(t *testing.T) {
		pool := database(t)
		seedMessage(t, pool, conversationA, 1, fixedTime.Add(-366*24*time.Hour))
		reached, proceed := make(chan struct{}, 1), make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		w := testWorker(pool, 0)
		w.DB = pausedDB(pool, "INSERT INTO message_body_clear_batches", reached, proceed)
		done := make(chan error, 1)
		go func() { _, err := w.ProcessTenant(ctx, tenantA); done <- err }()
		select {
		case <-reached:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		cancel()
		if err := <-done; err == nil {
			t.Fatal("cancelled update committed")
		}
		assertCounts(t, pool, 0, 0)
	})
}

func TestProcessTenantConcurrentWorkersAndHoldPlacement(t *testing.T) {
	t.Run("workers", func(t *testing.T) {
		pool := database(t)
		for i := int64(1); i <= 5; i++ {
			seedMessage(t, pool, conversationA, i, fixedTime.Add(-366*24*time.Hour))
		}
		type outcome struct {
			count int
			err   error
		}
		done := make(chan outcome, 2)
		for i := 0; i < 2; i++ {
			go func() {
				w := testWorker(pool, 2)
				count := 0
				for {
					r, err := w.ProcessTenant(context.Background(), tenantA)
					if err != nil || r.ClearedCount == 0 {
						done <- outcome{count, err}
						return
					}
					count += r.ClearedCount
				}
			}()
		}
		total := 0
		for i := 0; i < 2; i++ {
			r := <-done
			if r.err != nil {
				t.Fatal(r.err)
			}
			total += r.count
		}
		if total != 5 {
			t.Fatalf("worker count %d", total)
		}
		assertCounts(t, pool, 5, 3)
	})
	t.Run("hold locks first", func(t *testing.T) {
		pool := database(t)
		seedMessage(t, pool, conversationA, 1, fixedTime.Add(-366*24*time.Hour))
		reached, proceed := make(chan struct{}, 1), make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		svc := holdService(pool)
		svc.DB = pausedDB(pool, "INSERT INTO conversation_legal_holds", reached, proceed)
		done := make(chan error, 1)
		go func() {
			_, _, err := svc.PlaceLegalHold(ctx, identity(), conversationA, "00000000-0000-4000-8000-000000009001", "FIRST")
			done <- err
		}()
		select {
		case <-reached:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if batch, err := testWorker(pool, 0).ProcessTenant(ctx, tenantA); err != nil || batch.ClearedCount != 0 {
			t.Fatalf("crossed hold lock: %+v %v", batch, err)
		}
		close(proceed)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if batch, err := testWorker(pool, 0).ProcessTenant(ctx, tenantA); err != nil || batch.ClearedCount != 0 {
			t.Fatalf("crossed committed hold: %+v %v", batch, err)
		}
		assertCounts(t, pool, 0, 0)
	})
	t.Run("cleaner locks first", func(t *testing.T) {
		pool := database(t)
		seedMessage(t, pool, conversationA, 1, fixedTime.Add(-366*24*time.Hour))
		reached, proceed := make(chan struct{}, 1), make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		w := testWorker(pool, 0)
		w.DB = pausedDB(pool, "INSERT INTO message_body_clear_batches", reached, proceed)
		clearDone := make(chan error, 1)
		go func() { _, err := w.ProcessTenant(ctx, tenantA); clearDone <- err }()
		select {
		case <-reached:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		pid := make(chan uint32, 1)
		svc := holdService(pool)
		svc.DB = wrappedDB{Beginner: pool, wrap: func(tx pgx.Tx) pgx.Tx { pid <- tx.Conn().PgConn().PID(); return tx }}
		holdDone := make(chan error, 1)
		go func() {
			_, _, err := svc.PlaceLegalHold(ctx, identity(), conversationA, "00000000-0000-4000-8000-000000009002", "AFTER")
			holdDone <- err
		}()
		waitForLock(t, pool, <-pid)
		close(proceed)
		if err := <-clearDone; err != nil {
			t.Fatal(err)
		}
		if err := <-holdDone; err != nil {
			t.Fatal(err)
		}
		assertCounts(t, pool, 1, 1)
		if batch, err := testWorker(pool, 0).ProcessTenant(ctx, tenantA); err != nil || batch.ClearedCount != 0 {
			t.Fatalf("cleared body restored: %+v %v", batch, err)
		}
	})
}

func TestProcessTenantRetentionLockAndDeadlockRetry(t *testing.T) {
	t.Run("updated period read after wait", func(t *testing.T) {
		pool := database(t)
		seedMessage(t, pool, conversationA, 1, fixedTime.Add(-2*24*time.Hour))
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(context.Background(), `UPDATE tenants SET message_body_retention_days=1,retention_version=1,
 retention_approval_reference='APPROVED',retention_approved_by_user_id=$2,retention_approved_at=$3 WHERE id=$1`, tenantA, userA, fixedTime); err != nil {
			t.Fatal(err)
		}
		pid := make(chan uint32, 1)
		w := testWorker(pool, 0)
		w.DB = wrappedDB{Beginner: pool, wrap: func(tx pgx.Tx) pgx.Tx { pid <- tx.Conn().PgConn().PID(); return tx }}
		done := make(chan error, 1)
		go func() {
			batch, err := w.ProcessTenant(context.Background(), tenantA)
			if err == nil && (batch.ClearedCount != 1 || batch.RetentionDays != 1) {
				err = fmt.Errorf("stale period: %+v", batch)
			}
			done <- err
		}()
		waitForLock(t, pool, <-pid)
		if err := tx.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		assertCounts(t, pool, 1, 1)
	})
	t.Run("clock after both locks and policy waits", func(t *testing.T) {
		pool := database(t)
		seedMessage(t, pool, conversationA, 1, fixedTime.Add(-366*24*time.Hour))
		reached, proceed := make(chan struct{}, 1), make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		w := testWorker(pool, 0)
		clockCalled := false
		w.clock = func(ctx context.Context, tx pgx.Tx) (time.Time, error) {
			clockCalled = true
			for _, query := range []string{"SELECT id FROM tenants WHERE id=$1 FOR UPDATE NOWAIT", "SELECT id FROM conversations WHERE id=$1 FOR UPDATE NOWAIT"} {
				other, err := pool.Begin(ctx)
				if err != nil {
					return time.Time{}, err
				}
				id := tenantA
				if strings.Contains(query, "conversations") {
					id = conversationA
				}
				_, err = other.Exec(ctx, query, id)
				other.Rollback(context.Background())
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
					return time.Time{}, fmt.Errorf("clock before lock: %v", err)
				}
			}
			return fixedTime, nil
		}
		w.DB = pausedDB(pool, "INSERT INTO message_body_clear_batches", reached, proceed)
		done := make(chan error, 1)
		go func() { _, err := w.ProcessTenant(ctx, tenantA); done <- err }()
		select {
		case <-reached:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		updateDone := make(chan error, 1)
		go func() {
			_, err := conn.Exec(ctx, `UPDATE tenants SET message_body_retention_days=1,retention_version=1,
 retention_approval_reference='APPROVED',retention_approved_by_user_id=$2,retention_approved_at=$3 WHERE id=$1`, tenantA, userA, fixedTime)
			updateDone <- err
		}()
		waitForLock(t, pool, conn.Conn().PgConn().PID())
		close(proceed)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if err := <-updateDone; err != nil {
			t.Fatal(err)
		}
		if !clockCalled {
			t.Fatal("clock not called")
		}
		assertCounts(t, pool, 1, 1)
	})
	for _, scenario := range []string{"hold on retry", "limit", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			pool := database(t)
			seedMessage(t, pool, conversationA, 1, fixedTime.Add(-366*24*time.Hour))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var attempts atomic.Int32
			w := testWorker(pool, 0)
			var placementErr error
			w.DB = wrappedDB{Beginner: pool, wrap: func(tx pgx.Tx) pgx.Tx {
				attempt := attempts.Add(1)
				return hookedTx{Tx: tx, after: func(sql string) error {
					if strings.Contains(sql, "UPDATE messages") && (attempt == 1 || scenario == "limit") {
						return &pgconn.PgError{Code: "40P01", Message: "injected deadlock"}
					}
					return nil
				}, rolledBack: func() {
					if attempt == 1 && scenario == "hold on retry" {
						_, _, placementErr = holdService(pool).PlaceLegalHold(context.Background(), identity(), conversationA, "00000000-0000-4000-8000-000000009003", "RETRY")
					}
					if scenario == "cancel" {
						cancel()
					}
				}}
			}}
			result, err := w.ProcessTenant(ctx, tenantA)
			if placementErr != nil {
				t.Fatal(placementErr)
			}
			if result.ClearedCount != 0 {
				t.Fatalf("partial retry result: %+v", result)
			}
			switch scenario {
			case "hold on retry":
				if err != nil || attempts.Load() != 2 {
					t.Fatalf("retry did not recheck hold: %d %v", attempts.Load(), err)
				}
			case "limit":
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "40P01" || attempts.Load() != 3 {
					t.Fatalf("retry limit: %d %v", attempts.Load(), err)
				}
			case "cancel":
				if err == nil || attempts.Load() != 1 {
					t.Fatalf("retry after cancellation: %d %v", attempts.Load(), err)
				}
			}
			assertCounts(t, pool, 0, 0)
		})
	}
}
