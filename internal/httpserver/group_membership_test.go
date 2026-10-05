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

const groupIntervalID = "00000000-0000-4000-8000-000000000861"

type groupMembershipStub struct {
	conversationStub
	get   func(context.Context, access.TrustedIdentity, string) (policystore.GroupMembership, error)
	leave func(context.Context, access.TrustedIdentity, string, string) (policystore.GroupLeaveResult, error)
}

func (s groupMembershipStub) GetOwnGroupMembership(ctx context.Context, id access.TrustedIdentity, group string) (policystore.GroupMembership, error) {
	return s.get(ctx, id, group)
}

func (s groupMembershipStub) LeaveGroup(ctx context.Context, id access.TrustedIdentity, group, interval string) (policystore.GroupLeaveResult, error) {
	return s.leave(ctx, id, group, interval)
}

func groupLeaveRequest(body string) *http.Request {
	r := adminRequest(http.MethodPost, "/api/v1/groups/"+targetUserID+"/leave")
	r.Header.Set("Content-Type", "application/json")
	r.Body = io.NopCloser(strings.NewReader(body))
	return r
}

func TestGroupMembershipAndLeaveRoutesUseVerifiedIdentity(t *testing.T) {
	service := groupMembershipStub{
		get: func(_ context.Context, id access.TrustedIdentity, group string) (policystore.GroupMembership, error) {
			if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || group != targetUserID {
				t.Fatalf("membership arguments: %+v %s", id, group)
			}
			return policystore.GroupMembership{IntervalID: groupIntervalID, Role: "member", JoinSeq: 4, GroupStatus: "policy_blocked"}, nil
		},
		leave: func(_ context.Context, id access.TrustedIdentity, group, interval string) (policystore.GroupLeaveResult, error) {
			if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || group != targetUserID || interval != groupIntervalID {
				t.Fatalf("leave arguments: %+v %s %s", id, group, interval)
			}
			return policystore.GroupLeaveResult{IntervalID: groupIntervalID, Status: "left", LeaveSeq: 6}, nil
		},
	}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	get := adminRequest(http.MethodGet, "/api/v1/groups/"+targetUserID+"/membership")
	get.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, get)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"interval_id":"`+groupIntervalID+`"`) ||
		!strings.Contains(res.Body.String(), `"join_seq":4`) || !strings.Contains(res.Body.String(), `"group_status":"policy_blocked"`) {
		t.Fatalf("membership response: %d %s", res.Code, res.Body.String())
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, groupLeaveRequest(`{"interval_id":"`+groupIntervalID+`"}`))
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"status":"left"`) || !strings.Contains(res.Body.String(), `"leave_seq":6`) {
		t.Fatalf("leave response: %d %s", res.Code, res.Body.String())
	}
}

func TestGroupMembershipAndLeaveRoutesRejectMalformedAndMapErrors(t *testing.T) {
	service := groupMembershipStub{
		get: func(context.Context, access.TrustedIdentity, string) (policystore.GroupMembership, error) {
			t.Fatal("unexpected get")
			return policystore.GroupMembership{}, nil
		},
		leave: func(context.Context, access.TrustedIdentity, string, string) (policystore.GroupLeaveResult, error) {
			t.Fatal("unexpected leave")
			return policystore.GroupLeaveResult{}, nil
		},
	}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []*http.Request{
		adminRequest(http.MethodGet, "/api/v1/groups/bad/membership"),
		adminRequest(http.MethodGet, "/api/v1/groups/"+targetUserID+"/membership?x=1"),
		adminRequest(http.MethodGet, "/api/v1/groups/"+targetUserID+"/membership?"),
		groupLeaveRequest(`{}`), groupLeaveRequest(`{"interval_id":"bad"}`),
		groupLeaveRequest(`{"interval_id":"` + groupIntervalID + `","tenant_id":"` + tenantID + `"}`),
		groupLeaveRequest(`{"interval_id":"` + groupIntervalID + `"} true`),
		groupLeaveRequest(strings.Repeat(" ", 2048)),
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != 400 && res.Code != 404 {
			t.Fatalf("malformed request: %d %s", res.Code, res.Body.String())
		}
	}
	for _, tc := range []struct{ method, path, allow string }{
		{http.MethodPost, "/api/v1/groups/" + targetUserID + "/membership", "GET"},
		{http.MethodGet, "/api/v1/groups/" + targetUserID + "/leave", "POST"},
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(tc.method, tc.path))
		if res.Code != 405 || res.Header().Get("Allow") != tc.allow {
			t.Fatalf("wrong method: %d %s", res.Code, res.Body.String())
		}
	}
	for _, failure := range []struct {
		err    error
		status int
		code   string
	}{
		{policystore.ErrForbidden, 403, "invalid_identity"},
		{policystore.ErrGroupNotAvailable, 404, "not_found"},
		{policystore.ErrGroupOwnerTransferRequired, 409, "owner_transfer_required"},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private SQL detail")), 503, "unavailable"},
	} {
		failed := groupMembershipStub{
			get: func(context.Context, access.TrustedIdentity, string) (policystore.GroupMembership, error) {
				return policystore.GroupMembership{}, failure.err
			},
			leave: func(context.Context, access.TrustedIdentity, string, string) (policystore.GroupLeaveResult, error) {
				return policystore.GroupLeaveResult{}, failure.err
			},
		}
		h, err := HandlerWithConversations(Handler(nil), authFunc(verified), failed)
		if err != nil {
			t.Fatal(err)
		}
		for _, req := range []*http.Request{adminRequest(http.MethodGet, "/api/v1/groups/"+targetUserID+"/membership"), groupLeaveRequest(`{"interval_id":"` + groupIntervalID + `"}`)} {
			res := httptest.NewRecorder()
			h.ServeHTTP(res, req)
			if res.Code != failure.status || !strings.Contains(res.Body.String(), failure.code) || strings.Contains(res.Body.String(), "private SQL detail") {
				t.Fatalf("failure mapping: %d %s", res.Code, res.Body.String())
			}
		}
	}
}
