package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type groupRosterStub struct {
	conversationStub
	list func(context.Context, access.TrustedIdentity, string, string, int) (policystore.GroupRosterPage, error)
}

func (s groupRosterStub) ListGroupMembers(ctx context.Context, id access.TrustedIdentity, groupID, cursor string, limit int) (policystore.GroupRosterPage, error) {
	return s.list(ctx, id, groupID, cursor, limit)
}

func TestGroupRosterRouteReturnsMinimalPage(t *testing.T) {
	service := groupRosterStub{list: func(_ context.Context, id access.TrustedIdentity, groupID, cursor string, limit int) (policystore.GroupRosterPage, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) ||
			groupID != targetUserID || cursor != "page-2" || limit != 2 {
			t.Fatalf("roster arguments: %+v %s %s %d", id, groupID, cursor, limit)
		}
		return policystore.GroupRosterPage{Members: []policystore.GroupRosterMember{{IntervalID: targetMemID,
			DisplayName: "用户 A", Role: "member", OrganizationName: "公司 A"}}, HasMore: true, NextCursor: "next"}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/groups/"+targetUserID+"/members?cursor=page-2&limit=2"))
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"interval_id":"`+targetMemID+`"`) ||
		!strings.Contains(res.Body.String(), `"display_name":"用户 A"`) ||
		!strings.Contains(res.Body.String(), `"organization_name":"公司 A"`) ||
		!strings.Contains(res.Body.String(), `"next_cursor":"next"`) ||
		strings.Contains(res.Body.String(), "employee_no") {
		t.Fatalf("roster response: %d %s", res.Code, res.Body.String())
	}
}

func TestGroupRosterRouteRejectsInvalidRequestsAndMapsErrors(t *testing.T) {
	service := groupRosterStub{list: func(context.Context, access.TrustedIdentity, string, string, int) (policystore.GroupRosterPage, error) {
		t.Fatal("invalid request reached service")
		return policystore.GroupRosterPage{}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/groups/" + targetUserID + "/members"
	for _, suffix := range []string{"?", "?limit=0", "?limit=51", "?cursor=", "?unknown=x"} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(http.MethodGet, base+suffix))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("accepted %s: %d %s", suffix, res.Code, res.Body.String())
		}
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodPost, base))
	if res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != "GET" {
		t.Fatalf("method accepted: %d %s", res.Code, res.Body.String())
	}
	for _, failure := range []struct {
		err    error
		status int
		code   string
	}{
		{policystore.ErrInvalidGroupRosterRequest, 400, "invalid_request"},
		{policystore.ErrForbidden, 403, "invalid_identity"},
		{policystore.ErrGroupRosterPermissionDenied, 403, "group_permission_denied"},
		{policystore.ErrGroupNotAvailable, 404, "not_found"},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private detail")), 503, "unavailable"},
	} {
		failed, err := HandlerWithConversations(Handler(nil), authFunc(verified), groupRosterStub{
			list: func(context.Context, access.TrustedIdentity, string, string, int) (policystore.GroupRosterPage, error) {
				return policystore.GroupRosterPage{}, failure.err
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		res := httptest.NewRecorder()
		failed.ServeHTTP(res, adminRequest(http.MethodGet, base))
		if res.Code != failure.status || !strings.Contains(res.Body.String(), failure.code) || strings.Contains(res.Body.String(), "private detail") {
			t.Fatalf("error mapping: %d %s", res.Code, res.Body.String())
		}
	}
}
