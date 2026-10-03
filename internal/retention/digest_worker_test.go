package retention

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func seedDigestCandidate(t *testing.T, pool *pgxpool.Pool, cid string, seq int64, expires time.Time, clear bool) {
	t.Helper()
	seedMessage(t, pool, cid, seq, expires.Add(-31*24*time.Hour))
	exec(t, pool, `INSERT INTO message_idempotency(tenant_id,conversation_id,sender_user_id,client_msg_id,message_id,content_digest,accepted_at,expires_at)
 SELECT tenant_id,conversation_id,sender_user_id,client_msg_id,id,content_digest,accepted_at,$3 FROM messages WHERE conversation_id=$1 AND seq=$2`, cid, seq, expires)
	if clear {
		exec(t, pool, `UPDATE messages SET text_body=NULL,body_cleared_at=$3 WHERE conversation_id=$1 AND seq=$2`, cid, seq, fixedTime.Add(-time.Hour))
	}
}
func testDigestWorker(pool *pgxpool.Pool, size int) DigestWorker {
	return DigestWorker{DB: pool, BatchSize: size, clock: func(context.Context, pgx.Tx) (time.Time, error) { return fixedTime, nil }}
}
func assertDigestCounts(t *testing.T, pool *pgxpool.Pool, retired, batches int) {
	t.Helper()
	var m, i, b, matched int
	err := pool.QueryRow(context.Background(), `SELECT
 (SELECT count(*) FROM messages WHERE content_digest IS NULL),
 (SELECT count(*) FROM message_idempotency WHERE content_digest IS NULL),
 (SELECT count(*) FROM message_digest_retirement_batches),
 (SELECT count(*) FROM messages m JOIN message_idempotency i ON i.message_id=m.id AND i.tenant_id=m.tenant_id WHERE m.digest_retired_at IS NOT NULL AND m.digest_retired_at=i.digest_retired_at)`).Scan(&m, &i, &b, &matched)
	if err != nil || m != retired || i != retired || matched != retired || b != batches {
		t.Fatalf("retired %d/%d matched %d batches %d want %d/%d: %v", m, i, matched, b, retired, batches, err)
	}
}
func TestDigestProcessTenantConditionsAndEvidence(t *testing.T) {
	t.Run("batch boundary and evidence", func(t *testing.T) {
		pool := database(t)
		for _, row := range []struct {
			seq   int64
			delta time.Duration
		}{{1, time.Microsecond}, {2, 0}, {3, -2 * time.Hour}, {4, -time.Hour}} {
			seedDigestCandidate(t, pool, conversationA, row.seq, fixedTime.Add(row.delta), true)
		}
		seedDigestCandidate(t, pool, conversationB, 1, fixedTime.Add(-time.Hour), true)
		w := testDigestWorker(pool, 2)
		b, err := w.ProcessTenant(context.Background(), tenantA)
		if err != nil || b.RetiredCount != 2 || b.BatchID == "" || b.FirstSeq != 3 || b.LastSeq != 4 || !b.MinExpiresAt.Equal(fixedTime.Add(-2*time.Hour)) || !b.MaxExpiresAt.Equal(fixedTime.Add(-time.Hour)) || !b.RetiredAt.Equal(fixedTime) {
			t.Fatalf("batch: %+v %v", b, err)
		}
		assertDigestCounts(t, pool, 2, 1)
		b, err = w.ProcessTenant(context.Background(), tenantA)
		if err != nil || b.RetiredCount != 1 || b.FirstSeq != 2 {
			t.Fatalf("boundary: %+v %v", b, err)
		}
		b, err = w.ProcessTenant(context.Background(), tenantA)
		if err != nil || b.RetiredCount != 0 {
			t.Fatalf("repeat/future: %+v %v", b, err)
		}
		assertDigestCounts(t, pool, 3, 2)
		var seq int64
		if err := pool.QueryRow(context.Background(), "SELECT last_seq FROM conversations WHERE id=$1", conversationA).Scan(&seq); err != nil || seq != 4 {
			t.Fatalf("seq changed: %d %v", seq, err)
		}
	})
	for _, condition := range []string{"body present", "not expired", "future body clear", "missing key", "inactive tenant"} {
		t.Run(condition, func(t *testing.T) {
			pool := database(t)
			expiry := fixedTime.Add(-time.Hour)
			if condition == "not expired" {
				expiry = fixedTime.Add(time.Hour)
			}
			seedDigestCandidate(t, pool, conversationA, 1, expiry, condition != "body present" && condition != "future body clear")
			switch condition {
			case "future body clear":
				exec(t, pool, "UPDATE messages SET text_body=NULL,body_cleared_at=$1", fixedTime.Add(time.Hour))
			case "missing key":
				exec(t, pool, "DELETE FROM message_idempotency")
			case "inactive tenant":
				exec(t, pool, "UPDATE tenants SET status='suspended' WHERE id=$1", tenantA)
			}
			b, err := testDigestWorker(pool, 0).ProcessTenant(context.Background(), tenantA)
			if err != nil || b.RetiredCount != 0 {
				t.Fatalf("condition: %+v %v", b, err)
			}
			assertDigestCounts(t, pool, 0, 0)
		})
	}

	t.Run("large backlog query plan", func(t *testing.T) {
		pool := database(t)
		exec(t, pool, `INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest,accepted_at)
 SELECT $1,$2,n,$3,$4,('0199f04a-0000-7000-8000-'||lpad(to_hex(n),12,'0'))::uuid,'body',decode(repeat('ab',32),'hex'),$5 FROM generate_series(1,4000) n`, tenantA, conversationA, userA, memberA, fixedTime.Add(-31*24*time.Hour))
		exec(t, pool, `INSERT INTO message_idempotency(tenant_id,conversation_id,sender_user_id,client_msg_id,message_id,content_digest,accepted_at,expires_at)
 SELECT tenant_id,conversation_id,sender_user_id,client_msg_id,id,content_digest,accepted_at,CASE WHEN seq<=10 THEN $1::timestamptz ELSE $1::timestamptz+INTERVAL '1 hour' END FROM messages`, fixedTime)
		exec(t, pool, "UPDATE messages SET text_body=NULL,body_cleared_at=$1", fixedTime.Add(-time.Hour))
		exec(t, pool, "ANALYZE messages")
		exec(t, pool, "ANALYZE message_idempotency")
		rows, err := pool.Query(context.Background(), `EXPLAIN SELECT m.id FROM message_idempotency i JOIN messages m ON m.tenant_id=i.tenant_id AND m.id=i.message_id WHERE i.tenant_id=$1 AND i.conversation_id=$2 AND i.digest_retired_at IS NULL AND i.expires_at<=$3 AND m.digest_retired_at IS NULL AND m.text_body IS NULL AND m.body_cleared_at<=$3 ORDER BY i.expires_at,m.seq LIMIT 100`, tenantA, conversationA, fixedTime)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			t.Log(line)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		b, err := testDigestWorker(pool, 0).ProcessTenant(context.Background(), tenantA)
		if err != nil || b.RetiredCount != 10 {
			t.Fatalf("backlog: %+v %v", b, err)
		}
		assertDigestCounts(t, pool, 10, 1)
	})
	t.Run("configuration", func(t *testing.T) {
		if _, err := (DigestWorker{}).ProcessTenant(context.Background(), tenantA); !errors.Is(err, ErrWorkerUnconfigured) {
			t.Fatal(err)
		}
		pool := database(t)
		for _, n := range []int{-1, 1001} {
			if _, err := testDigestWorker(pool, n).ProcessTenant(context.Background(), tenantA); !errors.Is(err, ErrInvalidBatchSize) {
				t.Fatalf("batch %d: %v", n, err)
			}
		}
	})
	t.Run("database clock", func(t *testing.T) {
		pool := database(t)
		seedDigestCandidate(t, pool, conversationA, 1, time.Now().UTC().Add(-time.Hour), true)
		b, err := (DigestWorker{DB: pool}).ProcessTenant(context.Background(), tenantA)
		if err != nil || b.RetiredCount != 1 || b.RetiredAt.IsZero() {
			t.Fatalf("production clock: %+v %v", b, err)
		}
	})
}
