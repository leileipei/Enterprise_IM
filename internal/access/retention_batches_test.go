package access_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

func seedRetentionBatches(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	seedLegalHoldService(t, conn)
	for n := 1; n <= 3; n++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-00000000900%d", n)
		run(t, conn, `INSERT INTO message_body_clear_batches
 (id,tenant_id,conversation_id,retention_days,cutoff_at,cleared_at,first_seq,last_seq,cleared_count)
 VALUES ($1,$2,$3,365,$4::timestamptz-INTERVAL '8760 hours',$4,2,7,3)`, id, tenantA, legalHoldConversation, fixedTime)
		run(t, conn, `INSERT INTO message_digest_retirement_batches
 (id,tenant_id,conversation_id,retired_at,retired_count,first_seq,last_seq,min_expires_at,max_expires_at)
 VALUES ($1,$2,$3,$4,2,4,8,$4::timestamptz-INTERVAL '2 days',$4)`, id, tenantA, legalHoldConversation, fixedTime)
	}
}

func batchAdmin() access.TrustedIdentity {
	return access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
}

// Catches missing tenant/type filters, unstable tied-time ordering and lost metadata.
func TestRetentionBatchesPagingAndEvidence(t *testing.T) {
	conn := testDB(t)
	seedRetentionBatches(t, conn)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	ctx := context.Background()
	for _, kind := range []string{"body", "digest"} {
		first, err := svc.ListRetentionBatches(ctx, batchAdmin(), legalHoldConversation, kind, "", 2)
		if err != nil || len(first.Batches) != 2 || first.NextCursor == "" {
			t.Fatalf("%s first: %+v %v", kind, first, err)
		}
		second, err := svc.ListRetentionBatches(ctx, batchAdmin(), legalHoldConversation, kind, first.NextCursor, 2)
		if err != nil || len(second.Batches) != 1 || second.NextCursor != "" {
			t.Fatalf("%s second: %+v %v", kind, second, err)
		}
		if first.Batches[0].ID != "00000000-0000-4000-8000-000000009003" ||
			first.Batches[1].ID != "00000000-0000-4000-8000-000000009002" ||
			second.Batches[0].ID != "00000000-0000-4000-8000-000000009001" {
			t.Fatalf("%s tied order: %+v %+v", kind, first, second)
		}
		b := first.Batches[0]
		if b.Kind != kind || b.ConversationID != legalHoldConversation || !b.ProcessedAt.Equal(fixedTime) {
			t.Fatalf("metadata: %+v", b)
		}
		if kind == "body" {
			if b.ProcessedCount != 3 || b.FirstSeq != 2 || b.LastSeq != 7 || b.RetentionDays == nil || *b.RetentionDays != 365 || b.CutoffAt == nil || !b.CutoffAt.Equal(fixedTime.Add(-8760*time.Hour)) || b.MinExpiresAt != nil || b.MaxExpiresAt != nil {
				t.Fatalf("body evidence: %+v", b)
			}
		} else if b.ProcessedCount != 2 || b.FirstSeq != 4 || b.LastSeq != 8 || b.RetentionDays != nil || b.CutoffAt != nil || b.MinExpiresAt == nil || !b.MinExpiresAt.Equal(fixedTime.Add(-48*time.Hour)) || b.MaxExpiresAt == nil || !b.MaxExpiresAt.Equal(fixedTime) {
			t.Fatalf("digest evidence: %+v", b)
		}
		for _, input := range []struct{ conversation, kind, cursor string }{
			{legalHoldOtherConversation, kind, first.NextCursor},
			{legalHoldConversation, map[string]string{"body": "digest", "digest": "body"}[kind], first.NextCursor},
			{legalHoldConversation, kind, "bad"},
			{legalHoldConversation, kind, strings.Repeat("a", 2000)},
		} {
			if _, err := svc.ListRetentionBatches(ctx, batchAdmin(), input.conversation, input.kind, input.cursor, 2); !errors.Is(err, access.ErrInvalidRetentionQuery) {
				t.Fatalf("cursor accepted: %+v %v", input, err)
			}
		}
		foreign := access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: personBM}
		if _, err := svc.ListRetentionBatches(ctx, foreign, legalHoldConversation, kind, first.NextCursor, 2); !errors.Is(err, access.ErrInvalidRetentionQuery) {
			t.Fatalf("foreign cursor: %v", err)
		}
	}
	empty, err := svc.ListRetentionBatches(ctx, batchAdmin(), legalHoldOtherConversation, "body", "", 100)
	if err != nil || empty.Batches == nil || len(empty.Batches) != 0 || empty.NextCursor != "" {
		t.Fatalf("empty: %+v %v", empty, err)
	}
	var allows int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='retention_batches_list' AND outcome='allow'`).Scan(&allows); err != nil || allows != 5 {
		t.Fatalf("query audit: %d %v", allows, err)
	}
	var bodies, digests int
	if err := conn.QueryRow(ctx, `SELECT count(*) FILTER (WHERE reason='listed_body'),count(*) FILTER (WHERE reason='listed_digest')
 FROM audit_events WHERE action='retention_batches_list' AND outcome='allow'`).Scan(&bodies, &digests); err != nil || bodies != 3 || digests != 2 {
		t.Fatalf("audit type missing: body=%d digest=%d %v", bodies, digests, err)
	}
}

// Catches authorization bypass, stale authorization on the next page and audit leakage.
func TestRetentionBatchesAuthorizationAndAuditFailure(t *testing.T) {
	conn := testDB(t)
	seedRetentionBatches(t, conn)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	ctx := context.Background()
	for _, input := range []struct {
		id           access.TrustedIdentity
		conversation string
		want         error
	}{
		{access.TrustedIdentity{TenantID: tenantA, UserID: personA, ActingMembershipID: personM2}, legalHoldConversation, access.ErrNotFound},
		{access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: personBM}, legalHoldConversation, access.ErrNotFound},
		{batchAdmin(), legalHoldOne, access.ErrNotFound},
	} {
		p, err := svc.ListRetentionBatches(ctx, input.id, input.conversation, "body", "", 2)
		if !errors.Is(err, input.want) || len(p.Batches) != 0 {
			t.Fatalf("denied: %+v %v", p, err)
		}
	}
	first, err := svc.ListRetentionBatches(ctx, batchAdmin(), legalHoldConversation, "body", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, `UPDATE admin_grants SET effective_to=$1 WHERE membership_id=$2 AND role='group_admin'`, fixedTime, adminM)
	if p, err := svc.ListRetentionBatches(ctx, batchAdmin(), legalHoldConversation, "body", first.NextCursor, 2); !errors.Is(err, access.ErrNotFound) || len(p.Batches) != 0 {
		t.Fatalf("revoked grant: %+v %v", p, err)
	}
	run(t, conn, `UPDATE admin_grants SET effective_to=NULL WHERE membership_id=$1 AND role='group_admin'`, adminM)
	run(t, conn, `UPDATE user_organizations SET status='ended',effective_to=$1 WHERE id=$2`, fixedTime, adminM)
	if _, err := svc.ListRetentionBatches(ctx, batchAdmin(), legalHoldConversation, "body", "", 2); !errors.Is(err, access.ErrInvalidIdentity) {
		t.Fatalf("ended: %v", err)
	}
	run(t, conn, `UPDATE user_organizations SET status='active',effective_to=NULL WHERE id=$1`, adminM)
	run(t, conn, `CREATE FUNCTION reject_batch_query_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='retention_batches_list' THEN RAISE EXCEPTION 'audit down'; END IF; RETURN NEW; END $$`)
	run(t, conn, `CREATE TRIGGER reject_batch_query_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_batch_query_audit()`)
	for _, kind := range []string{"body", "digest"} {
		p, err := svc.ListRetentionBatches(ctx, batchAdmin(), legalHoldConversation, kind, "", 2)
		if !errors.Is(err, access.ErrAuditUnavailable) || len(p.Batches) != 0 || p.NextCursor != "" {
			t.Fatalf("audit leakage: %+v %v", p, err)
		}
	}
}

func TestRetentionBatchesInvalidAndExpiringGrant(t *testing.T) {
	conn := testDB(t)
	seedRetentionBatches(t, conn)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	for _, input := range []struct {
		kind  string
		limit int
	}{{"", 1}, {"all", 1}, {"BODY", 1}, {"body", 0}, {"body", 501}} {
		if _, err := svc.ListRetentionBatches(context.Background(), batchAdmin(), legalHoldConversation, input.kind, "", input.limit); !errors.Is(err, access.ErrInvalidRetentionQuery) {
			t.Fatalf("invalid input: %+v %v", input, err)
		}
	}
	run(t, conn, `UPDATE admin_grants SET effective_to=$1 WHERE membership_id=$2 AND role='group_admin'`, fixedTime.Add(time.Hour), adminM)
	calls := 0
	svc.Now = func() time.Time {
		calls++
		if calls >= 3 {
			return fixedTime.Add(2 * time.Hour)
		}
		return fixedTime
	}
	if p, err := svc.ListRetentionBatches(context.Background(), batchAdmin(), legalHoldConversation, "body", "", 2); !errors.Is(err, access.ErrNotFound) || len(p.Batches) != 0 {
		t.Fatalf("grant expired during query: %+v %v", p, err)
	}
}

// Catches a stale REPEATABLE READ snapshot retained after the actor lock.
func TestRetentionBatchesUsesFreshAuthorizationSnapshot(t *testing.T) {
	conn := testDB(t)
	seedRetentionBatches(t, conn)
	other := secondConnection(t, conn)
	run(t, other, "SET default_transaction_isolation TO 'repeatable read'")
	changed := false
	svc := access.Service{DB: other, Now: func() time.Time {
		if !changed {
			run(t, conn, `UPDATE admin_grants SET status='revoked' WHERE membership_id=$1 AND role='group_admin'`, adminM)
			changed = true
		}
		return fixedTime
	}}
	page, err := svc.ListRetentionBatches(context.Background(), batchAdmin(), legalHoldConversation, "body", "", 2)
	if !errors.Is(err, access.ErrNotFound) || len(page.Batches) != 0 || !changed {
		t.Fatalf("stale grant: %+v %v", page, err)
	}
}

// Catches authorization expiry while blocked on a real conversation row lock.
func TestRetentionBatchesRechecksAfterConversationLockWait(t *testing.T) {
	conn := testDB(t)
	seedRetentionBatches(t, conn)
	run(t, conn, `UPDATE admin_grants SET effective_to=$1 WHERE membership_id=$2 AND role='group_admin'`, fixedTime.Add(time.Hour), adminM)
	other := secondConnection(t, conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	block, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer block.Rollback(context.Background())
	if _, err = block.Exec(ctx, `SELECT id FROM conversations WHERE id=$1 FOR UPDATE`, legalHoldConversation); err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	clock.Store(fixedTime.UnixNano())
	svc := access.Service{DB: other, Now: func() time.Time { return time.Unix(0, clock.Load()) }}
	result := make(chan error, 1)
	go func() {
		p, e := svc.ListRetentionBatches(ctx, batchAdmin(), legalHoldConversation, "body", "", 2)
		if len(p.Batches) != 0 {
			e = fmt.Errorf("leaked evidence: %+v", p)
		}
		result <- e
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := block.QueryRow(ctx, `SELECT COALESCE(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1`, int(other.PgConn().PID())).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case e := <-result:
			t.Fatalf("did not wait: %v", e)
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	clock.Store(fixedTime.Add(2 * time.Hour).UnixNano())
	if err := block.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("expired after wait: %v", err)
	}
}
