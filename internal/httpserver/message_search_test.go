package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type messageSearchFunc func(context.Context, access.TrustedIdentity, string, string, string, int) (policystore.MessageSearchPage, error)
type messageSearchStub struct{ direct, group messageSearchFunc }

func (s messageSearchStub) SearchTextMessages(c context.Context, id access.TrustedIdentity, conv, q, cursor string, limit int) (policystore.MessageSearchPage, error) {
	return s.direct(c, id, conv, q, cursor, limit)
}
func (s messageSearchStub) SearchGroupTextMessages(c context.Context, id access.TrustedIdentity, conv, q, cursor string, limit int) (policystore.MessageSearchPage, error) {
	return s.group(c, id, conv, q, cursor, limit)
}

func TestMessageSearchRouteIdentityDTOAndEmptyContinuation(t *testing.T) {
	for _, kind := range []string{"conversations", "groups"} {
		t.Run(kind, func(t *testing.T) {
			called := 0
			f := messageSearchFunc(func(_ context.Context, id access.TrustedIdentity, conv, q, cursor string, limit int) (policystore.MessageSearchPage, error) {
				called++
				if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || conv != targetUserID || q != "工单" || cursor != "next" || limit != 2 {
					t.Fatalf("input %+v %q %q %q %d", id, conv, q, cursor, limit)
				}
				return policystore.MessageSearchPage{ConversationID: conv, Messages: []policystore.PulledMessage{{MessageID: groupIntervalID, Seq: 9007199254740993, SenderUserID: actorID, Text: "工单正文", ServerTime: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)}}, HasMore: true, NextCursor: "more"}, nil
			})
			wrong := messageSearchFunc(func(context.Context, access.TrustedIdentity, string, string, string, int) (policystore.MessageSearchPage, error) {
				t.Fatal("wrong route kind")
				return policystore.MessageSearchPage{}, nil
			})
			svc := messageSearchStub{direct: f, group: wrong}
			if kind == "groups" {
				svc = messageSearchStub{direct: wrong, group: f}
			}
			h, err := HandlerWithMessageSearch(Handler(nil), authFunc(verified), svc)
			if err != nil {
				t.Fatal(err)
			}
			req := adminRequest("GET", "/api/v1/"+kind+"/"+targetUserID+"/messages/search?q=%20工单%20&limit=2&cursor=next")
			req.Header.Set("X-Tenant-ID", actorID)
			res := httptest.NewRecorder()
			h.ServeHTTP(res, req)
			var page struct {
				Conversation string `json:"conversation_id"`
				Messages     []struct {
					Seq    string `json:"seq"`
					ID     string `json:"id"`
					Sender string `json:"sender_user_id"`
					Text   string `json:"text"`
					At     string `json:"server_time"`
				} `json:"messages"`
				More bool   `json:"has_more"`
				Next string `json:"next_cursor"`
			}
			if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &page) != nil || page.Conversation != targetUserID || len(page.Messages) != 1 || page.Messages[0].Seq != "9007199254740993" || page.Messages[0].Text != "工单正文" || page.Messages[0].ID != groupIntervalID || page.Messages[0].Sender != actorID || page.Messages[0].At != "2026-10-03T00:00:00Z" || !page.More || page.Next != "more" || called != 1 || res.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("dto %d %s", res.Code, res.Body.String())
			}
		})
	}
	empty := messageSearchFunc(func(_ context.Context, _ access.TrustedIdentity, conv, q, cursor string, limit int) (policystore.MessageSearchPage, error) {
		if q != "abc" || limit != 20 {
			t.Fatal(q, limit)
		}
		return policystore.MessageSearchPage{ConversationID: conv, HasMore: true, NextCursor: "scan"}, nil
	})
	h, _ := HandlerWithMessageSearch(Handler(nil), authFunc(verified), messageSearchStub{direct: empty, group: empty})
	res := httptest.NewRecorder()
	h.ServeHTTP(res, adminRequest("GET", "/api/v1/conversations/"+targetUserID+"/messages/search?q=ABC"))
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"messages":[]`) || !strings.Contains(res.Body.String(), `"next_cursor":"scan"`) {
		t.Fatalf("empty scan %d %s", res.Code, res.Body.String())
	}
}

func TestMessageSearchRouteRejectsUnsafeInput(t *testing.T) {
	calls := 0
	f := messageSearchFunc(func(context.Context, access.TrustedIdentity, string, string, string, int) (policystore.MessageSearchPage, error) {
		calls++
		return policystore.MessageSearchPage{}, nil
	})
	h, _ := HandlerWithMessageSearch(Handler(nil), authFunc(verified), messageSearchStub{direct: f, group: f})
	path := "/api/v1/conversations/" + targetUserID + "/messages/search"
	for _, query := range []string{"", "?", "?q=", "?q=x", "?q=%20%20", "?q=%ffab", "?q=ab%00", "?q=" + strings.Repeat("字", 101), "?q=ab&q=cd", "?q=ab&limit=0", "?q=ab&limit=51", "?q=ab&limit=-1", "?q=ab&limit=1.5", "?q=ab&limit=", "?q=ab&limit=1&limit=2", "?q=ab&cursor=", "?q=ab&cursor=x&cursor=y", "?q=ab&cursor=" + strings.Repeat("a", 2049), "?q=ab&tenant_id=x", "?q=%zz"} {
		res := httptest.NewRecorder()
		h.ServeHTTP(res, adminRequest("GET", path+query))
		if res.Code != 400 {
			t.Fatalf("bad %q: %d %s", query, res.Code, res.Body.String())
		}
	}
	req := adminRequest("GET", path+"?q=ab")
	req.Body = io.NopCloser(strings.NewReader("{}"))
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != 400 {
		t.Fatal(res.Code)
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		res := httptest.NewRecorder()
		h.ServeHTTP(res, adminRequest(method, path+"?q=ab"))
		if res.Code != 405 || res.Header().Get("Allow") != "GET" {
			t.Fatal(method, res.Code)
		}
	}
	for _, header := range []string{"Authorization", "X-Acting-Membership-ID"} {
		req := adminRequest("GET", path+"?q=ab")
		req.Header.Del(header)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != 401 && res.Code != 400 {
			t.Fatal(header, res.Code)
		}
	}
	res = httptest.NewRecorder()
	h.ServeHTTP(res, adminRequest("GET", "/api/v1/groups/bad/messages/search?q=ab"))
	if res.Code != 400 {
		t.Fatal(res.Code)
	}
	if calls != 0 {
		t.Fatalf("unsafe request reached service %d", calls)
	}
}

func TestMessageSearchRouteMapsErrorsWithoutDetails(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{{policystore.ErrInvalidMessageSearch, 400, "invalid_search"}, {policystore.ErrForbidden, 403, "invalid_identity"}, {policystore.ErrMessageNotAvailable, 404, "not_found"}, {policystore.ErrAuditUnavailable, 503, "unavailable"}, {errors.New("postgres secret"), 503, "unavailable"}} {
		f := messageSearchFunc(func(context.Context, access.TrustedIdentity, string, string, string, int) (policystore.MessageSearchPage, error) {
			return policystore.MessageSearchPage{}, tc.err
		})
		h, _ := HandlerWithMessageSearch(Handler(nil), authFunc(verified), messageSearchStub{direct: f, group: f})
		res := httptest.NewRecorder()
		h.ServeHTTP(res, adminRequest("GET", "/api/v1/groups/"+targetUserID+"/messages/search?q=ab"))
		if res.Code != tc.status || !strings.Contains(res.Body.String(), tc.code) || strings.Contains(res.Body.String(), "secret") {
			t.Fatalf("error %v: %d %s", tc.err, res.Code, res.Body.String())
		}
	}
	if _, err := HandlerWithMessageSearch(nil, authFunc(verified), messageSearchStub{}); err == nil {
		t.Fatal("nil base")
	}
	if _, err := HandlerWithMessageSearch(Handler(nil), nil, messageSearchStub{}); err == nil {
		t.Fatal("nil auth")
	}
	if _, err := HandlerWithMessageSearch(Handler(nil), authFunc(verified), nil); err == nil {
		t.Fatal("nil service")
	}
}
