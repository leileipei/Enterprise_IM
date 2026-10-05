package policystore_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func assertProductionCrossMessageSearch(t *testing.T, conn *pgx.Conn, client *http.Client, first, second, token string) {
	t.Helper()
	type match struct {
		Conversation string `json:"conversation_id"`
		Kind         string `json:"conversation_kind"`
		ID           string `json:"id"`
		Seq          string `json:"seq"`
		Text         string `json:"text"`
		Sender       string `json:"sender_user_id"`
		At           string `json:"server_time"`
	}
	type page struct {
		Messages []match `json:"messages"`
		More     bool    `json:"has_more"`
		Next     string  `json:"next_cursor"`
	}
	route := "/api/v1/messages/search?q=" + url.QueryEscape("生产") + "&kind=all&limit=1"
	cursor, firstCursor := "", ""
	var matches []match
	pages := 0
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		addr := first
		if i%2 == 1 {
			addr = second
		}
		path := route
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var p page
		productionResponse(t, client, productionRequest(t, "GET", addr, path, token, nil, true), 200, &p)
		pages++
		matches = append(matches, p.Messages...)
		if !p.More {
			if p.Next != "" {
				t.Fatal("terminal cursor")
			}
			break
		}
		if p.Next == "" || seen[p.Next] {
			t.Fatal("non progressing cursor")
		}
		seen[p.Next] = true
		cursor = p.Next
		if firstCursor == "" {
			firstCursor = cursor
		}
		if i == 9 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(matches) != 3 {
		t.Fatalf("matches %+v", matches)
	}
	texts := map[string]bool{}
	for i, m := range matches {
		if m.ID == "" || m.Sender != adminA || m.At == "" || texts[m.Text] {
			t.Fatalf("invalid match %+v", m)
		}
		texts[m.Text] = true
		n, e := strconv.ParseInt(m.Seq, 10, 64)
		if e != nil || n < 1 {
			t.Fatal(m.Seq)
		}
		if i > 0 {
			prev := matches[i-1]
			p, _ := strconv.ParseInt(prev.Seq, 10, 64)
			if prev.Conversation > m.Conversation || (prev.Conversation == m.Conversation && p >= n) {
				t.Fatal("unordered results")
			}
		}
		if strings.Contains(m.Text, "API 消息") {
			if m.Conversation != directA || m.Kind != "direct" {
				t.Fatal(m)
			}
		} else if m.Kind != "group" || m.Seq != "1" {
			t.Fatal(m)
		}
	}
	for _, text := range []string{"生产 API 消息一", "生产 API 消息二", "生产搜索工单"} {
		if !texts[text] {
			t.Fatal("missing", text)
		}
	}
	for _, tc := range []struct {
		kind string
		want int
	}{{"direct", 2}, {"group", 1}} {
		var p page
		productionResponse(t, client, productionRequest(t, "GET", second, "/api/v1/messages/search?q="+url.QueryEscape("生产")+"&kind="+tc.kind, token, nil, true), 200, &p)
		if len(p.Messages) != tc.want || p.More {
			t.Fatalf("kind %+v", p)
		}
		for _, m := range p.Messages {
			if m.Kind != tc.kind {
				t.Fatal(m)
			}
		}
	}
	for _, path := range []string{"/api/v1/messages/search?q=other&kind=all", "/api/v1/messages/search?q=" + url.QueryEscape("生产") + "&kind=group"} {
		productionResponse(t, client, productionRequest(t, "GET", second, path+"&cursor="+url.QueryEscape(firstCursor), token, nil, true), 400, nil)
	}
	for _, addr := range []string{first, second} {
		productionResponse(t, client, productionRequest(t, "GET", addr, route, "invalid", nil, true), 401, nil)
	}
	var n int
	if e := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='message_search_all' AND outcome='allow' AND resource_type='tenant' AND resource_id=$1 AND reason='cross_conversation_search_page'", tenantA).Scan(&n); e != nil || n != pages+2 {
		t.Fatalf("audit pages=%d count=%d %v", pages, n, e)
	}
	if e := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='message_search' AND outcome='allow'").Scan(&n); e != nil || n != 3 {
		t.Fatalf("old audit %d %v", n, e)
	}
	t.Logf("cross search: %d alternating-node all pages, 3 authorized results, 2 kind-filter pages, %d exact allow audits", pages, pages+2)
}
