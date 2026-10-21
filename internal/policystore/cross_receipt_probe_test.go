//go:build nativeclientintegration && (darwin || linux)

package policystore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestCrossReceiptGate(t *testing.T) {
	control := filepath.Join(t.TempDir(), "control.json")
	device := "00000000-0000-4000-8000-000000000001"
	if err := os.WriteFile(control, []byte(`{"`+device+`:read":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	forwarded := 0
	events := []map[string]any{}
	handler := crossReceiptGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded++
		raw, _ := io.ReadAll(r.Body)
		if string(raw) != `{"seqs":["1"]}` {
			t.Error("forwarded body changed")
		}
		meta := r.Context().Value(crossReceiptContextKey{}).(map[string]any)
		if meta["action"] != "read" || meta["body_sha256"] == "" {
			t.Error("missing request metadata")
		}
		w.WriteHeader(200)
	}), control, "conversation", time.Now(), func(e map[string]any) { events = append(events, e) })
	for i := 0; i < 3; i++ {
		if i == 2 {
			if err := os.WriteFile(control, []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest("PUT", "https://fixture/api/v1/conversations/conversation/read", strings.NewReader(`{"seqs":["1"]}`))
		req.Header.Set("X-IM-Device-ID", device)
		req.Header.Set("X-IM-Device-Token", "private-not-recorded")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if i < 2 && (res.Code != 503 || res.Header().Get("Retry-After") != "2") {
			t.Fatal("failure not held")
		}
		if i == 2 && res.Code != 200 {
			t.Fatal("release not forwarded")
		}
	}
	if forwarded != 1 || len(events) != 2 || events[0]["body_sha256"] != events[1]["body_sha256"] || events[0]["forwarded"] != false {
		t.Fatal("false forwarding evidence")
	}
}
func TestCrossReceiptControlRejectsMissingAndMalformed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "control.json")
	if _, err := crossReceiptHeld(p, "device:read"); err == nil {
		t.Fatal("missing control accepted")
	}
	for _, raw := range []string{`[]`, `{"device:read":true}`, `{"00000000-0000-4000-8000-000000000001:read":false}`, strings.Repeat("x", 8193)} {
		os.WriteFile(p, []byte(raw), 0600)
		if _, err := crossReceiptHeld(p, "device:read"); err == nil {
			t.Fatal("malformed control accepted")
		}
	}
}
func TestCrossReceiptRetryBackendFixture(t *testing.T) {
	t.Setenv("IM_TEST_CROSS_RECEIPT_RETRY", "1")
	TestCrossDeviceBackendFixture(t)
}

type crossReceiptContextKey struct{}

// Faults exist only in this native test TLS fixture. No success is fabricated.
func crossReceiptHeld(path, key string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(raw) > 8192 {
		return false, fmt.Errorf("invalid receipt control size")
	}
	var holds map[string]bool
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if decoder.Decode(&holds) != nil || holds == nil || len(holds) > 16 || decoder.Decode(new(any)) != io.EOF {
		return false, fmt.Errorf("invalid receipt control JSON")
	}
	for k, v := range holds {
		if !v || !regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}:(delivered|read)$`).MatchString(k) {
			return false, fmt.Errorf("invalid receipt control key")
		}
	}
	return holds[key], nil
}
func crossReceiptGate(next http.Handler, control, conversation string, began time.Time, observe func(map[string]any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := ""
		if r.Method == "PUT" {
			for _, a := range []string{"delivered", "read"} {
				if r.URL.Path == "/api/v1/conversations/"+conversation+"/"+a {
					action = a
				}
			}
		}
		if action == "" {
			next.ServeHTTP(w, r)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 32769))
		r.Body.Close()
		if err != nil || len(raw) > 32768 || r.Header.Get("X-IM-Device-ID") == "" || r.Header.Get("X-IM-Device-Token") == "" {
			http.Error(w, "invalid receipt fixture request", 400)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		meta := map[string]any{"action": action, "body_sha256": fmt.Sprintf("%x", sha256.Sum256(raw)), "elapsed_ms": time.Since(began).Milliseconds(), "injected": false, "forwarded": true}
		held, err := crossReceiptHeld(control, r.Header.Get("X-IM-Device-ID")+":"+action)
		if err != nil {
			http.Error(w, "invalid owned receipt control", 500)
			return
		}
		if held {
			meta["status"] = 503
			meta["forwarded"] = false
			meta["injected"] = true
			meta["method"] = r.Method
			meta["path"] = r.URL.Path
			meta["device"] = r.Header.Get("X-IM-Device-ID")
			meta["device_headers"] = true
			// Only read retries honor this header under the existing503 policy. Delivery
			// uses the existing exponential RetryPolicy; no new product behavior.
			if action == "read" {
				w.Header().Set("Retry-After", "2")
				meta["retry_after_seconds"] = 2
			}
			observe(meta)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			io.WriteString(w, `{"error_code":"unavailable"}`)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), crossReceiptContextKey{}, meta)))
	})
}
