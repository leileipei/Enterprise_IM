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

type auditQueryFunc func(context.Context, access.TrustedIdentity, access.AuditEventFilter, string, int) (access.AuditEventPage, error)

func (f auditQueryFunc) ListAuditEvents(c context.Context, id access.TrustedIdentity, filter access.AuditEventFilter, cursor string, limit int) (access.AuditEventPage, error) {
	return f(c, id, filter, cursor, limit)
}

const auditPath = "/api/v1/admin/audit-events"

func TestAuditQueryAdminDTOAndTrustedIdentity(t *testing.T) {
	at := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	svc := auditQueryFunc(func(_ context.Context, id access.TrustedIdentity, filter access.AuditEventFilter, cursor string, limit int) (access.AuditEventPage, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || filter.Action != "retention_policy_update" || filter.Outcome != "deny" || cursor != "next" || limit != 2 {
			t.Fatalf("input %+v %q %d", id, cursor, limit)
		}
		return access.AuditEventPage{Events: []access.AuditEvent{{ID: "9007199254740993", ActorUserID: actorID, ActingMembershipID: actingID, Action: "retention_policy_update", ResourceType: "tenant", Outcome: "deny", Reason: "version_conflict", OccurredAt: at}}, NextCursor: "more"}, nil
	})
	h, err := HandlerWithAuditQuery(Handler(nil), authFunc(verified), svc)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	h.ServeHTTP(r, adminRequest("GET", auditPath+"?limit=2&action=retention_policy_update&outcome=deny&cursor=next"))
	var page struct {
		Events []map[string]any `json:"events"`
		Next   string           `json:"next_cursor"`
	}
	if r.Code != 200 || r.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(r.Body.Bytes(), &page) != nil || len(page.Events) != 1 || page.Next != "more" {
		t.Fatalf("response %d %s", r.Code, r.Body.String())
	}
	p := page.Events[0]
	if len(p) != 9 || p["id"] != "9007199254740993" || p["actor_user_id"] != actorID || p["acting_membership_id"] != actingID || p["action"] != "retention_policy_update" || p["resource_type"] != "tenant" || p["resource_id"] != nil || p["outcome"] != "deny" || p["reason"] != "version_conflict" || p["occurred_at"] != "2026-10-03T00:00:00Z" {
		t.Fatalf("DTO %+v", p)
	}
	h, _ = HandlerWithAuditQuery(Handler(nil), authFunc(verified), auditQueryFunc(func(_ context.Context, _ access.TrustedIdentity, filter access.AuditEventFilter, cursor string, limit int) (access.AuditEventPage, error) {
		if filter.Action != "" || filter.Outcome != "" || cursor != "" || limit != 20 {
			t.Fatalf("defaults %q %d", cursor, limit)
		}
		return access.AuditEventPage{}, nil
	}))
	r = httptest.NewRecorder()
	h.ServeHTTP(r, adminRequest("GET", auditPath))
	if r.Code != 200 || r.Body.String() != "{\"events\":[],\"next_cursor\":\"\"}\n" {
		t.Fatalf("empty %d %s", r.Code, r.Body.String())
	}
}

func TestAuditQueryAdminRejectsUnsafeRequests(t *testing.T) {
	calls := 0
	h, _ := HandlerWithAuditQuery(Handler(nil), authFunc(verified), auditQueryFunc(func(context.Context, access.TrustedIdentity, access.AuditEventFilter, string, int) (access.AuditEventPage, error) {
		calls++
		return access.AuditEventPage{}, nil
	}))
	for _, query := range []string{"?resource_type=", "?resource_type=Bad", "?resource_type=bad%20type", "?resource_type=a&resource_type=b", "?resource_type=" + strings.Repeat("a", 65), "?resource_id=", "?resource_id=" + tenantID, "?resource_type=tenant&resource_id=bad", "?resource_type=tenant&resource_id=" + tenantID + "&resource_id=" + tenantID, "?resource_type=tenant&resource_id=%20" + tenantID, "?from=", "?until=", "?from=bad", "?until=bad", "?from=2026-10-03T00:00:00Z&from=2026-10-04T00:00:00Z", "?until=2026-10-03T00:00:00Z&until=2026-10-04T00:00:00Z", "?from=2026-10-03T00:00:00Z&until=2026-10-03T00:00:00Z", "?from=2026-10-04T00:00:00Z&until=2026-10-03T00:00:00Z", "?actor_user_id=", "?actor_user_id=bad", "?actor_user_id=" + actorID + "&actor_user_id=" + actorID, "?actor_user_id=%20" + actorID, "?actor_user_id=" + actorID + "%20", "?actor_user_id=" + strings.ReplaceAll(actorID, "-", ""), "?tenant_id=x", "?action=", "?action=bad%20action", "?action=a&action=b", "?outcome=", "?outcome=ALLOW", "?outcome=deny&outcome=allow", "?limit=0", "?limit=101", "?limit=-1", "?limit=1.5", "?limit=1&limit=2", "?limit=+2", "?cursor=", "?cursor=" + strings.Repeat("a", 1025), "?cursor=x&cursor=y", "?cursor=%zz"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, adminRequest("GET", auditPath+query))
		if r.Code != 400 || r.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("query %q %d", query, r.Code)
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, adminRequest(method, auditPath))
		if r.Code != 405 || r.Header().Get("Allow") != "GET" {
			t.Fatalf("method %s %d", method, r.Code)
		}
	}
	req := adminRequest("GET", auditPath)
	req.Body = io.NopCloser(strings.NewReader("{}"))
	r := httptest.NewRecorder()
	h.ServeHTTP(r, req)
	if r.Code != 400 {
		t.Fatalf("body %d", r.Code)
	}
	for _, header := range []string{"Authorization", "X-Acting-Membership-ID"} {
		req := adminRequest("GET", auditPath)
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

func TestAuditQueryAdminErrorMapping(t *testing.T) {
	for _, input := range []struct {
		err  error
		code int
		key  string
	}{{access.ErrInvalidAuditQuery, 400, "invalid_audit_query"}, {access.ErrInvalidIdentity, 403, "invalid_identity"}, {access.ErrNotFound, 404, "not_found"}, {access.ErrAuditUnavailable, 503, "unavailable"}, {errors.New("secret-database-approval"), 503, "unavailable"}} {
		h, _ := HandlerWithAuditQuery(Handler(nil), authFunc(verified), auditQueryFunc(func(context.Context, access.TrustedIdentity, access.AuditEventFilter, string, int) (access.AuditEventPage, error) {
			return access.AuditEventPage{}, input.err
		}))
		r := httptest.NewRecorder()
		h.ServeHTTP(r, adminRequest("GET", auditPath))
		if r.Code != input.code || !strings.Contains(r.Body.String(), `"error_code":"`+input.key+`"`) || strings.Contains(r.Body.String(), "secret-database-approval") || strings.Contains(r.Body.String(), "\"events\"") {
			t.Fatalf("error %d %s", r.Code, r.Body.String())
		}
	}
}

// A supported actor UUID must reach the authenticated read route.
func TestAuditQueryAdminAcceptsActorFilter(t *testing.T) {
	h, _ := HandlerWithAuditQuery(Handler(nil), authFunc(verified), auditQueryFunc(func(_ context.Context, _ access.TrustedIdentity, filter access.AuditEventFilter, _ string, _ int) (access.AuditEventPage, error) {
		if filter.ActorUserID != "abcdefab-cdef-4abc-8abc-abcdefabcdef" {
			t.Fatalf("actor not normalized: %+v", filter)
		}
		return access.AuditEventPage{}, nil
	}))
	r := httptest.NewRecorder()
	h.ServeHTTP(r, adminRequest("GET", auditPath+"?limit=20&action=retention_policy_update&outcome=allow&cursor=next&actor_user_id=ABCDEFAB-CDEF-4ABC-8ABC-ABCDEFABCDEF"))
	if r.Code != 200 {
		t.Fatalf("actor filter: %d %s", r.Code, r.Body.String())
	}
}

// Time range parameters must be accepted together with every existing filter.
func TestAuditQueryAdminAcceptsTimeRange(t *testing.T) {
	h, _ := HandlerWithAuditQuery(Handler(nil), authFunc(verified), auditQueryFunc(func(_ context.Context, _ access.TrustedIdentity, filter access.AuditEventFilter, _ string, _ int) (access.AuditEventPage, error) {
		if filter.From != "2026-10-03T00:00:00Z" || filter.Until != "2026-10-04T00:00:00Z" {
			t.Fatalf("HTTP time bounds %+v", filter)
		}
		return access.AuditEventPage{}, nil
	}))
	r := httptest.NewRecorder()
	h.ServeHTTP(r, adminRequest("GET", auditPath+"?action=retention_policy_update&outcome=allow&actor_user_id="+actorID+"&limit=20&cursor=next&from=2026-10-03T00:00:00Z&until=2026-10-04T00:00:00Z"))
	if r.Code != 200 {
		t.Fatalf("time filter: %d %s", r.Code, r.Body.String())
	}
}

func TestAuditQueryAdminAcceptsResourceFilter(t *testing.T) {
	h, _ := HandlerWithAuditQuery(Handler(nil), authFunc(verified), auditQueryFunc(func(_ context.Context, _ access.TrustedIdentity, filter access.AuditEventFilter, _ string, _ int) (access.AuditEventPage, error) {
		if filter.ResourceType != "conversation" || filter.ResourceID != "abcdefab-cdef-4abc-8abc-abcdefabcdef" {
			t.Fatalf("resource filter not passed: %+v", filter)
		}
		return access.AuditEventPage{}, nil
	}))
	r := httptest.NewRecorder()
	h.ServeHTTP(r, adminRequest("GET", auditPath+"?action=retention_policy_update&outcome=allow&actor_user_id="+actorID+"&limit=20&cursor=next&from=2026-10-03T00:00:00Z&until=2026-10-04T00:00:00Z&resource_type=conversation&resource_id=ABCDEFAB-CDEF-4ABC-8ABC-ABCDEFABCDEF"))
	if r.Code != 200 {
		t.Fatalf("resource filter: %d %s", r.Code, r.Body.String())
	}
}
