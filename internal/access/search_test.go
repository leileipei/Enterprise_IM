package access_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
)

func TestSearchManagedPeopleFiltersScopeTenantAndDuplicateMemberships(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,scope_organization_id,effective_from) VALUES ($1,$2,$3,$4,'organization_admin',$5,'2020-01-01')", "00000000-0000-4000-8000-000000000191", tenantA, adminM, orgA, orgA)
	run(t, conn, "INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,'A003','仅其他组织')", "00000000-0000-4000-8000-000000000192", tenantA)
	run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01')", "00000000-0000-4000-8000-000000000193", tenantA, "00000000-0000-4000-8000-000000000192", orgA2)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	page, err := svc.SearchManagedPeople(context.Background(), identity(), "A00", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.People) != 2 || page.People[0].ID != adminA || page.People[1].ID != personA || page.People[1].EmployeeNo != "A002" || page.HasMore {
		t.Fatalf("scope or duplicate leak: %+v", page)
	}
	var count, nullResources int
	if err := conn.QueryRow(context.Background(), "SELECT count(*), count(*) FILTER (WHERE resource_id IS NULL) FROM audit_events WHERE action='directory_search' AND outcome='allow'").Scan(&count, &nullResources); err != nil || count != 1 || nullResources != 1 {
		t.Fatalf("search audit: count=%d null=%d err=%v", count, nullResources, err)
	}
}

func TestSearchManagedPeopleEscapesWildcardsAndSignalsMore(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')", "00000000-0000-4000-8000-000000000194", tenantA, adminM, orgA)
	run(t, conn, "UPDATE users SET display_name='百分号%人员' WHERE id=$1", personA)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	percent, err := svc.SearchManagedPeople(context.Background(), identity(), "%", 20)
	if err != nil || len(percent.People) != 1 || percent.People[0].ID != personA {
		t.Fatalf("wildcard matched unrelated people: %+v %v", percent, err)
	}
	page, err := svc.SearchManagedPeople(context.Background(), identity(), "A00", 1)
	if err != nil || len(page.People) != 1 || page.People[0].ID != adminA || !page.HasMore {
		t.Fatalf("bounded search: %+v %v", page, err)
	}
}

func TestSearchManagedPeopleDeniesInvalidActorAndAuditFailure(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	if _, err := svc.SearchManagedPeople(context.Background(), identity(), "A00", 20); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("ungranted actor searched: %v", err)
	}
	run(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')", "00000000-0000-4000-8000-000000000195", tenantA, adminM, orgA)
	run(t, conn, "ALTER TABLE audit_events ADD CONSTRAINT reject_search_audit CHECK (action <> 'directory_search') NOT VALID")
	if _, err := svc.SearchManagedPeople(context.Background(), identity(), "A00", 20); !errors.Is(err, access.ErrAuditUnavailable) {
		t.Fatalf("audit failure did not fail closed: %v", err)
	}
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
	if _, err := svc.SearchManagedPeople(context.Background(), identity(), "A00", 20); !errors.Is(err, access.ErrAuditUnavailable) {
		t.Fatalf("invalid actor audit failure not propagated: %v", err)
	}
}

func TestSearchManagedPeopleRejectsInvalidText(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')", "00000000-0000-4000-8000-000000000196", tenantA, adminM, orgA)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	for _, query := range []string{"A\x00", string([]byte{'A', 0xff})} {
		if _, err := svc.SearchManagedPeople(context.Background(), identity(), query, 20); !errors.Is(err, access.ErrInvalidSearch) {
			t.Fatalf("invalid encoding was not treated as invalid search: %v", err)
		}
	}
}
