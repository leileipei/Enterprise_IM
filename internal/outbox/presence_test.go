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

func TestPublisherPresenceIsScopedToStreamAndExpires(t *testing.T) {
	raw := os.Getenv("IM_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("set IM_TEST_REDIS_URL for Redis tests")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(options)
	defer client.Close()
	stream := fmt.Sprintf("enterprise-im:test:presence:%d", time.Now().UnixNano())
	ctx := context.Background()
	defer client.Del(ctx, outbox.PublisherPresenceKey(stream))
	if err := outbox.RefreshPublisherPresence(ctx, client, stream); err != nil {
		t.Fatal(err)
	}
	present, err := outbox.PublisherPresent(ctx, client, stream)
	if err != nil || !present {
		t.Fatalf("publisher marker absent: %v %v", present, err)
	}
	present, err = outbox.PublisherPresent(ctx, client, stream+":wrong")
	if err != nil || present {
		t.Fatalf("wrong stream appeared present: %v %v", present, err)
	}
	ttl, err := client.TTL(ctx, outbox.PublisherPresenceKey(stream)).Result()
	if err != nil || ttl <= 0 || ttl > outbox.PublisherPresenceTTL {
		t.Fatalf("marker TTL: %v %v", ttl, err)
	}
	if err := client.PExpire(ctx, outbox.PublisherPresenceKey(stream), time.Millisecond).Err(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	present, err = outbox.PublisherPresent(ctx, client, stream)
	if err != nil || present {
		t.Fatalf("expired publisher appeared present: %v %v", present, err)
	}
}
