//go:build nativeclientintegration && (darwin || linux)

package policystore_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/leileipei/Enterprise_IM/internal/receiptoutbox"
	"github.com/redis/go-redis/v9"
)

// Explicit opt-in fixture lifetime. The TLS proxy only observes production
// responses; control routes query independent PostgreSQL facts, never emulate APIs.
func TestMobileBackendFixture(t *testing.T) {
	for _, key := range []string{"IM_TEST_DATABASE_URL", "IM_TEST_REDIS_URL", "IM_TEST_MOBILE_CONFIG", "IM_TEST_MOBILE_FACTS", "IM_TEST_MOBILE_STOP"} {
		if os.Getenv(key) == "" {
			t.Fatalf("explicit mobile backend gate requires %s", key)
		}
	}
	platform := os.Getenv("IM_TEST_MOBILE_PLATFORM")
	if platform != "android" && platform != "ios" {
		t.Fatal("explicit mobile platform required")
	}
	ctx := context.Background()
	conn := db(t)
	seedDirectConversation(t, conn)
	for _, n := range []string{"000004_external_identities", "000026_native_oauth_rate"} {
		b, e := os.ReadFile("../../db/migrations/" + n + ".up.sql")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = conn.PgConn().Exec(ctx, string(b)).ReadAll(); e != nil {
			t.Fatal(e)
		}
	}
	var schema string
	if e := conn.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); e != nil {
		t.Fatal(e)
	}
	dsn := processDatabaseURL(t, schema)
	parsed, _ := url.Parse(dsn)
	run(t, conn, "GRANT DELETE ON native_oauth_request_windows TO "+pgx.Identifier{parsed.User.Username()}.Sanitize())
	options, e := redis.ParseURL(os.Getenv("IM_TEST_REDIS_URL"))
	if e != nil {
		t.Fatal(e)
	}
	cache := redis.NewClient(options)
	t.Cleanup(func() { _ = cache.Close() })
	stream := "enterprise-im:test:mobile:" + schema
	receipts := stream + ":receipts"
	t.Cleanup(func() {
		if e := cache.Del(ctx, stream, receipts, outbox.PublisherPresenceKey(stream), receiptoutbox.PublisherPresenceKey(receipts)).Err(); e != nil {
			t.Error("owned Redis cleanup failed")
		}
	})
	worker := fileProductCommand(productionTestBinary(t, "im-outbox-worker"), []string{"IM_DATABASE_URL=" + dsn, "IM_OUTBOX_REDIS_URL=" + os.Getenv("IM_TEST_REDIS_URL"), "IM_OUTBOX_STREAM=" + stream})
	startRealtimeProcess(t, worker, func() bool { return cache.Exists(ctx, outbox.PublisherPresenceKey(stream)).Val() == 1 })
	receiptWorker := fileProductCommand(productionTestBinary(t, "im-receipt-worker"), []string{"IM_DATABASE_URL=" + dsn, "IM_RECEIPT_REDIS_URL=" + os.Getenv("IM_TEST_REDIS_URL"), "IM_RECEIPT_STREAM=" + receipts})
	startRealtimeProcess(t, receiptWorker, func() bool { return cache.Exists(ctx, receiptoutbox.PublisherPresenceKey(receipts)).Val() == 1 })
	// Real broker refresh uses the platform-bound synthetic identity issuer.
	env, refresh := nativeMessagingRefreshIssuerPlatform(t, conn, platform)
	upstream, _ := url.Parse(startProductionAPI(t, productionTestBinary(t, "im-api"), append(env, "IM_DATABASE_URL="+dsn, "IM_HTTP_ADDR=127.0.0.1:0", "IM_RECEIPTS_ENABLED=true", "IM_RECEIPT_WRITES_PER_SECOND=100", "IM_REALTIME_REDIS_URL="+os.Getenv("IM_TEST_REDIS_URL"), "IM_REALTIME_STREAM="+stream, "IM_RECEIPT_STREAM="+receipts)))
	key := freshFile().ID
	var mu sync.Mutex
	phases := []string{}
	rejections := []string{}
	events := []map[string]any{}
	var bobDevice string
	terminalRequests := -1
	revoked := false
	done := false
	facts := map[string]any{"platform": platform, "phases": phases}
	save := func() error {
		facts["phases"] = phases
		facts["old_device_rejections"] = rejections
		b, err := json.MarshalIndent(facts, "", "  ")
		if err != nil {
			return err
		}
		if err = os.WriteFile(os.Getenv("IM_TEST_MOBILE_FACTS"), append(b, '\n'), 0600); err != nil {
			return err
		}
		b, err = json.MarshalIndent(events, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(filepath.Dir(os.Getenv("IM_TEST_MOBILE_FACTS")), "backend-api-events.json"), append(b, '\n'), 0600)
	}
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	reverse.ModifyResponse = func(r *http.Response) error {
		mu.Lock()
		defer mu.Unlock()
		action := r.Request.URL.Path[strings.LastIndex(r.Request.URL.Path, "/")+1:]
		role := "other"
		if bobDevice != "" && r.Request.Header.Get("X-IM-Device-ID") == bobDevice {
			role = "bob"
		}
		events = append(events, map[string]any{"method": r.Request.Method, "action": action, "status": r.StatusCode, "device": role, "device_headers": r.Request.Header.Get("X-IM-Device-ID") != "" && r.Request.Header.Get("X-IM-Device-Token") != ""})
		if revoked && role == "bob" && r.StatusCode == 401 {
			for _, wanted := range []string{"sync", "delivered", "read", "messages"} {
				if action == wanted {
					found := false
					for _, old := range rejections {
						found = found || old == action
					}
					if !found {
						rejections = append(rejections, action)
					}
				}
			}
		}
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/__owned/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("X-Owned-Fixture") != key {
			http.NotFound(w, r)
			return
		}
		var input struct {
			PID    int    `json:"runtime_pid"`
			Device string `json:"bob_device_id"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 2048)).Decode(&input) != nil || input.PID <= 0 {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		order := []string{"sent", "delivered", "read", "revoked", "terminal", "done"}
		action := strings.TrimPrefix(r.URL.Path, "/__owned/")
		if len(phases) >= len(order) || action != order[len(phases)] {
			w.WriteHeader(409)
			return
		}
		if len(phases) == 0 {
			sends := 0
			for _, event := range events {
				if event["action"] == "messages" && event["method"] == "POST" && event["status"] == 200 {
					sends++
					if event["device_headers"] != true {
						t.Error("native UI omitted message device binding")
						w.WriteHeader(409)
						return
					}
				}
			}
			if sends != 3 {
				t.Error("native UI send count mismatch")
				w.WriteHeader(409)
				return
			}
			bobDevice = input.Device
			facts["runtime_pid"] = input.PID
		}
		if bobDevice == "" || input.Device != bobDevice || facts["runtime_pid"] != input.PID {
			w.WriteHeader(409)
			return
		}
		var messages, delivery, deviceRead, userRead, cursor, revocations, devices, platformDevices int
		queries := []struct {
			sql  string
			args []any
			dest []any
		}{
			{"SELECT count(*) FROM messages WHERE tenant_id=$1 AND conversation_id=$2", []any{tenantA, directA}, []any{&messages}},
			{"SELECT count(*),count(*) FILTER(WHERE read_at IS NOT NULL) FROM message_device_receipts WHERE tenant_id=$1 AND user_id=$2 AND conversation_id=$3 AND device_id=$4", []any{tenantA, personA, directA, bobDevice}, []any{&delivery, &deviceRead}},
			{"SELECT count(*) FROM message_user_receipts WHERE tenant_id=$1 AND user_id=$2 AND conversation_id=$3 AND read_at IS NOT NULL", []any{tenantA, personA, directA}, []any{&userRead}},
			{"SELECT COALESCE((SELECT read_seq FROM conversation_user_read_cursors WHERE tenant_id=$1 AND user_id=$2 AND conversation_id=$3),0)", []any{tenantA, personA, directA}, []any{&cursor}},
			{"SELECT count(*) FILTER(WHERE id=$3 AND user_id=$2 AND revoked_at IS NOT NULL),count(*),count(*) FILTER(WHERE platform=$4) FROM user_devices WHERE tenant_id=$1", []any{tenantA, personA, bobDevice, platform}, []any{&revocations, &devices, &platformDevices}},
		}
		for _, q := range queries {
			if err := conn.QueryRow(ctx, q.sql, q.args...).Scan(q.dest...); err != nil {
				t.Error("independent database query failed", err)
				w.WriteHeader(500)
				return
			}
		}
		wantDelivery, wantRead, wantRevoked := 0, 0, 0
		if len(phases) >= 1 {
			wantDelivery = 3
		}
		if len(phases) >= 2 {
			wantRead = 3
		}
		if len(phases) >= 3 {
			wantRevoked = 1
		}
		if messages != 3 || delivery != wantDelivery || deviceRead != wantRead || userRead != wantRead || cursor != wantRead || revocations != wantRevoked || devices != 2 || platformDevices != 2 {
			t.Errorf("independent phase %s facts messages=%d delivered=%d deviceRead=%d userRead=%d cursor=%d revoked=%d devices=%d platform=%d", action, messages, delivery, deviceRead, userRead, cursor, revocations, devices, platformDevices)
			w.WriteHeader(409)
			return
		}
		if action == "terminal" || action == "done" {
			if strings.Join(rejections, ",") != "sync,delivered,read,messages" {
				t.Error("missing actual revoked-device HTTP401 observations")
				w.WriteHeader(409)
				return
			}
		}
		bobEvents := 0
		for _, event := range events {
			if event["device"] == "bob" {
				bobEvents++
			}
		}
		if action == "terminal" {
			terminalRequests = bobEvents
		}
		if action == "done" {
			if terminalRequests < 0 || bobEvents != terminalRequests {
				t.Error("terminal device automatically requested again")
				w.WriteHeader(409)
				return
			}
			facts["automatic_request_delta"] = bobEvents - terminalRequests
		}
		facts["messages"] = messages
		facts["device_delivered"] = delivery
		facts["device_read"] = deviceRead
		facts["user_read"] = userRead
		facts["read_cursor"] = cursor
		facts["bob_revoked"] = revocations
		facts["device_count"] = devices
		phases = append(phases, action)
		facts["checkpoints"] = appendCheckpoint(facts["checkpoints"], map[string]any{"phase": action, "messages": messages, "device_delivered": delivery, "device_read": deviceRead, "user_read": userRead, "read_cursor": cursor, "bob_revoked": revocations, "device_count": devices})
		if action == "revoked" {
			revoked = true
		}
		if action == "done" {
			done = true
		}
		if err := save(); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		t.Logf("MOBILE_BACKEND_DB phase=%s messages=%d delivered=%d read=%d revoked=%d devices=%d", action, messages, delivery, userRead, revocations, devices)
		w.WriteHeader(204)
	})
	mux.Handle("/", reverse)
	proxy, ca := nativeProbeTLS(t, mux)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if err := save(); err != nil {
			t.Error(err)
		}
	})
	input := map[string]any{"origin": proxy.URL, "ca_pem": string(ca), "tenant": tenantA, "conversation": directA, "alice_user": adminA, "alice_membership": adminM, "alice_refresh": refresh["alice"], "bob_user": personA, "bob_membership": targetM2, "bob_refresh": refresh["bob"], "control": proxy.URL + "/__owned", "control_key": key}
	b, _ := json.Marshal(input)
	path := os.Getenv("IM_TEST_MOBILE_CONFIG")
	if e = os.WriteFile(path+".tmp", b, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(path+".tmp", path); e != nil {
		t.Fatal(e)
	}
	timeout := time.After(17 * time.Minute)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-timeout:
			t.Fatal("mobile scenario deadline")
		case <-ticker.C:
			mu.Lock()
			complete := done
			mu.Unlock()
			if complete {
				t.Log("MOBILE_BACKEND_DB all independent checkpoints passed")
				return
			}
			if _, err := os.Stat(os.Getenv("IM_TEST_MOBILE_STOP")); err == nil {
				t.Fatal("mobile scenario stopped before complete")
			}
		}
	}
}
func appendCheckpoint(old any, row map[string]any) []map[string]any {
	if old == nil {
		return []map[string]any{row}
	}
	return append(old.([]map[string]any), row)
}
