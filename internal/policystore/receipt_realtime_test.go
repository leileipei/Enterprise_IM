package policystore_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"github.com/leileipei/Enterprise_IM/internal/receiptoutbox"
	"github.com/redis/go-redis/v9"
	"os"
	"testing"
	"time"
)

func receiptRedis(t *testing.T) *redis.Client {
	t.Helper()
	raw := os.Getenv("IM_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("set IM_TEST_REDIS_URL for real Redis receipt tests")
	}
	o, e := redis.ParseURL(raw)
	if e != nil {
		t.Fatal("invalid receipt Redis fixture")
	}
	o.ContextTimeoutEnabled = true
	c := redis.NewClient(o)
	if e = c.Ping(context.Background()).Err(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func TestReceiptRealtimeForgedEventRejected(t *testing.T) {
	c, s, d := receiptDirectFixture(t)
	ctx := context.Background()
	receiptDeliver(t, s, d, directA)
	var eid string
	var rev int64
	if e := c.QueryRow(ctx, "SELECT id::text,revision FROM receipt_outbox_events").Scan(&eid, &rev); e != nil {
		t.Fatal(e)
	}
	users, e := s.ResolveReceiptEvent(ctx, tenantA, directA, eid, rev)
	if e != nil || len(users) != 2 || users[0] != adminA || users[1] != personA {
		t.Fatal("durable targets mismatch", users, e)
	}
	for _, args := range []struct {
		tenant, cid, event string
		revision           int64
	}{{tenantB, directA, eid, rev}, {tenantA, "00000000-0000-4000-8000-000000009999", eid, rev}, {tenantA, directA, "00000000-0000-4000-8000-000000009999", rev}, {tenantA, directA, eid, rev + 1}} {
		users, e = s.ResolveReceiptEvent(ctx, args.tenant, args.cid, args.event, args.revision)
		if e != nil || len(users) != 0 {
			t.Fatal("forged notification accepted", users, e)
		}
	}
	// Existing message resolver and receipt resolver retain distinct contracts.
	var mid, oldEID string
	if e = c.QueryRow(ctx, "SELECT message_id::text,id::text FROM outbox_events LIMIT 1").Scan(&mid, &oldEID); e != nil {
		t.Fatal(e)
	}
	users, e = s.ResolveMessageEvent(ctx, tenantA, oldEID, directA, mid, 1)
	if e != nil || len(users) != 2 {
		t.Fatal("legacy notification changed", users, e)
	}
	if _, e = s.ResolveReceiptEvent(ctx, tenantA, directA, eid, 0); !errors.Is(e, policystore.ErrInvalidReceiptRequest) {
		t.Fatal(e)
	}
}

type failedReceiptCommitDB struct{ access.Beginner }

func (d failedReceiptCommitDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, e := d.Beginner.Begin(ctx)
	if e != nil {
		return nil, e
	}
	return failedReceiptCommitTx{tx}, nil
}

type failedReceiptCommitTx struct{ pgx.Tx }

func (tx failedReceiptCommitTx) Commit(context.Context) error {
	return errors.New("injected receipt commit failure")
}
func TestReceiptOutboxReplayAndIsolation(t *testing.T) {
	c, s, d := receiptDirectFixture(t)
	ctx := context.Background()
	receiptDeliver(t, s, d, directA)
	client := receiptRedis(t)
	stream := fmt.Sprintf("im:test:receipt:%d", time.Now().UnixNano())
	t.Cleanup(func() { client.Del(ctx, stream) })
	publisher := receiptoutbox.RedisPublisher{Client: client, Stream: stream}
	w := receiptoutbox.Worker{DB: failedReceiptCommitDB{c}, Publisher: publisher}
	if processed, e := w.ProcessOne(ctx); !processed || e == nil {
		t.Fatal("commit failure not observed", processed, e)
	}
	var status string
	if e := c.QueryRow(ctx, "SELECT status FROM receipt_outbox_events").Scan(&status); e != nil || status != "pending" {
		t.Fatal("committed after failure", status, e)
	}
	w.DB = c
	if processed, e := w.ProcessOne(ctx); !processed || e != nil {
		t.Fatal("replay", processed, e)
	}
	events, e := client.XRange(ctx, stream, "-", "+").Result()
	if e != nil || len(events) != 2 || events[0].Values["event_id"] != events[1].Values["event_id"] || len(events[0].Values) != 5 {
		t.Fatal("replay identity/minimal payload", events, e)
	}
	if processed, e := w.ProcessOne(ctx); processed || e != nil {
		t.Fatal("republished committed fact", processed, e)
	}
	var oldPending int
	if e = c.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE status='pending'").Scan(&oldPending); e != nil || oldPending != 1 {
		t.Fatal("receipt worker claimed old message queue", e)
	}
}
func TestExpiredProofCleanupPreservesReceipts(t *testing.T) {
	c, s, d := receiptDirectFixture(t)
	ctx := context.Background()
	receiptDeliver(t, s, d, directA)
	if _, e := s.ConfirmRead(ctx, groupMemberIdentity(), d, directA, []int64{1}); e != nil {
		t.Fatal(e)
	}
	if n, e := s.CleanupExpiredSyncProofs(ctx, 1000); e != nil || n != 0 {
		t.Fatal("live proof removed", n, e)
	}
	s.Now = func() time.Time { return at.Add(300 * time.Second) }
	if n, e := s.CleanupExpiredSyncProofs(ctx, 1); e != nil || n != 1 {
		t.Fatal("expired proof not removed", n, e)
	}
	for _, table := range []string{"device_sync_proofs", "device_sync_proof_items"} {
		var n int
		if e := c.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); e != nil || n != 0 {
			t.Fatal("expired proof residue", table, n, e)
		}
	}
	for _, table := range []string{"user_devices", "message_device_receipts", "message_user_receipts"} {
		var n int
		if e := c.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); e != nil || n != 1 {
			t.Fatal("valid evidence removed", table, n, e)
		}
	}
	for _, limit := range []int{0, 1001} {
		if _, e := s.CleanupExpiredSyncProofs(ctx, limit); !errors.Is(e, policystore.ErrInvalidReceiptRequest) {
			t.Fatal(e)
		}
	}
}

type receiptPublishFunc func(context.Context, receiptoutbox.Event) error

func (f receiptPublishFunc) Publish(ctx context.Context, e receiptoutbox.Event) error {
	return f(ctx, e)
}
func TestReceiptOutboxSkipLockedAndPublishDeadline(t *testing.T) {
	c, s, d := receiptDirectFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receiptDeliver(t, s, d, directA)
	peer := filePeer(t, c)
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	done := make(chan error, 1)
	first := receiptoutbox.Worker{DB: c, Publisher: receiptPublishFunc(func(ctx context.Context, _ receiptoutbox.Event) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			return errors.New("unbounded publish context")
		}
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})}
	go func() { _, e := first.ProcessOne(ctx); done <- e }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker never claimed event")
	}
	start := time.Now()
	processed, e := (receiptoutbox.Worker{DB: peer, Publisher: receiptPublishFunc(func(context.Context, receiptoutbox.Event) error { return errors.New("duplicate claim") })}).ProcessOne(ctx)
	if processed || e != nil || time.Since(start) > time.Second {
		t.Fatal("locked event was blocked or claimed", processed, e)
	}
	// Release without closing the channel twice; the defer handles any test failure.
	release <- struct{}{}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}
func TestReceiptOutboxPublishFailureSchedulesDurableRetry(t *testing.T) {
	c, s, d := receiptDirectFixture(t)
	ctx := context.Background()
	receiptDeliver(t, s, d, directA)
	now := time.Now().UTC().Add(time.Second)
	worker := receiptoutbox.Worker{DB: c, Now: func() time.Time { return now }, Publisher: receiptPublishFunc(func(context.Context, receiptoutbox.Event) error { return errors.New("injected publisher failure") })}
	if processed, e := worker.ProcessOne(ctx); !processed || e == nil {
		t.Fatal(processed, e)
	}
	var attempts int
	var next time.Time
	if e := c.QueryRow(ctx, "SELECT attempt_count,next_retry_at FROM receipt_outbox_events").Scan(&attempts, &next); e != nil || attempts != 1 || next.Sub(now) < 999*time.Millisecond || next.Sub(now) > 1001*time.Millisecond {
		t.Fatal("retry state missing", attempts, next, e)
	}
	if processed, e := worker.ProcessOne(ctx); processed || e != nil {
		t.Fatal("retried before durable deadline", processed, e)
	}
	now = now.Add(time.Second)
	worker.Publisher = receiptPublishFunc(func(context.Context, receiptoutbox.Event) error { return nil })
	if processed, e := worker.ProcessOne(ctx); !processed || e != nil {
		t.Fatal("scheduled recovery failed", processed, e)
	}
}
