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

func groupIntervalFor(t *testing.T, conn *pgx.Conn, groupID, userID string) string {
	t.Helper()
	var intervalID string
	if err := conn.QueryRow(context.Background(), `SELECT id::text FROM conversation_membership_intervals
 WHERE conversation_id=$1 AND user_id=$2 AND status='active'`, groupID, userID).Scan(&intervalID); err != nil {
		t.Fatal(err)
	}
	return intervalID
}

func TestRemoveGroupMemberClosesOnlySpecifiedIntervalAcrossRejoin(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	oldID := groupIntervalFor(t, conn, group.ID, personA)
	run(t, conn, "UPDATE conversations SET last_seq=3 WHERE id=$1", group.ID)
	removed, err := svc.RemoveGroupMember(context.Background(), publisher(), group.ID, oldID)
	if err != nil || removed.IntervalID != oldID || removed.Status != "removed" || removed.LeaveSeq != 3 {
		t.Fatalf("remove: %+v %v", removed, err)
	}
	var lastSeq int64
	if err := conn.QueryRow(context.Background(), "SELECT last_seq FROM conversations WHERE id=$1", group.ID).Scan(&lastSeq); err != nil || lastSeq != 3 {
		t.Fatalf("removal advanced message sequence: %d %v", lastSeq, err)
	}
	run(t, conn, "UPDATE conversations SET last_seq=5 WHERE id=$1", group.ID)
	joined, err := svc.InviteGroupMember(context.Background(), publisher(), group.ID, inviteGroupRequest(targetM2))
	if err != nil || joined.JoinSeq != 6 || joined.IntervalID == oldID {
		t.Fatalf("rejoin: %+v %v", joined, err)
	}
	replay, err := svc.RemoveGroupMember(context.Background(), publisher(), group.ID, oldID)
	if err != nil || replay != removed {
		t.Fatalf("old removal replay: %+v %v", replay, err)
	}
	current, err := svc.GetOwnGroupMembership(context.Background(), groupMemberIdentity(), group.ID)
	if err != nil || current.IntervalID != joined.IntervalID {
		t.Fatalf("new interval changed: %+v %v", current, err)
	}
	var audits int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='group_remove' AND outcome='allow'").Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("duplicate removal audit: %d %v", audits, err)
	}
}

func TestRemoveGroupMemberEnforcesRolesAndHidesUnrelatedIntervals(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2, groupMemberC))
	if err != nil {
		t.Fatal(err)
	}
	ownerID := groupIntervalFor(t, conn, group.ID, adminA)
	memberID := groupIntervalFor(t, conn, group.ID, personA)
	thirdID := groupIntervalFor(t, conn, group.ID, groupUserC)
	member := groupMemberIdentity()
	if _, err := svc.RemoveGroupMember(context.Background(), member, group.ID, thirdID); !errors.Is(err, policystore.ErrGroupRemovePermissionDenied) {
		t.Fatalf("ordinary member removed another: %v", err)
	}
	run(t, conn, "UPDATE conversation_membership_intervals SET role='admin' WHERE id=$1", memberID)
	if _, err := svc.RemoveGroupMember(context.Background(), member, group.ID, ownerID); !errors.Is(err, policystore.ErrGroupOwnerTransferRequired) {
		t.Fatalf("administrator removed owner: %v", err)
	}
	if _, err := svc.RemoveGroupMember(context.Background(), member, group.ID, memberID); !errors.Is(err, policystore.ErrGroupRemovePermissionDenied) {
		t.Fatalf("administrator removed self: %v", err)
	}
	run(t, conn, "UPDATE conversation_membership_intervals SET role='admin' WHERE id=$1", thirdID)
	if _, err := svc.RemoveGroupMember(context.Background(), member, group.ID, thirdID); !errors.Is(err, policystore.ErrGroupRemovePermissionDenied) {
		t.Fatalf("administrator removed peer administrator: %v", err)
	}
	if _, err := svc.RemoveGroupMember(context.Background(), publisher(), group.ID, thirdID); err != nil {
		t.Fatalf("owner could not remove administrator: %v", err)
	}
	if _, err := svc.RemoveGroupMember(context.Background(), member, group.ID, thirdID); !errors.Is(err, policystore.ErrGroupRemovePermissionDenied) {
		t.Fatalf("administrator replayed another's removal: %v", err)
	}
	if _, err := svc.RemoveGroupMember(context.Background(), access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}, group.ID, memberID); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("cross-tenant interval visible: %v", err)
	}
	secondRequest := createGroupRequest(groupMemberC)
	secondRequest.ClientRequestID = "00000000-0000-4000-8000-000000000891"
	secondGroup, err := svc.CreateGroup(context.Background(), publisher(), secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	otherInterval := groupIntervalFor(t, conn, secondGroup.ID, groupUserC)
	if _, err := svc.RemoveGroupMember(context.Background(), publisher(), group.ID, otherInterval); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("other group's interval visible: %v", err)
	}
}

func TestRemoveGroupMemberAllowsBlockedGroupAndAdministratorRemovingMember(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2, groupMemberC))
	if err != nil {
		t.Fatal(err)
	}
	memberID := groupIntervalFor(t, conn, group.ID, personA)
	targetID := groupIntervalFor(t, conn, group.ID, groupUserC)
	run(t, conn, "UPDATE conversation_membership_intervals SET role='admin' WHERE id=$1", memberID)
	run(t, conn, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", group.ID)
	if _, err := svc.RemoveGroupMember(context.Background(), groupMemberIdentity(), group.ID, targetID); err != nil {
		t.Fatalf("admin removal in blocked group: %v", err)
	}
	var status string
	if err := conn.QueryRow(context.Background(), "SELECT status FROM conversations WHERE id=$1", group.ID).Scan(&status); err != nil || status != "policy_blocked" {
		t.Fatalf("group restored without full policy recheck: %s %v", status, err)
	}
}

func TestRemoveGroupMemberAuditFailureAndExpiredActorRollBack(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	creator := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := creator.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	targetID := groupIntervalFor(t, conn, group.ID, personA)
	run(t, conn, `CREATE FUNCTION fail_remove_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.action='group_remove' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END $$`)
	run(t, conn, `CREATE TRIGGER fail_remove_audit BEFORE INSERT ON audit_events
 FOR EACH ROW EXECUTE FUNCTION fail_remove_audit()`)
	if _, err := creator.RemoveGroupMember(context.Background(), publisher(), group.ID, targetID); !errors.Is(err, policystore.ErrAuditUnavailable) {
		t.Fatalf("expected audit failure: %v", err)
	}
	var status string
	if err := conn.QueryRow(context.Background(), "SELECT status FROM conversation_membership_intervals WHERE id=$1", targetID).Scan(&status); err != nil || status != "active" {
		t.Fatalf("removal committed without audit: %s %v", status, err)
	}
	run(t, conn, "DROP TRIGGER fail_remove_audit ON audit_events")
	run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at.Add(time.Second), adminM)
	checks := 0
	svc := policystore.Service{DB: conn, Now: func() time.Time {
		checks++
		if checks == 1 {
			return at
		}
		return at.Add(2 * time.Second)
	}}
	if _, err := svc.RemoveGroupMember(context.Background(), publisher(), group.ID, targetID); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("expired actor removed member: %v", err)
	}
	checks = 0
	svc.Now = func() time.Time {
		checks++
		if checks < 3 {
			return at
		}
		return at.Add(2 * time.Second)
	}
	if _, err := svc.RemoveGroupMember(context.Background(), publisher(), group.ID, targetID); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("actor expired while target interval was locked: %v", err)
	}
}

func TestRemoveGroupMemberAndSelfLeaveSerializeOnGroup(t *testing.T) {
	first := db(t)
	seed(t, first)
	ctx := context.Background()
	svc := policystore.Service{DB: first, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(ctx, publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	targetID := groupIntervalFor(t, first, group.ID, personA)
	var searchPath string
	if err := first.QueryRow(ctx, "SHOW search_path").Scan(&searchPath); err != nil {
		t.Fatal(err)
	}
	second, err := pgx.Connect(ctx, os.Getenv("IM_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close(ctx) })
	if _, err := second.Exec(ctx, "SET search_path TO "+searchPath); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	go func() {
		ready.Done()
		<-start
		_, err := (policystore.Service{DB: first, Now: func() time.Time { return at }}).
			RemoveGroupMember(ctx, publisher(), group.ID, targetID)
		results <- err
	}()
	go func() {
		ready.Done()
		<-start
		_, err := (policystore.Service{DB: second, Now: func() time.Time { return at }}).
			LeaveGroup(ctx, groupMemberIdentity(), group.ID, targetID)
		results <- err
	}()
	ready.Wait()
	close(start)
	a, b := <-results, <-results
	if (a == nil) == (b == nil) || (a != nil && !errors.Is(a, policystore.ErrGroupNotAvailable)) ||
		(b != nil && !errors.Is(b, policystore.ErrGroupNotAvailable)) {
		t.Fatalf("concurrent remove/leave: %v %v", a, b)
	}
	var status string
	if err := first.QueryRow(ctx, "SELECT status FROM conversation_membership_intervals WHERE id=$1", targetID).Scan(&status); err != nil || (status != "left" && status != "removed") {
		t.Fatalf("concurrent final interval: %s %v", status, err)
	}
}
