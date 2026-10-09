package policystore_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestClientPolicyDefaultsCASAndAdminScope(t *testing.T) {
	c := db(t)
	seed(t, c)
	s := deviceService(c)
	ctx := context.Background()
	defaults, e := s.GetClientPolicy(ctx, deviceIdentity())
	if e != nil || defaults.Version != 0 || defaults.DeviceLeaseDays != 30 || defaults.MaxActiveDevices != 10 || defaults.OfflineCacheSeconds != 300 || defaults.GroupReceiptVisibility != "owner_admin" {
		t.Fatalf("defaults %+v %v", defaults, e)
	}
	next := defaults
	next.DeviceLeaseDays = 90
	next.MaxActiveDevices = 100
	next.OfflineCacheSeconds = 0
	next.GroupReceiptVisibility = "sender_aggregate"
	if _, e = s.SetClientPolicy(ctx, deviceIdentity(), 0, next); !errors.Is(e, policystore.ErrForbidden) {
		t.Fatal("ordinary writer", e)
	}
	run(t, c, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES('00000000-0000-4000-8000-000000000271',$1,$2,$3,'group_admin','2020-01-01')`, tenantA, adminM, orgA)
	updated, e := s.SetClientPolicy(ctx, publisher(), 0, next)
	if e != nil || updated.Version != 1 {
		t.Fatal("CAS", e)
	}
	if _, e = s.SetClientPolicy(ctx, publisher(), 0, next); !errors.Is(e, policystore.ErrVersionConflict) {
		t.Fatal("stale CAS", e)
	}
	got, e := s.GetClientPolicy(ctx, deviceIdentity())
	if e != nil || got != updated {
		t.Fatalf("effective policy %+v %v", got, e)
	}
	for _, bad := range []policystore.ClientPolicy{{DeviceLeaseDays: 0, MaxActiveDevices: 10, OfflineCacheSeconds: 300, GroupReceiptVisibility: "owner_admin"}, {DeviceLeaseDays: 91, MaxActiveDevices: 10, OfflineCacheSeconds: 300, GroupReceiptVisibility: "owner_admin"}, {DeviceLeaseDays: 30, MaxActiveDevices: 101, OfflineCacheSeconds: 300, GroupReceiptVisibility: "owner_admin"}, {DeviceLeaseDays: 30, MaxActiveDevices: 10, OfflineCacheSeconds: 3601, GroupReceiptVisibility: "owner_admin"}, {DeviceLeaseDays: 30, MaxActiveDevices: 10, OfflineCacheSeconds: 300, GroupReceiptVisibility: "all"}} {
		if _, e = s.SetClientPolicy(ctx, publisher(), 1, bad); !errors.Is(e, policystore.ErrInvalidClientPolicy) {
			t.Fatal("range", e)
		}
	}
	if _, e = s.SetClientPolicy(ctx, publisher(), math.MaxInt64, next); !errors.Is(e, policystore.ErrInvalidClientPolicy) {
		t.Fatal("overflow", e)
	}
	run(t, c, "UPDATE admin_grants SET role='organization_admin',scope_organization_id=$1", orgA)
	if _, e = s.SetClientPolicy(ctx, publisher(), 1, next); !errors.Is(e, policystore.ErrForbidden) {
		t.Fatal("organization admin", e)
	}
}

func TestClientPolicyAuditFailureRollsBack(t *testing.T) {
	c := db(t)
	seed(t, c)
	s := deviceService(c)
	ctx := context.Background()
	run(t, c, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES('00000000-0000-4000-8000-000000000271',$1,$2,$3,'group_admin','2020-01-01')`, tenantA, adminM, orgA)
	next, e := s.GetClientPolicy(ctx, publisher())
	if e != nil {
		t.Fatal(e)
	}
	next.DeviceLeaseDays = 1
	run(t, c, `CREATE FUNCTION fail_client_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$; CREATE TRIGGER fail_client_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_client_audit()`)
	if _, e = s.SetClientPolicy(ctx, publisher(), 0, next); !errors.Is(e, policystore.ErrAuditUnavailable) {
		t.Fatal(e)
	}
	var n int
	if e = c.QueryRow(ctx, "SELECT count(*) FROM tenant_client_policies").Scan(&n); e != nil || n != 0 {
		t.Fatal("policy write escaped rollback")
	}
}
