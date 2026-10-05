package policystore_test

import (
	"context"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"testing"
)

func TestFileMessageSearchExcludesAttachments(t *testing.T) {
	for _, kind := range []string{"direct", "group"} {
		t.Run(kind, func(t *testing.T) {
			_, s, cid, _ := fileHistoryFixture(t, kind)
			search := s.SearchTextMessages
			if kind == "group" {
				search = s.SearchGroupTextMessages
			}
			for _, query := range []string{"caption", "报告"} {
				page, e := search(context.Background(), publisher(), cid, query, "", 5)
				if e != nil || len(page.Messages) != 0 {
					t.Fatal("attachment matched", query, page, e)
				}
			}
			cursor := ""
			for i, want := range []int64{1, 3} {
				page, e := search(context.Background(), publisher(), cid, "match", cursor, 1)
				if e != nil || len(page.Messages) != 1 || page.Messages[0].Seq != want || page.Messages[0].MessageType != policystore.MessageTypeText || page.Messages[0].Attachment != nil || page.HasMore != (i == 0) {
					t.Fatal(page, e)
				}
				cursor = page.NextCursor
			}
			matches := crossCollect(t, s, publisher(), "match", kind, 1)
			if len(matches) != 2 || matches[0].Message.Seq != 1 || matches[1].Message.Seq != 3 {
				t.Fatal("cross search lost text timeline", matches)
			}
			for _, m := range matches {
				if m.Message.MessageType != "text" || m.Message.Attachment != nil {
					t.Fatal(m)
				}
			}
			if matches = crossCollect(t, s, publisher(), "caption", kind, 1); len(matches) != 0 {
				t.Fatal("cross search matched caption", matches)
			}
		})
	}
}
