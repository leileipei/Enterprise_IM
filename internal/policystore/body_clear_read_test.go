package policystore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestClearedBodiesPullAndReplay(t *testing.T) {
	for _, kind := range []string{"direct", "group"} {
		t.Run(kind, func(t *testing.T) {
			conn := db(t)
			seedDirectConversation(t, conn)
			svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
			conversationID := directA
			send, pull := svc.SendTextMessage, svc.PullTextMessages
			if kind == "group" {
				group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
				if err != nil {
					t.Fatal(err)
				}
				conversationID = group.ID
				send, pull = svc.SendGroupTextMessage, svc.PullGroupTextMessages
			}
			ctx := context.Background()
			clientID := clientUUIDv7(at, 950)
			first, err := send(ctx, publisher(), conversationID, clientID, "kept for replay")
			if err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 2; i++ {
				if _, err := send(ctx, publisher(), conversationID, clientUUIDv7(at, 950+i), "visible middle"); err != nil {
					t.Fatal(err)
				}
			}
			run(t, conn, `UPDATE messages SET text_body=NULL,body_cleared_at=$2
 WHERE conversation_id=$1 AND seq IN (1,3)`, conversationID, at.Add(365*24*time.Hour))
			page, err := pull(ctx, publisher(), conversationID, 0, 2)
			if err != nil || len(page.Messages) != 2 || !page.HasMore || page.NextAfterSeq != 2 {
				t.Fatalf("first page: %+v %v", page, err)
			}
			assertBodyPlaceholder(t, page.Messages[0], 1)
			if page.Messages[1].Redacted || page.Messages[1].Text != "visible middle" || page.Messages[1].Seq != 2 {
				t.Fatalf("uncleared message changed: %+v", page.Messages[1])
			}
			page, err = pull(ctx, publisher(), conversationID, 2, 2)
			if err != nil || len(page.Messages) != 1 || page.HasMore || page.NextAfterSeq != 3 {
				t.Fatalf("last page: %+v %v", page, err)
			}
			assertBodyPlaceholder(t, page.Messages[0], 3)
			replay, err := send(ctx, publisher(), conversationID, clientID, "kept for replay")
			if err != nil || replay.MessageID != first.MessageID || replay.Seq != first.Seq || replay.ConversationID != first.ConversationID || !replay.ServerTime.Equal(first.ServerTime) || !replay.Duplicate {
				t.Fatalf("changed replay ACK: %+v %v", replay, err)
			}
			if _, err := send(ctx, publisher(), conversationID, clientID, "different"); !errors.Is(err, policystore.ErrIdempotencyConflict) {
				t.Fatalf("changed retry did not conflict: %v", err)
			}
			for _, table := range []string{"messages", "message_idempotency", "outbox_events"} {
				var count int
				if err := conn.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE conversation_id=$1", conversationID).Scan(&count); err != nil || count != 3 {
					t.Fatalf("%s changed on retry: %d %v", table, count, err)
				}
			}
			var cleared int
			if err := conn.QueryRow(ctx, "SELECT count(*) FROM messages WHERE conversation_id=$1 AND text_body IS NULL AND body_cleared_at IS NOT NULL", conversationID).Scan(&cleared); err != nil || cleared != 2 {
				t.Fatalf("retry restored cleared bodies: %d %v", cleared, err)
			}
		})
	}
}

func assertBodyPlaceholder(t *testing.T, item policystore.PulledMessage, seq int64) {
	t.Helper()
	if item.Seq != seq || !item.Redacted || item.MessageID != "" || item.SenderUserID != "" || item.Text != "" || !item.ServerTime.IsZero() {
		t.Fatalf("cleared message leaked fields: %+v", item)
	}
}
