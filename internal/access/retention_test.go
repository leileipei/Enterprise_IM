package access_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

const retentionGrant = "00000000-0000-4000-8000-000000000181"

func TestTenantRetentionPolicyRequiresGroupAdminAndAuditsVersionedWrites(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, `INSERT INTO admin_grants
 (id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')`, retentionGrant, tenantA, adminM, orgA)
	run(t, conn, `INSERT INTO admin_grants
 (id,tenant_id,membership_id,membership_organization_id,role,scope_organization_id,effective_from)
 VALUES ('00000000-0000-4000-8000-000000000182',$1,$2,$3,'organization_admin',$3,'2020-01-01')`,
		tenantA, personM, orgA)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	orgAdmin := access.TrustedIdentity{TenantID: tenantA, UserID: personA, ActingMembershipID: personM}
	policy, err := svc.GetRetentionPolicy(context.Background(), admin)
	if err != nil || policy.MessageBodyDays != 365 || policy.Version != 0 || policy.ApprovalReference != "" {
		t.Fatalf("default policy: %+v %v", policy, err)
	}
	if _, err := svc.GetRetentionPolicy(context.Background(), orgAdmin); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("organization admin read: %v", err)
	}
	if _, err := svc.SetRetentionPolicy(context.Background(), orgAdmin, 0, 730, "CAB-2026-01"); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("organization admin write: %v", err)
	}
	policy, err = svc.SetRetentionPolicy(context.Background(), admin, 0, 730, "CAB-2026-01")
	if err != nil || policy.MessageBodyDays != 730 || policy.Version != 1 ||
		policy.ApprovalReference != "CAB-2026-01" || policy.ApprovedByUserID != adminA ||
		policy.ApprovedAt == nil || !policy.ApprovedAt.Equal(fixedTime) {
		t.Fatalf("approved update: %+v %v", policy, err)
	}
	if _, err := svc.SetRetentionPolicy(context.Background(), admin, 0, 30, "CAB-stale"); !errors.Is(err, access.ErrConflict) {
		t.Fatalf("stale version: %v", err)
	}
	policy, err = svc.GetRetentionPolicy(context.Background(), admin)
	if err != nil || policy.MessageBodyDays != 730 || policy.Version != 1 {
		t.Fatalf("stale write changed policy: %+v %v", policy, err)
	}
	var updates int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM audit_events
 WHERE tenant_id=$1 AND action='retention_policy_update' AND outcome='allow'`, tenantA).Scan(&updates); err != nil || updates != 1 {
		t.Fatalf("update audit: %d %v", updates, err)
	}
	var otherDays int
	if err := conn.QueryRow(context.Background(), "SELECT message_body_retention_days FROM tenants WHERE id=$1", tenantB).Scan(&otherDays); err != nil || otherDays != 365 {
		t.Fatalf("other tenant modified: %d %v", otherDays, err)
	}
	run(t, conn, `INSERT INTO admin_grants
 (id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ('00000000-0000-4000-8000-000000000185',$1,$2,$3,'group_admin','2020-01-01')`,
		tenantB, personBM, orgB)
	other, err := svc.GetRetentionPolicy(context.Background(), access.TrustedIdentity{
		TenantID: tenantB, UserID: personB, ActingMembershipID: personBM})
	if err != nil || other.MessageBodyDays != 365 || other.Version != 0 || other.ApprovalReference != "" {
		t.Fatalf("other tenant policy: %+v %v", other, err)
	}
}

func TestTenantRetentionPolicyRejectsInvalidInputAndRollsBackOnAuditFailure(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, `INSERT INTO admin_grants
 (id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')`, retentionGrant, tenantA, adminM, orgA)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	for _, input := range []struct {
		version int64
		days    int
		ref     string
	}{{-1, 365, "CAB"}, {0, 0, "CAB"}, {0, 3651, "CAB"}, {0, 365, ""},
		{0, 365, "\n"}} {
		if _, err := svc.SetRetentionPolicy(context.Background(), admin, input.version, input.days, input.ref); !errors.Is(err, access.ErrInvalidRetentionPolicy) {
			t.Fatalf("invalid input %+v: %v", input, err)
		}
	}
	run(t, conn, `CREATE FUNCTION reject_retention_audit() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.action='retention_policy_update' THEN RAISE EXCEPTION 'audit down'; END IF; RETURN NEW; END $$`)
	run(t, conn, `CREATE TRIGGER reject_retention_audit BEFORE INSERT ON audit_events
 FOR EACH ROW EXECUTE FUNCTION reject_retention_audit()`)
	if _, err := svc.SetRetentionPolicy(context.Background(), admin, 0, 730, "CAB-2026-02"); !errors.Is(err, access.ErrAuditUnavailable) {
		t.Fatalf("audit outage: %v", err)
	}
	var days int
	var version int64
	if err := conn.QueryRow(context.Background(), "SELECT message_body_retention_days,retention_version FROM tenants WHERE id=$1", tenantA).Scan(&days, &version); err != nil || days != 365 || version != 0 {
		t.Fatalf("audit outage changed policy: %d %d %v", days, version, err)
	}
	var historyCount int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM tenant_retention_policy_history
 WHERE tenant_id=$1`, tenantA).Scan(&historyCount); err != nil || historyCount != 0 {
		t.Fatalf("audit outage left approval history: %d %v", historyCount, err)
	}
}

func TestTenantRetentionPolicyRechecksGrantAfterTenantLockWait(t *testing.T) {
	first := testDB(t)
	seedAccess(t, first)
	run(t, first, `INSERT INTO admin_grants
 (id,tenant_id,membership_id,membership_organization_id,role,effective_from,effective_to)
 VALUES ($1,$2,$3,$4,'group_admin','2020-01-01',$5)`,
		retentionGrant, tenantA, adminM, orgA, fixedTime.Add(time.Hour))
	second := secondConnection(t, first)
	blocker, err := first.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := blocker.QueryRow(context.Background(), "SELECT id FROM tenants WHERE id=$1 FOR UPDATE", tenantA).
		Scan(new(string)); err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	clock.Store(fixedTime.UnixNano())
	started := make(chan struct{})
	svc := access.Service{DB: &syncedDB{conn: second, started: started},
		Now: func() time.Time { return time.Unix(0, clock.Load()) }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := svc.SetRetentionPolicy(ctx,
			access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM},
			0, 730, "CAB-after-wait")
		result <- err
	}()
	<-started
	time.Sleep(50 * time.Millisecond)
	clock.Store(fixedTime.Add(2 * time.Hour).UnixNano())
	if err := blocker.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("expired grant changed retention: %v", err)
	}
	var days int
	if err := first.QueryRow(context.Background(), "SELECT message_body_retention_days FROM tenants WHERE id=$1", tenantA).Scan(&days); err != nil || days != 365 {
		t.Fatalf("retention after expired grant: %d %v", days, err)
	}
}

func TestTenantRetentionPolicyConcurrentVersionWriters(t *testing.T) {
	first := testDB(t)
	seedAccess(t, first)
	run(t, first, `INSERT INTO admin_grants
 (id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')`, retentionGrant, tenantA, adminM, orgA)
	second := secondConnection(t, first)
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for _, conn := range []*pgx.Conn{first, second} {
		go func(conn *pgx.Conn) {
			_, err := (access.Service{DB: conn, Now: func() time.Time { return fixedTime }}).
				SetRetentionPolicy(ctx, admin, 0, 730, "CAB-concurrent")
			results <- err
		}(conn)
	}
	a, b := <-results, <-results
	if !((a == nil && errors.Is(b, access.ErrConflict)) ||
		(b == nil && errors.Is(a, access.ErrConflict))) {
		t.Fatalf("concurrent version updates: %v %v", a, b)
	}
	var version int64
	if err := first.QueryRow(context.Background(), "SELECT retention_version FROM tenants WHERE id=$1", tenantA).Scan(&version); err != nil || version != 1 {
		t.Fatalf("concurrent version: %d %v", version, err)
	}
}

func TestTenantRetentionPolicyCannotExtendAfterMessagesExist(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, `INSERT INTO admin_grants
 (id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')`, retentionGrant, tenantA, adminM, orgA)
	run(t, conn, `INSERT INTO conversations
 (id,tenant_id,direct_user_low_id,direct_user_high_id,direct_low_membership_id,
 direct_high_membership_id,created_by_user_id,last_seq)
 VALUES ('00000000-0000-4000-8000-000000000183',$1,$2,$3,$4,$5,$2,1)`,
		tenantA, adminA, personA, adminM, personM2)
	run(t, conn, `INSERT INTO messages
 (tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest)
 VALUES ($1,'00000000-0000-4000-8000-000000000183',1,$2,$3,
 '0199f04a-0000-7000-8000-000000000701','history',decode(repeat('ab',32),'hex'))`,
		tenantA, adminA, adminM)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	if _, err := svc.SetRetentionPolicy(context.Background(), admin, 0, 730, "CAB-extend"); !errors.Is(err, access.ErrConflict) {
		t.Fatalf("extension after messages: %v", err)
	}
	policy, err := svc.SetRetentionPolicy(context.Background(), admin, 0, 30, "CAB-shorten")
	if err != nil || policy.MessageBodyDays != 30 || policy.Version != 1 {
		t.Fatalf("approved reduction: %+v %v", policy, err)
	}
	if _, err := svc.SetRetentionPolicy(context.Background(), admin, 1, 365, "CAB-restore"); !errors.Is(err, access.ErrConflict) {
		t.Fatalf("extension after reduction: %v", err)
	}
}

func TestTenantRetentionPolicyPreservesEveryApprovedVersion(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, `INSERT INTO admin_grants
 (id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')`, retentionGrant, tenantA, adminM, orgA)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	if _, err := svc.SetRetentionPolicy(context.Background(), admin, 0, 730, "CAB-first"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetRetentionPolicy(context.Background(), admin, 1, 30, "CAB-second"); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(context.Background(), `SELECT version,message_body_retention_days,
 approval_reference,approved_by_user_id::text FROM tenant_retention_policy_history
 WHERE tenant_id=$1 ORDER BY version`, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var versions []int64
	var days []int
	var refs, approvers []string
	for rows.Next() {
		var version int64
		var day int
		var ref, approver string
		if err := rows.Scan(&version, &day, &ref, &approver); err != nil {
			t.Fatal(err)
		}
		versions, days = append(versions, version), append(days, day)
		refs, approvers = append(refs, ref), append(approvers, approver)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0] != 1 || versions[1] != 2 ||
		days[0] != 730 || days[1] != 30 || refs[0] != "CAB-first" ||
		refs[1] != "CAB-second" || approvers[0] != adminA || approvers[1] != adminA {
		t.Fatalf("approval history lost: versions=%v days=%v refs=%v approvers=%v",
			versions, days, refs, approvers)
	}
	reject(t, conn, `UPDATE tenant_retention_policy_history SET approval_reference='rewritten'
 WHERE tenant_id=$1 AND version=1`, tenantA)
	reject(t, conn, `DELETE FROM tenant_retention_policy_history
 WHERE tenant_id=$1 AND version=1`, tenantA)
}

func TestTenantRetentionPolicyDoesNotDeadlockWithMembershipEnd(t *testing.T) {
	first := testDB(t)
	seedAccess(t, first)
	run(t, first, `INSERT INTO admin_grants
 (id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')`, retentionGrant, tenantA, adminM, orgA)
	second, third := secondConnection(t, first), secondConnection(t, first)
	blocker, err := first.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := blocker.QueryRow(context.Background(), "SELECT id FROM tenants WHERE id=$1 FOR UPDATE", tenantA).
		Scan(new(string)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	setStarted, endStarted := make(chan struct{}), make(chan struct{})
	setResult, endResult := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := (access.Service{DB: &syncedDB{conn: second, started: setStarted},
			Now: func() time.Time { return fixedTime }}).
			SetRetentionPolicy(ctx, admin, 0, 730, "CAB-no-deadlock")
		setResult <- err
	}()
	<-setStarted
	time.Sleep(50 * time.Millisecond)
	go func() {
		endResult <- (access.Service{DB: &syncedDB{conn: third, started: endStarted},
			Now: func() time.Time { return fixedTime }}).
			EndMembership(ctx, admin, personM)
	}()
	<-endStarted
	time.Sleep(50 * time.Millisecond)
	if err := blocker.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if setErr, endErr := <-setResult, <-endResult; setErr != nil || endErr != nil {
		t.Fatalf("retention/end membership lock cycle: set=%v end=%v", setErr, endErr)
	}
}
