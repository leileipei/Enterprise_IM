package policystore_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"strings"
	"testing"
	"time"
)

func crossID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }
func seedCrossGroup(t *testing.T, conn *pgx.Conn, id string) {
	t.Helper()
	run(t, conn, `INSERT INTO conversations(id,tenant_id,kind,group_name,created_by_user_id,group_creator_membership_id,group_creator_organization_id,group_creator_legal_entity_id) VALUES($1,$2,'group','历史群',$3,$4,$5,$6)`, id, tenantA, adminA, adminM, orgA, legalA)
	if err := insertInterval(conn, crossID(500000+len(id)+crossCounter(id)), tenantA, id, adminA, adminM, orgA, legalA, "owner", "active", 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := insertInterval(conn, crossID(600000+len(id)+crossCounter(id)), tenantA, id, personA, targetM2, orgA, legalA, "member", "active", 1, nil); err != nil {
		t.Fatal(err)
	}
}
func crossCounter(id string) int { var n int; fmt.Sscanf(id[len(id)-12:], "%d", &n); return n }
func crossCollect(t *testing.T, svc policystore.Service, id access.TrustedIdentity, q, kind string, limit int) []policystore.CrossConversationMatch {
	t.Helper()
	var all []policystore.CrossConversationMatch
	cursor := ""
	seen := map[string]bool{}
	for n := 0; n < 20; n++ {
		p, err := svc.SearchAllTextMessages(context.Background(), id, q, kind, cursor, limit)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, p.Messages...)
		if !p.HasMore {
			if p.NextCursor != "" {
				t.Fatal("terminal cursor")
			}
			return all
		}
		if p.NextCursor == "" || seen[p.NextCursor] {
			t.Fatal("cursor not progressing")
		}
		seen[p.NextCursor] = true
		cursor = p.NextCursor
	}
	t.Fatal("too many pages")
	return nil
}

func TestCrossMessageSearchHistoricalScope(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 1700), "工单单聊"); err != nil {
		t.Fatal(err)
	}
	g := crossID(9101)
	seedCrossGroup(t, conn, g)
	insertGroupHistoryMessage(t, conn, g, 1, adminA, adminM, "工单群聊")
	run(t, conn, "UPDATE conversations SET status='ended' WHERE id=$1", directA)
	run(t, conn, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", g)
	run(t, conn, "UPDATE conversation_membership_intervals SET status='left',leave_seq=1,left_at=joined_at WHERE conversation_id=$1 AND user_id=$2", g, personA)
	run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at, targetM2)
	run(t, conn, "INSERT INTO user_organizations(id,tenant_id,user_id,organization_id,effective_from) VALUES($1,$2,$3,$4,$5)", crossID(9188), tenantA, personA, orgA, at)
	identity := groupMemberIdentity()
	identity.ActingMembershipID = crossID(9188)
	matches := crossCollect(t, svc, identity, "工单", "all", 1)
	if len(matches) != 2 || matches[0].ConversationID != directA || matches[1].ConversationID != g || matches[0].Kind != "direct" || matches[1].Kind != "group" {
		t.Fatalf("history %+v", matches)
	}
	for _, m := range matches {
		if m.Message.Redacted || m.Message.Text == "" {
			t.Fatal("hidden result")
		}
	}
	for _, id := range []access.TrustedIdentity{{TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}} {
		if got := crossCollect(t, svc, id, "工单", "all", 20); len(got) != 0 {
			t.Fatalf("tenant leak %+v", got)
		}
	}
	seedThirdGroupMember(t, conn)
	outsider := access.TrustedIdentity{TenantID: tenantA, UserID: groupUserC, ActingMembershipID: groupMemberC}
	if got := crossCollect(t, svc, outsider, "工单", "all", 20); len(got) != 0 {
		t.Fatalf("outsider %+v", got)
	}
}

func TestCrossMessageSearchCombinedBudgets(t *testing.T) {
	t.Run("empty conversations", func(t *testing.T) {
		conn := db(t)
		seed(t, conn)
		for i := 0; i < 21; i++ {
			seedCrossGroup(t, conn, crossID(9200+i))
		}
		svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
		p, err := svc.SearchAllTextMessages(context.Background(), publisher(), "目标", "all", "", 20)
		if err != nil || len(p.Messages) != 0 || !p.HasMore {
			t.Fatalf("empty budget %+v %v", p, err)
		}
		raw, _ := base64.RawURLEncoding.DecodeString(p.NextCursor)
		if !strings.Contains(string(raw), crossID(9219)) {
			t.Fatalf("did not process exactly20 %s", raw)
		}
		p, err = svc.SearchAllTextMessages(context.Background(), publisher(), "目标", "all", p.NextCursor, 20)
		if err != nil || p.HasMore || p.NextCursor != "" {
			t.Fatalf("last empty %+v %v", p, err)
		}
	})
	t.Run("combined messages", func(t *testing.T) {
		conn := db(t)
		seed(t, conn)
		for i := 0; i < 2; i++ {
			g := crossID(9300 + i)
			seedCrossGroup(t, conn, g)
			n := 300
			if i == 1 {
				n = 250
			}
			for seq := 1; seq <= n; seq++ {
				text := "无匹配"
				if i == 1 && seq >= 201 {
					text = "目标正文"
				}
				insertGroupHistoryMessage(t, conn, g, int64(seq), adminA, adminM, text)
			}
		}
		svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
		p, err := svc.SearchAllTextMessages(context.Background(), publisher(), "目标", "all", "", 2)
		if err != nil || len(p.Messages) != 0 || !p.HasMore {
			t.Fatalf("500 budget %+v %v", p, err)
		}
		raw, _ := base64.RawURLEncoding.DecodeString(p.NextCursor)
		if !strings.Contains(string(raw), `"a":"200"`) {
			t.Fatalf("not combined500 %s", raw)
		}
		p, err = svc.SearchAllTextMessages(context.Background(), publisher(), "目标", "all", p.NextCursor, 2)
		if err != nil || len(p.Messages) != 2 || p.Messages[0].Message.Seq != 201 || p.Messages[1].Message.Seq != 202 {
			t.Fatalf("after500 %+v %v", p, err)
		}
	})
}

func TestCrossMessageSearchAuditAndPartialFailure(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 1710), "工单secret"); err != nil {
		t.Fatal(err)
	}
	p, err := svc.SearchAllTextMessages(context.Background(), publisher(), "工单", "all", "", 20)
	if err != nil || len(p.Messages) != 1 {
		t.Fatalf("page %+v %v", p, err)
	}
	var n int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action='message_search_all' AND outcome='allow' AND resource_type='tenant' AND resource_id=$1 AND reason='cross_conversation_search_page'`, tenantA).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit %d %v", n, err)
	}
	run(t, conn, `CREATE FUNCTION fail_cross_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='message_search_all' THEN RAISE EXCEPTION 'audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_cross_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_cross_audit()`)
	p, err = svc.SearchAllTextMessages(context.Background(), publisher(), "工单", "all", "", 20)
	if err == nil || len(p.Messages) != 0 || p.NextCursor != "" {
		t.Fatalf("partial %+v %v", p, err)
	}
}

func TestCrossMessageSearchRejectsInput(t *testing.T) {
	svc := policystore.Service{}
	for _, kind := range []string{"", "ALL", "unknown"} {
		if _, err := svc.SearchAllTextMessages(context.Background(), publisher(), "工单", kind, "", 20); !errors.Is(err, policystore.ErrInvalidMessageSearch) {
			t.Fatal(err)
		}
	}
}

func TestCrossMessageSearchCursorDoesNotGrantScope(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	g1, g2 := crossID(9601), crossID(9603)
	for _, g := range []string{g1, g2} {
		seedCrossGroup(t, conn, g)
		insertGroupHistoryMessage(t, conn, g, 1, adminA, adminM, "工单正文")
	}
	// Creation metadata alone does not grant participation in this other group.
	run(t, conn, `INSERT INTO conversations(id,tenant_id,kind,group_name,created_by_user_id,group_creator_membership_id,group_creator_organization_id,group_creator_legal_entity_id) VALUES($1,$2,'group','other',$3,$4,$5,$6)`, crossID(9602), tenantA, adminA, adminM, orgA, legalA)
	if e := insertInterval(conn, crossID(699602), tenantA, crossID(9602), personA, targetM2, orgA, legalA, "owner", "active", 1, nil); e != nil {
		t.Fatal(e)
	}
	insertGroupHistoryMessage(t, conn, crossID(9602), 1, personA, targetM2, "工单other private")
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	p, err := svc.SearchAllTextMessages(context.Background(), publisher(), "工单", "all", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(p.NextCursor)
	forged := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), g1, crossID(9602), 1)))
	p, err = svc.SearchAllTextMessages(context.Background(), publisher(), "工单", "all", forged, 20)
	if err != nil || len(p.Messages) != 1 || p.Messages[0].ConversationID != g2 {
		t.Fatalf("scope %+v %v", p, err)
	}
	for _, kind := range []string{"direct", "group"} {
		if _, err := svc.SearchAllTextMessages(context.Background(), publisher(), "工单", kind, forged, 20); !errors.Is(err, policystore.ErrInvalidMessageSearch) {
			t.Fatalf("kind cursor %s %v", kind, err)
		}
	}
	if got := crossCollect(t, svc, publisher(), "工单", "direct", 20); len(got) != 0 {
		t.Fatal("direct included group")
	}
	if got := crossCollect(t, svc, publisher(), "工单", "group", 1); len(got) != 2 {
		t.Fatal("group missing")
	}
}

func TestCrossMessageSearchLiveProgressNeedsRefresh(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	g1, g2 := crossID(9701), crossID(9702)
	for _, g := range []string{g1, g2} {
		seedCrossGroup(t, conn, g)
		insertGroupHistoryMessage(t, conn, g, 1, adminA, adminM, "工单正文")
	}
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	p, err := svc.SearchAllTextMessages(context.Background(), publisher(), "工单", "all", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	p, err = svc.SearchAllTextMessages(context.Background(), publisher(), "工单", "all", p.NextCursor, 1)
	if err != nil || p.Messages[0].ConversationID != g2 {
		t.Fatalf("second %+v %v", p, err)
	}
	insertGroupHistoryMessage(t, conn, g1, 2, adminA, adminM, "工单新消息")
	p, err = svc.SearchAllTextMessages(context.Background(), publisher(), "工单", "all", p.NextCursor, 20)
	if err != nil || len(p.Messages) != 0 || p.HasMore {
		t.Fatalf("old cursor %+v %v", p, err)
	}
	if got := crossCollect(t, svc, publisher(), "工单", "all", 20); len(got) != 3 {
		t.Fatalf("refresh %+v", got)
	}
}

func TestCrossMessageSearchVisibilityAndLiteralQuery(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	for i, text := range []string{"工单 AbC%_\\", "前缀\uFEFFİ工单", "ΟΣ 工单", "工单 已清理"} {
		if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 1740+i), text); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{"\u0085ABC%_\\\u0085", "\uFEFFİ工单", "οσ"} {
		if got := crossCollect(t, svc, publisher(), q, "all", 20); len(got) != 1 {
			t.Fatalf("literal %q %+v", q, got)
		}
	}
	g := crossID(9801)
	seedCrossGroup(t, conn, g)
	insertGroupHistoryMessage(t, conn, g, 1, adminA, adminM, "工单可见")
	run(t, conn, "UPDATE conversation_membership_intervals SET status='left',leave_seq=1,left_at=joined_at WHERE conversation_id=$1 AND user_id=$2", g, personA)
	insertGroupHistoryMessage(t, conn, g, 2, adminA, adminM, "工单退群缺口")
	seedThirdGroupMember(t, conn)
	insertGroupHistoryMessage(t, conn, g, 3, groupUserC, groupMemberC, "工单伪造发送者")
	if got := crossCollect(t, svc, groupMemberIdentity(), "工单", "group", 20); len(got) != 1 || got[0].Message.Seq != 1 {
		t.Fatalf("interval %+v", got)
	}
	run(t, conn, "UPDATE messages SET text_body=NULL,body_cleared_at=$1 WHERE conversation_id=$2 AND seq=4", at, directA)
	run(t, conn, "UPDATE messages SET accepted_at=$1 WHERE conversation_id=$2 AND seq=1", at.Add(-365*24*time.Hour), g)
	run(t, conn, `INSERT INTO conversation_legal_holds(id,tenant_id,conversation_id,case_reference,create_request_id,placed_by_user_id,placed_by_membership_id,placed_at) VALUES($1,$2,$3,'case',$4,$5,$6,$7)`, crossID(9901), tenantA, g, crossID(9902), adminA, adminM, at)
	if got := crossCollect(t, svc, groupMemberIdentity(), "工单", "group", 20); len(got) != 0 {
		t.Fatal("hold expanded visibility")
	}
	run(t, conn, "UPDATE messages SET recipient_user_id=NULL,recipient_membership_id=NULL,sender_organization_id=NULL,recipient_organization_id=NULL WHERE conversation_id=$1 AND seq=1", directA)
	if got := crossCollect(t, svc, publisher(), "ABC%_\\", "all", 20); len(got) != 0 {
		t.Fatal("missing snapshot returned")
	}
	hard := policy.Rule{ID: "cross-visibility-hard", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, EffectiveFrom: at.Add(-time.Hour), Reason: "blocked"}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{hard}, "block"); err != nil {
		t.Fatal(err)
	}
	if got := crossCollect(t, svc, publisher(), "工单", "all", 20); len(got) != 0 {
		t.Fatalf("harddeny %+v", got)
	}
}
