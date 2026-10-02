package access_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

const (
	legalHoldReleaseRequestOne = "00000000-0000-4000-8000-0000000001b1"
	legalHoldReleaseRequestTwo = "00000000-0000-4000-8000-0000000001b2"
	legalHoldPlaceRequestThree = "00000000-0000-4000-8000-0000000001b3"
)

func TestReleaseLegalHoldKeepsOtherCasesActive(t *testing.T) {
	conn := testDB(t)
	seedLegalHoldService(t, conn)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	ctx := context.Background()
	first, _, err := svc.PlaceLegalHold(ctx, admin, legalHoldConversation, legalHoldRequestOne, "CASE-A")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := svc.PlaceLegalHold(ctx, admin, legalHoldConversation, legalHoldRequestTwo, "CASE-B")
	if err != nil {
		t.Fatal(err)
	}
	released, err := svc.ReleaseLegalHold(ctx, admin, legalHoldConversation, first.ID, legalHoldReleaseRequestOne, "APPROVAL-A")
	if err != nil || released.ID != first.ID || released.ReleasedAt == nil ||
		released.ReleaseApprovalReference != "APPROVAL-A" || released.ReleasedByUserID != adminA {
		t.Fatalf("release: %+v %v", released, err)
	}
	replay, err := svc.ReleaseLegalHold(ctx, admin, legalHoldConversation, first.ID, legalHoldReleaseRequestOne, "APPROVAL-A")
	if err != nil || replay.ID != first.ID || replay.ReleasedAt == nil {
		t.Fatalf("release replay: %+v %v", replay, err)
	}
	if _, err := svc.ReleaseLegalHold(ctx, admin, legalHoldConversation, first.ID, legalHoldReleaseRequestTwo, "APPROVAL-A"); !errors.Is(err, access.ErrConflict) {
		t.Fatalf("second release: %v", err)
	}
	if _, err := svc.ReleaseLegalHold(ctx, admin, legalHoldConversation, first.ID, legalHoldRequestOne, "APPROVAL-A"); !errors.Is(err, access.ErrConflict) {
		t.Fatalf("placement request reused for release: %v", err)
	}
	placedReplay, created, err := svc.PlaceLegalHold(ctx, admin, legalHoldConversation, legalHoldRequestOne, "CASE-A")
	if err != nil || created || placedReplay.ReleasedAt == nil || placedReplay.ID != first.ID {
		t.Fatalf("placement replay after release: %+v %v %v", placedReplay, created, err)
	}
	replacement, created, err := svc.PlaceLegalHold(ctx, admin, legalHoldConversation, legalHoldPlaceRequestThree, "CASE-A")
	if err != nil || !created || replacement.ID == first.ID {
		t.Fatalf("replacement case: %+v %v %v", replacement, created, err)
	}
	var active, events int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM conversation_legal_holds
 WHERE tenant_id=$1 AND conversation_id=$2 AND released_at IS NULL`, tenantA, legalHoldConversation).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM conversation_legal_hold_events
 WHERE tenant_id=$1 AND conversation_id=$2`, tenantA, legalHoldConversation).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if active != 2 || events != 4 || second.ReleasedAt != nil {
		t.Fatalf("active=%d events=%d other=%+v", active, events, second)
	}
}

func TestReleaseLegalHoldConcurrentAndAuditRollback(t *testing.T) {
	first := testDB(t)
	seedLegalHoldService(t, first)
	second := secondConnection(t, first)
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	svc := access.Service{DB: first, Now: func() time.Time { return fixedTime }}
	hold, _, err := svc.PlaceLegalHold(context.Background(), admin, legalHoldConversation, legalHoldRequestOne, "CASE-A")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for i, conn := range []*pgx.Conn{first, second} {
		request := legalHoldReleaseRequestOne
		if i == 1 {
			request = legalHoldReleaseRequestTwo
		}
		go func(conn *pgx.Conn, request string) {
			_, err := (access.Service{DB: conn, Now: func() time.Time { return fixedTime }}).
				ReleaseLegalHold(ctx, admin, legalHoldConversation, hold.ID, request, "APPROVAL")
			results <- err
		}(conn, request)
	}
	a, b := <-results, <-results
	if !((a == nil && errors.Is(b, access.ErrConflict)) || (b == nil && errors.Is(a, access.ErrConflict))) {
		t.Fatalf("concurrent releases: %v %v", a, b)
	}
	var releases int
	if err := first.QueryRow(ctx, `SELECT count(*) FROM conversation_legal_hold_events
 WHERE hold_id=$1 AND event_type='released'`, hold.ID).Scan(&releases); err != nil || releases != 1 {
		t.Fatalf("release events=%d %v", releases, err)
	}
}

func TestReleaseLegalHoldAuditAndGrantExpiryRollback(t *testing.T) {
	first := testDB(t)
	seedLegalHoldService(t, first)
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	svc := access.Service{DB: first, Now: func() time.Time { return fixedTime }}
	hold, _, err := svc.PlaceLegalHold(context.Background(), admin, legalHoldConversation, legalHoldRequestOne, "CASE-A")
	if err != nil {
		t.Fatal(err)
	}
	run(t, first, `CREATE FUNCTION reject_legal_hold_release_audit() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.action='legal_hold_release' THEN RAISE EXCEPTION 'audit down'; END IF; RETURN NEW; END $$`)
	run(t, first, `CREATE TRIGGER reject_legal_hold_release_audit BEFORE INSERT ON audit_events
 FOR EACH ROW EXECUTE FUNCTION reject_legal_hold_release_audit()`)
	if _, err := svc.ReleaseLegalHold(context.Background(), admin, legalHoldConversation, hold.ID,
		legalHoldReleaseRequestOne, "APPROVAL"); !errors.Is(err, access.ErrAuditUnavailable) {
		t.Fatalf("audit failure: %v", err)
	}
	run(t, first, `DROP TRIGGER reject_legal_hold_release_audit ON audit_events`)
	var releasedAt *time.Time
	if err := first.QueryRow(context.Background(), `SELECT released_at FROM conversation_legal_holds WHERE id=$1`, hold.ID).Scan(&releasedAt); err != nil || releasedAt != nil {
		t.Fatalf("release persisted after audit failure: %v %v", releasedAt, err)
	}
	run(t, first, `UPDATE admin_grants SET effective_to=$2
 WHERE tenant_id=$1 AND membership_id=$3 AND role='group_admin'`,
		tenantA, fixedTime.Add(time.Hour), adminM)
	second := secondConnection(t, first)
	blocker, err := first.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := blocker.QueryRow(context.Background(), `SELECT id FROM conversations
 WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantA, legalHoldConversation).Scan(new(string)); err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	clock.Store(fixedTime.UnixNano())
	started := make(chan struct{})
	deferred := access.Service{DB: &syncedDB{conn: second, started: started},
		Now: func() time.Time { return time.Unix(0, clock.Load()) }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := deferred.ReleaseLegalHold(ctx, admin, legalHoldConversation, hold.ID,
			legalHoldReleaseRequestOne, "APPROVAL")
		result <- err
	}()
	<-started
	time.Sleep(50 * time.Millisecond)
	clock.Store(fixedTime.Add(2 * time.Hour).UnixNano())
	if err := blocker.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("expired grant released hold: %v", err)
	}
	var events int
	if err := first.QueryRow(context.Background(), `SELECT count(*) FROM conversation_legal_hold_events
 WHERE hold_id=$1 AND event_type='released'`, hold.ID).Scan(&events); err != nil || events != 0 {
		t.Fatalf("expired grant left release event=%d %v", events, err)
	}
}
