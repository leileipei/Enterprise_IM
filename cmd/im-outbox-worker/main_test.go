package main

import "testing"

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
