package retention

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestProcessTenantExpiryAndBatchEvidence(t *testing.T) {
	for _, days := range []int{1, 365, 3650} {
		t.Run(fmt.Sprintf("%d days", days), func(t *testing.T) {
			pool := database(t)
			if days != 365 {
				setDays(t, pool, tenantA, days)
			}
			cutoff := fixedTime.Add(-time.Duration(days) * 24 * time.Hour)
			seedMessage(t, pool, conversationA, 1, cutoff.Add(time.Microsecond))
			seedMessage(t, pool, conversationA, 2, cutoff)
			seedMessage(t, pool, conversationA, 3, cutoff.Add(-time.Hour))
			seedMessage(t, pool, conversationA, 4, cutoff.Add(-2*time.Hour))
			seedMessage(t, pool, conversationB, 1, cutoff.Add(-time.Hour))
			w := testWorker(pool, 2)
			batch, err := w.ProcessTenant(context.Background(), tenantA)
			if err != nil || batch.ClearedCount != 2 || batch.FirstSeq != 3 || batch.LastSeq != 4 || batch.RetentionDays != days || batch.BatchID == "" || batch.ConversationID != conversationA || !batch.CutoffAt.Equal(cutoff) || !batch.ClearedAt.Equal(fixedTime) {
				t.Fatalf("batch: %+v %v", batch, err)
			}
			assertCounts(t, pool, 2, 1)
			var count int
			if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM messages WHERE tenant_id=$1 AND text_body IS NULL", tenantB).Scan(&count); err != nil || count != 0 {
				t.Fatalf("cross-tenant clear: %d %v", count, err)
			}
			batch, err = w.ProcessTenant(context.Background(), tenantA)
			if err != nil || batch.ClearedCount != 1 || batch.FirstSeq != 2 {
				t.Fatalf("boundary batch: %+v %v", batch, err)
			}
			batch, err = w.ProcessTenant(context.Background(), tenantA)
			if err != nil || batch.ClearedCount != 0 {
				t.Fatalf("not yet expired/repeated batch: %+v %v", batch, err)
			}
			assertCounts(t, pool, 3, 2)
			var unchanged int64
			if err := pool.QueryRow(context.Background(), "SELECT last_seq FROM conversations WHERE id=$1", conversationA).Scan(&unchanged); err != nil || unchanged != 4 {
				t.Fatalf("changed sequence: %d %v", unchanged, err)
			}
		})
	}
	t.Run("different tenant periods and suspended", func(t *testing.T) {
		pool := database(t)
		setDays(t, pool, tenantA, 1)
		setDays(t, pool, tenantB, 3650)
		seedMessage(t, pool, conversationA, 1, fixedTime.Add(-2*24*time.Hour))
		seedMessage(t, pool, conversationB, 1, fixedTime.Add(-2*24*time.Hour))
		w := testWorker(pool, 0)
		ids, err := w.ListActiveTenants(context.Background())
		if err != nil || len(ids) != 2 || ids[0] != tenantA || ids[1] != tenantB {
			t.Fatalf("tenant list: %v %v", ids, err)
		}
		if batch, err := w.ProcessTenant(context.Background(), tenantB); err != nil || batch.ClearedCount != 0 {
			t.Fatalf("other period: %+v %v", batch, err)
		}
		exec(t, pool, "UPDATE tenants SET status='suspended' WHERE id=$1", tenantA)
		if batch, err := w.ProcessTenant(context.Background(), tenantA); err != nil || batch.ClearedCount != 0 {
			t.Fatalf("suspended tenant: %+v %v", batch, err)
		}
		assertCounts(t, pool, 0, 0)
	})
}

func TestWorkerConfigurationAndDatabaseClock(t *testing.T) {
	if _, err := (Worker{}).ProcessTenant(context.Background(), tenantA); err == nil {
		t.Fatal("unconfigured worker accepted")
	}
	if _, err := (Worker{}).ListActiveTenants(context.Background()); err == nil {
		t.Fatal("unconfigured tenant list accepted")
	}
	pool := database(t)
	for _, size := range []int{-1, 1001} {
		if _, err := (Worker{DB: pool, BatchSize: size}).ProcessTenant(context.Background(), tenantA); err == nil {
			t.Fatalf("invalid size %d", size)
		}
	}
	seedMessage(t, pool, conversationA, 1, fixedTime.Add(-366*24*time.Hour))
	var before time.Time
	if err := pool.QueryRow(context.Background(), "SELECT clock_timestamp()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	batch, err := (Worker{DB: pool}).ProcessTenant(context.Background(), tenantA)
	if err != nil || batch.ClearedCount != 1 || batch.ClearedAt.Before(before) {
		t.Fatalf("database clock: %+v %v", batch, err)
	}
}
