package policystore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestRealtimeIdentityAuthorizesAndAuditsTicketAndConnect(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	for _, action := range []policystore.RealtimeAction{policystore.RealtimeTicket, policystore.RealtimeConnect} {
		if err := svc.AuthorizeRealtime(context.Background(), publisher(), action); err != nil {
			t.Fatalf("valid %s: %v", action, err)
		}
	}
	var audits int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE tenant_id=$1 AND action IN ('realtime_ticket','realtime_connect') AND outcome='allow'", tenantA).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("realtime audits: %d %v", audits, err)
	}
	active, err := svc.RealtimeIdentityActive(context.Background(), publisher())
	if err != nil || !active {
		t.Fatalf("active identity: %v %v", active, err)
	}
}

func TestRealtimeIdentityRejectsFrozenEndedForeignAndAuditFailure(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	foreign := access.TrustedIdentity{TenantID: tenantB, UserID: adminA, ActingMembershipID: adminM}
	if err := svc.AuthorizeRealtime(context.Background(), foreign, policystore.RealtimeTicket); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("foreign tenant: %v", err)
	}
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
	if err := svc.AuthorizeRealtime(context.Background(), publisher(), policystore.RealtimeConnect); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("frozen account: %v", err)
	}
	active, err := svc.RealtimeIdentityActive(context.Background(), publisher())
	if err != nil || active {
		t.Fatalf("frozen active: %v %v", active, err)
	}
	run(t, conn, "UPDATE users SET status='active' WHERE id=$1", adminA)
	run(t, conn, "UPDATE user_organizations SET status='ended' WHERE id=$1", adminM)
	if err := svc.AuthorizeRealtime(context.Background(), publisher(), policystore.RealtimeTicket); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("ended membership: %v", err)
	}
	run(t, conn, "UPDATE user_organizations SET status='active' WHERE id=$1", adminM)
	run(t, conn, `CREATE FUNCTION fail_realtime_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$`)
	run(t, conn, "CREATE TRIGGER fail_realtime_audit BEFORE INSERT ON audit_events FOR EACH ROW WHEN (NEW.action = 'realtime_ticket') EXECUTE FUNCTION fail_realtime_audit()")
	if err := svc.AuthorizeRealtime(context.Background(), publisher(), policystore.RealtimeTicket); !errors.Is(err, policystore.ErrAuditUnavailable) {
		t.Fatalf("audit failure: %v", err)
	}
}

func TestRealtimeIdentityChecksExpiryAfterDatabaseRead(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at.Add(time.Second), adminM)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at.Add(2 * time.Second) }}
	if err := svc.AuthorizeRealtime(context.Background(), publisher(), policystore.RealtimeTicket); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("expired membership: %v", err)
	}
}
