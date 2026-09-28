package policystore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestDirectorySameOrganizationReturnsOneMembershipAndAudits(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	departmentID := "00000000-0000-4000-8000-000000000261"
	run(t, conn, "INSERT INTO departments (id,tenant_id,organization_id,code,name) VALUES ($1,$2,$3,'ops','运营部')", departmentID, tenantA, orgA)
	run(t, conn, "INSERT INTO user_departments (id,tenant_id,user_organization_id,organization_id,department_id,effective_from) VALUES ($1,$2,$3,$4,$5,'2020-01-01')", "00000000-0000-4000-8000-000000000262", tenantA, targetM2, orgA, departmentID)
	run(t, conn, "INSERT INTO departments (id,tenant_id,organization_id,code,name,status) VALUES ($1,$2,$3,'hidden','停用部门','disabled')", "00000000-0000-4000-8000-000000000263", tenantA, orgA)
	run(t, conn, "INSERT INTO user_departments (id,tenant_id,user_organization_id,organization_id,department_id,effective_from) VALUES ($1,$2,$3,$4,$5,'2020-01-01')", "00000000-0000-4000-8000-000000000264", tenantA, targetM2, orgA, "00000000-0000-4000-8000-000000000263")
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	profile, err := svc.GetVisibleMembership(context.Background(), publisher(), targetM2)
	if err != nil || profile.UserID != personA || profile.DisplayName != "用户 A" || profile.EmployeeNo != "A002" ||
		profile.MembershipID != targetM2 || profile.OrganizationID != orgA || len(profile.Departments) != 1 ||
		profile.Departments[0].ID != departmentID {
		t.Fatalf("profile: %+v err=%v", profile, err)
	}
	var allowed bool
	var reason string
	if err := conn.QueryRow(context.Background(), "SELECT allowed,reason FROM policy_decision_events WHERE target_membership_id=$1 ORDER BY id DESC LIMIT 1", targetM2).Scan(&allowed, &reason); err != nil || !allowed || reason != string(policy.ReasonAllowedSameOrganization) {
		t.Fatalf("decision audit: %t %s %v", allowed, reason, err)
	}
}

func TestDirectoryCrossOrganizationNeedsPublishedAllowAndHonorsHardDeny(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if profile, err := svc.GetVisibleMembership(context.Background(), publisher(), targetM); !errors.Is(err, policystore.ErrDirectoryNotVisible) || profile.UserID != "" {
		t.Fatalf("cross organization visible by default: %+v %v", profile, err)
	}
	allow := policy.Rule{ID: "directory-allow", TenantID: tenantA, Effect: policy.EffectAllow,
		Action: policy.ActionDirectoryView, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		RequestedBy: adminA, ApprovedBy: adminA, Reason: "跨组织通讯录",
		EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{allow}, "开放目录"); err != nil {
		t.Fatal(err)
	}
	profile, err := svc.GetVisibleMembership(context.Background(), publisher(), targetM)
	if err != nil || profile.MembershipID != targetM || profile.OrganizationID != orgA2 {
		t.Fatalf("published allow did not open target membership: %+v %v", profile, err)
	}
	hard := policy.Rule{ID: "directory-hard-deny", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionDirectoryView, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		Reason: "集团强制隔离", EffectiveFrom: at.Add(-time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 1, []policy.Rule{allow, hard}, "隔离目录"); err != nil {
		t.Fatal(err)
	}
	if profile, err := svc.GetVisibleMembership(context.Background(), publisher(), targetM); !errors.Is(err, policystore.ErrDirectoryNotVisible) || profile.UserID != "" {
		t.Fatalf("hard deny bypassed: %+v %v", profile, err)
	}
}

func TestDirectoryHidesInactiveAndCrossTenantTargetsAndRejectsActor(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	for _, target := range []string{otherM, "00000000-0000-4000-8000-000000000299"} {
		if profile, err := svc.GetVisibleMembership(context.Background(), publisher(), target); !errors.Is(err, policystore.ErrDirectoryNotVisible) || profile.UserID != "" {
			t.Fatalf("hidden target %s: %+v %v", target, profile, err)
		}
	}
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", personA)
	if profile, err := svc.GetVisibleMembership(context.Background(), publisher(), targetM2); !errors.Is(err, policystore.ErrDirectoryNotVisible) || profile.UserID != "" {
		t.Fatalf("frozen target visible: %+v %v", profile, err)
	}
	badActor := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: targetM2}
	if _, err := svc.GetVisibleMembership(context.Background(), badActor, targetM2); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("foreign acting membership accepted: %v", err)
	}
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
	if _, err := svc.GetVisibleMembership(context.Background(), publisher(), targetM2); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("frozen actor accepted: %v", err)
	}
}

func TestDirectoryAuditFailureNeverReturnsProfile(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "ALTER TABLE policy_decision_events ADD CONSTRAINT reject_allowed_directory CHECK (allowed = false)")
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	profile, err := svc.GetVisibleMembership(context.Background(), publisher(), targetM2)
	if !errors.Is(err, policystore.ErrAuditUnavailable) || profile.UserID != "" {
		t.Fatalf("profile returned without durable audit: %+v %v", profile, err)
	}
}
