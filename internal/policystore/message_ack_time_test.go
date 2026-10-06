package policystore_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

// The clock deliberately has sub-microsecond precision. All timestamps are
// compared with the real persisted row, rather than assuming a rounding rule.
func TestMessageACKUsesPersistedTime(t *testing.T) {
	for _, group := range []bool{false, true} {
		for _, file := range []bool{false, true} {
			for _, nanos := range []int{123456789, 999999999} {
				name := fmt.Sprintf("group=%t/file=%t/nanos=%d", group, file, nanos)
				t.Run(name, func(t *testing.T) {
					var c *pgx.Conn
					var s policystore.Service
					var cid string
					req := policystore.MessageSendRequest{MessageType: "text", Text: "persisted ACK time"}
					if file {
						if group {
							c, s, cid, _, req = groupFileFixture(t)
						} else {
							c, s, _, req = directFileFixture(t)
							cid = directA
						}
					} else {
						c, s, cid = textCommitFixture(t, group)
					}
					now := at.Add(time.Duration(nanos))
					s.Now = func() time.Time { return now }
					req.ClientMessageID = clientUUIDv7(now, 9101)
					send, pull := s.SendMessage, s.PullTextMessages
					if group {
						send, pull = s.SendGroupMessage, s.PullGroupTextMessages
					}
					ctx := context.Background()
					first, err := send(ctx, publisher(), cid, req)
					if err != nil {
						t.Fatal(err)
					}
					var stored, keyTime, outboxTime time.Time
					err = c.QueryRow(ctx, `SELECT m.accepted_at,i.accepted_at,o.created_at
FROM messages m JOIN message_idempotency i ON i.message_id=m.id
JOIN outbox_events o ON o.message_id=m.id WHERE m.id=$1`, first.MessageID).Scan(&stored, &keyTime, &outboxTime)
					if err != nil {
						t.Fatal(err)
					}
					// Advance the clock so retry cannot accidentally reuse "now".
					now = now.Add(time.Second)
					replay, err := send(ctx, publisher(), cid, req)
					if err != nil {
						t.Fatal(err)
					}
					page, err := pull(ctx, publisher(), cid, 0, 10)
					if err != nil || len(page.Messages) != 1 {
						t.Fatalf("pull: %+v %v", page, err)
					}
					want := stored.UTC().Format(time.RFC3339Nano)
					for label, got := range map[string]time.Time{
						"first ACK": first.ServerTime, "replay ACK": replay.ServerTime,
						"history": page.Messages[0].ServerTime, "idempotency": keyTime, "outbox": outboxTime,
					} {
						if got.Format(time.RFC3339Nano) != want {
							t.Errorf("%s time=%s, persisted time=%s", label, got.Format(time.RFC3339Nano), want)
						}
					}
					if first.Duplicate || !replay.Duplicate || first.MessageID != replay.MessageID ||
						first.ConversationID != replay.ConversationID || first.Seq != replay.Seq ||
						page.Messages[0].MessageID != first.MessageID {
						t.Fatalf("ACK identity changed: first=%+v replay=%+v history=%+v", first, replay, page)
					}
					for _, table := range []string{"messages", "message_idempotency", "outbox_events"} {
						var n int
						if err := c.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE conversation_id=$1", cid).Scan(&n); err != nil || n != 1 {
							t.Fatalf("%s count=%d err=%v", table, n, err)
						}
					}
					if file {
						assertFileMessageWrites(t, c, cid, 1)
					}
				})
			}
		}
	}
}
