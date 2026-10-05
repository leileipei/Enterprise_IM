package policystore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

const (
	orgHiddenUser = "00000000-0000-4000-8000-000000000301"
	orgHiddenMem  = "00000000-0000-4000-8000-000000000302"
	orgShownUser  = "00000000-0000-4000-8000-000000000303"
	orgShownMem   = "00000000-0000-4000-8000-000000000304"
)

func TestOrganizationMembersPageCountsOnlyVisiblePeopleAndScopesAssignments(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,'A003','隐藏人员'),($3,$2,'A004','可见人员')", orgHiddenUser, tenantA, orgShownUser)
	run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01'),($5,$2,$6,$4,'2020-01-01')", orgHiddenMem, tenantA, orgHiddenUser, orgA, orgShownMem, orgShownUser)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	hard := policy.Rule{ID: "hide-one-member", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionDirectoryView, SourceOrganizationID: orgA, TargetOrganizationID: orgA, TargetMembershipID: orgHiddenMem,
		Reason: "隐藏此人", EffectiveFrom: at.Add(-time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{hard}, "组织成员可见性"); err != nil {
		t.Fatal(err)
	}
	page, err := svc.ListVisibleOrganizationMembers(context.Background(), publisher(), orgA, "", 2)
	if err != nil || len(page.People) != 2 || page.People[0].ID != adminA || page.People[1].ID != personA ||
		len(page.People[1].Memberships) != 1 || page.People[1].Memberships[0].MembershipID != targetM2 ||
		!page.HasMore || page.NextAfter != targetM2 {
		t.Fatalf("first visible organization page: %+v %v", page, err)
	}
	page, err = svc.ListVisibleOrganizationMembers(context.Background(), publisher(), orgA, page.NextAfter, 2)
	if err != nil || len(page.People) != 1 || page.People[0].ID != orgShownUser || page.HasMore || page.NextAfter != "" {
		t.Fatalf("second visible organization page: %+v %v", page, err)
	}
	var decisions, requests int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE action='directory_view'").Scan(&decisions); err != nil || decisions != 7 {
		t.Fatalf("candidate and anchor audits: %d %v", decisions, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='directory_organization_members' AND outcome='allow'").Scan(&requests); err != nil || requests != 2 {
		t.Fatalf("organization page audits: %d %v", requests, err)
	}
}

func TestOrganizationMembersHideMissingOrgAndInvisibleAnchors(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	for _, tc := range []struct{ orgID, after string }{
		{orgA2, ""}, {orgB, ""}, {"00000000-0000-4000-8000-000000000399", ""},
		{orgA, targetM}, {orgA, otherM}, {orgA, "00000000-0000-4000-8000-000000000398"},
	} {
		page, err := svc.ListVisibleOrganizationMembers(context.Background(), publisher(), tc.orgID, tc.after, 20)
		if !errors.Is(err, policystore.ErrDirectoryNotVisible) || len(page.People) != 0 {
			t.Fatalf("hidden organization or anchor leaked: %+v %v", page, err)
		}
	}
	page, err := svc.ListVisibleOrganizationMembers(context.Background(), publisher(), orgA, targetM2, 20)
	if err != nil || len(page.People) != 0 || page.HasMore {
		t.Fatalf("valid last anchor should return empty page: %+v %v", page, err)
	}
	run(t, conn, "UPDATE user_organizations SET status='ended' WHERE id=$1", targetM2)
	if page, err := svc.ListVisibleOrganizationMembers(context.Background(), publisher(), orgA, targetM2, 20); !errors.Is(err, policystore.ErrDirectoryNotVisible) || len(page.People) != 0 {
		t.Fatalf("ended anchor accepted: %+v %v", page, err)
	}
}

func TestOrganizationMembersHideFrozenTarget(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", personA)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	page, err := svc.ListVisibleOrganizationMembers(context.Background(), publisher(), orgA, "", 20)
	if err != nil || len(page.People) != 1 || page.People[0].ID != adminA {
		t.Fatalf("frozen target leaked: %+v %v", page, err)
	}
}

func TestOrganizationMembersCandidateLockConflictReturnsNoPartialPageOrAudit(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	ctx := context.Background()
	grantPublisher(t, conn)
	rules := make([]policy.Rule, 0, 50)
	for i := 1; i <= 50; i++ {
		userID := fmt.Sprintf("00000000-0000-4000-8000-%012d", 10000+i)
		membershipID := fmt.Sprintf("00000000-0000-4000-8000-%012d", 20000+i)
		run(t, conn, "INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,$3,'隐藏人员')",
			userID, tenantA, fmt.Sprintf("A001%03d", i))
		run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01')",
			membershipID, tenantA, userID, orgA)
		rules = append(rules, policy.Rule{ID: fmt.Sprintf("hide-%d", i), TenantID: tenantA,
			Effect: policy.EffectHardDeny, Action: policy.ActionDirectoryView,
			SourceOrganizationID: orgA, TargetOrganizationID: orgA, TargetMembershipID: membershipID,
			Reason: "隐藏候选", EffectiveFrom: at.Add(-time.Hour)})
	}
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if _, err := svc.Publish(ctx, publisher(), 0, rules, "隐藏批量候选"); err != nil {
		t.Fatal(err)
	}
	var searchPath string
	if err := conn.QueryRow(ctx, "SHOW search_path").Scan(&searchPath); err != nil {
		t.Fatal(err)
	}
	updater, err := pgx.Connect(ctx, os.Getenv("IM_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { updater.Close(ctx) })
	if _, err := updater.Exec(ctx, "SET search_path TO "+searchPath); err != nil {
		t.Fatal(err)
	}
	updateTx, err := updater.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer updateTx.Rollback(ctx)
	if _, err := updateTx.Exec(ctx, "UPDATE user_organizations SET title='调动中' WHERE id=$1", targetM2); err != nil {
		t.Fatal(err)
	}
	page, err := svc.ListVisibleOrganizationMembers(ctx, publisher(), orgA, "", 1)
	if err == nil || len(page.People) != 0 {
		t.Fatalf("locked candidate yielded partial page: %+v %v", page, err)
	}
	var decisions, requests int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM policy_decision_events WHERE action='directory_view'").Scan(&decisions); err != nil || decisions != 0 {
		t.Fatalf("partial decision audit committed: %d %v", decisions, err)
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action='directory_organization_members'").Scan(&requests); err != nil || requests != 0 {
		t.Fatalf("partial request audit committed: %d %v", requests, err)
	}
}

func TestOrganizationMembersFollowPublishedAllowAndHardDeny(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	allow := policy.Rule{ID: "org-members-allow", TenantID: tenantA, Effect: policy.EffectAllow,
		Action: policy.ActionDirectoryView, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		RequestedBy: adminA, ApprovedBy: adminA, Reason: "跨组织通讯录",
		EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{allow}, "开放组织成员"); err != nil {
		t.Fatal(err)
	}
	page, err := svc.ListVisibleOrganizationMembers(context.Background(), publisher(), orgA2, "", 20)
	if err != nil || len(page.People) != 1 || page.People[0].ID != personA ||
		len(page.People[0].Memberships) != 1 || page.People[0].Memberships[0].MembershipID != targetM {
		t.Fatalf("cross-organization allow not applied: %+v %v", page, err)
	}
	hard := policy.Rule{ID: "org-members-deny", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionDirectoryView, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		Reason: "强制隔离", EffectiveFrom: at.Add(-time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 1, []policy.Rule{allow, hard}, "拒绝组织成员"); err != nil {
		t.Fatal(err)
	}
	if page, err := svc.ListVisibleOrganizationMembers(context.Background(), publisher(), orgA2, "", 20); !errors.Is(err, policystore.ErrDirectoryNotVisible) || len(page.People) != 0 {
		t.Fatalf("hard deny bypassed: %+v %v", page, err)
	}
}

func TestOrganizationMembersRejectInvalidActorAndAuditFailures(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	for _, tc := range []struct {
		orgID, after string
		limit        int
	}{
		{"", "", 20}, {orgA, "bad", 20}, {orgA, "", 0}, {orgA, "", 21},
	} {
		if _, err := svc.ListVisibleOrganizationMembers(context.Background(), publisher(), tc.orgID, tc.after, tc.limit); !errors.Is(err, policystore.ErrInvalidDirectoryPage) {
			t.Fatalf("invalid page accepted: %+v %v", tc, err)
		}
	}
	run(t, conn, "UPDATE user_organizations SET status='ended' WHERE id=$1", adminM)
	if page, err := svc.ListVisibleOrganizationMembers(context.Background(), publisher(), orgA, "", 20); !errors.Is(err, policystore.ErrForbidden) || len(page.People) != 0 {
		t.Fatalf("ended actor accepted: %+v %v", page, err)
	}
	for _, tc := range []struct{ name, constraint string }{
		{"request", "ALTER TABLE audit_events ADD CONSTRAINT reject_org_members CHECK (action <> 'directory_organization_members')"},
		{"decision", "ALTER TABLE policy_decision_events ADD CONSTRAINT reject_org_member_decision CHECK (action <> 'directory_view')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := db(t)
			seed(t, conn)
			run(t, conn, tc.constraint)
			svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
			page, err := svc.ListVisibleOrganizationMembers(context.Background(), publisher(), orgA, "", 20)
			if !errors.Is(err, policystore.ErrAuditUnavailable) || len(page.People) != 0 {
				t.Fatalf("page returned without audit: %+v %v", page, err)
			}
			var decisions int
			if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE action='directory_view'").Scan(&decisions); err != nil || decisions != 0 {
				t.Fatalf("partial decisions committed: %d %v", decisions, err)
			}
		})
	}
}
