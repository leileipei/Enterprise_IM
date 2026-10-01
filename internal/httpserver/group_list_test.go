package httpserver

import (
	"context"
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

type groupListStub struct {
	conversationStub
	list func(context.Context, access.TrustedIdentity, string, int) (policystore.GroupListPage, error)
}

func (s groupListStub) ListGroups(ctx context.Context, id access.TrustedIdentity,
	cursor string, limit int) (policystore.GroupListPage, error) {
	return s.list(ctx, id, cursor, limit)
}

func TestGroupListRouteUsesVerifiedIdentityAndReturnsCurrentGroups(t *testing.T) {
	service := groupListStub{list: func(_ context.Context, id access.TrustedIdentity,
		cursor string, limit int) (policystore.GroupListPage, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) ||
			cursor != "opaque-cursor" || limit != 2 {
			t.Fatalf("group list args: %+v %q %d", id, cursor, limit)
		}
		return policystore.GroupListPage{Groups: []policystore.ListedGroup{{
			ID: targetUserID, Name: "项目群", Status: "policy_blocked", Role: "owner",
			SourceMembershipID: actingID, LastSeq: 7,
			UpdatedAt: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC),
		}}, HasMore: true, NextCursor: "next-page"}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	request := adminRequest(http.MethodGet, "/api/v1/groups?cursor=opaque-cursor&limit=2")
	request.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, request)
	for _, want := range []string{
		`"id":"` + targetUserID + `"`, `"type":"group"`, `"name":"项目群"`,
		`"status":"policy_blocked"`, `"role":"owner"`,
		`"source_membership_id":"` + actingID + `"`, `"last_seq":7`,
		`"next_cursor":"next-page"`,
	} {
		if !strings.Contains(res.Body.String(), want) {
			t.Fatalf("group list missing %s: %d %s", want, res.Code, res.Body.String())
		}
	}
	if res.Code != http.StatusOK {
		t.Fatalf("group list status: %d %s", res.Code, res.Body.String())
	}
}

func TestGroupListRouteRejectsInvalidQueryBodyAndMapsFailures(t *testing.T) {
	service := groupListStub{list: func(context.Context, access.TrustedIdentity, string, int) (policystore.GroupListPage, error) {
		t.Fatal("group list called for invalid request")
		return policystore.GroupListPage{}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/v1/groups?", "/api/v1/groups?limit=0", "/api/v1/groups?limit=51",
		"/api/v1/groups?limit=2&limit=3", "/api/v1/groups?cursor=",
		"/api/v1/groups?cursor=x&cursor=y", "/api/v1/groups?tenant_id=x",
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(http.MethodGet, path))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("query accepted %s: %d %s", path, res.Code, res.Body.String())
		}
	}
	request := adminRequest(http.MethodGet, "/api/v1/groups")
	request.Body = io.NopCloser(strings.NewReader("unexpected body"))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, request)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("body accepted: %d %s", res.Code, res.Body.String())
	}
	for _, failure := range []struct {
		err    error
		status int
	}{
		{policystore.ErrInvalidGroupListRequest, http.StatusBadRequest},
		{policystore.ErrForbidden, http.StatusForbidden},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private detail")), http.StatusServiceUnavailable},
	} {
		failed, err := HandlerWithConversations(Handler(nil), authFunc(verified), groupListStub{
			list: func(context.Context, access.TrustedIdentity, string, int) (policystore.GroupListPage, error) {
				return policystore.GroupListPage{}, failure.err
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		res := httptest.NewRecorder()
		failed.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/groups"))
		if res.Code != failure.status || strings.Contains(res.Body.String(), "private detail") {
			t.Fatalf("group list failure: %d %s", res.Code, res.Body.String())
		}
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodPut, "/api/v1/groups"))
	if res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != "GET, POST" {
		t.Fatalf("group list methods: %d %s", res.Code, res.Header().Get("Allow"))
	}
}
