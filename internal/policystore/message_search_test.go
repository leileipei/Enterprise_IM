package policystore_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestMessageSearchDirectMatchesVisibleBodyAndBindsCursor(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	for i, text := range []string{"普通消息", "工单 AbC%_\\ 第一条", "工单 abc%_\\ 第二条", "其他消息"} {
		if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 810+i), text); err != nil {
			t.Fatal(err)
		}
	}
	first, err := svc.SearchTextMessages(context.Background(), publisher(), directA, "  ABC%_\\  ", "", 1)
	if err != nil || len(first.Messages) != 1 || first.Messages[0].Seq != 2 || first.NextCursor == "" || !first.HasMore {
		t.Fatalf("first search %+v %v", first, err)
	}
	second, err := svc.SearchTextMessages(context.Background(), publisher(), strings.ToUpper(directA), "abc%_\\", first.NextCursor, 10)
	if err != nil || len(second.Messages) != 1 || second.Messages[0].Seq != 3 || second.NextCursor != "" || second.HasMore {
		t.Fatalf("second search %+v %v", second, err)
	}
	for _, change := range []struct {
		id      access.TrustedIdentity
		conv, q string
		group   bool
	}{
		{publisher(), directA, "工单", false}, {publisher(), directB, "abc%_\\", false},
		{groupMemberIdentity(), directA, "abc%_\\", false},
		{access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: targetM2}, directA, "abc%_\\", false},
		{access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}, directA, "abc%_\\", false},
		{publisher(), directA, "abc%_\\", true},
	} {
		if change.group {
			_, err = svc.SearchGroupTextMessages(context.Background(), change.id, change.conv, change.q, first.NextCursor, 1)
		} else {
			_, err = svc.SearchTextMessages(context.Background(), change.id, change.conv, change.q, first.NextCursor, 1)
		}
		if !errors.Is(err, policystore.ErrInvalidMessageSearch) {
			t.Fatalf("changed cursor accepted %+v: %v", change, err)
		}
	}
	// Literal punctuation is not a SQL wildcard; a hidden body's match must not enter results.
	run(t, conn, "UPDATE messages SET accepted_at=$1 WHERE conversation_id=$2 AND seq=2", at.Add(-365*24*time.Hour), directA)
	page, err := svc.SearchTextMessages(context.Background(), publisher(), directA, "abc%_\\", "", 20)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Seq != 3 {
		t.Fatalf("expired match %+v %v", page, err)
	}
	var count int
	if err = conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='message_search' AND outcome='allow' AND resource_id=$1", directA).Scan(&count); err != nil || count != 3 {
		t.Fatalf("search audit %d %v", count, err)
	}
}

func TestMessageSearchBoundedScanCanReturnEmptyPageWithContinuation(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 820), "不匹配"); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 503; i++ {
		text := "不匹配"
		if i >= 501 {
			text = "末尾目标"
		}
		run(t, conn, `INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,recipient_user_id,recipient_membership_id,sender_organization_id,recipient_organization_id,client_msg_id,text_body,content_digest,accepted_at)
   SELECT tenant_id,conversation_id,$1,sender_user_id,sender_membership_id,recipient_user_id,recipient_membership_id,sender_organization_id,recipient_organization_id,$2,$3,content_digest,accepted_at FROM messages WHERE conversation_id=$4 AND seq=1`, int64(i), clientUUIDv7(at, 900+i), text, directA)
	}
	first, err := svc.SearchTextMessages(context.Background(), publisher(), directA, "目标", "", 2)
	if err != nil || len(first.Messages) != 0 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("empty scan page %+v %v", first, err)
	}
	second, err := svc.SearchTextMessages(context.Background(), publisher(), directA, "目标", first.NextCursor, 2)
	if err != nil || len(second.Messages) != 2 || second.Messages[0].Seq != 501 || second.Messages[1].Seq != 502 || !second.HasMore {
		t.Fatalf("match limit %+v %v", second, err)
	}
	third, err := svc.SearchTextMessages(context.Background(), publisher(), directA, "目标", second.NextCursor, 2)
	if err != nil || len(third.Messages) != 1 || third.Messages[0].Seq != 503 || third.HasMore || third.NextCursor != "" {
		t.Fatalf("last page %+v %v", third, err)
	}
}

func TestMessageSearchGroupExcludesLeaveGapsAndInvalidSenders(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 1, adminA, adminM, "工单 可见")
	interval := groupIntervalFor(t, conn, group.ID, personA)
	if _, err = svc.LeaveGroup(context.Background(), groupMemberIdentity(), group.ID, interval); err != nil {
		t.Fatal(err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 2, adminA, adminM, "工单 离群缺口")
	if _, err = svc.InviteGroupMember(context.Background(), publisher(), group.ID, inviteGroupRequest(targetM2)); err != nil {
		t.Fatal(err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 3, adminA, adminM, "工单 再入群")
	insertGroupHistoryMessage(t, conn, group.ID, 4, groupUserC, groupMemberC, "工单 未入群发送者")
	run(t, conn, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", group.ID)
	page, err := svc.SearchGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, "工单", "", 20)
	if err != nil || len(page.Messages) != 2 || page.Messages[0].Seq != 1 || page.Messages[1].Seq != 3 || page.HasMore {
		t.Fatalf("group search %+v %v", page, err)
	}
	for _, m := range page.Messages {
		if m.Redacted || m.Text == "" || m.MessageID == "" {
			t.Fatalf("invalid match %+v", m)
		}
	}
	// Physical clearing and retention both hide text even before a cleaner runs.
	run(t, conn, "UPDATE messages SET accepted_at=$1 WHERE conversation_id=$2 AND seq=1", at.Add(-365*24*time.Hour), group.ID)
	run(t, conn, "UPDATE messages SET text_body=NULL,body_cleared_at=$1 WHERE conversation_id=$2 AND seq=3", at, group.ID)
	page, err = svc.SearchGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, "工单", "", 20)
	if err != nil || len(page.Messages) != 0 || page.HasMore {
		t.Fatalf("cleared/expired search %+v %v", page, err)
	}
}

func TestMessageSearchHardDenyAppliesToBothKinds(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 830), "工单 secret"); err != nil {
		t.Fatal(err)
	}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 1, adminA, adminM, "工单 secret")
	hard := policy.Rule{ID: "search-hard", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, EffectiveFrom: at.Add(-time.Hour), Reason: "search revoked"}
	if _, err = svc.Publish(context.Background(), publisher(), 0, []policy.Rule{hard}, "search revoked"); err != nil {
		t.Fatal(err)
	}
	direct, err := svc.SearchTextMessages(context.Background(), publisher(), directA, "工单", "", 20)
	if err != nil || len(direct.Messages) != 0 {
		t.Fatalf("hard denied direct %+v %v", direct, err)
	}
	g, err := svc.SearchGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, "工单", "", 20)
	if err != nil || len(g.Messages) != 0 {
		t.Fatalf("hard denied group %+v %v", g, err)
	}
}

func TestMessageSearchRejectsInvalidInputAndAuditFailure(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	for _, q := range []string{"", " ", "字", strings.Repeat("字", 101), "ab\x00", string([]byte{'a', 0xff})} {
		if _, err := svc.SearchTextMessages(context.Background(), publisher(), directA, q, "", 20); !errors.Is(err, policystore.ErrInvalidMessageSearch) {
			t.Fatalf("invalid q %q %v", q, err)
		}
	}
	for _, cursor := range []string{"bad", "e30", strings.Repeat("a", 2049)} {
		if _, err := svc.SearchTextMessages(context.Background(), publisher(), directA, "工单", cursor, 20); !errors.Is(err, policystore.ErrInvalidMessageSearch) {
			t.Fatalf("invalid cursor %q %v", cursor, err)
		}
	}
	for _, limit := range []int{0, 51} {
		if _, err := svc.SearchTextMessages(context.Background(), publisher(), directA, "工单", "", limit); !errors.Is(err, policystore.ErrInvalidMessageSearch) {
			t.Fatalf("limit %d %v", limit, err)
		}
	}
	if _, err := svc.SearchTextMessages(context.Background(), publisher(), "bad", "工单", "", 20); !errors.Is(err, policystore.ErrInvalidMessageSearch) {
		t.Fatal(err)
	}
	foreign := access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}
	if _, err := svc.SearchTextMessages(context.Background(), foreign, directA, "工单", "", 20); !errors.Is(err, policystore.ErrMessageNotAvailable) {
		t.Fatalf("tenant isolation %v", err)
	}
	run(t, conn, `ALTER TABLE audit_events ADD CONSTRAINT reject_search CHECK(action <> 'message_search') NOT VALID`)
	page, err := svc.SearchTextMessages(context.Background(), publisher(), directA, "工单", "", 20)
	if !errors.Is(err, policystore.ErrAuditUnavailable) || len(page.Messages) != 0 || page.NextCursor != "" {
		t.Fatalf("audit fail open %+v %v", page, err)
	}
}

func TestMessageSearchRechecksIdentityOnEveryPageAndAtReturn(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	for i := 0; i < 2; i++ {
		if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 840+i), "工单内容"); err != nil {
			t.Fatal(err)
		}
	}
	first, err := svc.SearchTextMessages(context.Background(), publisher(), directA, "工单", "", 1)
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first %+v %v", first, err)
	}
	run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at.Add(time.Second), adminM)
	calls := 0
	late := policystore.Service{DB: conn, Now: func() time.Time {
		calls++
		if calls >= 3 {
			return at.Add(2 * time.Second)
		}
		return at
	}}
	page, err := late.SearchTextMessages(context.Background(), publisher(), directA, "工单", first.NextCursor, 1)
	if !errors.Is(err, policystore.ErrForbidden) || len(page.Messages) != 0 {
		t.Fatalf("expired before return %+v %v", page, err)
	}
	svc.Now = func() time.Time { return at.Add(2 * time.Second) }
	if _, err = svc.SearchTextMessages(context.Background(), publisher(), directA, "工单", first.NextCursor, 1); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("expired next page %v", err)
	}
}

func TestMessageSearchLegalHoldDoesNotExtendVisibility(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 850), "工单保全"); err != nil {
		t.Fatal(err)
	}
	holdSvc := access.Service{DB: conn, Now: func() time.Time { return at }}
	if _, _, err := holdSvc.PlaceLegalHold(context.Background(), publisher(), directA, "00000000-0000-4000-8000-000000008f18", "CASE-SEARCH"); err != nil {
		t.Fatal(err)
	}
	run(t, conn, "UPDATE messages SET accepted_at=$1 WHERE conversation_id=$2", at.Add(-365*24*time.Hour), directA)
	page, err := svc.SearchTextMessages(context.Background(), publisher(), directA, "工单", "", 20)
	if err != nil || len(page.Messages) != 0 || page.HasMore {
		t.Fatalf("preserved expired body leaked %+v %v", page, err)
	}
	var body string
	if err := conn.QueryRow(context.Background(), "SELECT text_body FROM messages WHERE conversation_id=$1", directA).Scan(&body); err != nil || body != "工单保全" {
		t.Fatalf("hold fixture %q %v", body, err)
	}
}

func TestMessageSearchGroupIdentityAndAuditFailClosed(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 1, adminA, adminM, "工单正文")
	outsider := access.TrustedIdentity{TenantID: tenantA, UserID: groupUserC, ActingMembershipID: groupMemberC}
	if _, err = svc.SearchGroupTextMessages(context.Background(), outsider, group.ID, "工单", "", 20); !errors.Is(err, policystore.ErrMessageNotAvailable) {
		t.Fatalf("nonmember %v", err)
	}
	foreign := access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}
	if _, err = svc.SearchGroupTextMessages(context.Background(), foreign, group.ID, "工单", "", 20); !errors.Is(err, policystore.ErrMessageNotAvailable) {
		t.Fatalf("foreign group %v", err)
	}
	run(t, conn, `ALTER TABLE audit_events ADD CONSTRAINT reject_group_search CHECK(action <> 'message_search') NOT VALID`)
	page, err := svc.SearchGroupTextMessages(context.Background(), publisher(), group.ID, "工单", "", 20)
	if !errors.Is(err, policystore.ErrAuditUnavailable) || len(page.Messages) != 0 || page.NextCursor != "" {
		t.Fatalf("group audit fail open %+v %v", page, err)
	}
}

type searchSnapshotGateDB struct {
	conn            *pgx.Conn
	reached, resume chan struct{}
}

func (db searchSnapshotGateDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := db.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &searchSnapshotGateTx{Tx: tx, reached: db.reached, resume: db.resume}, nil
}

type searchSnapshotGateTx struct {
	pgx.Tx
	reached, resume chan struct{}
	once            sync.Once
}

func (tx *searchSnapshotGateTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "FOR SHARE OF t,u,m,o,l") {
		tx.once.Do(func() {
			// Establish a real PostgreSQL snapshot before the actor is revoked.
			var n int
			_ = tx.Tx.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&n)
			close(tx.reached)
			select {
			case <-tx.resume:
			case <-ctx.Done():
			}
		})
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func TestMessageSearchRechecksRevocationWithRepeatableReadDefault(t *testing.T) {
	for _, kind := range []string{"direct", "group"} {
		t.Run(kind, func(t *testing.T) {
			conn := db(t)
			seedDirectConversation(t, conn)
			svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
			conv := directA
			if kind == "group" {
				g, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
				if err != nil {
					t.Fatal(err)
				}
				conv = g.ID
				insertGroupHistoryMessage(t, conn, conv, 1, adminA, adminM, "工单正文")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var searchPath string
			if err := conn.QueryRow(ctx, "SHOW search_path").Scan(&searchPath); err != nil {
				t.Fatal(err)
			}
			updater, err := pgx.Connect(ctx, os.Getenv("IM_TEST_DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			defer updater.Close(context.Background())
			run(t, updater, "SET search_path TO "+searchPath)
			run(t, conn, "SET default_transaction_isolation='repeatable read'")
			reached, resume := make(chan struct{}), make(chan struct{})
			search := policystore.Service{DB: searchSnapshotGateDB{conn, reached, resume}, Now: func() time.Time { return at }}
			type result struct {
				page policystore.MessageSearchPage
				err  error
			}
			done := make(chan result, 1)
			go func() {
				var p policystore.MessageSearchPage
				var e error
				if kind == "group" {
					p, e = search.SearchGroupTextMessages(ctx, publisher(), conv, "工单", "", 20)
				} else {
					p, e = search.SearchTextMessages(ctx, publisher(), conv, "工单", "", 20)
				}
				done <- result{p, e}
			}()
			select {
			case <-reached:
			case <-ctx.Done():
				close(resume)
				t.Fatal("search did not reach snapshot gate")
			}
			_, err = updater.Exec(ctx, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
			close(resume)
			got := <-done
			if err != nil {
				t.Fatal(err)
			}
			if !errors.Is(got.err, policystore.ErrForbidden) || len(got.page.Messages) != 0 {
				t.Fatalf("revocation missed %+v %v", got.page, got.err)
			}
			var allows int
			if err = conn.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action='message_search' AND outcome='allow'").Scan(&allows); err != nil || allows != 0 {
				t.Fatalf("revoked search audit %d %v", allows, err)
			}
		})
	}
}
