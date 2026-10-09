package receiptoutbox

import (
	"context"
	"fmt"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/redis/go-redis/v9"
	"os"
	"testing"
	"time"
)

func TestReceiptPublisherPresenceIndependent(t *testing.T) {
	raw := os.Getenv("IM_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("real Redis receipt publisher fixture required")
	}
	options, e := redis.ParseURL(raw)
	if e != nil {
		t.Fatal("invalid Redis fixture")
	}
	client := redis.NewClient(options)
	defer client.Close()
	ctx := context.Background()
	stream := fmt.Sprintf("im:test:receipt-presence:%d", time.Now().UnixNano())
	defer client.Del(ctx, PublisherPresenceKey(stream), outbox.PublisherPresenceKey(stream))
	if e = outbox.RefreshPublisherPresence(ctx, client, stream); e != nil {
		t.Fatal(e)
	}
	if present, e := PublisherPresent(ctx, client, stream); e != nil || present {
		t.Fatal("legacy publisher forged receipt presence", present, e)
	}
	if e = RefreshPublisherPresence(ctx, client, stream); e != nil {
		t.Fatal(e)
	}
	ttl, e := client.TTL(ctx, PublisherPresenceKey(stream)).Result()
	if e != nil || ttl <= 0 || ttl > PublisherPresenceTTL {
		t.Fatal("unbounded receipt presence", ttl, e)
	}
	client.Del(ctx, PublisherPresenceKey(stream))
	if present, e := outbox.PublisherPresent(ctx, client, stream); e != nil || !present {
		t.Fatal("receipt marker changed legacy presence", present, e)
	}
}
