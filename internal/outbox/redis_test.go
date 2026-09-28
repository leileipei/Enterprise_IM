package outbox_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/redis/go-redis/v9"
)

func TestRedisPublisherWritesIdentifierOnlyStreamEvent(t *testing.T) {
	rawURL := os.Getenv("IM_TEST_REDIS_URL")
	if rawURL == "" {
		t.Skip("set IM_TEST_REDIS_URL for Redis tests")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { client.Close() })
	stream := fmt.Sprintf("enterprise-im-test-%d", time.Now().UnixNano())
	t.Cleanup(func() { client.Del(context.Background(), stream) })
	publisher := outbox.RedisPublisher{Client: client, Stream: stream}
	event := outbox.Event{ID: eventID, TenantID: tenantID, ConversationID: conversationID,
		MessageID: messageID, Seq: 19, EventType: "message_created"}
	if err := publisher.Publish(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	entries, err := client.XRange(context.Background(), stream, "-", "+").Result()
	if err != nil || len(entries) != 1 {
		t.Fatalf("stream entries: %v %v", entries, err)
	}
	values := entries[0].Values
	want := map[string]string{
		"event_id": eventID, "tenant_id": tenantID, "conversation_id": conversationID,
		"message_id": messageID, "seq": "19", "event_type": "message_created",
	}
	if len(values) != len(want) {
		t.Fatalf("unexpected event fields: %+v", values)
	}
	for key, expected := range want {
		if values[key] != expected {
			t.Fatalf("field %s = %v, want %s", key, values[key], expected)
		}
	}
}

func TestRedisPublisherReportsOutage(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := (outbox.RedisPublisher{Client: client, Stream: "test"}).Publish(ctx, outbox.Event{ID: eventID}); err == nil {
		t.Fatal("unreachable Redis accepted event")
	}
}
