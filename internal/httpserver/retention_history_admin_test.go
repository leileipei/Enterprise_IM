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
)

type retentionHistoryFunc func(context.Context, access.TrustedIdentity, string, int) (access.RetentionPolicyHistoryPage, error)

func (f retentionHistoryFunc) ListRetentionPolicyHistory(c context.Context, id access.TrustedIdentity, cursor string, limit int) (access.RetentionPolicyHistoryPage, error) {
	return f(c, id, cursor, limit)
}

const historyPath = "/api/v1/admin/retention-policy/history"

func TestRetentionHistoryAdminDTOAndTrustedIdentity(t *testing.T) {
	at := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	svc := retentionHistoryFunc(func(_ context.Context, id access.TrustedIdentity, cursor string, limit int) (access.RetentionPolicyHistoryPage, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || cursor != "next" || limit != 2 {
			t.Fatalf("input %+v %q %d", id, cursor, limit)
		}
		return access.RetentionPolicyHistoryPage{History: []access.RetentionPolicy{{Version: 3, MessageBodyDays: 30, ApprovalReference: "CAB-3", ApprovedByUserID: actorID, ApprovedAt: &at}}, NextCursor: "more"}, nil
	})
	h, err := HandlerWithRetentionHistory(Handler(nil), authFunc(verified), svc)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	h.ServeHTTP(r, adminRequest("GET", historyPath+"?limit=2&cursor=next"))
	var page struct {
		History []map[string]any `json:"history"`
		Next    string           `json:"next_cursor"`
	}
	if r.Code != 200 || r.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(r.Body.Bytes(), &page) != nil || len(page.History) != 1 || page.Next != "more" {
		t.Fatalf("response %d %s", r.Code, r.Body.String())
	}
	p := page.History[0]
	if len(p) != 5 || p["version"] != float64(3) || p["message_body_days"] != float64(30) || p["approval_reference"] != "CAB-3" || p["approved_by_user_id"] != actorID || p["approved_at"] != "2026-10-03T00:00:00Z" {
		t.Fatalf("DTO %+v", p)
	}
	h, _ = HandlerWithRetentionHistory(Handler(nil), authFunc(verified), retentionHistoryFunc(func(_ context.Context, _ access.TrustedIdentity, cursor string, limit int) (access.RetentionPolicyHistoryPage, error) {
		if cursor != "" || limit != 20 {
			t.Fatalf("defaults %q %d", cursor, limit)
		}
		return access.RetentionPolicyHistoryPage{}, nil
	}))
	r = httptest.NewRecorder()
	h.ServeHTTP(r, adminRequest("GET", historyPath))
	if r.Code != 200 || r.Body.String() != "{\"history\":[],\"next_cursor\":\"\"}\n" {
		t.Fatalf("empty %d %s", r.Code, r.Body.String())
	}
}

func TestRetentionHistoryAdminRejectsUnsafeRequests(t *testing.T) {
	calls := 0
	h, _ := HandlerWithRetentionHistory(Handler(nil), authFunc(verified), retentionHistoryFunc(func(context.Context, access.TrustedIdentity, string, int) (access.RetentionPolicyHistoryPage, error) {
		calls++
		return access.RetentionPolicyHistoryPage{}, nil
	}))
	for _, query := range []string{"?tenant_id=x", "?limit=0", "?limit=101", "?limit=-1", "?limit=1.5", "?limit=1&limit=2", "?limit=+2", "?cursor=", "?cursor=" + strings.Repeat("a", 1025), "?cursor=x&cursor=y", "?cursor=%zz"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, adminRequest("GET", historyPath+query))
		if r.Code != 400 || r.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("query %q %d", query, r.Code)
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, adminRequest(method, historyPath))
		if r.Code != 405 || r.Header().Get("Allow") != "GET" {
			t.Fatalf("method %s %d", method, r.Code)
		}
	}
	req := adminRequest("GET", historyPath)
	req.Body = io.NopCloser(strings.NewReader("{}"))
	r := httptest.NewRecorder()
	h.ServeHTTP(r, req)
	if r.Code != 400 {
		t.Fatalf("body %d", r.Code)
	}
	for _, header := range []string{"Authorization", "X-Acting-Membership-ID"} {
		req := adminRequest("GET", historyPath)
		req.Header.Del(header)
		r := httptest.NewRecorder()
		h.ServeHTTP(r, req)
		want := 401
		if header != "Authorization" {
			want = 400
		}
		if r.Code != want {
			t.Fatalf("header %s %d", header, r.Code)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid calls %d", calls)
	}
	r = httptest.NewRecorder()
	h.ServeHTTP(r, adminRequest("GET", "/health/live"))
	if r.Code != 200 {
		t.Fatalf("fallthrough %d", r.Code)
	}
}

func TestRetentionHistoryAdminErrorMapping(t *testing.T) {
	for _, input := range []struct {
		err  error
		code int
		key  string
	}{{access.ErrInvalidRetentionHistoryQuery, 400, "invalid_retention_history_query"}, {access.ErrInvalidIdentity, 403, "invalid_identity"}, {access.ErrNotFound, 404, "not_found"}, {access.ErrAuditUnavailable, 503, "unavailable"}, {errors.New("secret-database-approval"), 503, "unavailable"}} {
		h, _ := HandlerWithRetentionHistory(Handler(nil), authFunc(verified), retentionHistoryFunc(func(context.Context, access.TrustedIdentity, string, int) (access.RetentionPolicyHistoryPage, error) {
			return access.RetentionPolicyHistoryPage{}, input.err
		}))
		r := httptest.NewRecorder()
		h.ServeHTTP(r, adminRequest("GET", historyPath))
		if r.Code != input.code || !strings.Contains(r.Body.String(), `"error_code":"`+input.key+`"`) || strings.Contains(r.Body.String(), "secret-database-approval") || strings.Contains(r.Body.String(), "\"history\"") {
			t.Fatalf("error %d %s", r.Code, r.Body.String())
		}
	}
}
