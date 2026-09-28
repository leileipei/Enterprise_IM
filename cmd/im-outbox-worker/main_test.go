package main

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testEnv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestConfigFromEnvRequiresDatabaseAndRedis(t *testing.T) {
	for _, values := range []map[string]string{
		nil,
		{"IM_DATABASE_URL": "postgres://localhost/db"},
		{"IM_OUTBOX_REDIS_URL": "redis://localhost:6379/0"},
		{"IM_DATABASE_URL": "postgres://localhost/db", "IM_OUTBOX_REDIS_URL": "http://localhost:6379"},
		{"IM_DATABASE_URL": "postgres://localhost/db", "IM_OUTBOX_REDIS_URL": "redis://"},
		{"IM_DATABASE_URL": "postgres://localhost/db", "IM_OUTBOX_REDIS_URL": "redis://localhost:6379/0", "IM_OUTBOX_STREAM": " bad "},
	} {
		if _, err := configFromEnv(testEnv(values)); err == nil {
			t.Fatalf("unsafe worker config accepted: %+v", values)
		}
	}
}

func TestConfigFromEnvDefaultsAndCustomStream(t *testing.T) {
	base := map[string]string{"IM_DATABASE_URL": "postgres://localhost/db", "IM_OUTBOX_REDIS_URL": "redis://localhost:6379/0"}
	cfg, err := configFromEnv(testEnv(base))
	if err != nil || cfg.DatabaseURL != base["IM_DATABASE_URL"] || cfg.RedisURL != base["IM_OUTBOX_REDIS_URL"] || cfg.Stream != "enterprise-im:message-created:v1" {
		t.Fatalf("default worker config: %+v %v", cfg, err)
	}
	base["IM_OUTBOX_STREAM"] = "enterprise-im:staging:message-created:v1"
	cfg, err = configFromEnv(testEnv(base))
	if err != nil || cfg.Stream != base["IM_OUTBOX_STREAM"] {
		t.Fatalf("custom stream: %+v %v", cfg, err)
	}
}

func TestRedisOptionsHonorPublishContextDuringSilentServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); io.Copy(io.Discard, conn) }()
		}
	}()
	options, err := redisOptionsFromURL("redis://" + listener.Addr().String() + "/0?read_timeout=2s&max_retries=0")
	if err != nil {
		t.Fatal(err)
	}
	if !options.ContextTimeoutEnabled {
		t.Fatal("Redis client ignores publish context deadline")
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = client.XAdd(ctx, &redis.XAddArgs{Stream: "test", Values: map[string]any{"event_id": "one"}}).Err()
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("silent Redis exceeded context timeout: %v after %v", err, time.Since(started))
	}
}
