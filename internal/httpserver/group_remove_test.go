package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type groupRemoveStub struct {
	conversationStub
	remove func(context.Context, access.TrustedIdentity, string, string) (policystore.GroupRemoveResult, error)
}

func (s groupRemoveStub) RemoveGroupMember(ctx context.Context, id access.TrustedIdentity, group, interval string) (policystore.GroupRemoveResult, error) {
	return s.remove(ctx, id, group, interval)
}

func groupRemoveRequest(body string) *http.Request {
	r := adminRequest(http.MethodPost, "/api/v1/groups/"+targetUserID+"/removals")
	r.Header.Set("Content-Type", "application/json")
	r.Body = io.NopCloser(strings.NewReader(body))
	return r
}

func TestGroupRemoveRouteUsesVerifiedIdentityAndReturnsEndSeq(t *testing.T) {
	service := groupRemoveStub{remove: func(_ context.Context, id access.TrustedIdentity, group, interval string) (policystore.GroupRemoveResult, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || group != targetUserID || interval != groupIntervalID {
			t.Fatalf("remove arguments: %+v %s %s", id, group, interval)
		}
		return policystore.GroupRemoveResult{IntervalID: groupIntervalID, Status: "removed", LeaveSeq: 6}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := groupRemoveRequest(`{"interval_id":"` + groupIntervalID + `"}`)
	req.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"interval_id":"`+groupIntervalID+`"`) ||
		!strings.Contains(res.Body.String(), `"status":"removed"`) || !strings.Contains(res.Body.String(), `"leave_seq":6`) {
		t.Fatalf("remove response: %d %s", res.Code, res.Body.String())
	}
}

func TestGroupRemoveRouteRejectsMalformedAndMapsErrors(t *testing.T) {
	notCalled := groupRemoveStub{remove: func(context.Context, access.TrustedIdentity, string, string) (policystore.GroupRemoveResult, error) {
		t.Fatal("remove called for malformed request")
		return policystore.GroupRemoveResult{}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), notCalled)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		``, `{}`, `null`, `[]`, `{"interval_id":"bad"}`,
		`{"interval_id":"` + groupIntervalID + `","tenant_id":"` + tenantID + `"}`,
		`{"interval_id":"` + groupIntervalID + `","interval_id":"` + groupIntervalID + `"}`,
		`{"Interval_id":"` + groupIntervalID + `"}`,
		`{"interval_id":"` + groupIntervalID + `"} true`, strings.Repeat(" ", 2048),
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, groupRemoveRequest(body))
		if res.Code != 400 {
			t.Fatalf("malformed removal %q: %d %s", body[:min(len(body), 50)], res.Code, res.Body.String())
		}
	}
	for _, req := range []*http.Request{
		adminRequest(http.MethodGet, "/api/v1/groups/"+targetUserID+"/removals"),
		groupRemoveRequest(`{"interval_id":"` + groupIntervalID + `"}`),
	} {
		if req.Method == http.MethodPost {
			req.URL.RawQuery = "tenant_id=" + tenantID
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if req.Method == http.MethodGet && (res.Code != 405 || res.Header().Get("Allow") != "POST") {
			t.Fatalf("wrong method: %d %s", res.Code, res.Body.String())
		}
		if req.Method == http.MethodPost && res.Code != 400 {
			t.Fatalf("query accepted: %d %s", res.Code, res.Body.String())
		}
	}
	for _, failure := range []struct {
		err    error
		status int
		code   string
	}{
		{policystore.ErrInvalidGroupMembershipRequest, 400, "invalid_request"},
		{policystore.ErrForbidden, 403, "invalid_identity"},
		{policystore.ErrGroupRemovePermissionDenied, 403, "group_permission_denied"},
		{policystore.ErrGroupNotAvailable, 404, "not_found"},
		{policystore.ErrGroupOwnerTransferRequired, 409, "owner_transfer_required"},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private SQL detail")), 503, "unavailable"},
	} {
		failed := groupRemoveStub{remove: func(context.Context, access.TrustedIdentity, string, string) (policystore.GroupRemoveResult, error) {
			return policystore.GroupRemoveResult{}, failure.err
		}}
		h, _ := HandlerWithConversations(Handler(nil), authFunc(verified), failed)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, groupRemoveRequest(`{"interval_id":"`+groupIntervalID+`"}`))
		if res.Code != failure.status || !strings.Contains(res.Body.String(), failure.code) || strings.Contains(res.Body.String(), "private SQL detail") {
			t.Fatalf("error mapping: %d %s", res.Code, res.Body.String())
		}
	}
}
