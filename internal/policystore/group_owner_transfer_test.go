package policystore_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func ownerTransferRequest(source, target string) policystore.GroupOwnerTransferRequest {
	return policystore.GroupOwnerTransferRequest{
		ClientRequestID: ownerTransferRequestID, SourceIntervalID: source, TargetIntervalID: target,
	}
}

func TestTransferGroupOwnerAllowsOldOwnerToLeave(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	source := groupIntervalFor(t, conn, group.ID, adminA)
	target := groupIntervalFor(t, conn, group.ID, personA)
	run(t, conn, "UPDATE conversations SET last_seq=7 WHERE id=$1", group.ID)
	result, err := svc.TransferGroupOwner(context.Background(), publisher(), group.ID, ownerTransferRequest(source, target))
	if err != nil || !result.Created || result.SourceIntervalID != source || result.TargetIntervalID != target {
		t.Fatalf("owner transfer: %+v %v", result, err)
	}
	var oldRole, newRole string
	var seq int64
	if err := conn.QueryRow(context.Background(), "SELECT role FROM conversation_membership_intervals WHERE id=$1", source).Scan(&oldRole); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT role FROM conversation_membership_intervals WHERE id=$1", target).Scan(&newRole); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT last_seq FROM conversations WHERE id=$1", group.ID).Scan(&seq); err != nil || oldRole != "member" || newRole != "owner" || seq != 7 {
		t.Fatalf("roles or sequence: %s %s %d %v", oldRole, newRole, seq, err)
	}
	if _, err := svc.LeaveGroup(context.Background(), publisher(), group.ID, source); err != nil {
		t.Fatalf("former owner cannot leave: %v", err)
	}
	if _, err := svc.LeaveGroup(context.Background(), groupMemberIdentity(), group.ID, target); !errors.Is(err, policystore.ErrGroupOwnerTransferRequired) {
		t.Fatalf("new owner left without transfer: %v", err)
	}
	var audits int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='group_owner_transfer' AND outcome='allow'").Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("allow audit: %d %v", audits, err)
	}
}

func TestTransferGroupOwnerReplayNeverReversesLaterTransfer(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	firstID := groupIntervalFor(t, conn, group.ID, adminA)
	secondID := groupIntervalFor(t, conn, group.ID, personA)
	first := ownerTransferRequest(firstID, secondID)
	if _, err := svc.TransferGroupOwner(context.Background(), publisher(), group.ID, first); err != nil {
		t.Fatal(err)
	}
	back := ownerTransferRequest(secondID, firstID)
	back.ClientRequestID = "00000000-0000-4000-8000-000000000904"
	if _, err := svc.TransferGroupOwner(context.Background(), groupMemberIdentity(), group.ID, back); err != nil {
		t.Fatal(err)
	}
	replay, err := svc.TransferGroupOwner(context.Background(), publisher(), group.ID, first)
	if err != nil || replay.Created || replay.SourceIntervalID != firstID || replay.TargetIntervalID != secondID {
		t.Fatalf("old request replay: %+v %v", replay, err)
	}
	var owner string
	if err := conn.QueryRow(context.Background(), `SELECT user_id::text FROM conversation_membership_intervals
 WHERE conversation_id=$1 AND role='owner' AND status='active'`, group.ID).Scan(&owner); err != nil || owner != adminA {
		t.Fatalf("old request reversed current owner: %s %v", owner, err)
	}
	changed := first
	changed.TargetIntervalID = firstID
	if _, err := svc.TransferGroupOwner(context.Background(), publisher(), group.ID, changed); !errors.Is(err, policystore.ErrGroupOwnerTransferConflict) {
		t.Fatalf("changed request ID accepted: %v", err)
	}
	var audits int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='group_owner_transfer' AND outcome='allow'").Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("replay created extra audit: %d %v", audits, err)
	}
}

func TestTransferGroupOwnerRejectsUnauthorizedAndUnavailableTargets(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	source := groupIntervalFor(t, conn, group.ID, adminA)
	target := groupIntervalFor(t, conn, group.ID, personA)
	if _, err := svc.TransferGroupOwner(context.Background(), groupMemberIdentity(), group.ID, ownerTransferRequest(target, source)); !errors.Is(err, policystore.ErrGroupOwnerTransferPermissionDenied) {
		t.Fatalf("member transferred ownership: %v", err)
	}
	if _, err := svc.TransferGroupOwner(context.Background(), publisher(), group.ID, ownerTransferRequest(source, source)); !errors.Is(err, policystore.ErrInvalidGroupOwnerTransferRequest) {
		t.Fatalf("self transfer accepted: %v", err)
	}
	otherReq := createGroupRequest(targetM2)
	otherReq.ClientRequestID = "00000000-0000-4000-8000-000000000905"
	otherGroup, err := svc.CreateGroup(context.Background(), publisher(), otherReq)
	if err != nil {
		t.Fatal(err)
	}
	otherTarget := groupIntervalFor(t, conn, otherGroup.ID, personA)
	if _, err := svc.TransferGroupOwner(context.Background(), publisher(), group.ID, ownerTransferRequest(source, otherTarget)); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("other group target visible: %v", err)
	}
	if _, err := svc.TransferGroupOwner(context.Background(), access.TrustedIdentity{
		TenantID: tenantB, UserID: personB, ActingMembershipID: otherM,
	}, group.ID, ownerTransferRequest(source, target)); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("cross-tenant group visible: %v", err)
	}
	run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at, targetM2)
	if _, err := svc.TransferGroupOwner(context.Background(), publisher(), group.ID, ownerTransferRequest(source, target)); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("expired target became owner: %v", err)
	}
	var role string
	if err := conn.QueryRow(context.Background(), "SELECT role FROM conversation_membership_intervals WHERE id=$1", source).Scan(&role); err != nil || role != "owner" {
		t.Fatalf("owner changed after denial: %s %v", role, err)
	}
	run(t, conn, "UPDATE user_organizations SET effective_to=NULL WHERE id=$1", targetM2)
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", personA)
	if _, err := svc.TransferGroupOwner(context.Background(), publisher(), group.ID, ownerTransferRequest(source, target)); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("frozen target became owner: %v", err)
	}
	run(t, conn, "UPDATE users SET status='active' WHERE id=$1", personA)
	run(t, conn, "UPDATE conversations SET status='ended' WHERE id=$1", group.ID)
	if _, err := svc.TransferGroupOwner(context.Background(), publisher(), group.ID, ownerTransferRequest(source, target)); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("ended group transferred ownership: %v", err)
	}
}

func TestTransferGroupOwnerBlockedGroupAndAuditRollback(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	source := groupIntervalFor(t, conn, group.ID, adminA)
	target := groupIntervalFor(t, conn, group.ID, personA)
	run(t, conn, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", group.ID)
	run(t, conn, `CREATE FUNCTION fail_owner_transfer_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.action='group_owner_transfer' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END $$`)
	run(t, conn, `CREATE TRIGGER fail_owner_transfer_audit BEFORE INSERT ON audit_events
 FOR EACH ROW EXECUTE FUNCTION fail_owner_transfer_audit()`)
	if _, err := svc.TransferGroupOwner(context.Background(), publisher(), group.ID, ownerTransferRequest(source, target)); !errors.Is(err, policystore.ErrAuditUnavailable) {
		t.Fatalf("audit failure not returned: %v", err)
	}
	var count int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM group_owner_transfer_requests").Scan(&count); err != nil || count != 0 {
		t.Fatalf("request committed without audit: %d %v", count, err)
	}
	run(t, conn, "DROP TRIGGER fail_owner_transfer_audit ON audit_events")
	if _, err := svc.TransferGroupOwner(context.Background(), publisher(), group.ID, ownerTransferRequest(source, target)); err != nil {
		t.Fatalf("blocked group transfer: %v", err)
	}
	var status string
	if err := conn.QueryRow(context.Background(), "SELECT status FROM conversations WHERE id=$1", group.ID).Scan(&status); err != nil || status != "policy_blocked" {
		t.Fatalf("group status changed: %s %v", status, err)
	}
}

func TestTransferGroupOwnerPromotesAdministratorAndRejectsExpiredActor(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	creator := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := creator.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	source := groupIntervalFor(t, conn, group.ID, adminA)
	target := groupIntervalFor(t, conn, group.ID, personA)
	run(t, conn, "UPDATE conversation_membership_intervals SET role='admin' WHERE id=$1", target)
	run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at.Add(time.Second), adminM)
	checks := 0
	svc := policystore.Service{DB: conn, Now: func() time.Time {
		checks++
		if checks < 3 {
			return at
		}
		return at.Add(2 * time.Second)
	}}
	if _, err := svc.TransferGroupOwner(context.Background(), publisher(), group.ID, ownerTransferRequest(source, target)); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("expired actor transferred ownership: %v", err)
	}
	var denies int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM audit_events
 WHERE action='group_owner_transfer' AND outcome='deny' AND reason='invalid_identity'`).Scan(&denies); err != nil || denies != 1 {
		t.Fatalf("expired actor denial audit: %d %v", denies, err)
	}
	var owner string
	if err := conn.QueryRow(context.Background(), `SELECT user_id::text FROM conversation_membership_intervals
 WHERE conversation_id=$1 AND status='active' AND role='owner'`, group.ID).Scan(&owner); err != nil || owner != adminA {
		t.Fatalf("owner changed after actor expired: %s %v", owner, err)
	}
	if _, err := creator.TransferGroupOwner(context.Background(), publisher(), group.ID, ownerTransferRequest(source, target)); err != nil {
		t.Fatalf("administrator target could not be promoted: %v", err)
	}
}

func TestTransferGroupOwnerAuditsRecognizedFrozenActor(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	source := groupIntervalFor(t, conn, group.ID, adminA)
	target := groupIntervalFor(t, conn, group.ID, personA)
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
	if _, err := svc.TransferGroupOwner(context.Background(), publisher(), group.ID, ownerTransferRequest(source, target)); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("frozen actor transferred ownership: %v", err)
	}
	var denies int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM audit_events
 WHERE action='group_owner_transfer' AND outcome='deny' AND reason='invalid_identity'`).Scan(&denies); err != nil || denies != 1 {
		t.Fatalf("frozen actor denial audit: %d %v", denies, err)
	}
}

func TestTransferGroupOwnerAllowsFormerOriginWithNewActingMembership(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	source := groupIntervalFor(t, conn, group.ID, adminA)
	target := groupIntervalFor(t, conn, group.ID, personA)
	newMembership := "00000000-0000-4000-8000-000000000906"
	run(t, conn, `INSERT INTO user_organizations
 (id,tenant_id,user_id,organization_id,effective_from,is_primary)
 VALUES ($1,$2,$3,$4,'2020-01-01',false)`, newMembership, tenantA, adminA, orgA2)
	run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at, adminM)
	acting := publisher()
	acting.ActingMembershipID = newMembership
	if _, err := svc.TransferGroupOwner(context.Background(), acting, group.ID, ownerTransferRequest(source, target)); err != nil {
		t.Fatalf("owner locked in group after original membership ended: %v", err)
	}
}

func TestTransferGroupOwnerSerializesWithTargetLeave(t *testing.T) {
	first := db(t)
	seed(t, first)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	svc := policystore.Service{DB: first, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(ctx, publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	source := groupIntervalFor(t, first, group.ID, adminA)
	target := groupIntervalFor(t, first, group.ID, personA)
	var searchPath string
	if err := first.QueryRow(ctx, "SHOW search_path").Scan(&searchPath); err != nil {
		t.Fatal(err)
	}
	second, err := pgx.Connect(ctx, os.Getenv("IM_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close(context.Background()) })
	if _, err := second.Exec(ctx, "SET search_path TO "+searchPath); err != nil {
		t.Fatal(err)
	}
	type result struct {
		operation string
		err       error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	go func() {
		ready.Done()
		<-start
		_, err := (policystore.Service{DB: first, Now: func() time.Time { return at }}).
			TransferGroupOwner(ctx, publisher(), group.ID, ownerTransferRequest(source, target))
		results <- result{"transfer", err}
	}()
	go func() {
		ready.Done()
		<-start
		_, err := (policystore.Service{DB: second, Now: func() time.Time { return at }}).
			LeaveGroup(ctx, groupMemberIdentity(), group.ID, target)
		results <- result{"leave", err}
	}()
	ready.Wait()
	close(start)
	a, b := <-results, <-results
	for _, outcome := range []result{a, b} {
		switch outcome.operation {
		case "transfer":
			if outcome.err != nil && !errors.Is(outcome.err, policystore.ErrGroupNotAvailable) {
				t.Fatalf("transfer failed unexpectedly: %v", outcome.err)
			}
		case "leave":
			if outcome.err != nil && !errors.Is(outcome.err, policystore.ErrGroupOwnerTransferRequired) {
				t.Fatalf("leave failed unexpectedly: %v", outcome.err)
			}
		}
	}
	if (a.err == nil) == (b.err == nil) {
		t.Fatalf("both concurrent operations had same outcome: %+v %+v", a, b)
	}
	var owners int
	if err := first.QueryRow(ctx, `SELECT count(*) FROM conversation_membership_intervals
 WHERE conversation_id=$1 AND role='owner' AND status='active'`, group.ID).Scan(&owners); err != nil || owners != 1 {
		t.Fatalf("concurrent transfer owner count: %d %v", owners, err)
	}
}
