package policystore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func groupMemberIdentity() access.TrustedIdentity {
	return access.TrustedIdentity{TenantID: tenantA, UserID: personA, ActingMembershipID: targetM2}
}

func TestGroupMembershipAndLeaveGroupRetainIntervalAcrossRejoin(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	id := groupMemberIdentity()
	membership, err := svc.GetOwnGroupMembership(context.Background(), id, group.ID)
	if err != nil || membership.IntervalID == "" || membership.Role != "member" || membership.JoinSeq != 1 || membership.GroupStatus != "active" {
		t.Fatalf("own group interval: %+v %v", membership, err)
	}
	run(t, conn, "UPDATE conversations SET last_seq=3 WHERE id=$1", group.ID)
	left, err := svc.LeaveGroup(context.Background(), id, group.ID, membership.IntervalID)
	if err != nil || left.IntervalID != membership.IntervalID || left.Status != "left" || left.LeaveSeq != 3 {
		t.Fatalf("leave interval: %+v %v", left, err)
	}
	if _, err := svc.GetOwnGroupMembership(context.Background(), id, group.ID); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("left member remains active: %v", err)
	}
	var lastSeq int64
	if err := conn.QueryRow(context.Background(), "SELECT last_seq FROM conversations WHERE id=$1", group.ID).Scan(&lastSeq); err != nil || lastSeq != 3 {
		t.Fatalf("leave advanced message sequence: %d %v", lastSeq, err)
	}
	run(t, conn, `INSERT INTO conversation_membership_intervals
 (tenant_id,conversation_id,user_id,source_membership_id,source_organization_id,source_legal_entity_id,
  role,status,join_seq,joined_policy_version,joined_at)
 VALUES ($1,$2,$3,$4,$5,$6,'member','active',4,0,$7)`, tenantA, group.ID, personA, targetM2, orgA, legalA, at)
	replay, err := svc.LeaveGroup(context.Background(), id, group.ID, membership.IntervalID)
	if err != nil || replay != left {
		t.Fatalf("old interval replay: %+v %v", replay, err)
	}
	current, err := svc.GetOwnGroupMembership(context.Background(), id, group.ID)
	if err != nil || current.IntervalID == membership.IntervalID || current.JoinSeq != 4 {
		t.Fatalf("rejoined interval changed by old request: %+v %v", current, err)
	}
	var audits int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='group_leave' AND outcome='allow'").Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("leave audit count: %d %v", audits, err)
	}
}

func TestLeaveGroupRejectsOwnerAndOtherMemberInterval(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := svc.GetOwnGroupMembership(context.Background(), publisher(), group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LeaveGroup(context.Background(), publisher(), group.ID, owner.IntervalID); !errors.Is(err, policystore.ErrGroupOwnerTransferRequired) {
		t.Fatalf("owner left group: %v", err)
	}
	if _, err := svc.LeaveGroup(context.Background(), groupMemberIdentity(), group.ID, owner.IntervalID); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("other interval accessible: %v", err)
	}
	if _, err := svc.GetOwnGroupMembership(context.Background(), access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}, group.ID); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("cross-tenant group visible: %v", err)
	}
}

func TestLeaveGroupAllowsPolicyBlockedAndOtherCurrentMembership(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", group.ID)
	id := groupMemberIdentity()
	id.ActingMembershipID = targetM
	membership, err := svc.GetOwnGroupMembership(context.Background(), id, group.ID)
	if err != nil || membership.GroupStatus != "policy_blocked" {
		t.Fatalf("blocked group own interval: %+v %v", membership, err)
	}
	if _, err := svc.LeaveGroup(context.Background(), id, group.ID, membership.IntervalID); err != nil {
		t.Fatalf("blocked group leave: %v", err)
	}
}

func TestLeaveGroupAuditFailureRollsBack(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	membership, err := svc.GetOwnGroupMembership(context.Background(), groupMemberIdentity(), group.ID)
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, `CREATE FUNCTION fail_group_leave_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.action='group_leave' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END $$`)
	run(t, conn, `CREATE TRIGGER fail_group_leave_audit BEFORE INSERT ON audit_events
 FOR EACH ROW EXECUTE FUNCTION fail_group_leave_audit()`)
	if _, err := svc.LeaveGroup(context.Background(), groupMemberIdentity(), group.ID, membership.IntervalID); !errors.Is(err, policystore.ErrAuditUnavailable) {
		t.Fatalf("expected audit failure: %v", err)
	}
	current, err := svc.GetOwnGroupMembership(context.Background(), groupMemberIdentity(), group.ID)
	if err != nil || current.IntervalID != membership.IntervalID {
		t.Fatalf("leave committed without audit: %+v %v", current, err)
	}
}

func TestLeaveGroupRejectsMembershipExpiringDuringTransaction(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	creator := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := creator.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	membership, err := creator.GetOwnGroupMembership(context.Background(), groupMemberIdentity(), group.ID)
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at.Add(time.Second), targetM2)
	checks := 0
	svc := policystore.Service{DB: conn, Now: func() time.Time {
		checks++
		if checks == 1 {
			return at
		}
		return at.Add(2 * time.Second)
	}}
	if _, err := svc.LeaveGroup(context.Background(), groupMemberIdentity(), group.ID, membership.IntervalID); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("expired membership could leave: %v", err)
	}
	current, err := creator.GetOwnGroupMembership(context.Background(), groupMemberIdentity(), group.ID)
	if err != nil || current.IntervalID != membership.IntervalID {
		t.Fatalf("interval changed: %+v %v", current, err)
	}
}
