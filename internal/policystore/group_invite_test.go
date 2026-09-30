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
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

const (
	groupInviteRequestID  = "00000000-0000-4000-8000-000000000871"
	groupInviteIntervalID = "00000000-0000-4000-8000-000000000872"
)

func TestGroupInvitationMigrationPreservesRecordedRequests(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	insertGroup(t, conn)
	if err := insertInterval(conn, groupInviteIntervalID, tenantA, groupA, personA, targetM2, orgA, legalA, "member", "active", 1, nil); err != nil {
		t.Fatal(err)
	}
	run(t, conn, `INSERT INTO group_invitation_requests
 (tenant_id,group_id,inviter_user_id,request_id,acting_membership_id,target_membership_id,interval_id,request_digest)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, tenantA, groupA, adminA, groupInviteRequestID, adminM, targetM2, groupInviteIntervalID, make([]byte, 32))
	reject(t, conn, `INSERT INTO group_invitation_requests
 (tenant_id,group_id,inviter_user_id,request_id,acting_membership_id,target_membership_id,interval_id,request_digest)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, tenantA, groupA, adminA, groupInviteRequestID, adminM, targetM2, groupInviteIntervalID, make([]byte, 32))
	reject(t, conn, `INSERT INTO group_invitation_requests
 (tenant_id,group_id,inviter_user_id,request_id,acting_membership_id,target_membership_id,interval_id,request_digest)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, tenantA, groupA, adminA,
		"00000000-0000-4000-8000-000000000875", adminM, adminM, groupInviteIntervalID, make([]byte, 32))
	down, err := os.ReadFile("../../db/migrations/000011_group_invitation.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(down)).ReadAll(); err == nil {
		t.Fatal("down removed recorded invitation")
	}
	if _, err := conn.Exec(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM group_invitation_requests").Scan(&count); err != nil || count != 1 {
		t.Fatalf("invitation survived rejected down: %d %v", count, err)
	}
	run(t, conn, "DELETE FROM group_invitation_requests")
	if _, err := conn.PgConn().Exec(context.Background(), string(down)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile("../../db/migrations/000011_group_invitation.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(up)).ReadAll(); err != nil {
		t.Fatal(err)
	}
}

func inviteGroupRequest(target string) policystore.InviteGroupRequest {
	return policystore.InviteGroupRequest{ClientRequestID: groupInviteRequestID, TargetMembershipID: target}
}

func TestInviteGroupMemberCreatesNextIntervalAndReplayNeverRejoins(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, "UPDATE conversations SET last_seq=3 WHERE id=$1", group.ID)
	invited, err := svc.InviteGroupMember(context.Background(), publisher(), group.ID, inviteGroupRequest(groupMemberC))
	if err != nil || !invited.Created || invited.IntervalID == "" || invited.JoinSeq != 4 || invited.PolicyVersion != 0 {
		t.Fatalf("invite: %+v %v", invited, err)
	}
	var count, decisions, audits int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM conversation_membership_intervals
 WHERE conversation_id=$1 AND user_id=$2 AND source_membership_id=$3 AND status='active' AND join_seq=4`, group.ID, groupUserC, groupMemberC).Scan(&count); err != nil || count != 1 {
		t.Fatalf("new interval: %d %v", count, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE action='invite_group'").Scan(&decisions); err != nil || decisions != 4 {
		t.Fatalf("pairwise decisions: %d %v", decisions, err)
	}
	newIdentity := access.TrustedIdentity{TenantID: tenantA, UserID: groupUserC, ActingMembershipID: groupMemberC}
	if _, err := svc.LeaveGroup(context.Background(), newIdentity, group.ID, invited.IntervalID); err != nil {
		t.Fatal(err)
	}
	replay, err := svc.InviteGroupMember(context.Background(), publisher(), group.ID, inviteGroupRequest(groupMemberC))
	if err != nil || replay.IntervalID != invited.IntervalID || replay.JoinSeq != invited.JoinSeq || replay.Created {
		t.Fatalf("replay after leave: %+v %v", replay, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversation_membership_intervals WHERE conversation_id=$1 AND user_id=$2", group.ID, groupUserC).Scan(&count); err != nil || count != 1 {
		t.Fatalf("old request rejoined target: %d %v", count, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='group_invite' AND outcome='allow'").Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("invitation allow audit: %d %v", audits, err)
	}
	run(t, conn, "UPDATE conversations SET last_seq=5 WHERE id=$1", group.ID)
	newRequest := inviteGroupRequest(groupMemberC)
	newRequest.ClientRequestID = "00000000-0000-4000-8000-000000000874"
	rejoined, err := svc.InviteGroupMember(context.Background(), publisher(), group.ID, newRequest)
	if err != nil || !rejoined.Created || rejoined.IntervalID == invited.IntervalID || rejoined.JoinSeq != 6 {
		t.Fatalf("new request after leave: %+v %v", rejoined, err)
	}
	oldReplay, err := svc.InviteGroupMember(context.Background(), publisher(), group.ID, inviteGroupRequest(groupMemberC))
	if err != nil || oldReplay.IntervalID != invited.IntervalID || oldReplay.JoinSeq != 4 || oldReplay.Created {
		t.Fatalf("old request changed rejoined interval: %+v %v", oldReplay, err)
	}
	changed := publisher()
	changed.ActingMembershipID = "00000000-0000-4000-8000-000000000873"
	run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01')", changed.ActingMembershipID, tenantA, adminA, orgA2)
	if _, err := svc.InviteGroupMember(context.Background(), changed, group.ID, inviteGroupRequest(groupMemberC)); !errors.Is(err, policystore.ErrGroupInviteConflict) {
		t.Fatalf("changed acting membership reused request: %v", err)
	}
	var conflicts int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='group_invite' AND outcome='deny' AND reason='idempotency_conflict'").Scan(&conflicts); err != nil || conflicts != 1 {
		t.Fatalf("request conflict not audited: %d %v", conflicts, err)
	}
}

func TestInviteGroupMemberRejectsNonAdminBlockedAndDuplicateUser(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InviteGroupMember(context.Background(), groupMemberIdentity(), group.ID, inviteGroupRequest(groupMemberC)); !errors.Is(err, policystore.ErrGroupInvitePermissionDenied) {
		t.Fatalf("ordinary member invited: %v", err)
	}
	run(t, conn, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", group.ID)
	if _, err := svc.InviteGroupMember(context.Background(), publisher(), group.ID, inviteGroupRequest(groupMemberC)); !errors.Is(err, policystore.ErrGroupPolicyBlocked) {
		t.Fatalf("blocked group invited: %v", err)
	}
	run(t, conn, "UPDATE conversations SET status='active' WHERE id=$1", group.ID)
	if _, err := svc.InviteGroupMember(context.Background(), publisher(), group.ID, inviteGroupRequest(targetM)); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("same user through second membership invited: %v", err)
	}
	if _, err := svc.InviteGroupMember(context.Background(), publisher(), group.ID, inviteGroupRequest(otherM)); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("cross tenant invited: %v", err)
	}
	var denials int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='group_invite' AND outcome='deny'").Scan(&denials); err != nil || denials != 4 {
		t.Fatalf("rejected invitations not audited: %d %v", denials, err)
	}
}

func TestInviteGroupMemberChecksNonInviterPairAndAuditsDenial(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, "INSERT INTO policy_versions (tenant_id,version,status,published_by_user_id,reason) VALUES ($1,1,'draft',$2,'限制邀请')", tenantA, adminA)
	run(t, conn, `INSERT INTO policy_rules
 (tenant_id,version,rule_id,effect,action,source_organization_id,target_organization_id,
  source_membership_id,target_membership_id,reason,effective_from)
 VALUES ($1,1,'member-deny','hard_deny','invite_group',$2,$2,$3,$4,'成员间禁止',$5)`, tenantA, orgA, targetM2, groupMemberC, at.Add(-time.Hour))
	run(t, conn, "UPDATE policy_versions SET status='published',published_at=$2 WHERE tenant_id=$1 AND version=1", tenantA, at)
	run(t, conn, "INSERT INTO policy_current (tenant_id,current_version) VALUES ($1,1)", tenantA)
	if _, err := svc.InviteGroupMember(context.Background(), publisher(), group.ID, inviteGroupRequest(groupMemberC)); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("non-inviter hard deny ignored: %v", err)
	}
	var count, decisions, denies int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversation_membership_intervals WHERE conversation_id=$1 AND user_id=$2", group.ID, groupUserC).Scan(&count); err != nil || count != 0 {
		t.Fatalf("denied invitation created interval: %d %v", count, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE action='invite_group'").Scan(&decisions); err != nil || decisions != 4 {
		t.Fatalf("incomplete decision audit: %d %v", decisions, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='group_invite' AND outcome='deny'").Scan(&denies); err != nil || denies != 1 {
		t.Fatalf("missing deny audit: %d %v", denies, err)
	}
}

func TestInviteGroupMemberAuditFailureRollsBack(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, `CREATE FUNCTION fail_invite_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.action='group_invite' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END $$`)
	run(t, conn, `CREATE TRIGGER fail_invite_audit BEFORE INSERT ON audit_events
 FOR EACH ROW EXECUTE FUNCTION fail_invite_audit()`)
	if _, err := svc.InviteGroupMember(context.Background(), publisher(), group.ID, inviteGroupRequest(groupMemberC)); !errors.Is(err, policystore.ErrAuditUnavailable) {
		t.Fatalf("expected audit failure: %v", err)
	}
	var count int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversation_membership_intervals WHERE conversation_id=$1 AND user_id=$2", group.ID, groupUserC).Scan(&count); err != nil || count != 0 {
		t.Fatalf("interval committed without audit: %d %v", count, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM group_invitation_requests").Scan(&count); err != nil || count != 0 {
		t.Fatalf("request ledger committed without audit: %d %v", count, err)
	}
}

func TestInviteGroupMemberFinalReviewDoesNotDuplicateAllowedDecisions(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	creator := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := creator.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	svc := policystore.Service{DB: conn, Now: func() time.Time { checks++; return at.Add(time.Duration(checks) * time.Millisecond) }}
	if _, err := svc.InviteGroupMember(context.Background(), publisher(), group.ID, inviteGroupRequest(groupMemberC)); err != nil {
		t.Fatal(err)
	}
	var decisions int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE action='invite_group'").Scan(&decisions); err != nil || decisions != 4 {
		t.Fatalf("allowed pairs audited more than once: %d %v", decisions, err)
	}
}

func TestInviteGroupMemberAllowsGroupAdministrator(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, "UPDATE conversation_membership_intervals SET role='admin' WHERE conversation_id=$1 AND user_id=$2", group.ID, personA)
	if _, err := svc.InviteGroupMember(context.Background(), groupMemberIdentity(), group.ID, inviteGroupRequest(groupMemberC)); err != nil {
		t.Fatalf("group administrator cannot invite: %v", err)
	}
}

func TestInviteGroupMemberCrossLegalRequiresPublishedApproval(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	run(t, conn, "INSERT INTO legal_entities (id,tenant_id,code,name) VALUES ($1,$2,'c','法人 C')", directLegalC, tenantA)
	run(t, conn, "INSERT INTO organizations (id,tenant_id,legal_entity_id,org_type,code,name) VALUES ($1,$2,$3,'company','c','公司 C')", directOrgC, tenantA, directLegalC)
	run(t, conn, "UPDATE user_organizations SET organization_id=$1 WHERE id=$2", directOrgC, groupMemberC)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	rule := policy.Rule{ID: "cross-legal-invite", TenantID: tenantA, Effect: policy.EffectAllow,
		Action: policy.ActionInviteGroup, SourceOrganizationID: orgA, TargetOrganizationID: directOrgC,
		Bidirectional: true, RequestedBy: adminA, ApprovedBy: adminA, Reason: "跨法人邀请",
		EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{rule}, "邀请审批待补全"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InviteGroupMember(context.Background(), publisher(), group.ID, inviteGroupRequest(groupMemberC)); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("unapproved cross-legal invite: %v", err)
	}
	rule.CrossLegalApproved = true
	if _, err := svc.Publish(context.Background(), publisher(), 1, []policy.Rule{rule}, "批准跨法人邀请"); err != nil {
		t.Fatal(err)
	}
	invited, err := svc.InviteGroupMember(context.Background(), publisher(), group.ID, inviteGroupRequest(groupMemberC))
	if err != nil || !invited.Created || invited.PolicyVersion != 2 {
		t.Fatalf("approved invite: %+v %v", invited, err)
	}
}

func TestInviteGroupMemberConcurrentSameRequestCreatesOneInterval(t *testing.T) {
	first := db(t)
	seed(t, first)
	seedThirdGroupMember(t, first)
	ctx := context.Background()
	svc := policystore.Service{DB: first, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(ctx, publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
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
	type outcome struct {
		invite policystore.GroupInvitation
		err    error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	for _, conn := range []*pgx.Conn{first, second} {
		go func(conn *pgx.Conn) {
			ready.Done()
			<-start
			result, err := (policystore.Service{DB: conn, Now: func() time.Time { return at }}).
				InviteGroupMember(ctx, publisher(), group.ID, inviteGroupRequest(groupMemberC))
			results <- outcome{result, err}
		}(conn)
	}
	ready.Wait()
	close(start)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || a.invite.IntervalID == "" || a.invite.IntervalID != b.invite.IntervalID || a.invite.Created == b.invite.Created {
		t.Fatalf("concurrent invites: %+v %+v", a, b)
	}
	var count int
	if err := first.QueryRow(ctx, "SELECT count(*) FROM conversation_membership_intervals WHERE conversation_id=$1 AND user_id=$2", group.ID, groupUserC).Scan(&count); err != nil || count != 1 {
		t.Fatalf("concurrent invite count: %d %v", count, err)
	}
}

func TestInviteGroupMemberRejectsRuleExpiringBeforeWrite(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	run(t, conn, "UPDATE user_organizations SET organization_id=$1 WHERE id=$2", orgA2, groupMemberC)
	grantPublisher(t, conn)
	creator := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := creator.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	rule := policy.Rule{ID: "brief-invite", TenantID: tenantA, Effect: policy.EffectAllow,
		Action: policy.ActionInviteGroup, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		Bidirectional: true, RequestedBy: adminA, ApprovedBy: adminA, Reason: "限时邀请",
		EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Second)}
	if _, err := creator.Publish(context.Background(), publisher(), 0, []policy.Rule{rule}, "限时邀请"); err != nil {
		t.Fatal(err)
	}
	checks := 0
	svc := policystore.Service{DB: conn, Now: func() time.Time {
		checks++
		if checks < 3 {
			return at
		}
		return at.Add(2 * time.Second)
	}}
	if _, err := svc.InviteGroupMember(context.Background(), publisher(), group.ID, inviteGroupRequest(groupMemberC)); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("expired rule accepted: %v", err)
	}
	var intervals, denials int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversation_membership_intervals WHERE conversation_id=$1 AND user_id=$2", group.ID, groupUserC).Scan(&intervals); err != nil || intervals != 0 {
		t.Fatalf("expired rule created interval: %d %v", intervals, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='group_invite' AND outcome='deny'").Scan(&denials); err != nil || denials != 1 {
		t.Fatalf("expired rule denial audit: %d %v", denials, err)
	}
}
