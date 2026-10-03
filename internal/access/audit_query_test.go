package access_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

func seedAuditQuery(t *testing.T, conn *pgx.Conn) access.Service {
	t.Helper()
	seedAccess(t, conn)
	run(t, conn, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')`, retentionGrant, tenantA, adminM, orgA)
	for _, row := range []struct {
		eventID, tenant, user, member, outcome, reason string
		at                                             time.Time
	}{
		{"9007199254740993", tenantA, adminA, adminM, "deny", "version_conflict", fixedTime},
		{"9007199254740992", tenantA, adminA, adminM, "allow", "updated", fixedTime},
		{"10", tenantA, adminA, adminM, "allow", "same_time_update", fixedTime},
		{"9", tenantA, adminA, adminM, "deny", "extension_blocked", fixedTime},
		{"8", tenantA, adminA, adminM, "allow", "old_update", fixedTime.Add(-time.Hour)},
		{"9007199254740994", tenantB, personB, personBM, "deny", "foreign_reason", fixedTime},
	} {
		run(t, conn, `INSERT INTO audit_events(id,tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
 OVERRIDING SYSTEM VALUE VALUES ($1,$2,$3,$4,'retention_policy_update','tenant',NULL,$5,$6,$7)`, row.eventID, row.tenant, row.user, row.member, row.outcome, row.reason, row.at)
	}
	run(t, conn, `SELECT setval(pg_get_serial_sequence('audit_events','id'),9007199254740994)`)
	return access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
}

// Missing tenant restriction, string ID ordering or timestamp-only cursors leak/skip these rows.
func TestAuditQueryPagingFiltersAndTenantIsolation(t *testing.T) {
	conn := testDB(t)
	svc := seedAuditQuery(t, conn)
	ctx := context.Background()
	first, err := svc.ListAuditEvents(ctx, identity(), "retention_policy_update", "", "", 2)
	if err != nil || len(first.Events) != 2 || first.NextCursor == "" {
		t.Fatalf("first %+v %v", first, err)
	}
	if first.Events[0].ID != "9007199254740993" || first.Events[1].ID != "9007199254740992" || first.Events[0].Reason != "version_conflict" || first.Events[0].ActorUserID != adminA || first.Events[0].ResourceID != nil || !first.Events[0].OccurredAt.Equal(fixedTime) {
		t.Fatalf("metadata %+v", first)
	}
	run(t, conn, `INSERT INTO audit_events(tenant_id,actor_user_id,acting_membership_id,action,resource_type,outcome,reason,occurred_at) VALUES ($1,$2,$3,'retention_policy_update','tenant','allow','new_update',$4)`, tenantA, adminA, adminM, fixedTime.Add(time.Hour))
	second, err := svc.ListAuditEvents(ctx, identity(), "retention_policy_update", "", first.NextCursor, 2)
	if err != nil || len(second.Events) != 2 || second.Events[0].ID != "10" || second.Events[1].ID != "9" || second.NextCursor == "" {
		t.Fatalf("second %+v %v", second, err)
	}
	last, err := svc.ListAuditEvents(ctx, identity(), "retention_policy_update", "", second.NextCursor, 2)
	if err != nil || len(last.Events) != 1 || last.Events[0].ID != "8" || last.NextCursor != "" {
		t.Fatalf("last %+v %v", last, err)
	}
	fresh, err := svc.ListAuditEvents(ctx, identity(), "retention_policy_update", "", "", 1)
	if err != nil || len(fresh.Events) != 1 || fresh.Events[0].Reason != "new_update" {
		t.Fatalf("refresh %+v %v", fresh, err)
	}
	denied, err := svc.ListAuditEvents(ctx, identity(), "retention_policy_update", "deny", "", 10)
	if err != nil || len(denied.Events) != 2 || denied.Events[0].Reason != "version_conflict" || denied.Events[1].Reason != "extension_blocked" {
		t.Fatalf("filter %+v %v", denied, err)
	}
	empty, err := svc.ListAuditEvents(ctx, identity(), "unrecorded_action", "", "", 10)
	if err != nil || empty.Events == nil || len(empty.Events) != 0 || empty.NextCursor != "" {
		t.Fatalf("empty %+v %v", empty, err)
	}
	run(t, conn, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES ('00000000-0000-4000-8000-000000009c12',$1,$2,$3,'group_admin','2020-01-01')`, tenantB, personBM, orgB)
	foreign := access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: personBM}
	b, err := svc.ListAuditEvents(ctx, foreign, "retention_policy_update", "", "", 2)
	if err != nil || len(b.Events) != 1 || b.Events[0].ID != "9007199254740994" || b.Events[0].Reason != "foreign_reason" || b.Events[0].ActorUserID != personB {
		t.Fatalf("other %+v %v", b, err)
	}
	for _, q := range []struct {
		id              access.TrustedIdentity
		action, outcome string
	}{{foreign, "retention_policy_update", ""}, {identity(), "retention_policy_read", ""}, {identity(), "retention_policy_update", "deny"}} {
		if _, err := svc.ListAuditEvents(ctx, q.id, q.action, q.outcome, first.NextCursor, 2); !errors.Is(err, access.ErrInvalidAuditQuery) {
			t.Fatalf("cursor context %v", err)
		}
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='audit_events_list' AND outcome='allow'`).Scan(&n); err != nil || n != 7 {
		t.Fatalf("read audit %d %v", n, err)
	}
	// Tenant A has six earlier query audits and six update events; tenant B has its own query audit.
	// An unfiltered refresh includes earlier query audits, but not its own uncommitted event.
	all, err := svc.ListAuditEvents(ctx, identity(), "", "", "", 100)
	if err != nil || len(all.Events) != 12 {
		t.Fatalf("unfiltered %+v %v", all, err)
	}
	for _, e := range all.Events {
		if e.Reason == "foreign_reason" {
			t.Fatal("foreign row")
		}
	}
}

func TestAuditQueryInvalidInputs(t *testing.T) {
	conn := testDB(t)
	svc := seedAuditQuery(t, conn)
	for _, q := range []struct {
		action, outcome, cursor string
		limit                   int
	}{{"", "", "", 0}, {"", "", "", 101}, {"bad action", "", "", 20}, {"A", "", "", 20}, {strings.Repeat("a", 65), "", "", 20}, {"", "ALLOW", "", 20}, {"", "all", "", 20}, {"", "", "bad", 20}, {"", "", strings.Repeat("a", 1025), 20}} {
		if _, err := svc.ListAuditEvents(context.Background(), identity(), q.action, q.outcome, q.cursor, q.limit); !errors.Is(err, access.ErrInvalidAuditQuery) {
			t.Fatalf("invalid %+v %v", q, err)
		}
	}
	for _, raw := range []string{
		`{"t":"` + tenantA + `","a":"","o":"","at":"2026-09-28T10:00:00Z","id":"0"}`,
		`{"t":"` + tenantA + `","a":"","o":"","at":"2026-09-28T10:00:00Z","id":"01"}`,
		`{"t":"` + tenantA + `","a":"","o":"","at":"2026-02-30T00:00:00Z","id":"9"}`,
		`{"t":"` + tenantA + `","a":"","o":"","at":"2026-09-28T10:00:00Z","id":"9223372036854775808"}`,
		`{"t":"` + tenantA + `","a":"","o":"","at":"2026-09-28T10:00:00Z","id":"9","x":1}`,
		`{"t":"` + tenantA + `","a":"","o":"","at":"2026-09-28T10:00:00Z","id":"9","id":"9"}`,
	} {
		c := base64.RawURLEncoding.EncodeToString([]byte(raw))
		if _, err := svc.ListAuditEvents(context.Background(), identity(), "", "", c, 20); !errors.Is(err, access.ErrInvalidAuditQuery) {
			t.Fatalf("raw %s %v", raw, err)
		}
	}
	if _, err := (access.Service{}).ListAuditEvents(context.Background(), identity(), "", "", "", 20); !errors.Is(err, access.ErrInvalidIdentity) {
		t.Fatalf("nil db %v", err)
	}
}

func TestAuditQueryAuthorizationAndAuditFailure(t *testing.T) {
	conn := testDB(t)
	svc := seedAuditQuery(t, conn)
	ctx := context.Background()
	run(t, conn, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,scope_organization_id,effective_from) VALUES ('00000000-0000-4000-8000-000000009d12',$1,$2,$3,'organization_admin',$3,'2020-01-01')`, tenantA, personM, orgA)
	for _, id := range []access.TrustedIdentity{{TenantID: tenantA, UserID: personA, ActingMembershipID: personM}, {TenantID: tenantA, UserID: personA, ActingMembershipID: personM2}} {
		if p, err := svc.ListAuditEvents(ctx, id, "", "", "", 20); !errors.Is(err, access.ErrNotFound) || len(p.Events) != 0 {
			t.Fatalf("scope %+v %v", p, err)
		}
	}
	first, err := svc.ListAuditEvents(ctx, identity(), "retention_policy_update", "", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, `UPDATE admin_grants SET status='revoked' WHERE id=$1`, retentionGrant)
	if p, err := svc.ListAuditEvents(ctx, identity(), "retention_policy_update", "", first.NextCursor, 2); !errors.Is(err, access.ErrNotFound) || len(p.Events) != 0 {
		t.Fatalf("revoked %+v %v", p, err)
	}
	run(t, conn, `UPDATE admin_grants SET status='active' WHERE id=$1`, retentionGrant)
	run(t, conn, `UPDATE users SET status='frozen' WHERE id=$1`, adminA)
	if _, err := svc.ListAuditEvents(ctx, identity(), "", "", "", 20); !errors.Is(err, access.ErrInvalidIdentity) {
		t.Fatalf("frozen %v", err)
	}
	run(t, conn, `UPDATE users SET status='active' WHERE id=$1`, adminA)
	run(t, conn, `UPDATE user_organizations SET status='ended',effective_to=$1 WHERE id=$2`, fixedTime, adminM)
	if _, err := svc.ListAuditEvents(ctx, identity(), "", "", "", 20); !errors.Is(err, access.ErrInvalidIdentity) {
		t.Fatalf("ended %v", err)
	}
	run(t, conn, `UPDATE user_organizations SET status='active',effective_to=NULL WHERE id=$1`, adminM)
	run(t, conn, `CREATE FUNCTION reject_audit_read() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='audit_events_list' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END $$`)
	run(t, conn, `CREATE TRIGGER reject_audit_read BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_audit_read()`)
	if p, err := svc.ListAuditEvents(ctx, identity(), "", "", "", 20); !errors.Is(err, access.ErrAuditUnavailable) || len(p.Events) != 0 || p.NextCursor != "" {
		t.Fatalf("audit failure %+v %v", p, err)
	}
}

func TestAuditQueryExpiryRollsBackAllow(t *testing.T) {
	for _, expireAt := range []int{2, 3, 4} {
		t.Run(fmt.Sprint(expireAt), func(t *testing.T) {
			conn := testDB(t)
			svc := seedAuditQuery(t, conn)
			run(t, conn, `UPDATE admin_grants SET effective_to=$1 WHERE id=$2`, fixedTime.Add(time.Hour), retentionGrant)
			calls := 0
			svc.Now = func() time.Time {
				calls++
				if calls >= expireAt {
					return fixedTime.Add(2 * time.Hour)
				}
				return fixedTime
			}
			if p, err := svc.ListAuditEvents(context.Background(), identity(), "", "", "", 20); !errors.Is(err, access.ErrNotFound) || len(p.Events) != 0 {
				t.Fatalf("expiry %+v %v", p, err)
			}
			var n int
			if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action='audit_events_list' AND outcome='allow'`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("allow %d %v", n, err)
			}
		})
	}
}
func TestAuditQueryFreshSnapshotAfterActorLock(t *testing.T) {
	conn := testDB(t)
	seedAuditQuery(t, conn)
	other := secondConnection(t, conn)
	run(t, other, "SET default_transaction_isolation TO 'repeatable read'")
	changed := false
	svc := access.Service{DB: other, Now: func() time.Time {
		if !changed {
			run(t, conn, `UPDATE admin_grants SET status='revoked' WHERE id=$1`, retentionGrant)
			changed = true
		}
		return fixedTime
	}}
	if p, err := svc.ListAuditEvents(context.Background(), identity(), "", "", "", 2); !errors.Is(err, access.ErrNotFound) || len(p.Events) != 0 || !changed {
		t.Fatalf("stale grant %+v %v", p, err)
	}
}

func TestAuditQueryRechecksAfterTenantLockWait(t *testing.T) {
	conn := testDB(t)
	seedAuditQuery(t, conn)
	run(t, conn, `UPDATE admin_grants SET effective_to=$1 WHERE id=$2`, fixedTime.Add(time.Hour), retentionGrant)
	other := secondConnection(t, conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	block, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer block.Rollback(context.Background())
	if _, err = block.Exec(ctx, `SELECT id FROM tenants WHERE id=$1 FOR UPDATE`, tenantA); err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	clock.Store(fixedTime.UnixNano())
	svc := access.Service{DB: other, Now: func() time.Time { return time.Unix(0, clock.Load()) }}
	result := make(chan error, 1)
	go func() {
		p, e := svc.ListAuditEvents(ctx, identity(), "", "", "", 2)
		if len(p.Events) != 0 {
			e = fmt.Errorf("history leaked: %+v", p)
		}
		result <- e
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := block.QueryRow(ctx, `SELECT COALESCE(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1`, int(other.PgConn().PID())).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case e := <-result:
			t.Fatalf("did not wait: %v", e)
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	clock.Store(fixedTime.Add(2 * time.Hour).UnixNano())
	if err = block.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("expired after wait %v", err)
	}
}
