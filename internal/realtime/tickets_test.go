package realtime_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/realtime"
	"github.com/redis/go-redis/v9"
)

var identity = access.TrustedIdentity{
	TenantID:           "00000000-0000-4000-8000-000000000201",
	UserID:             "00000000-0000-4000-8000-000000000231",
	ActingMembershipID: "00000000-0000-4000-8000-000000000241",
}

func redisDB(t *testing.T) *redis.Client {
	t.Helper()
	raw := os.Getenv("IM_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("set IM_TEST_REDIS_URL to run Redis integration tests")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(options)
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestRedisTicketConsumedOnceAcrossConcurrentRequests(t *testing.T) {
	client := redisDB(t)
	store := realtime.RedisTickets{Client: client}
	ticket, err := store.Issue(context.Background(), identity)
	if err != nil || len(ticket) != 43 {
		t.Fatalf("issued ticket: len=%d err=%v", len(ticket), err)
	}
	results := make([]access.TrustedIdentity, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index], errs[index] = store.Consume(context.Background(), ticket)
		}(i)
	}
	wg.Wait()
	success, invalid := 0, 0
	for i := range results {
		if errs[i] == nil && results[i] == identity {
			success++
		} else if errors.Is(errs[i], realtime.ErrInvalidTicket) {
			invalid++
		} else {
			t.Fatalf("unexpected consume: %+v %v", results[i], errs[i])
		}
	}
	if success != 1 || invalid != 1 {
		t.Fatalf("ticket replay accepted: success=%d invalid=%d", success, invalid)
	}
}

func TestRedisTicketExpiryAndMalformedInput(t *testing.T) {
	client := redisDB(t)
	store := realtime.RedisTickets{Client: client}
	ticket, err := store.Issue(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	key, err := realtime.TicketKey(ticket)
	if err != nil {
		t.Fatal(err)
	}
	if key == ticket {
		t.Fatal("raw ticket used as Redis key")
	}
	ttl, err := client.TTL(context.Background(), key).Result()
	if err != nil || ttl <= 0 || ttl > 30*time.Second {
		t.Fatalf("ticket TTL: %v %v", ttl, err)
	}
	if err := client.PExpire(context.Background(), key, time.Millisecond).Err(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := store.Consume(context.Background(), ticket); !errors.Is(err, realtime.ErrInvalidTicket) {
		t.Fatalf("expired ticket accepted: %v", err)
	}
	for _, candidate := range []string{"", "short", ticket + "x", ticket[:42] + "+"} {
		if _, err := store.Consume(context.Background(), candidate); !errors.Is(err, realtime.ErrInvalidTicket) {
			t.Fatalf("malformed ticket %q: %v", candidate, err)
		}
	}
}

func TestRedisTicketFailsClosedWhenStoreUnavailable(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond,
		ReadTimeout: 50 * time.Millisecond, WriteTimeout: 50 * time.Millisecond, MaxRetries: 0})
	defer client.Close()
	store := realtime.RedisTickets{Client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := store.Issue(ctx, identity); !errors.Is(err, realtime.ErrTicketUnavailable) {
		t.Fatalf("issue during outage: %v", err)
	}
}
