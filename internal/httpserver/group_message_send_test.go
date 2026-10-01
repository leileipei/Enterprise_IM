package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type groupSendStub struct {
	conversationStub
	send func(context.Context, access.TrustedIdentity, string, string, string) (policystore.MessageACK, error)
}

func (s groupSendStub) SendGroupTextMessage(ctx context.Context, id access.TrustedIdentity,
	group, clientID, body string) (policystore.MessageACK, error) {
	return s.send(ctx, id, group, clientID, body)
}

func TestGroupMessageRouteUsesTrustedIdentityAndReturnsDuplicateACK(t *testing.T) {
	groupID := targetUserID
	clientID := "0199f04a-0000-7000-8000-000000000471"
	service := groupSendStub{send: func(_ context.Context, id access.TrustedIdentity,
		group, client, body string) (policystore.MessageACK, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) ||
			group != groupID || client != clientID || body != "群内你好" {
			t.Fatalf("group send input: %+v %s %s %s", id, group, client, body)
		}
		return policystore.MessageACK{MessageID: groupIntervalID, ConversationID: group,
			Seq: 5, ServerTime: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC), Duplicate: true}, nil
	}}
	h, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := messageRequest("/api/v1/groups/"+groupID+"/messages",
		`{"client_msg_id":"`+clientID+`","text":"群内你好"}`)
	req.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"message_id":"`+groupIntervalID+`"`) ||
		!strings.Contains(res.Body.String(), `"seq":5`) || !strings.Contains(res.Body.String(), `"duplicate":true`) {
		t.Fatalf("group send ACK: %d %s", res.Code, res.Body.String())
	}
}

func TestGroupMessageRouteRejectsMalformedAndMapsBlocked(t *testing.T) {
	notCalled := groupSendStub{send: func(context.Context, access.TrustedIdentity, string, string, string) (policystore.MessageACK, error) {
		t.Fatal("group send called for malformed request")
		return policystore.MessageACK{}, nil
	}}
	h, err := HandlerWithConversations(Handler(nil), authFunc(verified), notCalled)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/groups/" + targetUserID + "/messages"
	clientID := "0199f04a-0000-7000-8000-000000000471"
	for _, body := range []string{
		``, `{}`, `null`, `[]`,
		`{"client_msg_id":"bad","text":"ok"}`,
		`{"client_msg_id":"` + clientID + `","text":" "}`,
		`{"client_msg_id":"` + clientID + `","text":"ok","tenant_id":"` + tenantID + `"}`,
		`{"client_msg_id":"` + clientID + `","text":"ok"} true`,
	} {
		res := httptest.NewRecorder()
		h.ServeHTTP(res, messageRequest(path, body))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("malformed group message %q: %d %s", body, res.Code, res.Body.String())
		}
	}
	res := httptest.NewRecorder()
	req := messageRequest(path+"?tenant_id="+tenantID,
		`{"client_msg_id":"`+clientID+`","text":"ok"}`)
	h.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("query accepted: %d %s", res.Code, res.Body.String())
	}
	for _, failure := range []struct {
		err    error
		status int
		code   string
	}{
		{policystore.ErrConversationContextChanged, 409, "conversation_context_changed"},
		{policystore.ErrGroupPolicyBlocked, 409, "group_policy_blocked"},
		{policystore.ErrMessageNotAvailable, 404, "not_found"},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private SQL detail")), 503, "unavailable"},
	} {
		failed := groupSendStub{send: func(context.Context, access.TrustedIdentity, string, string, string) (policystore.MessageACK, error) {
			return policystore.MessageACK{}, failure.err
		}}
		f, _ := HandlerWithConversations(Handler(nil), authFunc(verified), failed)
		res := httptest.NewRecorder()
		f.ServeHTTP(res, messageRequest(path, `{"client_msg_id":"`+clientID+`","text":"ok"}`))
		if res.Code != failure.status || !strings.Contains(res.Body.String(), failure.code) ||
			strings.Contains(res.Body.String(), "private SQL detail") {
			t.Fatalf("group send error: %d %s", res.Code, res.Body.String())
		}
	}
}
