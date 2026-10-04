package httpserver

import (
	"context"
	"encoding/json"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"net/http/httptest"
	"testing"
	"time"
)

type typedPullStub struct {
	conversationStub
	calls int
	page  policystore.MessagePage
}

func (s *typedPullStub) PullTextMessages(context.Context, access.TrustedIdentity, string, int64, int) (policystore.MessagePage, error) {
	s.calls++
	return s.page, nil
}
func (s *typedPullStub) PullGroupTextMessages(context.Context, access.TrustedIdentity, string, int64, int) (policystore.MessagePage, error) {
	s.calls++
	return s.page, nil
}
func TestFileMessagePullFormat(t *testing.T) {
	size := int64(9007199254740993)
	for _, route := range []string{"conversations", "groups"} {
		for _, typed := range []bool{false, true} {
			t.Run(route+map[bool]string{true: "/typed", false: "/legacy"}[typed], func(t *testing.T) {
				svc := &typedPullStub{page: policystore.MessagePage{ConversationID: targetUserID, NextAfterSeq: 7, Messages: []policystore.PulledMessage{
					{Seq: 4, MessageID: targetMemID, SenderUserID: actorID, MessageType: "file", Text: "", ServerTime: time.Now(), Attachment: &policystore.MessageAttachment{FileID: actingID, Available: true, OriginalFilename: "report.pdf", ActualSizeBytes: &size, DetectedMediaType: "application/pdf"}},
					{Seq: 5, MessageID: targetMemID, SenderUserID: actorID, MessageType: "file", Text: "caption", ServerTime: time.Now(), Attachment: &policystore.MessageAttachment{FileID: actingID, Available: false, OriginalFilename: "must hide", ActualSizeBytes: &size, DetectedMediaType: "must hide"}},
					{Seq: 6, MessageID: targetMemID, SenderUserID: actorID, MessageType: "text", Text: "text", ServerTime: time.Now()},
					{Seq: 7, Redacted: true, MessageID: "hidden", SenderUserID: "hidden", MessageType: "file", Text: "hidden", Attachment: &policystore.MessageAttachment{FileID: "hidden", Available: true, OriginalFilename: "hidden"}},
				}}}
				h, e := HandlerWithConversations(Handler(nil), authFunc(verified), svc)
				if e != nil {
					t.Fatal(e)
				}
				path := "/api/v1/" + route + "/" + targetUserID + "/messages?after_seq=0"
				if typed {
					path += "&message_format=typed_v1"
				}
				res := httptest.NewRecorder()
				h.ServeHTTP(res, adminRequest("GET", path))
				if res.Code != 200 {
					t.Fatal(res.Code, res.Body.String())
				}
				var body struct {
					Messages []map[string]any `json:"messages"`
				}
				if e = json.Unmarshal(res.Body.Bytes(), &body); e != nil || len(body.Messages) != 4 {
					t.Fatal(body, e)
				}
				items := body.Messages
				if len(items[3]) != 2 || items[3]["seq"] != float64(7) || items[3]["redacted"] != true {
					t.Fatal("redaction leaked", items[3])
				}
				if typed {
					if items[0]["message_type"] != "file" || items[0]["caption"] != "" || items[1]["caption"] != "caption" || items[2]["message_type"] != "text" || items[2]["text"] != "text" {
						t.Fatal(items)
					}
					a := items[0]["attachment"].(map[string]any)
					if len(a) != 6 || a["file_id"] != actingID || a["available"] != true || a["download_available"] != false || a["actual_size_bytes"] != "9007199254740993" {
						t.Fatal(a)
					}
					a = items[1]["attachment"].(map[string]any)
					if len(a) != 3 || a["available"] != false || a["download_available"] != false {
						t.Fatal("unavailable leaked metadata", a)
					}
					if _, ok := items[0]["text"]; ok {
						t.Fatal("caption exposed as text")
					}
				} else {
					for _, i := range []int{0, 1} {
						if items[i]["text"] != "附件消息（当前客户端不支持查看）" || len(items[i]) != 5 {
							t.Fatal("legacy file leaked", items[i])
						}
					}
				}
			})
		}
	}
	svc := &typedPullStub{}
	h, _ := HandlerWithConversations(Handler(nil), authFunc(verified), svc)
	for _, suffix := range []string{"message_format=", "message_format=other", "message_format=typed_v1&message_format=typed_v1", "message_format=typed_v1&unknown=x"} {
		res := httptest.NewRecorder()
		h.ServeHTTP(res, adminRequest("GET", "/api/v1/conversations/"+targetUserID+"/messages?after_seq=0&"+suffix))
		if res.Code != 400 {
			t.Fatal(suffix, res.Code)
		}
	}
	if svc.calls != 0 {
		t.Fatal("invalid format reached service")
	}
}
