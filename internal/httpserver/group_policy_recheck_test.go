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

type groupRecheckStub struct {
	conversationStub
	recheck func(context.Context, access.TrustedIdentity, string) (policystore.GroupPolicyRecheck, error)
}

func (s groupRecheckStub) RecheckGroupPolicy(ctx context.Context, id access.TrustedIdentity, group string) (policystore.GroupPolicyRecheck, error) {
	return s.recheck(ctx, id, group)
}

func groupRecheckRequest() *http.Request {
	return adminRequest(http.MethodPost, "/api/v1/groups/"+targetUserID+"/policy-rechecks")
}

func TestGroupPolicyRecheckRouteUsesTrustedIdentityAndStrictEmptyRequest(t *testing.T) {
	service := groupRecheckStub{recheck: func(_ context.Context, id access.TrustedIdentity, group string) (policystore.GroupPolicyRecheck, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || group != targetUserID {
			t.Fatalf("untrusted recheck input: %+v %s", id, group)
		}
		return policystore.GroupPolicyRecheck{Status: "active", PolicyVersion: 3}, nil
	}}
	h, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := groupRecheckRequest()
	req.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"status":"active"`) || !strings.Contains(res.Body.String(), `"policy_version":3`) {
		t.Fatalf("recheck response: %d %s", res.Code, res.Body.String())
	}
	for _, bad := range []string{
		"/api/v1/groups/" + targetUserID + "/policy-rechecks?force=1",
		"/api/v1/groups/" + targetUserID + "/policy-rechecks?",
	} {
		r := adminRequest(http.MethodPost, bad)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, r)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("query accepted: %d %s", res.Code, res.Body.String())
		}
	}
	r := groupRecheckRequest()
	r.Body = io.NopCloser(strings.NewReader(`{}`))
	res = httptest.NewRecorder()
	h.ServeHTTP(res, r)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("body accepted: %d %s", res.Code, res.Body.String())
	}
	r = adminRequest(http.MethodGet, "/api/v1/groups/"+targetUserID+"/policy-rechecks")
	res = httptest.NewRecorder()
	h.ServeHTTP(res, r)
	if res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != "POST" {
		t.Fatalf("wrong method: %d %s", res.Code, res.Body.String())
	}
}

func TestGroupPolicyRecheckRouteMapsErrors(t *testing.T) {
	for _, failure := range []struct {
		err    error
		status int
		code   string
	}{
		{policystore.ErrForbidden, 403, "invalid_identity"},
		{policystore.ErrGroupRecheckPermissionDenied, 403, "group_permission_denied"},
		{policystore.ErrGroupNotAvailable, 404, "not_found"},
		{policystore.ErrGroupPolicyBlocked, 409, "group_policy_blocked"},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private SQL detail")), 503, "unavailable"},
	} {
		service := groupRecheckStub{recheck: func(context.Context, access.TrustedIdentity, string) (policystore.GroupPolicyRecheck, error) {
			return policystore.GroupPolicyRecheck{}, failure.err
		}}
		h, _ := HandlerWithConversations(Handler(nil), authFunc(verified), service)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, groupRecheckRequest())
		if res.Code != failure.status || !strings.Contains(res.Body.String(), failure.code) || strings.Contains(res.Body.String(), "private SQL detail") {
			t.Fatalf("error mapping: %d %s", res.Code, res.Body.String())
		}
	}
}
