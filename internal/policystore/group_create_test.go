package policystore_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func seedThirdGroupMember(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	run(t, conn, "INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,'A003','用户 C')", groupUserC, tenantA)
	run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01')", groupMemberC, tenantA, groupUserC, orgA)
}

func createGroupRequest(members ...string) policystore.CreateGroupRequest {
	return policystore.CreateGroupRequest{ClientRequestID: groupRequestID, Name: "  项目群  ", MemberMembershipIDs: members}
}

func TestCreateGroupCommitsAllMembersAndReplaysSameRequest(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2, groupMemberC))
	if err != nil || group.ID == "" || !group.Created || group.LastSeq != 0 || group.PolicyVersion != 0 || group.MemberCount != 3 {
		t.Fatalf("create group: %+v %v", group, err)
	}
	var name, creatorMember string
	var count, owners, audits, decisions int
	if err := conn.QueryRow(context.Background(), `SELECT group_name,group_creator_membership_id::text FROM conversations WHERE id=$1`, group.ID).Scan(&name, &creatorMember); err != nil || name != "项目群" || creatorMember != adminM {
		t.Fatalf("group snapshot: %q %q %v", name, creatorMember, err)
	}
	if err := conn.QueryRow(context.Background(), `SELECT count(*),count(*) FILTER (WHERE role='owner')
 FROM conversation_membership_intervals WHERE conversation_id=$1 AND join_seq=1 AND leave_seq IS NULL
 AND joined_policy_version=0`, group.ID).Scan(&count, &owners); err != nil || count != 3 || owners != 1 {
		t.Fatalf("group intervals: %d %d %v", count, owners, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE action='create_group'").Scan(&decisions); err != nil || decisions != 6 {
		t.Fatalf("pairwise decisions: %d %v", decisions, err)
	}
	replay, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(groupMemberC, targetM2))
	if err != nil || replay.ID != group.ID || replay.Created {
		t.Fatalf("idempotent replay: %+v %v", replay, err)
	}
	conflict := createGroupRequest(targetM2)
	if _, err := svc.CreateGroup(context.Background(), publisher(), conflict); !errors.Is(err, policystore.ErrGroupRequestConflict) {
		t.Fatalf("changed payload should conflict: %v", err)
	}
	otherCreatorMembership := "00000000-0000-4000-8000-000000000854"
	run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01')", otherCreatorMembership, tenantA, adminA, orgA2)
	otherIdentity := publisher()
	otherIdentity.ActingMembershipID = otherCreatorMembership
	if _, err := svc.CreateGroup(context.Background(), otherIdentity, createGroupRequest(targetM2, groupMemberC)); !errors.Is(err, policystore.ErrGroupRequestConflict) {
		t.Fatalf("different creator membership should conflict: %v", err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversations WHERE kind='group'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate groups: %d %v", count, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='group_create' AND outcome='allow'").Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("group allow audits: %d %v", audits, err)
	}
}

func TestCreateGroupRejectsCrossOrganizationAndNonCreatorHardDeny(t *testing.T) {
	for _, scenario := range []string{"cross_org", "member_pair_hard_deny"} {
		t.Run(scenario, func(t *testing.T) {
			conn := db(t)
			seed(t, conn)
			seedThirdGroupMember(t, conn)
			members := []string{targetM2, groupMemberC}
			if scenario == "cross_org" {
				run(t, conn, "UPDATE user_organizations SET organization_id=$1 WHERE id=$2", orgA2, groupMemberC)
			}
			if scenario == "member_pair_hard_deny" {
				run(t, conn, "INSERT INTO policy_versions (tenant_id,version,status,published_by_user_id,reason) VALUES ($1,1,'draft',$2,'限制建群')", tenantA, adminA)
				run(t, conn, `INSERT INTO policy_rules
 (tenant_id,version,rule_id,effect,action,source_organization_id,target_organization_id,
  source_membership_id,target_membership_id,reason,effective_from)
 VALUES ($1,1,'pair-deny','hard_deny','create_group',$2,$2,$3,$4,'成员间禁止',$5)`,
					tenantA, orgA, targetM2, groupMemberC, at.Add(-time.Hour))
				run(t, conn, "UPDATE policy_versions SET status='published',published_at=$2 WHERE tenant_id=$1 AND version=1", tenantA, at)
				run(t, conn, "INSERT INTO policy_current (tenant_id,current_version) VALUES ($1,1)", tenantA)
			}
			svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
			if _, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(members...)); !errors.Is(err, policystore.ErrGroupNotAvailable) {
				t.Fatalf("denied group: %v", err)
			}
			var groups, intervals, denies, decisions int
			if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversations WHERE kind='group'").Scan(&groups); err != nil || groups != 0 {
				t.Fatalf("group partially created: %d %v", groups, err)
			}
			if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversation_membership_intervals").Scan(&intervals); err != nil || intervals != 0 {
				t.Fatalf("interval partially created: %d %v", intervals, err)
			}
			if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='group_create' AND outcome='deny'").Scan(&denies); err != nil || denies != 1 {
				t.Fatalf("denial not audited: %d %v", denies, err)
			}
			if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE action='create_group'").Scan(&decisions); err != nil || decisions != 6 {
				t.Fatalf("incomplete pairwise decisions: %d %v", decisions, err)
			}
		})
	}
}

func TestCreateGroupAuditFailureRollsBack(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, `CREATE FUNCTION fail_group_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.action='group_create' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END $$`)
	run(t, conn, `CREATE TRIGGER fail_group_audit BEFORE INSERT ON audit_events
 FOR EACH ROW EXECUTE FUNCTION fail_group_audit()`)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if _, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2)); !errors.Is(err, policystore.ErrAuditUnavailable) {
		t.Fatalf("expected audit failure: %v", err)
	}
	var groups, decisions int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversations WHERE kind='group'").Scan(&groups); err != nil || groups != 0 {
		t.Fatalf("group committed without audit: %d %v", groups, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE action='create_group'").Scan(&decisions); err != nil || decisions != 0 {
		t.Fatalf("decision audit partially committed: %d %v", decisions, err)
	}
}

func TestCreateGroupCrossLegalRequiresPublishedApproval(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "INSERT INTO legal_entities (id,tenant_id,code,name) VALUES ($1,$2,'c','法人 C')", directLegalC, tenantA)
	run(t, conn, "INSERT INTO organizations (id,tenant_id,legal_entity_id,org_type,code,name) VALUES ($1,$2,$3,'company','c','公司 C')", directOrgC, tenantA, directLegalC)
	run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01')", directMemberC, tenantA, personA, directOrgC)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	rule := policy.Rule{ID: "cross-legal-create", TenantID: tenantA, Effect: policy.EffectAllow,
		Action: policy.ActionCreateGroup, SourceOrganizationID: orgA, TargetOrganizationID: directOrgC,
		Bidirectional: true, RequestedBy: adminA, ApprovedBy: adminA, Reason: "跨法人项目群",
		EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{rule}, "建群审批待补全"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(directMemberC)); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("cross-legal group without approval: %v", err)
	}
	var reason string
	if err := conn.QueryRow(context.Background(), `SELECT reason FROM audit_events
 WHERE action='group_create' AND outcome='deny' ORDER BY id DESC LIMIT 1`).Scan(&reason); err != nil || reason != "cross_legal_approval_required" {
		t.Fatalf("cross-legal denial reason: %q %v", reason, err)
	}
	rule.CrossLegalApproved = true
	if _, err := svc.Publish(context.Background(), publisher(), 1, []policy.Rule{rule}, "批准跨法人建群"); err != nil {
		t.Fatal(err)
	}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(directMemberC))
	if err != nil || group.ID == "" || !group.Created || group.PolicyVersion != 2 {
		t.Fatalf("approved cross-legal group: %+v %v", group, err)
	}
}

func TestCreateGroupRejectsInvalidIdentityAndUnavailableMembers(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if _, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2, targetM2)); !errors.Is(err, policystore.ErrInvalidGroupRequest) {
		t.Fatalf("duplicate membership ID: %v", err)
	}
	if _, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2, targetM)); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("same user via two memberships: %v", err)
	}
	for _, target := range []string{otherM, "00000000-0000-4000-8000-000000000899"} {
		if _, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(target)); !errors.Is(err, policystore.ErrGroupNotAvailable) {
			t.Fatalf("unavailable target %s: %v", target, err)
		}
	}
	wrongActor := publisher()
	wrongActor.UserID = personA
	if _, err := svc.CreateGroup(context.Background(), wrongActor, createGroupRequest(targetM2)); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("untrusted actor: %v", err)
	}
	var groups int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversations WHERE kind='group'").Scan(&groups); err != nil || groups != 0 {
		t.Fatalf("invalid request created group: %d %v", groups, err)
	}
}

func TestCreateGroupRechecksExpiringApprovalBeforeWrite(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "INSERT INTO legal_entities (id,tenant_id,code,name) VALUES ($1,$2,'c','法人 C')", directLegalC, tenantA)
	run(t, conn, "INSERT INTO organizations (id,tenant_id,legal_entity_id,org_type,code,name) VALUES ($1,$2,$3,'company','c','公司 C')", directOrgC, tenantA, directLegalC)
	run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01')", directMemberC, tenantA, personA, directOrgC)
	grantPublisher(t, conn)
	clockCalls := 0
	svc := policystore.Service{DB: conn, Now: func() time.Time {
		clockCalls++
		if clockCalls < 3 {
			return at
		}
		return at.Add(2 * time.Hour)
	}}
	rule := policy.Rule{ID: "temporary-group-approval", TenantID: tenantA, Effect: policy.EffectAllow,
		Action: policy.ActionCreateGroup, SourceOrganizationID: orgA, TargetOrganizationID: directOrgC,
		Bidirectional: true, RequestedBy: adminA, ApprovedBy: adminA, CrossLegalApproved: true,
		Reason: "临时跨法人群", EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Hour)}
	if _, err := (policystore.Service{DB: conn, Now: func() time.Time { return at }}).
		Publish(context.Background(), publisher(), 0, []policy.Rule{rule}, "临时授权"); err != nil {
		t.Fatal(err)
	}
	clockCalls = 0
	if _, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(directMemberC)); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("expired approval created group: %v", err)
	}
	var groups int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversations WHERE kind='group'").Scan(&groups); err != nil || groups != 0 {
		t.Fatalf("expired approval left group: %d %v", groups, err)
	}
}

func TestCreateGroupConcurrentSameRequestUsesOneConversation(t *testing.T) {
	first := db(t)
	seed(t, first)
	ctx := context.Background()
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
	type result struct {
		group policystore.GroupConversation
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	for _, conn := range []*pgx.Conn{first, second} {
		go func(conn *pgx.Conn) {
			ready.Done()
			<-start
			group, err := (policystore.Service{DB: conn, Now: func() time.Time { return at }}).
				CreateGroup(ctx, publisher(), createGroupRequest(targetM2))
			results <- result{group, err}
		}(conn)
	}
	ready.Wait()
	close(start)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || a.group.ID == "" || a.group.ID != b.group.ID || a.group.Created == b.group.Created {
		t.Fatalf("concurrent request not idempotent: %+v %+v", a, b)
	}
	var count int
	if err := first.QueryRow(ctx, "SELECT count(*) FROM conversations WHERE kind='group'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("concurrent group count: %d %v", count, err)
	}
}
