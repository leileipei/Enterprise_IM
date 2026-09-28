package access_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
)

var fixedTime = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

func identity() access.TrustedIdentity {
	return access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
}

func TestGroupAdminSeesBothCurrentMemberships(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')", "00000000-0000-4000-8000-000000000171", tenantA, adminM, orgA)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	person, err := svc.GetManagedPerson(context.Background(), identity(), personA)
	if err != nil {
		t.Fatal(err)
	}
	if person.ID != personA || person.DisplayName != "双任职人员" || len(person.Memberships) != 2 ||
		person.Memberships[0].OrganizationID != orgA || person.Memberships[1].OrganizationID != orgA2 {
		t.Fatalf("unexpected person: %+v", person)
	}
	var count int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE tenant_id=$1 AND action='directory_view' AND outcome='allow'", tenantA).Scan(&count); err != nil || count != 1 {
		t.Fatalf("allow audit count=%d err=%v", count, err)
	}
}

func TestOrganizationAdminSeesOnlyGrantedOrganizationAndDepartments(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, "INSERT INTO user_departments (id,tenant_id,user_organization_id,organization_id,department_id,effective_from) VALUES ($1,$2,$3,$4,$5,'2020-01-01')", "00000000-0000-4000-8000-000000000181", tenantA, personM, orgA, depA)
	run(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,scope_organization_id,effective_from) VALUES ($1,$2,$3,$4,'organization_admin',$5,'2020-01-01')", "00000000-0000-4000-8000-000000000172", tenantA, adminM, orgA, orgA)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	person, err := svc.GetManagedPerson(context.Background(), identity(), personA)
	if err != nil {
		t.Fatal(err)
	}
	if len(person.Memberships) != 1 || person.Memberships[0].OrganizationID != orgA ||
		len(person.Memberships[0].Departments) != 1 || person.Memberships[0].Departments[0].ID != depA {
		t.Fatalf("scope leaked or department absent: %+v", person)
	}
}

func TestOutOfScopeAndCrossTenantPersonDoNotLeakDetails(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,scope_organization_id,effective_from) VALUES ($1,$2,$3,$4,'organization_admin',$5,'2020-01-01')", "00000000-0000-4000-8000-000000000173", tenantA, adminM, orgA, orgA)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	for _, target := range []string{personB, "00000000-0000-4000-8000-000000000199"} {
		person, err := svc.GetManagedPerson(context.Background(), identity(), target)
		if !errors.Is(err, access.ErrNotFound) || person.ID != "" || person.DisplayName != "" {
			t.Fatalf("target %s leaked: person=%+v err=%v", target, person, err)
		}
	}
	var count int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE tenant_id=$1 AND action='directory_view' AND outcome='deny'", tenantA).Scan(&count); err != nil || count != 2 {
		t.Fatalf("deny audit count=%d err=%v", count, err)
	}
}

func TestInvalidActingMembershipCannotReadPerson(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')", "00000000-0000-4000-8000-000000000174", tenantA, adminM, orgA)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	id := identity()
	id.ActingMembershipID = personM
	if _, err := svc.GetManagedPerson(context.Background(), id, personA); !errors.Is(err, access.ErrInvalidIdentity) {
		t.Fatalf("foreign acting membership accepted: %v", err)
	}
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
	if _, err := svc.GetManagedPerson(context.Background(), identity(), personA); !errors.Is(err, access.ErrInvalidIdentity) {
		t.Fatalf("frozen administrator accepted: %v", err)
	}
}
