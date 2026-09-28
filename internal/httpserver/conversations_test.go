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
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type conversationStub func(context.Context, access.TrustedIdentity, string) (policystore.DirectConversation, error)

func (f conversationStub) StartDirectConversation(ctx context.Context, id access.TrustedIdentity, target string) (policystore.DirectConversation, error) {
	return f(ctx, id, target)
}

func conversationRequest(body string) *http.Request {
	req := adminRequest(http.MethodPost, "/api/v1/conversations")
	req.Body = io.NopCloser(strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestConversationRouteUsesVerifiedActorAndReturnsConversation(t *testing.T) {
	service := conversationStub(func(_ context.Context, id access.TrustedIdentity, target string) (policystore.DirectConversation, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || target != targetMemID {
			t.Fatalf("untrusted conversation input: %+v %q", id, target)
		}
		return policystore.DirectConversation{ID: targetUserID, LastSeq: 0, PolicyVersion: 3,
			CrossLegal: true, DecisionReason: policy.ReasonAllowedRule}, nil
	})
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := conversationRequest(`{"target_membership_id":"` + targetMemID + `"}`)
	req.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"id":"`+targetUserID+`"`) ||
		!strings.Contains(res.Body.String(), `"type":"direct"`) ||
		!strings.Contains(res.Body.String(), `"policy_version":3`) ||
		!strings.Contains(res.Body.String(), `"cross_legal":true`) ||
		!strings.Contains(res.Body.String(), `"decision_reason":"allowed_rule"`) {
		t.Fatalf("conversation response: %d %s", res.Code, res.Body.String())
	}
}

func TestConversationRouteRejectsMalformedAndPrivilegeFields(t *testing.T) {
	service := conversationStub(func(context.Context, access.TrustedIdentity, string) (policystore.DirectConversation, error) {
		t.Fatal("service called for malformed request")
		return policystore.DirectConversation{}, nil
	})
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		``, `{}`, `null`, `[]`, `{"target_membership_id":"bad"}`,
		`{"target_membership_id":null}`, `{"target_membership_id":42}`,
		`{"target_membership_id":"` + targetMemID + `","tenant_id":"` + tenantID + `"}`,
		`{"target_membership_id":"` + targetMemID + `"} true`,
		strings.Repeat(" ", 1025),
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, conversationRequest(body))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("malformed conversation request %q: %d %s", body[:min(len(body), 70)], res.Code, res.Body.String())
		}
	}
	for _, request := range []*http.Request{
		adminRequest(http.MethodGet, "/api/v1/conversations"),
		adminRequest(http.MethodPost, "/api/v1/conversations?scope_allowed=true"),
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, request)
		if request.Method == http.MethodGet && (res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != http.MethodPost) {
			t.Fatalf("wrong conversation method: %d %s", res.Code, res.Body.String())
		}
		if request.URL.RawQuery != "" && res.Code != http.StatusBadRequest {
			t.Fatalf("conversation query accepted: %d %s", res.Code, res.Body.String())
		}
	}
	req := conversationRequest(`{"target_membership_id":"` + targetMemID + `"}`)
	req.Header.Set("Content-Type", "text/plain")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("wrong content type accepted: %d %s", res.Code, res.Body.String())
	}
}

func TestConversationRouteAuthenticationAndErrorMapping(t *testing.T) {
	service := conversationStub(func(context.Context, access.TrustedIdentity, string) (policystore.DirectConversation, error) {
		return policystore.DirectConversation{}, nil
	})
	if _, err := HandlerWithConversations(nil, authFunc(verified), service); err == nil {
		t.Fatal("nil base accepted")
	}
	if _, err := HandlerWithConversations(Handler(nil), nil, service); err == nil {
		t.Fatal("nil authenticator accepted")
	}
	if _, err := HandlerWithConversations(Handler(nil), authFunc(verified), nil); err == nil {
		t.Fatal("nil service accepted")
	}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", strings.NewReader(`{"target_membership_id":"`+targetMemID+`"}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("anonymous conversation: %d %s", res.Code, res.Body.String())
	}
	for _, failure := range []struct {
		err    error
		status int
		code   string
	}{
		{policystore.ErrForbidden, http.StatusForbidden, "invalid_identity"},
		{policystore.ErrChatNotAvailable, http.StatusNotFound, "not_found"},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private SQL detail")), http.StatusServiceUnavailable, "unavailable"},
	} {
		failed, err := HandlerWithConversations(Handler(nil), authFunc(verified), conversationStub(func(context.Context, access.TrustedIdentity, string) (policystore.DirectConversation, error) {
			return policystore.DirectConversation{}, failure.err
		}))
		if err != nil {
			t.Fatal(err)
		}
		res := httptest.NewRecorder()
		failed.ServeHTTP(res, conversationRequest(`{"target_membership_id":"`+targetMemID+`"}`))
		if res.Code != failure.status || !strings.Contains(res.Body.String(), failure.code) || strings.Contains(res.Body.String(), "private SQL detail") {
			t.Fatalf("conversation error mapping: %d %s", res.Code, res.Body.String())
		}
	}
}
