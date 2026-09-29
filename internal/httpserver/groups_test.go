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

const groupRequestID = "00000000-0000-4000-8000-000000000851"

type groupCreateStub struct {
	conversationStub
	create func(context.Context, access.TrustedIdentity, policystore.CreateGroupRequest) (policystore.GroupConversation, error)
}

func (s groupCreateStub) CreateGroup(ctx context.Context, id access.TrustedIdentity, req policystore.CreateGroupRequest) (policystore.GroupConversation, error) {
	return s.create(ctx, id, req)
}

func groupRequest(body string) *http.Request {
	req := adminRequest(http.MethodPost, "/api/v1/groups")
	req.Body = io.NopCloser(strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestGroupCreateRouteUsesVerifiedIdentityAndReturnsResult(t *testing.T) {
	service := groupCreateStub{create: func(_ context.Context, id access.TrustedIdentity, req policystore.CreateGroupRequest) (policystore.GroupConversation, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) ||
			req.ClientRequestID != groupRequestID || req.Name != "项目群" ||
			len(req.MemberMembershipIDs) != 1 || req.MemberMembershipIDs[0] != targetMemID {
			t.Fatalf("group create arguments: %+v %+v", id, req)
		}
		return policystore.GroupConversation{ID: targetUserID, LastSeq: 0, PolicyVersion: 2, MemberCount: 2, Created: true}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := groupRequest(`{"client_request_id":"` + groupRequestID + `","name":"项目群","member_membership_ids":["` + targetMemID + `"]}`)
	req.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusCreated || !strings.Contains(res.Body.String(), `"id":"`+targetUserID+`"`) ||
		!strings.Contains(res.Body.String(), `"type":"group"`) ||
		!strings.Contains(res.Body.String(), `"policy_version":2`) ||
		!strings.Contains(res.Body.String(), `"member_count":2`) {
		t.Fatalf("group response: %d %s", res.Code, res.Body.String())
	}
}

func TestGroupCreateRouteRejectsMalformedRequestsAndMapsErrors(t *testing.T) {
	notCalled := groupCreateStub{create: func(context.Context, access.TrustedIdentity, policystore.CreateGroupRequest) (policystore.GroupConversation, error) {
		t.Fatal("service called for malformed group request")
		return policystore.GroupConversation{}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), notCalled)
	if err != nil {
		t.Fatal(err)
	}
	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, "/api/v1/groups", strings.NewReader(`{}`)))
	if unauthenticated.Code != http.StatusUnauthorized || unauthenticated.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("anonymous group request: %d %s", unauthenticated.Code, unauthenticated.Body.String())
	}
	for _, body := range []string{
		``, `{}`, `null`, `[]`,
		`{"client_request_id":"bad","name":"群","member_membership_ids":["` + targetMemID + `"]}`,
		`{"client_request_id":"` + groupRequestID + `","name":" ","member_membership_ids":["` + targetMemID + `"]}`,
		`{"client_request_id":"` + groupRequestID + `","name":"群","member_membership_ids":[]}`,
		`{"client_request_id":"` + groupRequestID + `","name":"群","member_membership_ids":["` + targetMemID + `","` + targetMemID + `"]}`,
		`{"client_request_id":"` + groupRequestID + `","name":"群","member_membership_ids":["` + actingID + `"]}`,
		`{"client_request_id":"` + groupRequestID + `","name":"群","member_membership_ids":["` + targetMemID + `"],"tenant_id":"` + tenantID + `"}`,
		`{"client_request_id":"` + groupRequestID + `","name":"群","member_membership_ids":["` + targetMemID + `"]} true`,
		strings.Repeat(" ", 4097),
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, groupRequest(body))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("malformed group request %q: %d %s", body[:min(len(body), 70)], res.Code, res.Body.String())
		}
	}
	for _, request := range []*http.Request{
		adminRequest(http.MethodPut, "/api/v1/groups"),
		groupRequest(`{"client_request_id":"` + groupRequestID + `","name":"群","member_membership_ids":["` + targetMemID + `"]}`),
	} {
		if request.Method == http.MethodPost {
			request.URL.RawQuery = "tenant_id=" + tenantID
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, request)
		if request.Method == http.MethodPut && (res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != "POST") {
			t.Fatalf("wrong method: %d %s", res.Code, res.Body.String())
		}
		if request.Method == http.MethodPost && res.Code != http.StatusBadRequest {
			t.Fatalf("query accepted: %d %s", res.Code, res.Body.String())
		}
	}
	req := groupRequest(`{"client_request_id":"` + groupRequestID + `","name":"群","member_membership_ids":["` + targetMemID + `"]}`)
	req.Header.Set("Content-Type", "text/plain")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("wrong media type: %d %s", res.Code, res.Body.String())
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{policystore.ErrInvalidGroupRequest, 400, "invalid_request"},
		{policystore.ErrForbidden, 403, "invalid_identity"},
		{policystore.ErrGroupNotAvailable, 404, "not_found"},
		{policystore.ErrGroupRequestConflict, 409, "idempotency_conflict"},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private SQL detail")), 503, "unavailable"},
	} {
		failed := groupCreateStub{create: func(context.Context, access.TrustedIdentity, policystore.CreateGroupRequest) (policystore.GroupConversation, error) {
			return policystore.GroupConversation{}, tc.err
		}}
		handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), failed)
		if err != nil {
			t.Fatal(err)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, groupRequest(`{"client_request_id":"`+groupRequestID+`","name":"群","member_membership_ids":["`+targetMemID+`"]}`))
		if res.Code != tc.status || !strings.Contains(res.Body.String(), tc.code) || strings.Contains(res.Body.String(), "private SQL detail") {
			t.Fatalf("group failure: %d %s", res.Code, res.Body.String())
		}
	}
}

func TestGroupCreateRouteReplayReturnsOK(t *testing.T) {
	service := groupCreateStub{create: func(context.Context, access.TrustedIdentity, policystore.CreateGroupRequest) (policystore.GroupConversation, error) {
		return policystore.GroupConversation{ID: targetUserID, MemberCount: 2, Created: false}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, groupRequest(`{"client_request_id":"`+groupRequestID+`","name":"项目群","member_membership_ids":["`+targetMemID+`"]}`))
	if res.Code != http.StatusOK {
		t.Fatalf("group replay: %d %s", res.Code, res.Body.String())
	}
}
