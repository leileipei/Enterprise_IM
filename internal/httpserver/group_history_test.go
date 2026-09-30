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

type groupHistoryStub struct {
	conversationStub
	pull func(context.Context, access.TrustedIdentity, string, int64, int) (policystore.MessagePage, error)
}

func (s groupHistoryStub) PullGroupTextMessages(ctx context.Context, id access.TrustedIdentity, group string, after int64, limit int) (policystore.MessagePage, error) {
	return s.pull(ctx, id, group, after, limit)
}

func TestGroupHistoryRouteUsesTrustedIdentityAndRedactsGap(t *testing.T) {
	service := groupHistoryStub{pull: func(_ context.Context, id access.TrustedIdentity, group string, after int64, limit int) (policystore.MessagePage, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) ||
			group != targetUserID || after != 2 || limit != 2 {
			t.Fatalf("group history input: %+v %s %d %d", id, group, after, limit)
		}
		return policystore.MessagePage{ConversationID: group, Messages: []policystore.PulledMessage{
			{Seq: 3, Redacted: true},
			{MessageID: groupIntervalID, Seq: 4, SenderUserID: targetMemID, Text: "可见正文",
				ServerTime: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)},
		}, NextAfterSeq: 4, HasMore: true}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := adminRequest(http.MethodGet, "/api/v1/groups/"+targetUserID+"/messages?after_seq=2&limit=2")
	req.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	body := res.Body.String()
	if res.Code != 200 || !strings.Contains(body, `"conversation_id":"`+targetUserID+`"`) ||
		!strings.Contains(body, `"next_after_seq":4`) || !strings.Contains(body, `"has_more":true`) ||
		!strings.Contains(body, `"seq":3,"redacted":true`) || !strings.Contains(body, `"text":"可见正文"`) ||
		strings.Contains(body, `"seq":3,"sender_user_id"`) {
		t.Fatalf("group history response: %d %s", res.Code, body)
	}
}

func TestGroupHistoryRouteRejectsInvalidQueryAndMapsFailures(t *testing.T) {
	notCalled := groupHistoryStub{pull: func(context.Context, access.TrustedIdentity, string, int64, int) (policystore.MessagePage, error) {
		t.Fatal("group pull called for invalid request")
		return policystore.MessagePage{}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), notCalled)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"?", "?after_seq=", "?after_seq=-1", "?after_seq=0&after_seq=1",
		"?after_seq=0&limit=0", "?after_seq=0&limit=501", "?after_seq=0&tenant_id=" + tenantID,
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/groups/"+targetUserID+"/messages"+path))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("bad group history query %q: %d %s", path, res.Code, res.Body.String())
		}
	}
	wrongMethod := adminRequest(http.MethodPost, "/api/v1/groups/"+targetUserID+"/messages")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, wrongMethod)
	if res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != "GET" {
		t.Fatalf("wrong method: %d %s", res.Code, res.Body.String())
	}
	for _, failure := range []struct {
		err    error
		status int
		code   string
	}{
		{policystore.ErrInvalidMessageRequest, 400, "invalid_request"},
		{policystore.ErrForbidden, 403, "invalid_identity"},
		{policystore.ErrMessageNotAvailable, 404, "not_found"},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private SQL detail")), 503, "unavailable"},
	} {
		failed := groupHistoryStub{pull: func(context.Context, access.TrustedIdentity, string, int64, int) (policystore.MessagePage, error) {
			return policystore.MessagePage{}, failure.err
		}}
		h, _ := HandlerWithConversations(Handler(nil), authFunc(verified), failed)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/groups/"+targetUserID+"/messages?after_seq=0"))
		if res.Code != failure.status || !strings.Contains(res.Body.String(), failure.code) ||
			strings.Contains(res.Body.String(), "private SQL detail") {
			t.Fatalf("group history error: %d %s", res.Code, res.Body.String())
		}
	}
}
