package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type crossSearchFunc func(context.Context, access.TrustedIdentity, string, string, string, int) (policystore.CrossConversationSearchPage, error)

func (f crossSearchFunc) SearchAllTextMessages(c context.Context, id access.TrustedIdentity, q, k, cur string, l int) (policystore.CrossConversationSearchPage, error) {
	return f(c, id, q, k, cur, l)
}
func TestCrossMessageSearchRouteContract(t *testing.T) {
	calls := 0
	f := crossSearchFunc(func(_ context.Context, id access.TrustedIdentity, q, k, cur string, l int) (policystore.CrossConversationSearchPage, error) {
		calls++
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || q != "abc" || k != "all" || l != 20 {
			t.Fatalf("input %+v %q %q %d", id, q, k, l)
		}
		p := policystore.CrossConversationSearchPage{HasMore: true, NextCursor: "more"}
		if cur == "" {
			for _, conv := range []string{targetUserID, groupIntervalID} {
				p.Messages = append(p.Messages, policystore.CrossConversationMatch{ConversationID: conv, Kind: "direct", Message: policystore.PulledMessage{MessageID: actorID, Seq: 9007199254740993, Text: "abc正文", SenderUserID: actorID, ServerTime: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)}})
			}
		}
		return p, nil
	})
	h, e := HandlerWithCrossMessageSearch(Handler(nil), authFunc(verified), f)
	if e != nil {
		t.Fatal(e)
	}
	for _, suffix := range []string{"", "&cursor=resume"} {
		r := adminRequest("GET", "/api/v1/messages/search?q=%20ABC%20"+suffix)
		r.Header.Set("X-Tenant-ID", actorID)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var p struct {
			Messages []struct{ Conversation, Kind, Seq, Text string }
			More     bool   `json:"has_more"`
			Next     string `json:"next_cursor"`
		}
		if e := json.Unmarshal(w.Body.Bytes(), &p); e != nil {
			t.Fatal(e)
		}
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !p.More || p.Next != "more" || strings.Contains(w.Body.String(), "redacted") || strings.Contains(w.Body.String(), "name") {
			t.Fatal(w.Code, w.Body.String())
		}
		if suffix == "" {
			var raw map[string]any
			json.Unmarshal(w.Body.Bytes(), &raw)
			ms := raw["messages"].([]any)
			if len(ms) != 2 || ms[0].(map[string]any)["seq"] != "9007199254740993" || ms[0].(map[string]any)["conversation_id"] != targetUserID || ms[1].(map[string]any)["conversation_id"] != groupIntervalID {
				t.Fatal(raw)
			}
		} else if !strings.Contains(w.Body.String(), `"messages":[]`) {
			t.Fatal(w.Body.String())
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}
func TestCrossMessageSearchRouteRejectsUnsafeInput(t *testing.T) {
	f := crossSearchFunc(func(context.Context, access.TrustedIdentity, string, string, string, int) (policystore.CrossConversationSearchPage, error) {
		t.Fatal("unsafe reached service")
		return policystore.CrossConversationSearchPage{}, nil
	})
	h, e := HandlerWithCrossMessageSearch(Handler(nil), authFunc(verified), f)
	if e != nil {
		t.Fatal(e)
	}
	for _, q := range []string{"", "q=a", "q=%00ab", "q=%ffab", "q=" + strings.Repeat("界", 101), "q=ab&q=cd", "q=ab&tenant_id=x", "q=ab&kind=", "q=ab&kind=ALL", "q=ab&kind=invalid", "q=ab&kind=all&kind=all", "q=ab&cursor=", "q=ab&cursor=" + strings.Repeat("x", 2049), "q=ab&limit=0", "q=ab&limit=51", "q=ab&limit=01", "q=ab&limit=%2B1", "q=ab&limit=-1", "q=ab&limit=1.5", "q=ab&limit=1&limit=2", "q=ab%zz", "q=ab;x=y"} {
		t.Run(q[:min(len(q), 40)], func(t *testing.T) {
			r := adminRequest("GET", "/api/v1/messages/search")
			r.URL.RawQuery = q
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 400 {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	for _, body := range []string{"x", strings.Repeat("x", 1000)} {
		r := adminRequest("GET", "/api/v1/messages/search?q=ab")
		r.Body = io.NopCloser(strings.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
}
func TestCrossMessageSearchRouteMapsErrors(t *testing.T) {
	for _, tc := range []struct {
		err   error
		code  int
		label string
	}{{policystore.ErrInvalidMessageSearch, 400, "invalid_search"}, {policystore.ErrForbidden, 403, "invalid_identity"}, {errors.New("secret database details"), 503, "unavailable"}, {policystore.ErrAuditUnavailable, 503, "unavailable"}} {
		h, e := HandlerWithCrossMessageSearch(Handler(nil), authFunc(verified), crossSearchFunc(func(context.Context, access.TrustedIdentity, string, string, string, int) (policystore.CrossConversationSearchPage, error) {
			return policystore.CrossConversationSearchPage{NextCursor: "secret", Messages: []policystore.CrossConversationMatch{{Kind: "secret"}}}, tc.err
		}))
		if e != nil {
			t.Fatal(e)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, adminRequest("GET", "/api/v1/messages/search?q=ab"))
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.label) || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "messages") || strings.Contains(w.Body.String(), "cursor") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	called := false
	h, _ := HandlerWithCrossMessageSearch(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(418) }), authFunc(verified), crossSearchFunc(func(context.Context, access.TrustedIdentity, string, string, string, int) (policystore.CrossConversationSearchPage, error) {
		t.Fatal("unexpected service")
		return policystore.CrossConversationSearchPage{}, nil
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, adminRequest("POST", "/api/v1/messages/search?q=ab"))
	if w.Code != 405 || w.Header().Get("Allow") != "GET" {
		t.Fatal(w.Code)
	}
	r := adminRequest("GET", "/api/v1/messages/search?q=ab")
	r.Header.Del("Authorization")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, adminRequest("GET", "/api/v1/other"))
	if w.Code != 418 || !called {
		t.Fatal(w.Code, called)
	}
}
