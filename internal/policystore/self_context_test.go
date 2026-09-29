package policystore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

const secondAdminMembership = "00000000-0000-4000-8000-000000000245"
const userWithoutMembership = "00000000-0000-4000-8000-000000000246"

func TestSelfContextReturnsCurrentMembershipsInStableOrder(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, `INSERT INTO user_organizations
 (id,tenant_id,user_id,organization_id,effective_from,is_primary,title)
 VALUES ($1,$2,$3,$4,'2020-01-01',false,'兼任')`, secondAdminMembership, tenantA, adminA, orgA2)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	self, err := svc.GetSelfContext(context.Background(), tenantA, adminA)
	if err != nil || self.TenantID != tenantA || self.UserID != adminA ||
		self.DisplayName != "管理员" || self.GlobalEmployeeNo != "A001" || len(self.Memberships) != 2 {
		t.Fatalf("self context: %+v %v", self, err)
	}
	if self.Memberships[0].ID != adminM || !self.Memberships[0].IsPrimary ||
		self.Memberships[0].OrganizationName != "公司 A" ||
		self.Memberships[0].LegalEntityID != legalA || self.Memberships[0].LegalEntityName != "法人 A" ||
		self.Memberships[1].ID != secondAdminMembership || self.Memberships[1].Title != "兼任" {
		t.Fatalf("membership order/details: %+v", self.Memberships)
	}
}

func TestSelfContextFiltersInactiveAndAllowsEmptyMemberships(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	run(t, conn, `INSERT INTO user_organizations
 (id,tenant_id,user_id,organization_id,effective_from,effective_to,is_primary)
 VALUES ($1,$2,$3,$4,'2020-01-01',$5,false)`, secondAdminMembership, tenantA, adminA, orgA2, at.Add(-time.Hour))
	self, err := svc.GetSelfContext(context.Background(), tenantA, adminA)
	if err != nil || len(self.Memberships) != 1 || self.Memberships[0].ID != adminM {
		t.Fatalf("expired membership visible: %+v %v", self.Memberships, err)
	}
	run(t, conn, "UPDATE organizations SET status='disabled' WHERE id=$1", orgA)
	self, err = svc.GetSelfContext(context.Background(), tenantA, adminA)
	if err != nil || len(self.Memberships) != 0 {
		t.Fatalf("disabled organization visible: %+v %v", self.Memberships, err)
	}
	run(t, conn, "UPDATE organizations SET status='active' WHERE id=$1", orgA)
	run(t, conn, "UPDATE legal_entities SET status='disabled' WHERE id=$1", legalA)
	self, err = svc.GetSelfContext(context.Background(), tenantA, adminA)
	if err != nil || len(self.Memberships) != 0 {
		t.Fatalf("disabled legal entity visible: %+v %v", self.Memberships, err)
	}
	run(t, conn, "INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,'A999','新员工')", userWithoutMembership, tenantA)
	self, err = svc.GetSelfContext(context.Background(), tenantA, userWithoutMembership)
	if err != nil || len(self.Memberships) != 0 || self.DisplayName != "新员工" {
		t.Fatalf("valid user without membership: %+v %v", self, err)
	}
}

func TestSelfContextRejectsFrozenForeignAndDatabaseFailure(t *testing.T) {
	if _, err := (policystore.Service{}).GetSelfContext(context.Background(), tenantA, adminA); err == nil || errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("missing database should be unavailable: %v", err)
	}
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if _, err := svc.GetSelfContext(context.Background(), tenantB, adminA); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("foreign identity: %v", err)
	}
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
	if _, err := svc.GetSelfContext(context.Background(), tenantA, adminA); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("frozen account: %v", err)
	}
	conn.Close(context.Background())
	if _, err := svc.GetSelfContext(context.Background(), tenantA, adminA); err == nil {
		t.Fatal("database failure hidden")
	}
}
