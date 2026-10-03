package policystore_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func assertProductionMessageSearch(t *testing.T, conn *pgx.Conn, client *http.Client, first, second, token string) {
	t.Helper()
	type match struct {
		ID   string `json:"id"`
		Seq  string `json:"seq"`
		Text string `json:"text"`
	}
	type page struct {
		Conversation string  `json:"conversation_id"`
		Messages     []match `json:"messages"`
		More         bool    `json:"has_more"`
		Next         string  `json:"next_cursor"`
	}
	route := "/api/v1/conversations/" + directA + "/messages/search?q=" + url.QueryEscape("生产 API 消息") + "&limit=1"
	var a, b page
	productionResponse(t, client, productionRequest(t, "GET", first, route, token, nil, true), 200, &a)
	if a.Conversation != directA || len(a.Messages) != 1 || a.Messages[0].Seq != "1" || a.Messages[0].Text != "生产 API 消息一" || a.Next == "" || !a.More {
		t.Fatalf("first search %+v", a)
	}
	productionResponse(t, client, productionRequest(t, "GET", second, route+"&cursor="+url.QueryEscape(a.Next), token, nil, true), 200, &b)
	if len(b.Messages) != 1 || b.Messages[0].Seq != "2" || b.Messages[0].Text != "生产 API 消息二" || b.Next != "" || b.More {
		t.Fatalf("second search %+v", b)
	}
	productionResponse(t, client, productionRequest(t, "GET", second, "/api/v1/conversations/"+directA+"/messages/search?q=other&cursor="+url.QueryEscape(a.Next), token, nil, true), 400, nil)
	var group struct {
		ID string `json:"id"`
	}
	productionResponse(t, client, productionRequest(t, "POST", first, "/api/v1/groups", token, []byte(`{"client_request_id":"00000000-0000-4000-8000-000000008f17","name":"搜索验收群","member_membership_ids":["`+targetM2+`"]}`), true), 201, &group)
	productionResponse(t, client, productionRequest(t, "POST", second, "/api/v1/groups/"+group.ID+"/messages", token, []byte(fmt.Sprintf(`{"client_msg_id":"%s","text":"生产搜索工单"}`, clientUUIDv7(time.Now(), 1450))), true), 200, nil)
	var g page
	groupRoute := "/api/v1/groups/" + group.ID + "/messages/search?q=" + url.QueryEscape("搜索工单")
	productionResponse(t, client, productionRequest(t, "GET", first, groupRoute, token, nil, true), 200, &g)
	if g.Conversation != group.ID || len(g.Messages) != 1 || g.Messages[0].Text != "生产搜索工单" || g.Messages[0].Seq != "1" || g.More {
		t.Fatalf("group search %+v", g)
	}
	// A direct cursor is not a group cursor, even if the caller owns both.
	productionResponse(t, client, productionRequest(t, "GET", second, groupRoute+"&cursor="+url.QueryEscape(a.Next), token, nil, true), 400, nil)
	for _, addr := range []string{first, second} {
		productionResponse(t, client, productionRequest(t, "GET", addr, route, "invalid", nil, true), 401, nil)
	}
	var n int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE tenant_id=$1 AND action='message_search' AND outcome='allow'", tenantA).Scan(&n); err != nil || n != 3 {
		t.Fatalf("search audits %d %v", n, err)
	}
}
