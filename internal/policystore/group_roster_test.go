package policystore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestGroupRosterPagesActiveIntervalsForOwner(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2, groupMemberC))
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", group.ID)
	first, err := svc.ListGroupMembers(context.Background(), publisher(), group.ID, "", 2)
	if err != nil || len(first.Members) != 2 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first roster page: %+v %v", first, err)
	}
	second, err := svc.ListGroupMembers(context.Background(), publisher(), group.ID, first.NextCursor, 2)
	if err != nil || len(second.Members) != 1 || second.HasMore || second.NextCursor != "" {
		t.Fatalf("second roster page: %+v %v", second, err)
	}
	got := map[string]policystore.GroupRosterMember{}
	for _, member := range append(first.Members, second.Members...) {
		got[member.DisplayName] = member
	}
	if len(got) != 3 || got["管理员"].Role != "owner" || got["管理员"].OrganizationName != "公司 A" ||
		got["用户 A"].Role != "member" || got["用户 C"].Role != "member" || got["用户 A"].IntervalID == "" {
		t.Fatalf("roster entries: %+v", got)
	}
	var audits int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='group_roster_list' AND outcome='allow'").Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("roster audit count: %d %v", audits, err)
	}
}

func TestGroupRosterRejectsMemberAndOutsider(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ListGroupMembers(context.Background(), groupMemberIdentity(), group.ID, "", 20); !errors.Is(err, policystore.ErrGroupRosterPermissionDenied) {
		t.Fatalf("ordinary member saw roster: %v", err)
	}
	run(t, conn, "UPDATE conversation_membership_intervals SET role='admin' WHERE conversation_id=$1 AND user_id=$2 AND status='active'", group.ID, personA)
	adminPage, err := svc.ListGroupMembers(context.Background(), groupMemberIdentity(), group.ID, "", 20)
	if err != nil || len(adminPage.Members) != 2 {
		t.Fatalf("group administrator cannot view roster: %+v %v", adminPage, err)
	}
	foreign := access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}
	if _, err := svc.ListGroupMembers(context.Background(), foreign, group.ID, "", 20); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("other tenant saw roster: %v", err)
	}
	for _, limit := range []int{0, 51} {
		if _, err := svc.ListGroupMembers(context.Background(), publisher(), group.ID, "", limit); !errors.Is(err, policystore.ErrInvalidGroupRosterRequest) {
			t.Fatalf("bad limit %d: %v", limit, err)
		}
	}
	if _, err := svc.ListGroupMembers(context.Background(), publisher(), group.ID, "bad cursor", 20); !errors.Is(err, policystore.ErrInvalidGroupRosterRequest) {
		t.Fatalf("bad cursor: %v", err)
	}
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
	if _, err := svc.ListGroupMembers(context.Background(), publisher(), group.ID, "", 20); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("frozen owner saw roster: %v", err)
	}
}

func TestGroupRosterOmitsClosedIntervalAndFailsClosedOnAudit(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	old := groupIntervalFor(t, conn, group.ID, personA)
	if _, err := svc.LeaveGroup(context.Background(), groupMemberIdentity(), group.ID, old); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ListGroupMembers(context.Background(), groupMemberIdentity(), group.ID, "", 20); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("departed member accessed roster: %v", err)
	}
	page, err := svc.ListGroupMembers(context.Background(), publisher(), group.ID, "", 20)
	if err != nil || len(page.Members) != 1 || page.Members[0].Role != "owner" {
		t.Fatalf("departed member listed: %+v %v", page, err)
	}
	run(t, conn, `INSERT INTO conversation_membership_intervals
 (tenant_id,conversation_id,user_id,source_membership_id,source_organization_id,source_legal_entity_id,
  role,status,join_seq,joined_policy_version,joined_at)
 VALUES ($1,$2,$3,$4,$5,$6,'member','active',1,0,$7)`, tenantA, group.ID, personA, targetM2, orgA, legalA, at.Add(time.Second))
	page, err = svc.ListGroupMembers(context.Background(), publisher(), group.ID, "", 20)
	if err != nil || len(page.Members) != 2 || page.Members[0].IntervalID == old || page.Members[1].IntervalID == old {
		t.Fatalf("old interval reappeared: %+v %v", page, err)
	}
	run(t, conn, "ALTER TABLE audit_events ADD CONSTRAINT group_roster_audit_disabled CHECK (action <> 'group_roster_list') NOT VALID")
	if _, err := svc.ListGroupMembers(context.Background(), publisher(), group.ID, "", 20); !errors.Is(err, policystore.ErrAuditUnavailable) {
		t.Fatalf("audit failure exposed roster: %v", err)
	}
}
