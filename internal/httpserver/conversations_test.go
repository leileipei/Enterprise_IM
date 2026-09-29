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
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type conversationStub func(context.Context, access.TrustedIdentity, string) (policystore.DirectConversation, error)

func (f conversationStub) StartDirectConversation(ctx context.Context, id access.TrustedIdentity, target string) (policystore.DirectConversation, error) {
	return f(ctx, id, target)
}

func (conversationStub) CreateGroup(context.Context, access.TrustedIdentity, policystore.CreateGroupRequest) (policystore.GroupConversation, error) {
	panic("unexpected group create")
}

func (conversationStub) SendTextMessage(context.Context, access.TrustedIdentity, string, string, string) (policystore.MessageACK, error) {
	panic("unexpected message send")
}

func (conversationStub) PullTextMessages(context.Context, access.TrustedIdentity, string, int64, int) (policystore.MessagePage, error) {
	panic("unexpected message pull")
}

func (conversationStub) ListDirectConversations(context.Context, access.TrustedIdentity, string, int) (policystore.ConversationListPage, error) {
	panic("unexpected conversation list")
}

type conversationListStub struct {
	conversationStub
	list func(context.Context, access.TrustedIdentity, string, int) (policystore.ConversationListPage, error)
}

func (s conversationListStub) ListDirectConversations(ctx context.Context, id access.TrustedIdentity, cursor string, limit int) (policystore.ConversationListPage, error) {
	return s.list(ctx, id, cursor, limit)
}

func TestConversationListRouteUsesVerifiedIdentityAndHidesPeerProfile(t *testing.T) {
	service := conversationListStub{list: func(_ context.Context, id access.TrustedIdentity, cursor string, limit int) (policystore.ConversationListPage, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) ||
			cursor != "opaque-cursor" || limit != 2 {
			t.Fatalf("list arguments: %+v %q %d", id, cursor, limit)
		}
		return policystore.ConversationListPage{Conversations: []policystore.ListedConversation{
			{ID: targetUserID, LastSeq: 3, UpdatedAt: time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC),
				PeerVisible: true, PeerDisplayName: "同事", PeerOrganizationName: "总部"},
			{ID: targetMemID, LastSeq: 5, UpdatedAt: time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC),
				PeerVisible: false, PeerDisplayName: "不得出现", PeerOrganizationName: "隐藏组织"},
		}, HasMore: true, NextCursor: "next-page"}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := adminRequest(http.MethodGet, "/api/v1/conversations?cursor=opaque-cursor&limit=2")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"next_cursor":"next-page"`) ||
		!strings.Contains(res.Body.String(), `"display_name":"同事"`) ||
		strings.Contains(res.Body.String(), "不得出现") || strings.Contains(res.Body.String(), "隐藏组织") ||
		!strings.Contains(res.Body.String(), `"peer_visible":false`) {
		t.Fatalf("list response: %d %s", res.Code, res.Body.String())
	}
}

func TestConversationListRouteRejectsInvalidQueryAndMapsFailures(t *testing.T) {
	service := conversationListStub{list: func(context.Context, access.TrustedIdentity, string, int) (policystore.ConversationListPage, error) {
		t.Fatal("list called for invalid query")
		return policystore.ConversationListPage{}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/v1/conversations?", "/api/v1/conversations?limit=0", "/api/v1/conversations?limit=51",
		"/api/v1/conversations?limit=2&limit=3", "/api/v1/conversations?cursor=",
		"/api/v1/conversations?cursor=x&cursor=y", "/api/v1/conversations?tenant_id=x",
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(http.MethodGet, path))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("query accepted %s: %d %s", path, res.Code, res.Body.String())
		}
	}
	for _, failure := range []struct {
		serviceError error
		status       int
	}{
		{policystore.ErrInvalidConversationListRequest, http.StatusBadRequest},
		{policystore.ErrForbidden, http.StatusForbidden},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private detail")), http.StatusServiceUnavailable},
	} {
		failed, err := HandlerWithConversations(Handler(nil), authFunc(verified), conversationListStub{list: func(context.Context, access.TrustedIdentity, string, int) (policystore.ConversationListPage, error) {
			return policystore.ConversationListPage{}, failure.serviceError
		}})
		if err != nil {
			t.Fatal(err)
		}
		res := httptest.NewRecorder()
		failed.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/conversations"))
		if res.Code != failure.status || strings.Contains(res.Body.String(), "private detail") {
			t.Fatalf("list failure: %d %s", res.Code, res.Body.String())
		}
	}
}

type messageStub struct {
	conversationStub
	send func(context.Context, access.TrustedIdentity, string, string, string) (policystore.MessageACK, error)
}

func (m messageStub) SendTextMessage(ctx context.Context, id access.TrustedIdentity, conversationID, clientID, body string) (policystore.MessageACK, error) {
	return m.send(ctx, id, conversationID, clientID, body)
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
		adminRequest(http.MethodPut, "/api/v1/conversations"),
		adminRequest(http.MethodPost, "/api/v1/conversations?scope_allowed=true"),
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, request)
		if request.Method == http.MethodPut && (res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != "GET, POST") {
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

func messageRequest(path, body string) *http.Request {
	req := adminRequest(http.MethodPost, path)
	req.Body = io.NopCloser(strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestMessageRouteUsesVerifiedActorAndReturnsCommittedACK(t *testing.T) {
	conversationID := "00000000-0000-4000-8000-000000000471"
	clientID := "0199f04a-0000-7000-8000-000000000471"
	path := "/api/v1/conversations/" + conversationID + "/messages"
	service := messageStub{send: func(_ context.Context, id access.TrustedIdentity, chat, client, body string) (policystore.MessageACK, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) ||
			chat != conversationID || client != clientID || body != "你好" {
			t.Fatalf("untrusted message arguments: %+v %q %q %q", id, chat, client, body)
		}
		return policystore.MessageACK{MessageID: targetUserID, ConversationID: chat, Seq: 7,
			ServerTime: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := messageRequest(path, `{"client_msg_id":"`+clientID+`","text":"你好"}`)
	req.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"message_id":"`+targetUserID+`"`) ||
		!strings.Contains(res.Body.String(), `"conversation_id":"`+conversationID+`"`) ||
		!strings.Contains(res.Body.String(), `"seq":7`) || !strings.Contains(res.Body.String(), `"server_time":"2026-09-28T10:00:00Z"`) {
		t.Fatalf("message ACK: %d %s", res.Code, res.Body.String())
	}
}

func TestMessageRouteAcceptsEscapedTextByDecodedByteLength(t *testing.T) {
	conversationID := "00000000-0000-4000-8000-000000000471"
	clientID := "0199f04a-0000-7000-8000-000000000471"
	service := messageStub{send: func(_ context.Context, _ access.TrustedIdentity, _, _, body string) (policystore.MessageACK, error) {
		if len(body) != 16*1024 || body != strings.Repeat("a", 16*1024) {
			t.Fatalf("escaped text changed: %d bytes", len(body))
		}
		return policystore.MessageACK{MessageID: targetUserID, ConversationID: conversationID, Seq: 1,
			ServerTime: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"client_msg_id":"` + clientID + `","text":"` + strings.Repeat(`\u0061`, 16*1024) + `"}`
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, messageRequest("/api/v1/conversations/"+conversationID+"/messages", body))
	if res.Code != http.StatusOK {
		t.Fatalf("valid escaped text: %d %s", res.Code, res.Body.String())
	}
}

func TestMessageRouteRejectsInvalidRequestAndMapsErrors(t *testing.T) {
	conversationID := "00000000-0000-4000-8000-000000000471"
	clientID := "0199f04a-0000-7000-8000-000000000471"
	path := "/api/v1/conversations/" + conversationID + "/messages"
	service := messageStub{send: func(context.Context, access.TrustedIdentity, string, string, string) (policystore.MessageACK, error) {
		t.Fatal("service called for malformed request")
		return policystore.MessageACK{}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "null", "[]", `{}`, `{"client_msg_id":"bad","text":"ok"}`,
		`{"client_msg_id":"` + clientID + `","text":""}`,
		`{"client_msg_id":"` + clientID + `","text":"x","tenant_id":"` + tenantID + `"}`,
		`{"client_msg_id":"` + clientID + `","text":"x"} true`,
		"{\"client_msg_id\":\"" + clientID + "\",\"text\":\"\xff\"}",
		`{"client_msg_id":"` + clientID + `","text":"` + strings.Repeat("x", 16*1024+1) + `"}`,
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, messageRequest(path, body))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("invalid body: %d %s", res.Code, res.Body.String())
		}
	}
	for _, path := range []string{"/api/v1/conversations/not-uuid/messages", "/api/v1/conversations/" + conversationID + "/messages/extra"} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, messageRequest(path, `{"client_msg_id":"`+clientID+`","text":"ok"}`))
		if res.Code != http.StatusNotFound {
			t.Fatalf("bad path: %d %s", res.Code, res.Body.String())
		}
	}
	req := messageRequest(path, `{"client_msg_id":"`+clientID+`","text":"ok"}`)
	req.Method = http.MethodPut
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != "GET, POST" {
		t.Fatalf("method: %d %s", res.Code, res.Body.String())
	}
	req = messageRequest(path, `{"client_msg_id":"`+clientID+`","text":"ok"}`)
	req.Header.Set("Content-Type", "text/plain")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("content type: %d %s", res.Code, res.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"client_msg_id":"`+clientID+`","text":"ok"}`))
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d %s", res.Code, res.Body.String())
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{policystore.ErrInvalidClientMessageID, 400, "invalid_request"},
		{policystore.ErrRetryExpired, 410, "retry_window_expired"},
		{policystore.ErrForbidden, 403, "invalid_identity"},
		{policystore.ErrMessageNotAvailable, 404, "not_found"},
		{policystore.ErrIdempotencyConflict, 409, "idempotency_conflict"},
		{policystore.ErrConversationContextChanged, 409, "conversation_context_changed"},
		{policystore.ErrMessageRateLimited, 429, "rate_limited"},
		{errors.New("private SQL error"), 503, "unavailable"},
	} {
		failed, err := HandlerWithConversations(Handler(nil), authFunc(verified), messageStub{send: func(context.Context, access.TrustedIdentity, string, string, string) (policystore.MessageACK, error) {
			return policystore.MessageACK{}, tc.err
		}})
		if err != nil {
			t.Fatal(err)
		}
		res := httptest.NewRecorder()
		failed.ServeHTTP(res, messageRequest(path, `{"client_msg_id":"`+clientID+`","text":"ok"}`))
		if res.Code != tc.status || !strings.Contains(res.Body.String(), tc.code) || strings.Contains(res.Body.String(), "private SQL error") {
			t.Fatalf("error mapping %v: %d %s", tc.err, res.Code, res.Body.String())
		}
	}
}

type pullStub struct {
	conversationStub
	pull func(context.Context, access.TrustedIdentity, string, int64, int) (policystore.MessagePage, error)
}

func (p pullStub) PullTextMessages(ctx context.Context, id access.TrustedIdentity, conversationID string, afterSeq int64, limit int) (policystore.MessagePage, error) {
	return p.pull(ctx, id, conversationID, afterSeq, limit)
}

func TestMessagePullRouteStrictCursorIdentityAndRedaction(t *testing.T) {
	conversationID := "00000000-0000-4000-8000-000000000471"
	path := "/api/v1/conversations/" + conversationID + "/messages"
	called := 0
	service := pullStub{pull: func(_ context.Context, id access.TrustedIdentity, chat string, afterSeq int64, limit int) (policystore.MessagePage, error) {
		called++
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || chat != conversationID || afterSeq != 8 || limit != 2 {
			t.Fatalf("untrusted pull arguments: %+v %s %d %d", id, chat, afterSeq, limit)
		}
		return policystore.MessagePage{ConversationID: chat, NextAfterSeq: 10, HasMore: true,
			Messages: []policystore.PulledMessage{
				{MessageID: targetUserID, Seq: 9, SenderUserID: actorID, Text: "你好", ServerTime: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)},
				{Seq: 10, Redacted: true},
			}}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodGet, path+"?after_seq=8&limit=2"))
	if res.Code != 200 || res.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(res.Body.String(), `"next_after_seq":10`) ||
		!strings.Contains(res.Body.String(), `"has_more":true`) ||
		!strings.Contains(res.Body.String(), `"text":"你好"`) ||
		!strings.Contains(res.Body.String(), `{"seq":10,"redacted":true}`) || called != 1 {
		t.Fatalf("pull response: %d %s called=%d", res.Code, res.Body.String(), called)
	}
	for _, query := range []string{"", "?after_seq=-1", "?after_seq=1&limit=0", "?after_seq=1&limit=501",
		"?after_seq=1&limit=1&limit=2", "?after_seq=1&tenant_id=" + tenantID,
		"?after_seq=9223372036854775808", "?after_seq=1%3Blimit%3D2"} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(http.MethodGet, path+query))
		if res.Code != 400 || called != 1 {
			t.Fatalf("invalid query %q: %d %s", query, res.Code, res.Body.String())
		}
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path+"?after_seq=8", nil))
	if res.Code != 401 || called != 1 {
		t.Fatalf("anonymous pull: %d %s", res.Code, res.Body.String())
	}
}

func TestMessagePullRouteDoesNotExposeStoreErrors(t *testing.T) {
	path := "/api/v1/conversations/00000000-0000-4000-8000-000000000471/messages?after_seq=0"
	for _, tc := range []struct {
		err  error
		code int
	}{
		{policystore.ErrForbidden, 403},
		{policystore.ErrMessageNotAvailable, 404},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private SQL detail")), 503},
	} {
		service := pullStub{pull: func(context.Context, access.TrustedIdentity, string, int64, int) (policystore.MessagePage, error) {
			return policystore.MessagePage{}, tc.err
		}}
		handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
		if err != nil {
			t.Fatal(err)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(http.MethodGet, path))
		if res.Code != tc.code || strings.Contains(res.Body.String(), "private SQL detail") || strings.Contains(res.Body.String(), "text") {
			t.Fatalf("pull error: %d %s", res.Code, res.Body.String())
		}
	}
}
