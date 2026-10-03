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
	legalHoldOtherConversation = "00000000-0000-4000-8000-000000000197"
	legalHoldOtherUser         = "00000000-0000-4000-8000-000000000198"
	legalHoldOtherMembership   = "00000000-0000-4000-8000-000000000199"
	legalHoldPlaceRequestTwo   = "00000000-0000-4000-8000-0000000001a1"
)

func seedLegalHoldService(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	seedAccess(t, conn)
	run(t, conn, `INSERT INTO users (id,tenant_id,global_employee_no,display_name)
 VALUES ($1,$2,'A003','用户三')`, legalHoldOtherUser, tenantA)
	run(t, conn, `INSERT INTO user_organizations
 (id,tenant_id,user_id,organization_id,effective_from,is_primary)
 VALUES ($1,$2,$3,$4,'2020-01-01',true)`, legalHoldOtherMembership,
		tenantA, legalHoldOtherUser, orgA)
	run(t, conn, `INSERT INTO admin_grants
 (id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ('00000000-0000-4000-8000-0000000001a2',$1,$2,$3,'group_admin','2020-01-01'),
 ('00000000-0000-4000-8000-0000000001a3',$1,$4,$3,'group_admin','2020-01-01'),
 ('00000000-0000-4000-8000-0000000001a4',$5,$6,$7,'group_admin','2020-01-01')`,
		tenantA, adminM, orgA, personM, tenantB, personBM, orgB)
	run(t, conn, `INSERT INTO admin_grants
 (id,tenant_id,membership_id,membership_organization_id,role,scope_organization_id,effective_from)
 VALUES ('00000000-0000-4000-8000-0000000001a5',$1,$2,$3,'organization_admin',$3,'2020-01-01')`,
		tenantA, personM2, orgA2)
	run(t, conn, `INSERT INTO conversations
 (id,tenant_id,direct_user_low_id,direct_user_high_id,direct_low_membership_id,
 direct_high_membership_id,created_by_user_id)
 VALUES ($1,$2,$3,$4,$5,$6,$3),($7,$2,$3,$8,$5,$9,$3)`,
		legalHoldConversation, tenantA, adminA, personA, adminM, personM,
		legalHoldOtherConversation, legalHoldOtherUser, legalHoldOtherMembership)
}

func TestPlaceLegalHoldAuthorizationAndReplay(t *testing.T) {
	conn := testDB(t)
	seedLegalHoldService(t, conn)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	orgAdmin := access.TrustedIdentity{TenantID: tenantA, UserID: personA, ActingMembershipID: personM2}
	otherAdmin := access.TrustedIdentity{TenantID: tenantA, UserID: personA, ActingMembershipID: personM}
	foreignAdmin := access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: personBM}
	ctx := context.Background()
	if _, _, err := svc.PlaceLegalHold(ctx, orgAdmin, legalHoldConversation, legalHoldRequestOne, "CASE-A"); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("organization admin placed hold: %v", err)
	}
	if _, _, err := svc.PlaceLegalHold(ctx, foreignAdmin, legalHoldConversation, legalHoldRequestOne, "CASE-A"); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("cross-tenant conversation visible: %v", err)
	}
	if _, _, err := svc.PlaceLegalHold(ctx, admin, legalHoldOne, legalHoldRequestOne, "CASE-A"); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("missing conversation visible: %v", err)
	}
	if _, _, err := svc.PlaceLegalHold(ctx, access.TrustedIdentity{
		TenantID: tenantA, UserID: personB, ActingMembershipID: adminM},
		legalHoldConversation, legalHoldRequestOne, "CASE-A"); !errors.Is(err, access.ErrInvalidIdentity) {
		t.Fatalf("invalid identity placed hold: %v", err)
	}
	hold, created, err := svc.PlaceLegalHold(ctx, admin, legalHoldConversation, legalHoldRequestOne, "CASE-A")
	if err != nil || !created || hold.ID == "" || hold.ConversationID != legalHoldConversation ||
		hold.CaseReference != "CASE-A" || hold.PlacedByUserID != adminA ||
		hold.ReleasedAt != nil {
		t.Fatalf("placed hold: %+v created=%v err=%v", hold, created, err)
	}
	replay, created, err := svc.PlaceLegalHold(ctx, admin, legalHoldConversation, legalHoldRequestOne, "CASE-A")
	if err != nil || created || replay.ID != hold.ID {
		t.Fatalf("placement replay: %+v created=%v err=%v", replay, created, err)
	}
	for _, input := range []struct {
		actor        access.TrustedIdentity
		conversation string
		reference    string
	}{
		{otherAdmin, legalHoldConversation, "CASE-A"},
		{admin, legalHoldOtherConversation, "CASE-A"},
		{admin, legalHoldConversation, "CASE-B"},
	} {
		if _, _, err := svc.PlaceLegalHold(ctx, input.actor, input.conversation,
			legalHoldRequestOne, input.reference); !errors.Is(err, access.ErrConflict) {
			t.Fatalf("request ID reuse %+v: %v", input, err)
		}
	}
	if _, _, err := svc.PlaceLegalHold(ctx, admin, legalHoldConversation,
		legalHoldPlaceRequestTwo, "CASE-A"); !errors.Is(err, access.ErrConflict) {
		t.Fatalf("duplicate active case: %v", err)
	}
	var holds, events, allows int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM conversation_legal_holds
 WHERE tenant_id=$1 AND conversation_id=$2`, tenantA, legalHoldConversation).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM conversation_legal_hold_events
 WHERE tenant_id=$1 AND conversation_id=$2`, tenantA, legalHoldConversation).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM audit_events
 WHERE tenant_id=$1 AND action='legal_hold_place' AND outcome='allow'`, tenantA).Scan(&allows); err != nil {
		t.Fatal(err)
	}
	if holds != 1 || events != 1 || allows != 2 {
		t.Fatalf("placement retry changed evidence: holds=%d events=%d audit=%d", holds, events, allows)
	}
}

func TestPlaceLegalHoldRechecksExpiredGrantAfterConversationLockWait(t *testing.T) {
	first := testDB(t)
	seedLegalHoldService(t, first)
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
	svc := access.Service{DB: &syncedDB{conn: second, started: started},
		Now: func() time.Time { return time.Unix(0, clock.Load()) }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, err := svc.PlaceLegalHold(ctx, access.TrustedIdentity{
			TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM},
			legalHoldConversation, legalHoldRequestOne, "CASE-EXPIRY")
		result <- err
	}()
	<-started
	time.Sleep(50 * time.Millisecond)
	clock.Store(fixedTime.Add(2 * time.Hour).UnixNano())
	if err := blocker.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("expired grant placed hold: %v", err)
	}
	var holds, events int
	if err := first.QueryRow(context.Background(), `SELECT count(*) FROM conversation_legal_holds
 WHERE tenant_id=$1`, tenantA).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if err := first.QueryRow(context.Background(), `SELECT count(*) FROM conversation_legal_hold_events
 WHERE tenant_id=$1`, tenantA).Scan(&events); err != nil || holds != 0 || events != 0 {
		t.Fatalf("expired grant left evidence: holds=%d events=%d err=%v", holds, events, err)
	}
}

func TestPlaceLegalHoldAuditFailureRollsBack(t *testing.T) {
	conn := testDB(t)
	seedLegalHoldService(t, conn)
	run(t, conn, `CREATE FUNCTION reject_legal_hold_audit() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.action='legal_hold_place' THEN RAISE EXCEPTION 'audit down'; END IF; RETURN NEW; END $$`)
	run(t, conn, `CREATE TRIGGER reject_legal_hold_audit BEFORE INSERT ON audit_events
 FOR EACH ROW EXECUTE FUNCTION reject_legal_hold_audit()`)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	_, _, err := svc.PlaceLegalHold(context.Background(), access.TrustedIdentity{
		TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM},
		legalHoldConversation, legalHoldRequestOne, "CASE-ROLLBACK")
	if !errors.Is(err, access.ErrAuditUnavailable) {
		t.Fatalf("audit outage did not fail closed: %v", err)
	}
	var holds, events int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM conversation_legal_holds`).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM conversation_legal_hold_events`).Scan(&events); err != nil || holds != 0 || events != 0 {
		t.Fatalf("audit outage left partial hold: holds=%d events=%d err=%v", holds, events, err)
	}
}

func TestLegalHoldManagementAuditsExpiredActingMembership(t *testing.T) {
	conn := testDB(t)
	seedLegalHoldService(t, conn)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	hold, _, err := svc.PlaceLegalHold(context.Background(), admin, legalHoldConversation,
		legalHoldRequestOne, "CASE-EXISTING")
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, `UPDATE user_organizations SET status='ended',effective_to=$2
 WHERE tenant_id=$1 AND id=$3`, tenantA, fixedTime, adminM)
	if _, _, err := svc.PlaceLegalHold(context.Background(), admin, legalHoldConversation,
		legalHoldRequestTwo, "CASE-DENIED"); !errors.Is(err, access.ErrInvalidIdentity) {
		t.Fatalf("ended administrator placed hold: %v", err)
	}
	if _, err := svc.ReleaseLegalHold(context.Background(), admin, legalHoldConversation,
		hold.ID, legalHoldRelease, "APPROVAL-DENIED"); !errors.Is(err, access.ErrInvalidIdentity) {
		t.Fatalf("ended administrator released hold: %v", err)
	}
	if _, err := svc.ListLegalHolds(context.Background(), admin, legalHoldConversation,
		"", 10); !errors.Is(err, access.ErrInvalidIdentity) {
		t.Fatalf("ended administrator listed holds: %v", err)
	}
	var denied, holds, events int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM audit_events
 WHERE tenant_id=$1 AND acting_membership_id=$2 AND outcome='deny'
 AND action IN ('legal_hold_place','legal_hold_release','legal_hold_list')`,
		tenantA, adminM).Scan(&denied); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM conversation_legal_holds
 WHERE tenant_id=$1`, tenantA).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM conversation_legal_hold_events
 WHERE tenant_id=$1`, tenantA).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if denied != 3 || holds != 1 || events != 1 {
		t.Fatalf("ended administrator evidence: denied=%d holds=%d events=%d", denied, holds, events)
	}
}

func TestLegalHoldPlaceAuditsAuthorizationLostAfterWrite(t *testing.T) {
	conn := testDB(t)
	seedLegalHoldService(t, conn)
	run(t, conn, `UPDATE admin_grants SET effective_to=$2
 WHERE tenant_id=$1 AND membership_id=$3 AND role='group_admin'`,
		tenantA, fixedTime.Add(time.Hour), adminM)
	var calls atomic.Int64
	svc := access.Service{DB: conn, Now: func() time.Time {
		if calls.Add(1) >= 3 {
			return fixedTime.Add(2 * time.Hour)
		}
		return fixedTime
	}}
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	if _, _, err := svc.PlaceLegalHold(context.Background(), admin, legalHoldConversation,
		legalHoldRequestOne, "CASE-EXPIRED-AFTER-WRITE"); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("expired grant placed hold: %v", err)
	}
	var holds, events, denied int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM conversation_legal_holds`).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM conversation_legal_hold_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM audit_events
 WHERE action='legal_hold_place' AND outcome='deny'`).Scan(&denied); err != nil {
		t.Fatal(err)
	}
	if holds != 0 || events != 0 || denied != 1 {
		t.Fatalf("expired grant evidence: holds=%d events=%d denied=%d", holds, events, denied)
	}
}

func TestPlaceLegalHoldConcurrentCaseAndMembershipEnd(t *testing.T) {
	first := testDB(t)
	seedLegalHoldService(t, first)
	second := secondConnection(t, first)
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for i, conn := range []*pgx.Conn{first, second} {
		requestID := legalHoldRequestOne
		if i == 1 {
			requestID = legalHoldRequestTwo
		}
		go func(conn *pgx.Conn, requestID string) {
			_, _, err := (access.Service{DB: conn, Now: func() time.Time { return fixedTime }}).
				PlaceLegalHold(ctx, admin, legalHoldConversation, requestID, "CASE-CONCURRENT")
			results <- err
		}(conn, requestID)
	}
	a, b := <-results, <-results
	if !((a == nil && errors.Is(b, access.ErrConflict)) ||
		(b == nil && errors.Is(a, access.ErrConflict))) {
		t.Fatalf("concurrent same-case holds: %v %v", a, b)
	}
	var count int
	if err := first.QueryRow(ctx, `SELECT count(*) FROM conversation_legal_holds
 WHERE tenant_id=$1 AND conversation_id=$2 AND released_at IS NULL`,
		tenantA, legalHoldConversation).Scan(&count); err != nil || count != 1 {
		t.Fatalf("concurrent active holds: %d %v", count, err)
	}
	blocker, err := first.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := blocker.QueryRow(ctx, `SELECT id FROM conversations
 WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantA, legalHoldOtherConversation).
		Scan(new(string)); err != nil {
		t.Fatal(err)
	}
	placeStarted, endStarted := make(chan struct{}), make(chan struct{})
	placeResult, endResult := make(chan error, 1), make(chan error, 1)
	third := secondConnection(t, first)
	go func() {
		_, _, err := (access.Service{DB: &syncedDB{conn: second, started: placeStarted},
			Now: func() time.Time { return fixedTime }}).
			PlaceLegalHold(ctx, admin, legalHoldOtherConversation,
				"00000000-0000-4000-8000-0000000001a6", "CASE-NO-CYCLE")
		placeResult <- err
	}()
	<-placeStarted
	time.Sleep(50 * time.Millisecond)
	go func() {
		endResult <- (access.Service{DB: &syncedDB{conn: third, started: endStarted},
			Now: func() time.Time { return fixedTime }}).EndMembership(ctx, admin, personM)
	}()
	<-endStarted
	time.Sleep(50 * time.Millisecond)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if placeErr, endErr := <-placeResult, <-endResult; placeErr != nil || endErr != nil {
		t.Fatalf("hold/end membership lock cycle: place=%v end=%v", placeErr, endErr)
	}
}

func TestPlaceLegalHoldSerializesWithActorMembershipEnd(t *testing.T) {
	first := testDB(t)
	seedLegalHoldService(t, first)
	second := secondConnection(t, first)
	third := secondConnection(t, first)
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	otherAdmin := access.TrustedIdentity{TenantID: tenantA, UserID: personA, ActingMembershipID: personM}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	blocker, err := first.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := blocker.QueryRow(ctx, `SELECT id FROM conversations
 WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantA, legalHoldConversation).Scan(new(string)); err != nil {
		t.Fatal(err)
	}
	placeStarted, endStarted := make(chan struct{}), make(chan struct{})
	placeResult, endResult := make(chan error, 1), make(chan error, 1)
	go func() {
		_, _, err := (access.Service{DB: &syncedDB{conn: second, started: placeStarted},
			Now: func() time.Time { return fixedTime }}).PlaceLegalHold(ctx, admin,
			legalHoldConversation, legalHoldRequestOne, "CASE-ACTOR-END")
		placeResult <- err
	}()
	<-placeStarted
	time.Sleep(50 * time.Millisecond)
	go func() {
		endResult <- (access.Service{DB: &syncedDB{conn: third, started: endStarted},
			Now: func() time.Time { return fixedTime }}).EndMembership(ctx, otherAdmin, adminM)
	}()
	<-endStarted
	time.Sleep(50 * time.Millisecond)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if placeErr, endErr := <-placeResult, <-endResult; placeErr != nil || endErr != nil {
		t.Fatalf("actor end lock ordering: place=%v end=%v", placeErr, endErr)
	}
	if _, _, err := (access.Service{DB: first, Now: func() time.Time { return fixedTime.Add(time.Second) }}).
		PlaceLegalHold(ctx, admin, legalHoldConversation, legalHoldRequestTwo,
			"CASE-AFTER-END"); !errors.Is(err, access.ErrInvalidIdentity) {
		t.Fatalf("ended actor placed another hold: %v", err)
	}
	var holds int
	if err := first.QueryRow(ctx, `SELECT count(*) FROM conversation_legal_holds
 WHERE tenant_id=$1 AND conversation_id=$2`, tenantA, legalHoldConversation).Scan(&holds); err != nil || holds != 1 {
		t.Fatalf("actor end changed committed holds: %d %v", holds, err)
	}
}
