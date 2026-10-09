package main

import (
	"context"
	"github.com/redis/go-redis/v9"
	"io"
	"net"
	"testing"
	"time"
)

func testReceiptEnv(v map[string]string) func(string) string {
	return func(k string) string { return v[k] }
}
func TestReceiptWorkerConfigAndPublishBudget(t *testing.T) {
	base := map[string]string{"IM_DATABASE_URL": "postgres://localhost/test", "IM_RECEIPT_REDIS_URL": "redis://localhost:6379/0"}
	cfg, e := configFromEnv(testReceiptEnv(base))
	if e != nil || cfg.Stream != "enterprise-im:receipt-changed:v1" {
		t.Fatal(cfg, e)
	}
	for _, stream := range []string{" bad ", "enterprise-im:message-created:v1"} {
		base["IM_RECEIPT_STREAM"] = stream
		if _, e = configFromEnv(testReceiptEnv(base)); e == nil {
			t.Fatal("unsafe stream accepted")
		}
	}
	for _, env := range []map[string]string{nil, {"IM_DATABASE_URL": "postgres://localhost/test"}, {"IM_DATABASE_URL": "postgres://localhost/test", "IM_RECEIPT_REDIS_URL": "http://localhost"}} {
		if _, e = configFromEnv(testReceiptEnv(env)); e == nil {
			t.Fatal("unsafe config accepted")
		}
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	go func() {
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(io.Discard, c) }()
		}
	}()
	options, e := redisOptionsFromURL("redis://" + listener.Addr().String() + "/0?read_timeout=2s&max_retries=0")
	if e != nil || !options.ContextTimeoutEnabled {
		t.Fatal(options, e)
	}
	client := redis.NewClient(options)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if e = client.XAdd(ctx, &redis.XAddArgs{Stream: "budget", Values: map[string]any{"event_id": "one"}}).Err(); e == nil || time.Since(start) > time.Second {
		t.Fatal("publish context ignored", e, time.Since(start))
	}
}
