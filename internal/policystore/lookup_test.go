package policystore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestDirectoryLookupReturnsOnlyVisibleMembershipsAndAudits(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	person, err := svc.FindVisiblePersonByEmployeeNo(context.Background(), publisher(), " A002 ")
	if err != nil || person.ID != personA || person.DisplayName != "用户 A" || person.EmployeeNo != "A002" ||
		len(person.Memberships) != 1 || person.Memberships[0].MembershipID != targetM2 || person.Memberships[0].OrganizationID != orgA {
		t.Fatalf("filtered person: %+v %v", person, err)
	}
	var decisions, allowed, requests int
	if err := conn.QueryRow(context.Background(), "SELECT count(*), count(*) FILTER (WHERE allowed) FROM policy_decision_events WHERE action='directory_view'").Scan(&decisions, &allowed); err != nil || decisions != 2 || allowed != 1 {
		t.Fatalf("decision audit: %d %d %v", decisions, allowed, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='directory_lookup' AND outcome='allow' AND resource_id IS NULL").Scan(&requests); err != nil || requests != 1 {
		t.Fatalf("lookup audit: %d %v", requests, err)
	}
}

func TestDirectoryLookupPublishedAllowAndHardDenyAffectOnlyMatchingMembership(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	allow := policy.Rule{ID: "lookup-allow", TenantID: tenantA, Effect: policy.EffectAllow,
		Action: policy.ActionDirectoryView, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		RequestedBy: adminA, ApprovedBy: adminA, Reason: "项目目录",
		EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{allow}, "开放项目目录"); err != nil {
		t.Fatal(err)
	}
	person, err := svc.FindVisiblePersonByEmployeeNo(context.Background(), publisher(), "A002")
	if err != nil || len(person.Memberships) != 2 {
		t.Fatalf("published allow not applied: %+v %v", person, err)
	}
	hard := policy.Rule{ID: "lookup-deny", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionDirectoryView, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		Reason: "强制隔离", EffectiveFrom: at.Add(-time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 1, []policy.Rule{allow, hard}, "强制隔离目录"); err != nil {
		t.Fatal(err)
	}
	person, err = svc.FindVisiblePersonByEmployeeNo(context.Background(), publisher(), "A002")
	if err != nil || len(person.Memberships) != 1 || person.Memberships[0].MembershipID != targetM2 {
		t.Fatalf("hard deny bypassed or overapplied: %+v %v", person, err)
	}
}

func TestDirectoryLookupHidesMissingCrossTenantAndFrozenPeople(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	hiddenUser := "00000000-0000-4000-8000-000000000271"
	run(t, conn, "INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,'A003','仅其他组织')", hiddenUser, tenantA)
	run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01')", "00000000-0000-4000-8000-000000000272", tenantA, hiddenUser, orgA2)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	for _, number := range []string{"B001", "UNKNOWN", "A003"} {
		person, err := svc.FindVisiblePersonByEmployeeNo(context.Background(), publisher(), number)
		if !errors.Is(err, policystore.ErrDirectoryNotVisible) || person.ID != "" {
			t.Fatalf("lookup %s leaked: %+v %v", number, person, err)
		}
	}
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", personA)
	if person, err := svc.FindVisiblePersonByEmployeeNo(context.Background(), publisher(), "A002"); !errors.Is(err, policystore.ErrDirectoryNotVisible) || person.ID != "" {
		t.Fatalf("frozen target visible: %+v %v", person, err)
	}
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
	if _, err := svc.FindVisiblePersonByEmployeeNo(context.Background(), publisher(), "A002"); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("frozen actor accepted: %v", err)
	}
	var denied int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='directory_lookup' AND outcome='deny'").Scan(&denied); err != nil || denied != 5 {
		t.Fatalf("deny audit: %d %v", denied, err)
	}
}

func TestDirectoryLookupAuditFailureDoesNotReturnProfile(t *testing.T) {
	for _, tc := range []struct{ name, constraint string }{
		{"request audit", "ALTER TABLE audit_events ADD CONSTRAINT reject_lookup CHECK (action <> 'directory_lookup')"},
		{"decision audit", "ALTER TABLE policy_decision_events ADD CONSTRAINT reject_decision CHECK (action <> 'directory_view')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := db(t)
			seed(t, conn)
			run(t, conn, tc.constraint)
			svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
			person, err := svc.FindVisiblePersonByEmployeeNo(context.Background(), publisher(), "A002")
			if !errors.Is(err, policystore.ErrAuditUnavailable) || person.ID != "" {
				t.Fatalf("profile returned without audit: %+v %v", person, err)
			}
			var decisions, requests int
			if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events").Scan(&decisions); err != nil || decisions != 0 {
				t.Fatalf("partial decisions committed: %d %v", decisions, err)
			}
			if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='directory_lookup'").Scan(&requests); err != nil || requests != 0 {
				t.Fatalf("partial request audit committed: %d %v", requests, err)
			}
		})
	}
}

func TestDirectoryLookupRejectsInvalidEmployeeNumber(t *testing.T) {
	conn := db(t)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	for _, number := range []string{"", " ", "A\x00", string([]byte{'A', 0xff}), strings.Repeat("A", 129)} {
		if _, err := svc.FindVisiblePersonByEmployeeNo(context.Background(), publisher(), number); !errors.Is(err, policystore.ErrInvalidEmployeeNo) {
			t.Fatalf("invalid employee number %q accepted: %v", number, err)
		}
	}
}
