package access_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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

func TestEndMembershipKeepsOtherOrganizationAndGroupAccount(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, "INSERT INTO user_departments (id,tenant_id,user_organization_id,organization_id,department_id,effective_from) VALUES ($1,$2,$3,$4,$5,'2020-01-01')", "00000000-0000-4000-8000-000000000182", tenantA, personM, orgA, depA)
	run(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,scope_organization_id,effective_from) VALUES ($1,$2,$3,$4,'organization_admin',$5,'2020-01-01')", "00000000-0000-4000-8000-000000000175", tenantA, adminM, orgA, orgA)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	if err := svc.EndMembership(context.Background(), identity(), personM); err != nil {
		t.Fatal(err)
	}
	var memberStatus, deptStatus, userStatus, otherStatus string
	var memberEnd, deptEnd time.Time
	if err := conn.QueryRow(context.Background(), "SELECT status,effective_to FROM user_organizations WHERE id=$1", personM).Scan(&memberStatus, &memberEnd); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT status,effective_to FROM user_departments WHERE user_organization_id=$1", personM).Scan(&deptStatus, &deptEnd); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT status FROM users WHERE id=$1", personA).Scan(&userStatus); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT status FROM user_organizations WHERE id=$1", personM2).Scan(&otherStatus); err != nil {
		t.Fatal(err)
	}
	if memberStatus != "ended" || deptStatus != "ended" || userStatus != "active" || otherStatus != "active" ||
		!memberEnd.Equal(fixedTime) || !deptEnd.Equal(fixedTime) {
		t.Fatalf("end state: member=%s %v department=%s %v user=%s other=%s", memberStatus, memberEnd, deptStatus, deptEnd, userStatus, otherStatus)
	}
	var count int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='membership_end' AND outcome='allow' AND resource_id=$1", personM).Scan(&count); err != nil || count != 1 {
		t.Fatalf("allow audit count=%d err=%v", count, err)
	}
}

func TestEndMembershipRejectsOutOfScopeAndCrossTenant(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,scope_organization_id,effective_from) VALUES ($1,$2,$3,$4,'organization_admin',$5,'2020-01-01')", "00000000-0000-4000-8000-000000000176", tenantA, adminM, orgA, orgA)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	for _, target := range []string{personM2, personBM} {
		if err := svc.EndMembership(context.Background(), identity(), target); !errors.Is(err, access.ErrNotFound) {
			t.Fatalf("target %s allowed: %v", target, err)
		}
	}
	var count int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='membership_end' AND outcome='deny'").Scan(&count); err != nil || count != 2 {
		t.Fatalf("deny audit count=%d err=%v", count, err)
	}
	var status string
	if err := conn.QueryRow(context.Background(), "SELECT status FROM user_organizations WHERE id=$1", personM2).Scan(&status); err != nil || status != "active" {
		t.Fatalf("out-of-scope membership changed: %s %v", status, err)
	}
}

func TestEndMembershipConflictsOnRepeatOrFutureDepartment(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')", "00000000-0000-4000-8000-000000000177", tenantA, adminM, orgA)
	run(t, conn, "INSERT INTO user_departments (id,tenant_id,user_organization_id,organization_id,department_id,effective_from) VALUES ($1,$2,$3,$4,$5,'2027-01-01')", "00000000-0000-4000-8000-000000000183", tenantA, personM, orgA, depA)
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	if err := svc.EndMembership(context.Background(), identity(), personM); !errors.Is(err, access.ErrConflict) {
		t.Fatalf("future department was not rejected: %v", err)
	}
	run(t, conn, "DELETE FROM user_departments WHERE user_organization_id=$1", personM)
	if err := svc.EndMembership(context.Background(), identity(), personM); err != nil {
		t.Fatal(err)
	}
	if err := svc.EndMembership(context.Background(), identity(), personM); !errors.Is(err, access.ErrConflict) {
		t.Fatalf("repeat end was not rejected: %v", err)
	}
}

func TestEndMembershipRollsBackIfAuditFails(t *testing.T) {
	conn := testDB(t)
	seedAccess(t, conn)
	run(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')", "00000000-0000-4000-8000-000000000178", tenantA, adminM, orgA)
	run(t, conn, "ALTER TABLE audit_events ADD CONSTRAINT reject_allow_audit CHECK (outcome <> 'allow')")
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	if err := svc.EndMembership(context.Background(), identity(), personM); !errors.Is(err, access.ErrAuditUnavailable) {
		t.Fatalf("audit failure not returned: %v", err)
	}
	var status string
	var effectiveTo *time.Time
	if err := conn.QueryRow(context.Background(), "SELECT status,effective_to FROM user_organizations WHERE id=$1", personM).Scan(&status, &effectiveTo); err != nil || status != "active" || effectiveTo != nil {
		t.Fatalf("membership committed without audit: %s %v %v", status, effectiveTo, err)
	}
}

func secondConnection(t *testing.T, first *pgx.Conn) *pgx.Conn {
	t.Helper()
	var schema string
	if err := first.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(context.Background(), os.Getenv("IM_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	if _, err := conn.Exec(context.Background(), "SET search_path TO "+schema+", public"); err != nil {
		t.Fatal(err)
	}
	return conn
}

type twoCallBarrier struct {
	mu      sync.Mutex
	count   int
	release chan struct{}
}

func (b *twoCallBarrier) wait() {
	b.mu.Lock()
	b.count++
	if b.count == 2 {
		close(b.release)
	}
	b.mu.Unlock()
	select {
	case <-b.release:
	case <-time.After(500 * time.Millisecond):
	}
}

type syncedDB struct {
	conn    *pgx.Conn
	barrier *twoCallBarrier
	started chan struct{}
	once    sync.Once
}

func (db *syncedDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := db.conn.Begin(ctx)
	if err == nil && db.started != nil {
		db.once.Do(func() { close(db.started) })
	}
	if err != nil || db.barrier == nil {
		return tx, err
	}
	return syncedTx{Tx: tx, barrier: db.barrier}, nil
}

type syncedTx struct {
	pgx.Tx
	barrier *twoCallBarrier
}

func (tx syncedTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := tx.Tx.Query(ctx, sql, args...)
	if err != nil || !strings.Contains(sql, "FROM admin_grants") {
		return rows, err
	}
	return &syncedRows{Rows: rows, barrier: tx.barrier}, nil
}

type syncedRows struct {
	pgx.Rows
	barrier *twoCallBarrier
	once    sync.Once
}

func (rows *syncedRows) Close() {
	rows.Rows.Close()
	rows.once.Do(rows.barrier.wait)
}

func TestConcurrentSelfEndDoesNotDeadlock(t *testing.T) {
	first := testDB(t)
	seedAccess(t, first)
	run(t, first, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')", "00000000-0000-4000-8000-000000000179", tenantA, adminM, orgA)
	second := secondConnection(t, first)
	barrier := &twoCallBarrier{release: make(chan struct{})}
	services := []access.Service{
		{DB: &syncedDB{conn: first, barrier: barrier}, Now: func() time.Time { return fixedTime }},
		{DB: &syncedDB{conn: second, barrier: barrier}, Now: func() time.Time { return fixedTime }},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for _, svc := range services {
		go func(svc access.Service) { results <- svc.EndMembership(ctx, identity(), adminM) }(svc)
	}
	a, b := <-results, <-results
	if !((a == nil && errors.Is(b, access.ErrInvalidIdentity)) || (b == nil && errors.Is(a, access.ErrInvalidIdentity))) {
		t.Fatalf("concurrent self-end: %v, %v", a, b)
	}
	var count int
	if err := first.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='membership_end'").Scan(&count); err != nil || count != 2 {
		t.Fatalf("both attempts must be audited: count=%d err=%v", count, err)
	}
}

func TestEndMembershipRechecksGrantAfterLockWait(t *testing.T) {
	first := testDB(t)
	seedAccess(t, first)
	run(t, first, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,effective_from,effective_to) VALUES ($1,$2,$3,$4,'group_admin','2020-01-01',$5)", "00000000-0000-4000-8000-000000000180", tenantA, adminM, orgA, fixedTime.Add(time.Hour))
	second := secondConnection(t, first)
	blocker, err := first.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := blocker.QueryRow(context.Background(), "SELECT id FROM user_organizations WHERE id=$1 FOR UPDATE", personM).Scan(new(string)); err != nil {
		t.Fatal(err)
	}
	var now atomic.Int64
	now.Store(fixedTime.UnixNano())
	started := make(chan struct{})
	svc := access.Service{DB: &syncedDB{conn: second, started: started}, Now: func() time.Time { return time.Unix(0, now.Load()) }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- svc.EndMembership(ctx, identity(), personM) }()
	<-started
	time.Sleep(50 * time.Millisecond)
	now.Store(fixedTime.Add(2 * time.Hour).UnixNano())
	if err := blocker.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("expired grant still authorized: %v", err)
	}
	var status string
	if err := first.QueryRow(context.Background(), "SELECT status FROM user_organizations WHERE id=$1", personM).Scan(&status); err != nil || status != "active" {
		t.Fatalf("membership changed after grant expiry: %s %v", status, err)
	}
}

func TestManagedPersonRechecksGrantAfterLockWait(t *testing.T) {
	first := testDB(t)
	seedAccess(t, first)
	grantID := "00000000-0000-4000-8000-000000000184"
	run(t, first, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,effective_from,effective_to) VALUES ($1,$2,$3,$4,'group_admin','2020-01-01',$5)", grantID, tenantA, adminM, orgA, fixedTime.Add(time.Hour))
	second := secondConnection(t, first)
	blocker, err := first.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(context.Background(), "UPDATE admin_grants SET status='active' WHERE id=$1", grantID); err != nil {
		t.Fatal(err)
	}
	var now atomic.Int64
	now.Store(fixedTime.UnixNano())
	started := make(chan struct{})
	svc := access.Service{DB: &syncedDB{conn: second, started: started}, Now: func() time.Time { return time.Unix(0, now.Load()) }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		person access.Person
		err    error
	}
	results := make(chan result, 1)
	go func() {
		person, err := svc.GetManagedPerson(ctx, identity(), personA)
		results <- result{person, err}
	}()
	<-started
	time.Sleep(50 * time.Millisecond)
	now.Store(fixedTime.Add(2 * time.Hour).UnixNano())
	if err := blocker.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := <-results
	if !errors.Is(got.err, access.ErrNotFound) || got.person.ID != "" {
		t.Fatalf("expired grant exposed person: %+v", got)
	}
}
