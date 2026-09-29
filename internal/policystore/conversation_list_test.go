package policystore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

const (
	listPersonC     = "00000000-0000-4000-8000-000000000452"
	listMemberC     = "00000000-0000-4000-8000-000000000453"
	listAdminOtherM = "00000000-0000-4000-8000-000000000454"
	listChatA       = "00000000-0000-4000-8000-000000000461"
	listChatB       = "00000000-0000-4000-8000-000000000462"
)

func seedConversationList(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	seed(t, conn)
	run(t, conn, `INSERT INTO users (id,tenant_id,global_employee_no,display_name)
VALUES ($1,$2,'A003','用户 C')`, listPersonC, tenantA)
	run(t, conn, `INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from)
VALUES ($1,$2,$3,$4,'2020-01-01')`, listMemberC, tenantA, listPersonC, orgA)
	run(t, conn, `INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from)
VALUES ($1,$2,$3,$4,'2020-01-01')`, listAdminOtherM, tenantA, adminA, orgA2)
	for _, item := range []struct{ chat, peerUser, peerMember string }{
		{listChatA, personA, targetM2}, {listChatB, listPersonC, listMemberC},
	} {
		run(t, conn, `INSERT INTO conversations
(id,tenant_id,kind,direct_user_low_id,direct_user_high_id,direct_low_membership_id,
 direct_high_membership_id,created_by_user_id,updated_at,last_seq)
VALUES ($1,$2,'direct',$3,$4,$5,$6,$3,$7,3)`, item.chat, tenantA, adminA,
			item.peerUser, adminM, item.peerMember, at)
	}
}

func TestListDirectConversationsStablePagesAndCurrentMembership(t *testing.T) {
	conn := db(t)
	seedConversationList(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	first, err := svc.ListDirectConversations(context.Background(), publisher(), "", 1)
	if err != nil || len(first.Conversations) != 1 || first.Conversations[0].ID != listChatB ||
		!first.Conversations[0].PeerVisible || first.Conversations[0].PeerDisplayName != "用户 C" ||
		first.Conversations[0].PeerOrganizationName != "公司 A" || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	second, err := svc.ListDirectConversations(context.Background(), publisher(), first.NextCursor, 1)
	if err != nil || len(second.Conversations) != 1 || second.Conversations[0].ID != listChatA ||
		second.HasMore || second.NextCursor != "" {
		t.Fatalf("second page: %+v %v", second, err)
	}
	otherRole := publisher()
	otherRole.ActingMembershipID = listAdminOtherM
	for _, id := range []access.TrustedIdentity{
		otherRole,
		{TenantID: tenantA, UserID: listPersonC, ActingMembershipID: listMemberC},
		{TenantID: tenantB, UserID: personB, ActingMembershipID: otherM},
	} {
		page, err := svc.ListDirectConversations(context.Background(), id, first.NextCursor, 20)
		if err != nil {
			t.Fatalf("other identity: %v", err)
		}
		if id.UserID != listPersonC && len(page.Conversations) != 0 {
			t.Fatalf("foreign conversation visible to %+v: %+v", id, page)
		}
		if id.UserID == listPersonC && (len(page.Conversations) != 0 || page.HasMore) {
			t.Fatalf("cursor crossed into other participant: %+v", page)
		}
	}
}

func TestListDirectConversationsHidesPeerProfileAfterDirectoryDeny(t *testing.T) {
	conn := db(t)
	seedConversationList(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	denial := policy.Rule{ID: "hide-list-peer", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionDirectoryView, SourceMembershipID: adminM, TargetMembershipID: targetM2,
		SourceOrganizationID: orgA, TargetOrganizationID: orgA,
		Reason: "隔离", EffectiveFrom: at.Add(-time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{denial}, "隐藏同事"); err != nil {
		t.Fatal(err)
	}
	page, err := svc.ListDirectConversations(context.Background(), publisher(), "", 20)
	if err != nil || len(page.Conversations) != 2 {
		t.Fatalf("list: %+v %v", page, err)
	}
	for _, item := range page.Conversations {
		if item.ID == listChatA && (item.PeerVisible || item.PeerDisplayName != "" || item.PeerOrganizationName != "") {
			t.Fatalf("hidden peer leaked: %+v", item)
		}
	}
	var denied int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM policy_decision_events
WHERE tenant_id=$1 AND action='directory_view' AND allowed=false`, tenantA).Scan(&denied); err != nil || denied != 1 {
		t.Fatalf("missing denied decision audit: %d %v", denied, err)
	}
}

func TestListDirectConversationsRechecksPolicyTimeBeforeReturningProfile(t *testing.T) {
	conn := db(t)
	seedConversationList(t, conn)
	run(t, conn, "DELETE FROM conversations WHERE id=$1", listChatB)
	grantPublisher(t, conn)
	initial := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	denial := policy.Rule{ID: "future-list-deny", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionDirectoryView, SourceMembershipID: adminM, TargetMembershipID: targetM2,
		SourceOrganizationID: orgA, TargetOrganizationID: orgA,
		Reason: "即将生效的隔离", EffectiveFrom: at.Add(time.Second)}
	if _, err := initial.Publish(context.Background(), publisher(), 0, []policy.Rule{denial}, "预定隔离"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	svc := policystore.Service{DB: conn, Now: func() time.Time {
		calls++
		if calls >= 3 {
			return at.Add(2 * time.Second)
		}
		return at
	}}
	page, err := svc.ListDirectConversations(context.Background(), publisher(), "", 20)
	if err != nil || len(page.Conversations) != 1 || page.Conversations[0].PeerVisible ||
		page.Conversations[0].PeerDisplayName != "" || page.Conversations[0].PeerOrganizationName != "" {
		t.Fatalf("profile leaked after rule became active: %+v %v (now calls=%d)", page, err, calls)
	}
}

func TestListDirectConversationsRejectsBadInputsAndAuditOutage(t *testing.T) {
	conn := db(t)
	seedConversationList(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	for _, cursor := range []string{"not-a-cursor", "***", ""} {
		for _, limit := range []int{0, 51} {
			if _, err := svc.ListDirectConversations(context.Background(), publisher(), cursor, limit); !errors.Is(err, policystore.ErrInvalidConversationListRequest) {
				t.Fatalf("bad limit accepted: %q %d %v", cursor, limit, err)
			}
		}
	}
	if _, err := svc.ListDirectConversations(context.Background(), publisher(), "***", 20); !errors.Is(err, policystore.ErrInvalidConversationListRequest) {
		t.Fatalf("bad cursor accepted: %v", err)
	}
	run(t, conn, "UPDATE user_organizations SET status='ended' WHERE id=$1", adminM)
	if _, err := svc.ListDirectConversations(context.Background(), publisher(), "", 20); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("ended membership listed: %v", err)
	}
	run(t, conn, "UPDATE user_organizations SET status='active' WHERE id=$1", adminM)
	run(t, conn, "ALTER TABLE audit_events ADD CONSTRAINT list_audit_disabled CHECK (action <> 'conversation_list') NOT VALID")
	if _, err := svc.ListDirectConversations(context.Background(), publisher(), "", 20); !errors.Is(err, policystore.ErrAuditUnavailable) {
		t.Fatalf("audit outage returned list: %v", err)
	}
}
