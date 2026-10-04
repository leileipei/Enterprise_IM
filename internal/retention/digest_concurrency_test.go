package retention

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDigestProcessTenantHoldsAndConcurrency(t *testing.T) {
	t.Run("multiple holds and skip locked", func(t *testing.T) {
		pool := database(t)
		seedDigestCandidate(t, pool, conversationA, 1, fixedTime, true)
		seedDigestCandidate(t, pool, conversationA2, 1, fixedTime, true)
		h1 := placeHold(t, pool, conversationA, 11)
		h2 := placeHold(t, pool, conversationA, 12)
		w := testDigestWorker(pool, 0)
		b, err := w.ProcessTenant(context.Background(), tenantA)
		if err != nil || b.ConversationID != conversationA2 || b.RetiredCount != 1 {
			t.Fatalf("held first: %+v %v", b, err)
		}
		releaseHold(t, pool, conversationA, h1, 11)
		b, err = w.ProcessTenant(context.Background(), tenantA)
		if err != nil || b.RetiredCount != 0 {
			t.Fatalf("one hold remains: %+v %v", b, err)
		}
		releaseHold(t, pool, conversationA, h2, 12)
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err = tx.Exec(context.Background(), "SELECT id FROM conversations WHERE id=$1 FOR UPDATE", conversationA); err != nil {
			t.Fatal(err)
		}
		b, err = w.ProcessTenant(context.Background(), tenantA)
		if err != nil || b.RetiredCount != 0 {
			t.Fatalf("locked: %+v %v", b, err)
		}
		tx.Rollback(context.Background())
		b, err = w.ProcessTenant(context.Background(), tenantA)
		if err != nil || b.RetiredCount != 1 {
			t.Fatalf("released: %+v %v", b, err)
		}
		assertDigestCounts(t, pool, 2, 2)
	})
	t.Run("two workers", func(t *testing.T) {
		pool := database(t)
		seedDigestCandidate(t, pool, conversationA, 1, fixedTime, true)
		seedDigestCandidate(t, pool, conversationA2, 1, fixedTime, true)
		// Hold the first conversation lock until the second worker proves SKIP LOCKED.
		// Uncoordinated calls need not overlap, so one-shot totals are not a concurrency guarantee.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		reached, proceed := make(chan struct{}, 1), make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(proceed) }) }
		defer release()
		first := testDigestWorker(pool, 0)
		first.DB = pausedDB(pool, "INSERT INTO message_digest_retirement_batches", reached, proceed)
		type outcome struct {
			batch DigestBatchResult
			err   error
		}
		done := make(chan outcome, 1)
		go func() { batch, err := first.ProcessTenant(ctx, tenantA); done <- outcome{batch, err} }()
		select {
		case <-reached:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		second, err := testDigestWorker(pool, 0).ProcessTenant(ctx, tenantA)
		if err != nil || second.ConversationID != conversationA2 || second.RetiredCount != 1 {
			t.Fatalf("second worker did not skip locked conversation: %+v %v", second, err)
		}
		release()
		var result outcome
		select {
		case result = <-done:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if result.err != nil || result.batch.ConversationID != conversationA || result.batch.RetiredCount != 1 {
			t.Fatalf("first worker: %+v %v", result.batch, result.err)
		}
		assertDigestCounts(t, pool, 2, 2)
	})
	t.Run("repeatable read hold race", func(t *testing.T) {
		pool := database(t)
		seedDigestCandidate(t, pool, conversationA, 1, fixedTime, true)
		cfg := pool.Config()
		cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
		other, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		placed := false
		w := testDigestWorker(pool, 0)
		w.DB = wrappedDB{Beginner: other, wrap: func(tx pgx.Tx) pgx.Tx {
			return hookedTx{Tx: tx, before: func(ctx context.Context, sql string) error {
				if strings.Contains(sql, "SELECT clock_timestamp()") && !placed {
					_, _, err := holdService(pool).PlaceLegalHold(ctx, identity(), conversationA, "00000000-0000-4000-8000-000000008011", "RACE")
					placed = err == nil
					return err
				}
				return nil
			}}
		}}
		b, err := w.ProcessTenant(context.Background(), tenantA)
		if err != nil || b.RetiredCount != 0 || !placed {
			t.Fatalf("snapshot: %+v %v placed=%v", b, err, placed)
		}
		assertDigestCounts(t, pool, 0, 0)
	})
	t.Run("cleaner before hold", func(t *testing.T) {
		pool := database(t)
		seedDigestCandidate(t, pool, conversationA, 1, fixedTime, true)
		reached, proceed := make(chan struct{}, 1), make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		w := testDigestWorker(pool, 0)
		w.DB = pausedDB(pool, "INSERT INTO message_digest_retirement_batches", reached, proceed)
		done := make(chan error, 1)
		go func() { _, e := w.ProcessTenant(ctx, tenantA); done <- e }()
		select {
		case <-reached:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		holdConn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holdConn.Release()
		holdDone := make(chan error, 1)
		svc := holdService(pool)
		svc.DB = holdConn.Conn()
		go func() {
			_, _, e := svc.PlaceLegalHold(ctx, identity(), conversationA, "00000000-0000-4000-8000-000000008012", "AFTER")
			holdDone <- e
		}()
		waitForLock(t, pool, holdConn.Conn().PgConn().PID())
		close(proceed)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if err := <-holdDone; err != nil {
			t.Fatal(err)
		}
		assertDigestCounts(t, pool, 1, 1)
	})

	t.Run("body cleaner serialization", func(t *testing.T) {
		pool := database(t)
		seedDigestCandidate(t, pool, conversationA, 1, fixedTime.Add(-366*24*time.Hour), false)
		reached, proceed := make(chan struct{}, 1), make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		body := testWorker(pool, 1)
		body.DB = pausedDB(pool, "INSERT INTO message_body_clear_batches", reached, proceed)
		done := make(chan error, 1)
		go func() { _, err := body.ProcessTenant(ctx, tenantA); done <- err }()
		select {
		case <-reached:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		b, err := testDigestWorker(pool, 0).ProcessTenant(ctx, tenantA)
		if err != nil || b.RetiredCount != 0 {
			t.Fatalf("uncommitted body: %+v %v", b, err)
		}
		close(proceed)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		b, err = testDigestWorker(pool, 0).ProcessTenant(ctx, tenantA)
		if err != nil || b.RetiredCount != 1 {
			t.Fatalf("committed body: %+v %v", b, err)
		}
	})

}

type digestFaultTx struct {
	pgx.Tx
	fault string
}

func (tx digestFaultTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := tx.Tx.Query(ctx, sql, args...)
	if err == nil && strings.Contains(sql, "UPDATE message_idempotency") {
		return digestFaultRows{Rows: rows, fault: tx.fault}, nil
	}
	return rows, err
}

type digestFaultRows struct {
	pgx.Rows
	fault string
}

func (r digestFaultRows) Next() bool {
	if r.fault == "missing result" {
		r.Rows.Close()
		return false
	}
	return r.Rows.Next()
}
func (r digestFaultRows) Scan(dest ...any) error {
	if err := r.Rows.Scan(dest...); err != nil {
		return err
	}
	if r.fault == "wrong ids" {
		*dest[0].(*string) = "00000000-0000-4000-8000-000000009999"
	}
	return nil
}

func TestDigestProcessTenantWaitsAndRollback(t *testing.T) {
	t.Run("expiry extended after row wait", func(t *testing.T) {
		pool := database(t)
		seedDigestCandidate(t, pool, conversationA, 1, fixedTime, true)
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err = tx.Exec(context.Background(), "UPDATE message_idempotency SET expires_at=$1", fixedTime.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		pid := make(chan uint32, 1)
		w := testDigestWorker(pool, 0)
		w.DB = wrappedDB{Beginner: pool, wrap: func(tx pgx.Tx) pgx.Tx { pid <- tx.Conn().PgConn().PID(); return tx }}
		done := make(chan error, 1)
		go func() {
			b, e := w.ProcessTenant(context.Background(), tenantA)
			if e == nil && b.RetiredCount != 0 {
				e = fmt.Errorf("stale expiry: %+v", b)
			}
			done <- e
		}()
		waitForLock(t, pool, <-pid)
		if err := tx.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		assertDigestCounts(t, pool, 0, 0)
	})
	for _, fault := range []string{"missing result", "wrong ids", "evidence failure", "idempotency update failure"} {
		t.Run(fault, func(t *testing.T) {
			pool := database(t)
			seedDigestCandidate(t, pool, conversationA, 1, fixedTime, true)
			w := testDigestWorker(pool, 0)
			if fault == "missing result" || fault == "wrong ids" {
				w.DB = wrappedDB{Beginner: pool, wrap: func(tx pgx.Tx) pgx.Tx { return digestFaultTx{Tx: tx, fault: fault} }}
			} else {
				table := "message_digest_retirement_batches"
				event := "INSERT"
				if fault == "idempotency update failure" {
					table = "message_idempotency"
					event = "UPDATE"
				}
				exec(t, pool, "CREATE FUNCTION reject_digest_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test failure'; END $$")
				exec(t, pool, "CREATE TRIGGER reject_digest_test BEFORE "+event+" ON "+table+" FOR EACH ROW EXECUTE FUNCTION reject_digest_test()")
			}
			if _, err := w.ProcessTenant(context.Background(), tenantA); err == nil {
				t.Fatal("fault committed")
			}
			assertDigestCounts(t, pool, 0, 0)
		})
	}
	for _, phase := range []string{"message lock", "evidence"} {
		t.Run("cancel "+phase, func(t *testing.T) {
			pool := database(t)
			seedDigestCandidate(t, pool, conversationA, 1, fixedTime, true)
			w := testDigestWorker(pool, 0)
			reached, proceed := make(chan struct{}, 1), make(chan struct{})
			part := "FROM messages WHERE"
			if phase == "evidence" {
				part = "INSERT INTO message_digest_retirement_batches"
			}
			w.DB = pausedDB(pool, part, reached, proceed)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, e := w.ProcessTenant(ctx, tenantA); done <- e }()
			select {
			case <-reached:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel: %v", err)
			}
			assertDigestCounts(t, pool, 0, 0)
		})
	}
	for _, scenario := range []string{"hold on retry", "limit", "cancel"} {
		t.Run("deadlock "+scenario, func(t *testing.T) {
			pool := database(t)
			seedDigestCandidate(t, pool, conversationA, 1, fixedTime, true)
			var attempts atomic.Int32
			w := testDigestWorker(pool, 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w.DB = wrappedDB{Beginner: pool, wrap: func(tx pgx.Tx) pgx.Tx {
				n := attempts.Add(1)
				return hookedTx{Tx: tx, after: func(sql string) error {
					if strings.Contains(sql, "INSERT INTO message_digest_retirement_batches") {
						return &pgconn.PgError{Code: "40P01"}
					}
					return nil
				}, rolledBack: func() {
					if n == 1 && scenario == "hold on retry" {
						placeHold(t, pool, conversationA, 30)
					}
					if scenario == "cancel" {
						cancel()
					}
				}}
			}}
			b, err := w.ProcessTenant(ctx, tenantA)
			if scenario == "hold on retry" {
				if err != nil || b.RetiredCount != 0 || attempts.Load() != 2 {
					t.Fatalf("retry: %+v %v attempts %d", b, err, attempts.Load())
				}
			} else if err == nil || (scenario == "limit" && attempts.Load() != 3) || (scenario == "cancel" && attempts.Load() != 1) {
				t.Fatalf("retry limit: %v %d", err, attempts.Load())
			}
			assertDigestCounts(t, pool, 0, 0)
		})
	}
}

func TestFileMessageDigestRetirementHoldConcurrency(t *testing.T) {
	t.Run("hold committed before conversation lock", func(t *testing.T) {
		pool := database(t)
		mid := seedFileDigestCandidate(t, pool, 1, fixedTime.Add(-time.Hour), true)
		placed := false
		w := testDigestWorker(pool, 0)
		w.DB = wrappedDB{Beginner: pool, wrap: func(tx pgx.Tx) pgx.Tx {
			return hookedTx{Tx: tx, before: func(ctx context.Context, sql string) error {
				if strings.Contains(sql, "SELECT clock_timestamp()") && !placed {
					_, _, err := holdService(pool).PlaceLegalHold(ctx, identity(), conversationA, "00000000-0000-4000-8000-000000008091", "FILE-BEFORE")
					placed = err == nil
					return err
				}
				return nil
			}}
		}}
		b, err := w.ProcessTenant(context.Background(), tenantA)
		if err != nil || b.RetiredCount != 0 || !placed {
			t.Fatal(b, err, placed)
		}
		assertFileDigestProof(t, pool, mid, false)
	})
	t.Run("hold waits for locked retirement", func(t *testing.T) {
		pool := database(t)
		mid := seedFileDigestCandidate(t, pool, 1, fixedTime.Add(-time.Hour), true)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		reached, proceed := make(chan struct{}, 1), make(chan struct{})
		var once sync.Once
		release := func() { once.Do(func() { close(proceed) }) }
		defer release()
		w := testDigestWorker(pool, 0)
		w.DB = pausedDB(pool, "INSERT INTO message_digest_retirement_batches", reached, proceed)
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
		svc := holdService(pool)
		svc.DB = conn.Conn()
		holdDone := make(chan error, 1)
		go func() {
			_, _, err := svc.PlaceLegalHold(ctx, identity(), conversationA, "00000000-0000-4000-8000-000000008092", "FILE-AFTER")
			holdDone <- err
		}()
		waitForLock(t, pool, conn.Conn().PgConn().PID())
		release()
		if err = <-done; err != nil {
			t.Fatal(err)
		}
		if err = <-holdDone; err != nil {
			t.Fatal(err)
		}
		assertFileDigestProof(t, pool, mid, true)
	})
}
