package httpserver

import (
	"context"
	"encoding/json"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fileMessageStub struct {
	conversationStub
	direct, group, text, groupText int
	last                           policystore.MessageSendRequest
	lastIdentity                   access.TrustedIdentity
	lastConversation               string
	err                            error
}

func (s *fileMessageStub) ack(cid string) (policystore.MessageACK, error) {
	return policystore.MessageACK{MessageID: targetUserID, ConversationID: cid, Seq: 9, ServerTime: time.Date(2026, 10, 4, 1, 2, 3, 0, time.UTC), Duplicate: true}, s.err
}
func (s *fileMessageStub) SendMessage(_ context.Context, id access.TrustedIdentity, cid string, req policystore.MessageSendRequest) (policystore.MessageACK, error) {
	s.direct++
	s.last = req
	s.lastIdentity = id
	s.lastConversation = cid
	return s.ack(cid)
}
func (s *fileMessageStub) SendGroupMessage(_ context.Context, id access.TrustedIdentity, cid string, req policystore.MessageSendRequest) (policystore.MessageACK, error) {
	s.group++
	s.last = req
	s.lastIdentity = id
	s.lastConversation = cid
	return s.ack(cid)
}
func (s *fileMessageStub) SendTextMessage(_ context.Context, id access.TrustedIdentity, cid, client, text string) (policystore.MessageACK, error) {
	s.text++
	s.last = policystore.MessageSendRequest{ClientMessageID: client, MessageType: "text", Text: text}
	return s.ack(cid)
}
func (s *fileMessageStub) SendGroupTextMessage(_ context.Context, id access.TrustedIdentity, cid, client, text string) (policystore.MessageACK, error) {
	s.groupText++
	s.last = policystore.MessageSendRequest{ClientMessageID: client, MessageType: "text", Text: text}
	return s.ack(cid)
}
func typedHTTPBody(kind string) string {
	if kind == "file" {
		return `{"client_msg_id":"` + typedClient + `","message_type":"file","file_id":"` + typedFile + `"}`
	}
	return `{"client_msg_id":"` + typedClient + `","message_type":"text","text":"text"}`
}
func TestFileMessageHTTPClosed(t *testing.T) {
	svc := &fileMessageStub{}
	h, e := HandlerWithConversations(Handler(nil), authFunc(verified), svc)
	if e != nil {
		t.Fatal(e)
	}
	for _, route := range []string{"conversations", "groups"} {
		for _, fid := range []string{typedFile, targetMemID} {
			body := strings.ReplaceAll(typedHTTPBody("file"), typedFile, fid)
			res := httptest.NewRecorder()
			h.ServeHTTP(res, messageRequest("/api/v1/"+route+"/"+targetUserID+"/messages", body))
			if res.Code != 503 || !strings.Contains(res.Body.String(), `"file_message_unavailable"`) {
				t.Fatal(res.Code, res.Body.String())
			}
		}
	}
	if svc.direct+svc.group+svc.text+svc.groupText != 0 {
		t.Fatal("closed route inspected a file", svc)
	}
	req := messageRequest("/api/v1/conversations/"+targetUserID+"/messages", typedHTTPBody("file"))
	req.Header.Set("Authorization", "Bearer invalid")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != 401 {
		t.Fatal(res.Code)
	}
	for _, extra := range []int{0, 1} {
		body := typedHTTPBody("file")
		body += strings.Repeat(" ", 128*1024-len(body)+extra)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, messageRequest("/api/v1/conversations/"+targetUserID+"/messages", body))
		want := 503
		if extra == 1 {
			want = 400
		}
		if res.Code != want {
			t.Fatal("request limit", extra, res.Code, res.Body.String())
		}
	}
}
func TestFileMessageHTTPRouting(t *testing.T) {
	svc := &fileMessageStub{}
	h, e := HandlerWithFileMessages(Handler(nil), authFunc(verified), svc, svc)
	if e != nil {
		t.Fatal(e)
	}
	for _, route := range []string{"conversations", "groups"} {
		for _, kind := range []string{"text", "file"} {
			res := httptest.NewRecorder()
			h.ServeHTTP(res, messageRequest("/api/v1/"+route+"/"+targetUserID+"/messages", typedHTTPBody(kind)))
			if res.Code != 200 {
				t.Fatal(route, kind, res.Code, res.Body.String())
			}
		}
	}
	if svc.direct != 1 || svc.group != 1 || svc.text != 1 || svc.groupText != 1 {
		t.Fatal("wrong service routing", svc)
	}
	if svc.lastIdentity != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || svc.lastConversation != targetUserID || svc.last.FileID != strings.ToLower(typedFile) {
		t.Fatal("untrusted identity or file request", svc)
	}
	if _, e = HandlerWithFileMessages(Handler(nil), authFunc(verified), svc, nil); e == nil {
		t.Fatal("nil file dependency accepted")
	}
}
func TestFileMessageHTTPACK(t *testing.T) {
	svc := &fileMessageStub{}
	h, e := HandlerWithFileMessages(Handler(nil), authFunc(verified), svc, svc)
	if e != nil {
		t.Fatal(e)
	}
	path := "/api/v1/conversations/" + targetUserID + "/messages"
	res := httptest.NewRecorder()
	h.ServeHTTP(res, messageRequest(path, typedHTTPBody("file")))
	var ack map[string]any
	if e = json.Unmarshal(res.Body.Bytes(), &ack); e != nil || res.Code != 200 || len(ack) != 5 || ack["message_id"] != targetUserID || ack["conversation_id"] != targetUserID || ack["seq"] != float64(9) || ack["server_time"] != "2026-10-04T01:02:03Z" || ack["duplicate"] != true {
		t.Fatal(ack, e)
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{policystore.ErrFileNotBindable, 409, "file_not_bindable"}, {policystore.ErrFileMessageUnavailable, 503, "file_message_unavailable"},
		{policystore.ErrRetryExpired, 410, "retry_window_expired"}, {policystore.ErrMessageNotAvailable, 404, "not_found"},
		{policystore.ErrIdempotencyConflict, 409, "idempotency_conflict"}, {policystore.ErrForbidden, 403, "invalid_identity"},
	} {
		svc.err = tc.err
		res := httptest.NewRecorder()
		h.ServeHTTP(res, messageRequest(path, typedHTTPBody("file")))
		if res.Code != tc.status || !strings.Contains(res.Body.String(), `"`+tc.code+`"`) {
			t.Fatal(tc, res.Code, res.Body.String())
		}
	}
}
