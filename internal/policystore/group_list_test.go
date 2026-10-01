package policystore_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

const groupListOtherMembership = "00000000-0000-4000-8000-000000000941"

func createListedGroup(t *testing.T, svc policystore.Service,
	clientID, name string, members ...string) string {
	t.Helper()
	group, err := svc.CreateGroup(context.Background(), publisher(), policystore.CreateGroupRequest{
		ClientRequestID: clientID, Name: name, MemberMembershipIDs: members,
	})
	if err != nil {
		t.Fatal(err)
	}
	return group.ID
}

func TestListGroupsPagesCurrentMembershipAndBlockedGroups(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, `INSERT INTO user_organizations
 (id,tenant_id,user_id,organization_id,effective_from)
 VALUES ($1,$2,$3,$4,'2020-01-01')`, groupListOtherMembership, tenantA, adminA, orgA2)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	older := createListedGroup(t, svc, "00000000-0000-4000-8000-000000000942", "老项目群", targetM2)
	newer := createListedGroup(t, svc, "00000000-0000-4000-8000-000000000943", "新项目群", targetM2)
	run(t, conn, "UPDATE conversations SET updated_at=$2,last_seq=2 WHERE id=$1", older, at.Add(-time.Hour))
	run(t, conn, "UPDATE conversations SET updated_at=$2,last_seq=5,status='policy_blocked' WHERE id=$1", newer, at)

	otherRole := publisher()
	otherRole.ActingMembershipID = groupListOtherMembership
	first, err := svc.ListGroups(context.Background(), otherRole, "", 1)
	if err != nil || len(first.Groups) != 1 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	if got := first.Groups[0]; got.ID != newer || got.Name != "新项目群" ||
		got.Status != "policy_blocked" || got.Role != "owner" ||
		got.SourceMembershipID != adminM || got.LastSeq != 5 || !got.UpdatedAt.Equal(at) {
		t.Fatalf("blocked group metadata: %+v", got)
	}
	second, err := svc.ListGroups(context.Background(), otherRole, first.NextCursor, 1)
	if err != nil || len(second.Groups) != 1 || second.Groups[0].ID != older ||
		second.Groups[0].Status != "active" || second.Groups[0].LastSeq != 2 ||
		second.HasMore || second.NextCursor != "" {
		t.Fatalf("second page: %+v %v", second, err)
	}
	member := access.TrustedIdentity{TenantID: tenantA, UserID: personA, ActingMembershipID: targetM}
	memberPage, err := svc.ListGroups(context.Background(), member, "", 20)
	if err != nil || len(memberPage.Groups) != 2 || memberPage.Groups[0].Role != "member" ||
		memberPage.Groups[0].SourceMembershipID != targetM2 {
		t.Fatalf("member using another valid role: %+v %v", memberPage, err)
	}
	foreign, err := svc.ListGroups(context.Background(), access.TrustedIdentity{
		TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}, "", 20)
	if err != nil || len(foreign.Groups) != 0 || foreign.HasMore {
		t.Fatalf("cross-tenant group leak: %+v %v", foreign, err)
	}
}

func TestListGroupsExcludesDepartedAndEndedGroups(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	left := createListedGroup(t, svc, "00000000-0000-4000-8000-000000000944", "已退群", targetM2)
	ended := createListedGroup(t, svc, "00000000-0000-4000-8000-000000000945", "已结束", targetM2)
	active := createListedGroup(t, svc, "00000000-0000-4000-8000-000000000946", "仍在群", targetM2)
	interval := groupIntervalFor(t, conn, left, personA)
	if _, err := svc.LeaveGroup(context.Background(), groupMemberIdentity(), left, interval); err != nil {
		t.Fatal(err)
	}
	run(t, conn, "UPDATE conversations SET status='ended' WHERE id=$1", ended)
	page, err := svc.ListGroups(context.Background(), groupMemberIdentity(), "", 20)
	if err != nil || len(page.Groups) != 1 || page.Groups[0].ID != active {
		t.Fatalf("left or ended group listed: %+v %v", page, err)
	}
}

func TestListGroupsRejectsInvalidIdentityCursorAndAuditFailure(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	for _, limit := range []int{0, 51} {
		if _, err := svc.ListGroups(context.Background(), publisher(), "", limit); !errors.Is(err, policystore.ErrInvalidGroupListRequest) {
			t.Fatalf("invalid limit %d: %v", limit, err)
		}
	}
	if _, err := svc.ListGroups(context.Background(), publisher(), "not-a-cursor", 20); !errors.Is(err, policystore.ErrInvalidGroupListRequest) {
		t.Fatalf("invalid cursor: %v", err)
	}
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
	if _, err := svc.ListGroups(context.Background(), publisher(), "", 20); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("frozen actor listed: %v", err)
	}
	run(t, conn, "UPDATE users SET status='active' WHERE id=$1", adminA)
	run(t, conn, "ALTER TABLE audit_events ADD CONSTRAINT group_list_audit_disabled CHECK (action <> 'group_list') NOT VALID")
	if _, err := svc.ListGroups(context.Background(), publisher(), "", 20); !errors.Is(err, policystore.ErrAuditUnavailable) {
		t.Fatalf("audit outage returned list: %v", err)
	}
}

func TestListGroupsKeepsPageOrderWhenGroupUpdatesDuringRowLockWait(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	a := createListedGroup(t, svc, "00000000-0000-4000-8000-000000000947", "A", targetM2)
	b := createListedGroup(t, svc, "00000000-0000-4000-8000-000000000948", "B", targetM2)
	c := createListedGroup(t, svc, "00000000-0000-4000-8000-000000000949", "C", targetM2)
	run(t, conn, "UPDATE conversations SET updated_at=$2 WHERE id=$1", a, at)
	run(t, conn, "UPDATE conversations SET updated_at=$2 WHERE id=$1", b, at.Add(-30*time.Minute))
	run(t, conn, "UPDATE conversations SET updated_at=$2 WHERE id=$1", c, at.Add(-time.Hour))
	var schema string
	if err := conn.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	open := func() *pgx.Conn {
		connection, err := pgx.Connect(context.Background(), os.Getenv("IM_TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Exec(context.Background(), "SET search_path TO "+schema+", public"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { connection.Close(context.Background()) })
		return connection
	}
	blocker, reader := open(), open()
	var readerPID int
	if err := reader.QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&readerPID); err != nil {
		t.Fatal(err)
	}
	blockTx, err := blocker.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer blockTx.Rollback(context.Background())
	if _, err := blockTx.Exec(context.Background(), "UPDATE conversations SET updated_at=$2 WHERE id=$1", b, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	type result struct {
		page policystore.GroupListPage
		err  error
	}
	done := make(chan result, 1)
	go func() {
		page, err := (policystore.Service{DB: reader, Now: func() time.Time { return at }}).
			ListGroups(context.Background(), publisher(), "", 2)
		done <- result{page, err}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var waitType *string
		if err := conn.QueryRow(context.Background(), "SELECT wait_event_type FROM pg_stat_activity WHERE pid=$1", readerPID).Scan(&waitType); err != nil {
			t.Fatal(err)
		}
		if waitType != nil && *waitType == "Lock" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("group list did not wait on concurrent update")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := blockTx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	var first result
	select {
	case first = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("group list did not finish after update")
	}
	if first.err != nil || len(first.page.Groups) != 2 || !first.page.HasMore ||
		first.page.Groups[0].ID != b || first.page.Groups[1].ID != a {
		t.Fatalf("first page reordered while waiting: %+v %v", first.page, first.err)
	}
	second, err := (policystore.Service{DB: reader, Now: func() time.Time { return at }}).
		ListGroups(context.Background(), publisher(), first.page.NextCursor, 2)
	if err != nil || len(second.Groups) != 1 || second.Groups[0].ID != c || second.HasMore {
		t.Fatalf("page cursor repeated or skipped group: %+v %v", second, err)
	}
}
