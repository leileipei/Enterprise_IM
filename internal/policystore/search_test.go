package policystore_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

const (
	searchHiddenUser = "00000000-0000-4000-8000-000000000281"
	searchHiddenMem  = "00000000-0000-4000-8000-000000000282"
	searchShownUser  = "00000000-0000-4000-8000-000000000283"
	searchShownMem   = "00000000-0000-4000-8000-000000000284"
)

type nameCandidateGateDB struct {
	conn    *pgx.Conn
	reached chan struct{}
	resume  chan struct{}
}

func (db nameCandidateGateDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := db.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return nameCandidateGateTx{Tx: tx, reached: db.reached, resume: db.resume}, nil
}

type nameCandidateGateTx struct {
	pgx.Tx
	reached chan struct{}
	resume  chan struct{}
}

func (tx nameCandidateGateTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := tx.Tx.Query(ctx, sql, args...)
	if err != nil || !strings.Contains(sql, "SELECT id::text FROM users") {
		return rows, err
	}
	return nameCandidateGateRows{Rows: rows, reached: tx.reached, resume: tx.resume}, nil
}

type nameCandidateGateRows struct {
	pgx.Rows
	reached chan struct{}
	resume  chan struct{}
}

func (rows nameCandidateGateRows) Close() {
	rows.Rows.Close()
	close(rows.reached)
	<-rows.resume
}

func TestDirectoryNameSearchOrdersByCurrentEmployeeNumberAfterConcurrentRename(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,'A003','用户 B')", searchShownUser, tenantA)
	run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01')", searchShownMem, tenantA, searchShownUser, orgA)
	ctx := context.Background()
	var searchPath string
	if err := conn.QueryRow(ctx, "SHOW search_path").Scan(&searchPath); err != nil {
		t.Fatal(err)
	}
	updater, err := pgx.Connect(ctx, os.Getenv("IM_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { updater.Close(ctx) })
	if _, err := updater.Exec(ctx, "SET search_path TO "+searchPath); err != nil {
		t.Fatal(err)
	}
	reached, resume := make(chan struct{}), make(chan struct{})
	svc := policystore.Service{DB: nameCandidateGateDB{conn: conn, reached: reached, resume: resume}, Now: func() time.Time { return at }}
	type result struct {
		page policystore.DirectorySearchPage
		err  error
	}
	finished := make(chan result, 1)
	go func() {
		page, err := svc.SearchVisiblePeople(ctx, publisher(), "用户", 1)
		finished <- result{page: page, err: err}
	}()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		close(resume)
		t.Fatal("name search did not enumerate candidates")
	}
	updaterTx, err := updater.Begin(ctx)
	if err != nil {
		close(resume)
		t.Fatal(err)
	}
	for _, change := range []struct{ id, number string }{{personA, "TEMP"}, {searchShownUser, "A002"}, {personA, "A003"}} {
		if _, err := updaterTx.Exec(ctx, "UPDATE users SET global_employee_no=$2 WHERE id=$1", change.id, change.number); err != nil {
			updaterTx.Rollback(ctx)
			close(resume)
			t.Fatal(err)
		}
	}
	if err := updaterTx.Commit(ctx); err != nil {
		close(resume)
		t.Fatal(err)
	}
	close(resume)
	got := <-finished
	if got.err != nil || len(got.page.People) != 1 || got.page.People[0].ID != searchShownUser ||
		got.page.People[0].EmployeeNo != "A002" || !got.page.HasMore {
		t.Fatalf("search used stale candidate ordering: %+v %v", got.page, got.err)
	}
}

func TestDirectoryNameSearchFiltersBeforeVisiblePagination(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,'A003','用户 B'),($3,$2,'A004','用户 C')", searchHiddenUser, tenantA, searchShownUser)
	run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01'),($5,$2,$6,$7,'2020-01-01')", searchHiddenMem, tenantA, searchHiddenUser, orgA2, searchShownMem, searchShownUser, orgA)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	page, err := svc.SearchVisiblePeople(context.Background(), publisher(), " 用户 ", 1)
	if err != nil || len(page.People) != 1 || page.People[0].ID != personA || !page.HasMore ||
		len(page.People[0].Memberships) != 1 || page.People[0].Memberships[0].MembershipID != targetM2 {
		t.Fatalf("first visible page: %+v %v", page, err)
	}
	page, err = svc.SearchVisiblePeople(context.Background(), publisher(), "用户", 2)
	if err != nil || len(page.People) != 2 || page.People[0].ID != personA || page.People[1].ID != searchShownUser || page.HasMore {
		t.Fatalf("hidden candidate affected pagination: %+v %v", page, err)
	}
	var decisions, requests int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE action='directory_view'").Scan(&decisions); err != nil || decisions != 8 {
		t.Fatalf("membership decisions: %d %v", decisions, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='directory_name_search' AND outcome='allow' AND resource_id IS NULL").Scan(&requests); err != nil || requests != 2 {
		t.Fatalf("search audits: %d %v", requests, err)
	}
}

func TestDirectoryNameSearchPublishedAllowAndHardDeny(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	allow := policy.Rule{ID: "name-allow", TenantID: tenantA, Effect: policy.EffectAllow,
		Action: policy.ActionDirectoryView, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		RequestedBy: adminA, ApprovedBy: adminA, Reason: "项目通讯录",
		EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{allow}, "开放姓名搜索"); err != nil {
		t.Fatal(err)
	}
	page, err := svc.SearchVisiblePeople(context.Background(), publisher(), "用户 A", 20)
	if err != nil || len(page.People) != 1 || len(page.People[0].Memberships) != 2 {
		t.Fatalf("allow not applied: %+v %v", page, err)
	}
	hard := policy.Rule{ID: "name-deny", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionDirectoryView, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		Reason: "强制隔离", EffectiveFrom: at.Add(-time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 1, []policy.Rule{allow, hard}, "隔离姓名搜索"); err != nil {
		t.Fatal(err)
	}
	page, err = svc.SearchVisiblePeople(context.Background(), publisher(), "用户 A", 20)
	if err != nil || len(page.People) != 1 || len(page.People[0].Memberships) != 1 || page.People[0].Memberships[0].MembershipID != targetM2 {
		t.Fatalf("hard deny not applied: %+v %v", page, err)
	}
}

func TestDirectoryNameSearchTreatsWildcardsLiterallyAndHidesOtherTenant(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "UPDATE users SET display_name='研发_员%' WHERE id=$1", personA)
	run(t, conn, "INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,'A003','研发X员Y')", searchHiddenUser, tenantA)
	run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01')", searchHiddenMem, tenantA, searchHiddenUser, orgA)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	page, err := svc.SearchVisiblePeople(context.Background(), publisher(), "_员%", 20)
	if err != nil || len(page.People) != 1 || page.People[0].ID != personA {
		t.Fatalf("wildcards matched as pattern: %+v %v", page, err)
	}
	run(t, conn, "UPDATE users SET display_name=$2 WHERE id=$1", searchHiddenUser, "研发\\员")
	page, err = svc.SearchVisiblePeople(context.Background(), publisher(), "\\员", 20)
	if err != nil || len(page.People) != 1 || page.People[0].ID != searchHiddenUser {
		t.Fatalf("backslash matched as pattern: %+v %v", page, err)
	}
	page, err = svc.SearchVisiblePeople(context.Background(), publisher(), "用户 B", 20)
	if err != nil || len(page.People) != 0 || page.HasMore {
		t.Fatalf("cross-tenant result leaked: %+v %v", page, err)
	}
}

func TestDirectoryNameSearchRejectsBroadAndInvalidQueries(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, `INSERT INTO users (id,tenant_id,global_employee_no,display_name)
SELECT gen_random_uuid(),$1,'W'||g::text,'广泛测试' FROM generate_series(1,500) g`, tenantA)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if page, err := svc.SearchVisiblePeople(context.Background(), publisher(), "广泛", 20); err != nil || len(page.People) != 0 || page.HasMore {
		t.Fatalf("500 hidden candidates should return an empty visible page: %+v %v", page, err)
	}
	run(t, conn, "INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES (gen_random_uuid(),$1,'W501','广泛测试')", tenantA)
	if page, err := svc.SearchVisiblePeople(context.Background(), publisher(), "广泛", 20); !errors.Is(err, policystore.ErrDirectorySearchTooBroad) || len(page.People) != 0 {
		t.Fatalf("broad search returned partial page: %+v %v", page, err)
	}
	for _, q := range []string{"", " ", "单", "A\x00B", string([]byte{'A', 0xff}), strings.Repeat("A", 101)} {
		if _, err := svc.SearchVisiblePeople(context.Background(), publisher(), q, 20); !errors.Is(err, policystore.ErrInvalidDirectorySearch) {
			t.Fatalf("invalid query %q accepted: %v", q, err)
		}
	}
	for _, limit := range []int{0, 21} {
		if _, err := svc.SearchVisiblePeople(context.Background(), publisher(), "用户", limit); !errors.Is(err, policystore.ErrInvalidDirectorySearch) {
			t.Fatalf("invalid limit %d accepted: %v", limit, err)
		}
	}
	var requests int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='directory_name_search' AND outcome='deny'").Scan(&requests); err != nil || requests != 1 {
		t.Fatalf("broad search audit: %d %v", requests, err)
	}
}

func TestDirectoryNameSearchAuditFailureReturnsNoPeople(t *testing.T) {
	for _, tc := range []struct{ name, constraint string }{
		{"request", "ALTER TABLE audit_events ADD CONSTRAINT reject_name_search CHECK (action <> 'directory_name_search')"},
		{"decision", "ALTER TABLE policy_decision_events ADD CONSTRAINT reject_name_decision CHECK (action <> 'directory_view')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := db(t)
			seed(t, conn)
			run(t, conn, tc.constraint)
			svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
			page, err := svc.SearchVisiblePeople(context.Background(), publisher(), "用户 A", 20)
			if !errors.Is(err, policystore.ErrAuditUnavailable) || len(page.People) != 0 {
				t.Fatalf("people returned without audit: %+v %v", page, err)
			}
			var count int
			if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE action='directory_view'").Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial decisions committed: %d %v", count, err)
			}
		})
	}
}

func TestDirectoryNameSearchHidesFrozenTargetsAndRejectsInvalidActor(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", personA)
	page, err := svc.SearchVisiblePeople(context.Background(), publisher(), "用户 A", 20)
	if err != nil || len(page.People) != 0 || page.HasMore {
		t.Fatalf("frozen target visible: %+v %v", page, err)
	}
	run(t, conn, "UPDATE user_organizations SET status='ended' WHERE id=$1", adminM)
	if page, err := svc.SearchVisiblePeople(context.Background(), publisher(), "用户 A", 20); !errors.Is(err, policystore.ErrForbidden) || len(page.People) != 0 {
		t.Fatalf("ended acting membership accepted: %+v %v", page, err)
	}
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
	if page, err := svc.SearchVisiblePeople(context.Background(), publisher(), "用户 A", 20); !errors.Is(err, policystore.ErrForbidden) || len(page.People) != 0 {
		t.Fatalf("frozen actor accepted: %+v %v", page, err)
	}
}
