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

func TestWorkerPublishesCommittedOutboxToRedis(t *testing.T) {
	rawURL := os.Getenv("IM_TEST_REDIS_URL")
	if rawURL == "" {
		t.Skip("set IM_TEST_REDIS_URL for Redis tests")
	}
	db := database(t)
	seedEvent(t, db)
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { client.Close() })
	stream := fmt.Sprintf("enterprise-im-worker-test-%d", time.Now().UnixNano())
	t.Cleanup(func() { client.Del(context.Background(), stream) })
	worker := outbox.Worker{DB: db, Now: func() time.Time { return fixedNow },
		Publisher: outbox.RedisPublisher{Client: client, Stream: stream}}
	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("real stream publish: %t %v", processed, err)
	}
	entries, err := client.XRange(context.Background(), stream, "-", "+").Result()
	if err != nil || len(entries) != 1 || entries[0].Values["event_id"] != eventID {
		t.Fatalf("published stream event: %+v %v", entries, err)
	}
	var status string
	if err := db.QueryRow(context.Background(), "SELECT status FROM outbox_events WHERE tenant_id=$1 AND id=$2", tenantID, eventID).Scan(&status); err != nil || status != "published" {
		t.Fatalf("database event status: %s %v", status, err)
	}
}

func TestWorkerRetryAfterDatabaseFailureKeepsStableEventID(t *testing.T) {
	rawURL := os.Getenv("IM_TEST_REDIS_URL")
	if rawURL == "" {
		t.Skip("set IM_TEST_REDIS_URL for Redis tests")
	}
	db := database(t)
	seedEvent(t, db)
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { client.Close() })
	stream := fmt.Sprintf("enterprise-im-retry-test-%d", time.Now().UnixNano())
	t.Cleanup(func() { client.Del(context.Background(), stream) })
	if _, err := db.Exec(context.Background(), `CREATE FUNCTION fail_mark_published() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'status unavailable'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(context.Background(), "CREATE TRIGGER fail_mark_published BEFORE UPDATE ON outbox_events FOR EACH ROW WHEN (NEW.status = 'published') EXECUTE FUNCTION fail_mark_published()"); err != nil {
		t.Fatal(err)
	}
	worker := outbox.Worker{DB: db, Now: func() time.Time { return fixedNow },
		Publisher: outbox.RedisPublisher{Client: client, Stream: stream}}
	if processed, err := worker.ProcessOne(context.Background()); !processed || err == nil {
		t.Fatalf("database failure after Redis publish: %t %v", processed, err)
	}
	if _, err := db.Exec(context.Background(), "DROP TRIGGER fail_mark_published ON outbox_events"); err != nil {
		t.Fatal(err)
	}
	if processed, err := worker.ProcessOne(context.Background()); !processed || err != nil {
		t.Fatalf("retry after database recovery: %t %v", processed, err)
	}
	entries, err := client.XRange(context.Background(), stream, "-", "+").Result()
	if err != nil || len(entries) != 2 || entries[0].Values["event_id"] != eventID || entries[1].Values["event_id"] != eventID {
		t.Fatalf("duplicate notification must retain event ID: %+v %v", entries, err)
	}
}
