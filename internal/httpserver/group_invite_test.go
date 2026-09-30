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

const inviteRequestID = "00000000-0000-4000-8000-000000000881"

type groupInviteStub struct {
	conversationStub
	invite func(context.Context, access.TrustedIdentity, string, policystore.InviteGroupRequest) (policystore.GroupInvitation, error)
}

func (s groupInviteStub) InviteGroupMember(ctx context.Context, id access.TrustedIdentity, group string, req policystore.InviteGroupRequest) (policystore.GroupInvitation, error) {
	return s.invite(ctx, id, group, req)
}

func groupInviteRequest(body string) *http.Request {
	r := adminRequest(http.MethodPost, "/api/v1/groups/"+targetUserID+"/invitations")
	r.Header.Set("Content-Type", "application/json")
	r.Body = io.NopCloser(strings.NewReader(body))
	return r
}

func TestGroupInviteRouteUsesVerifiedIdentityAndReturnsInterval(t *testing.T) {
	service := groupInviteStub{invite: func(_ context.Context, id access.TrustedIdentity, group string, req policystore.InviteGroupRequest) (policystore.GroupInvitation, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || group != targetUserID ||
			req.ClientRequestID != inviteRequestID || req.TargetMembershipID != targetMemID {
			t.Fatalf("invite arguments: %+v %s %+v", id, group, req)
		}
		return policystore.GroupInvitation{IntervalID: groupIntervalID, JoinSeq: 4, PolicyVersion: 2, Created: true}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := groupInviteRequest(`{"client_request_id":"` + inviteRequestID + `","target_membership_id":"` + targetMemID + `"}`)
	req.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != 201 || !strings.Contains(res.Body.String(), `"interval_id":"`+groupIntervalID+`"`) ||
		!strings.Contains(res.Body.String(), `"join_seq":4`) || !strings.Contains(res.Body.String(), `"policy_version":2`) {
		t.Fatalf("invite response: %d %s", res.Code, res.Body.String())
	}
	service.invite = func(context.Context, access.TrustedIdentity, string, policystore.InviteGroupRequest) (policystore.GroupInvitation, error) {
		return policystore.GroupInvitation{IntervalID: groupIntervalID, JoinSeq: 4, PolicyVersion: 2, Created: false}, nil
	}
	handler, _ = HandlerWithConversations(Handler(nil), authFunc(verified), service)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, groupInviteRequest(`{"client_request_id":"`+inviteRequestID+`","target_membership_id":"`+targetMemID+`"}`))
	if res.Code != 200 {
		t.Fatalf("invite replay: %d %s", res.Code, res.Body.String())
	}
}

func TestGroupInviteRouteRejectsMalformedAndMapsErrors(t *testing.T) {
	notCalled := groupInviteStub{invite: func(context.Context, access.TrustedIdentity, string, policystore.InviteGroupRequest) (policystore.GroupInvitation, error) {
		t.Fatal("invite service called for malformed request")
		return policystore.GroupInvitation{}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), notCalled)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		``, `{}`, `null`, `[]`,
		`{"client_request_id":"bad","target_membership_id":"` + targetMemID + `"}`,
		`{"client_request_id":"` + inviteRequestID + `","target_membership_id":"bad"}`,
		`{"client_request_id":"` + inviteRequestID + `","target_membership_id":"` + targetMemID + `","tenant_id":"` + tenantID + `"}`,
		`{"client_request_id":"` + inviteRequestID + `","target_membership_id":"` + targetMemID + `","target_membership_id":"` + targetMemID + `"}`,
		`{"Client_request_id":"` + inviteRequestID + `","target_membership_id":"` + targetMemID + `"}`,
		`{"client_request_id":"` + inviteRequestID + `","target_membership_id":"` + targetMemID + `"} true`,
		strings.Repeat(" ", 2048),
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, groupInviteRequest(body))
		if res.Code != 400 {
			t.Fatalf("malformed invite %q: %d %s", body[:min(len(body), 50)], res.Code, res.Body.String())
		}
	}
	for _, req := range []*http.Request{
		adminRequest(http.MethodGet, "/api/v1/groups/"+targetUserID+"/invitations"),
		groupInviteRequest(`{"client_request_id":"` + inviteRequestID + `","target_membership_id":"` + targetMemID + `"}`),
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
		{policystore.ErrInvalidGroupInviteRequest, 400, "invalid_request"},
		{policystore.ErrForbidden, 403, "invalid_identity"},
		{policystore.ErrGroupInvitePermissionDenied, 403, "group_permission_denied"},
		{policystore.ErrGroupNotAvailable, 404, "not_found"},
		{policystore.ErrGroupPolicyBlocked, 409, "group_policy_blocked"},
		{policystore.ErrGroupInviteConflict, 409, "idempotency_conflict"},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private SQL detail")), 503, "unavailable"},
	} {
		failed := groupInviteStub{invite: func(context.Context, access.TrustedIdentity, string, policystore.InviteGroupRequest) (policystore.GroupInvitation, error) {
			return policystore.GroupInvitation{}, failure.err
		}}
		h, _ := HandlerWithConversations(Handler(nil), authFunc(verified), failed)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, groupInviteRequest(`{"client_request_id":"`+inviteRequestID+`","target_membership_id":"`+targetMemID+`"}`))
		if res.Code != failure.status || !strings.Contains(res.Body.String(), failure.code) || strings.Contains(res.Body.String(), "private SQL detail") {
			t.Fatalf("failure mapping: %d %s", res.Code, res.Body.String())
		}
	}
}
