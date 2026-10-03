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

func seedRetentionHistory(t *testing.T, conn *pgx.Conn) access.Service {
	t.Helper()
	seedAccess(t, conn)
	run(t, conn, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')`, retentionGrant, tenantA, adminM, orgA)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	for i, days := range []int{300, 200, 100} {
		if _, err := svc.SetRetentionPolicy(context.Background(), identity(), int64(i), days, fmt.Sprintf("CAB-HISTORY-%d", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	return svc
}

// Catches missing tenant filtering, unstable paging by timestamps and inclusion of default v0.
func TestRetentionHistoryPagingAndTenantIsolation(t *testing.T) {
	conn := testDB(t)
	svc := seedRetentionHistory(t, conn)
	ctx := context.Background()
	run(t, conn, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ('00000000-0000-4000-8000-000000009b11',$1,$2,$3,'group_admin','2020-01-01')`, tenantB, personBM, orgB)
	foreign := access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: personBM}
	empty, err := svc.ListRetentionPolicyHistory(ctx, foreign, "", 20)
	if err != nil || empty.History == nil || len(empty.History) != 0 || empty.NextCursor != "" {
		t.Fatalf("default empty tenant %+v %v", empty, err)
	}
	for i, days := range []int{330, 220, 110} {
		if _, err := svc.SetRetentionPolicy(ctx, foreign, int64(i), days, fmt.Sprintf("CAB-OTHER-%d", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := svc.ListRetentionPolicyHistory(ctx, identity(), "", 2)
	if err != nil || len(first.History) != 2 || first.NextCursor == "" {
		t.Fatalf("first: %+v %v", first, err)
	}
	if first.History[0].Version != 3 || first.History[1].Version != 2 || first.History[0].MessageBodyDays != 100 ||
		first.History[0].ApprovalReference != "CAB-HISTORY-3" || first.History[0].ApprovedByUserID != adminA ||
		first.History[0].ApprovedAt == nil || !first.History[0].ApprovedAt.Equal(fixedTime) {
		t.Fatalf("approval: %+v", first)
	}
	if _, err := svc.SetRetentionPolicy(ctx, identity(), 3, 90, "CAB-NEW"); err != nil {
		t.Fatal(err)
	}
	second, err := svc.ListRetentionPolicyHistory(ctx, identity(), first.NextCursor, 2)
	if err != nil || len(second.History) != 1 || second.History[0].Version != 1 || second.NextCursor != "" {
		t.Fatalf("second: %+v %v", second, err)
	}
	refreshed, err := svc.ListRetentionPolicyHistory(ctx, identity(), "", 1)
	if err != nil || len(refreshed.History) != 1 || refreshed.History[0].Version != 4 {
		t.Fatalf("refresh: %+v %v", refreshed, err)
	}
	other, err := svc.ListRetentionPolicyHistory(ctx, foreign, "", 2)
	if err != nil || len(other.History) != 2 || other.History[0].Version != 3 || other.History[1].Version != 2 ||
		other.History[0].MessageBodyDays != 110 || other.History[0].ApprovalReference != "CAB-OTHER-3" || other.History[0].ApprovedByUserID != personB {
		t.Fatalf("other tenant metadata %+v %v", other, err)
	}
	otherLast, err := svc.ListRetentionPolicyHistory(ctx, foreign, other.NextCursor, 2)
	if err != nil || len(otherLast.History) != 1 || otherLast.History[0].Version != 1 || otherLast.History[0].ApprovalReference != "CAB-OTHER-1" || otherLast.History[0].ApprovedByUserID != personB || otherLast.NextCursor != "" {
		t.Fatalf("other tenant last page %+v %v", otherLast, err)
	}
	if _, err := svc.ListRetentionPolicyHistory(ctx, foreign, first.NextCursor, 20); !errors.Is(err, access.ErrInvalidRetentionHistoryQuery) {
		t.Fatalf("foreign cursor: %v", err)
	}
	var allows int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='retention_policy_history_list' AND outcome='allow'`).Scan(&allows); err != nil || allows != 6 {
		t.Fatalf("audits %d %v", allows, err)
	}
}

func TestRetentionHistoryInvalidCursorAndLimits(t *testing.T) {
	conn := testDB(t)
	svc := seedRetentionHistory(t, conn)
	for _, limit := range []int{0, -1, 101} {
		if _, err := svc.ListRetentionPolicyHistory(context.Background(), identity(), "", limit); !errors.Is(err, access.ErrInvalidRetentionHistoryQuery) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	for _, raw := range []string{`{"t":"` + tenantA + `","v":0}`, `{"t":"` + tenantA + `","v":-1}`, `{"t":"` + tenantA + `","v":2,"extra":1}`, `{"t":"` + tenantA + `","v":2,"v":2}`, `{"t":"other","v":2}`, `{"t":"` + tenantA + `","v":2.0}`} {
		cursor := base64.RawURLEncoding.EncodeToString([]byte(raw))
		if _, err := svc.ListRetentionPolicyHistory(context.Background(), identity(), cursor, 2); !errors.Is(err, access.ErrInvalidRetentionHistoryQuery) {
			t.Fatalf("cursor %s: %v", raw, err)
		}
	}
	for _, cursor := range []string{"bad", strings.Repeat("a", 1025)} {
		if _, err := svc.ListRetentionPolicyHistory(context.Background(), identity(), cursor, 2); !errors.Is(err, access.ErrInvalidRetentionHistoryQuery) {
			t.Fatalf("bad cursor: %v", err)
		}
	}
	if _, err := (access.Service{}).ListRetentionPolicyHistory(context.Background(), identity(), "", 20); !errors.Is(err, access.ErrInvalidIdentity) {
		t.Fatalf("missing db: %v", err)
	}
}

func TestRetentionHistoryAuthorizationAndAuditFailure(t *testing.T) {
	conn := testDB(t)
	svc := seedRetentionHistory(t, conn)
	ctx := context.Background()
	run(t, conn, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,scope_organization_id,effective_from)
 VALUES ('00000000-0000-4000-8000-000000009c11',$1,$2,$3,'organization_admin',$3,'2020-01-01')`, tenantA, personM, orgA)
	for _, id := range []access.TrustedIdentity{{TenantID: tenantA, UserID: personA, ActingMembershipID: personM}, {TenantID: tenantA, UserID: personA, ActingMembershipID: personM2}, {TenantID: tenantB, UserID: personB, ActingMembershipID: personBM}} {
		page, err := svc.ListRetentionPolicyHistory(ctx, id, "", 2)
		if !errors.Is(err, access.ErrNotFound) || len(page.History) != 0 || page.NextCursor != "" {
			t.Fatalf("denial %+v %v", page, err)
		}
	}
	first, err := svc.ListRetentionPolicyHistory(ctx, identity(), "", 2)
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, `UPDATE admin_grants SET status='revoked' WHERE id=$1`, retentionGrant)
	if p, err := svc.ListRetentionPolicyHistory(ctx, identity(), first.NextCursor, 2); !errors.Is(err, access.ErrNotFound) || len(p.History) != 0 {
		t.Fatalf("revoked page: %+v %v", p, err)
	}
	run(t, conn, `UPDATE admin_grants SET status='active' WHERE id=$1`, retentionGrant)
	run(t, conn, `UPDATE users SET status='frozen' WHERE id=$1`, adminA)
	if _, err := svc.ListRetentionPolicyHistory(ctx, identity(), "", 2); !errors.Is(err, access.ErrInvalidIdentity) {
		t.Fatalf("frozen: %v", err)
	}
	run(t, conn, `UPDATE users SET status='active' WHERE id=$1`, adminA)
	run(t, conn, `UPDATE user_organizations SET status='ended',effective_to=$1 WHERE id=$2`, fixedTime, adminM)
	if _, err := svc.ListRetentionPolicyHistory(ctx, identity(), "", 2); !errors.Is(err, access.ErrInvalidIdentity) {
		t.Fatalf("ended: %v", err)
	}
	run(t, conn, `UPDATE user_organizations SET status='active',effective_to=NULL WHERE id=$1`, adminM)
	run(t, conn, `CREATE FUNCTION reject_history_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='retention_policy_history_list' THEN RAISE EXCEPTION 'audit down'; END IF; RETURN NEW; END $$`)
	run(t, conn, `CREATE TRIGGER reject_history_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_history_audit()`)
	if p, err := svc.ListRetentionPolicyHistory(ctx, identity(), "", 2); !errors.Is(err, access.ErrAuditUnavailable) || len(p.History) != 0 || p.NextCursor != "" {
		t.Fatalf("audit leaked %+v %v", p, err)
	}
}

func TestRetentionHistoryRechecksDeadlineAfterQueryAndAudit(t *testing.T) {
	for _, expireAt := range []int{3, 4} {
		t.Run(fmt.Sprint(expireAt), func(t *testing.T) {
			conn := testDB(t)
			svc := seedRetentionHistory(t, conn)
			run(t, conn, `UPDATE admin_grants SET effective_to=$1 WHERE id=$2`, fixedTime.Add(time.Hour), retentionGrant)
			calls := 0
			svc.Now = func() time.Time {
				calls++
				if calls >= expireAt {
					return fixedTime.Add(2 * time.Hour)
				}
				return fixedTime
			}
			if p, err := svc.ListRetentionPolicyHistory(context.Background(), identity(), "", 2); !errors.Is(err, access.ErrNotFound) || len(p.History) != 0 {
				t.Fatalf("expired %+v %v", p, err)
			}
			var allows int
			if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action='retention_policy_history_list' AND outcome='allow'`).Scan(&allows); err != nil || allows != 0 {
				t.Fatalf("expired allow audit %d %v", allows, err)
			}
		})
	}
}

func TestRetentionHistoryFreshSnapshotAfterActorLock(t *testing.T) {
	conn := testDB(t)
	seedRetentionHistory(t, conn)
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
	if p, err := svc.ListRetentionPolicyHistory(context.Background(), identity(), "", 2); !errors.Is(err, access.ErrNotFound) || len(p.History) != 0 || !changed {
		t.Fatalf("stale grant %+v %v", p, err)
	}
}

func TestRetentionHistoryRechecksAfterTenantLockWait(t *testing.T) {
	conn := testDB(t)
	seedRetentionHistory(t, conn)
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
		p, e := svc.ListRetentionPolicyHistory(ctx, identity(), "", 2)
		if len(p.History) != 0 {
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
