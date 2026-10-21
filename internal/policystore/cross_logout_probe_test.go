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
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCrossLogoutGate(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "normal"
		if cancelled {
			name = "cancelled"
		}
		t.Run(name, func(t *testing.T) {
			control := filepath.Join(t.TempDir(), "control.json")
			device := "00000000-0000-4000-8000-000000000001"
			put := func(raw string) {
				t.Helper()
				if e := os.WriteFile(control+".new", []byte(raw), 0600); e != nil {
					t.Fatal(e)
				}
				if e := os.Rename(control+".new", control); e != nil {
					t.Fatal(e)
				}
			}
			put(`{"` + device + `":"2"}`)
			body := `{"next_after_seq":2,"messages":[{"seq":2}],"sync_proof":"not-recorded"}`
			held := make(chan map[string]any, 1)
			released := make(chan map[string]any, 1)
			handler := crossLogoutGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				_, _ = w.Write([]byte(body))
			}), control, "conversation", func(row map[string]any) {
				if row["phase"] == "held" {
					held <- row
				} else {
					released <- row
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest("GET", "https://fixture/api/v1/conversations/conversation/sync", nil).WithContext(ctx)
			req.Header.Set("X-IM-Device-ID", device)
			req.Header.Set("X-IM-Device-Token", "never-recorded")
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); handler.ServeHTTP(response, req) }()
			var first map[string]any
			select {
			case first = <-held:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream success not held")
			}
			if response.Body.Len() != 0 || first["next_seq"] != "2" || first["upstream_status"] != 200 {
				t.Fatal("response escaped before release")
			}
			if cancelled {
				cancel()
			}
			put(`{}`)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("release not completed")
			}
			final := <-released
			if response.Code != 200 || response.Body.String() != body || first["body_sha256"] != final["body_sha256"] || final["phase"] != "released" || final["request_cancelled"] != cancelled {
				t.Fatal("held response changed or cancellation not observed")
			}
		})
	}
}
func TestCrossLogoutControlRejectsMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.json")
	device := "00000000-0000-4000-8000-000000000001"
	if _, e := crossLogoutHeld(path, device, "2"); e == nil {
		t.Fatal("missing control accepted")
	}
	for _, raw := range []string{`[]`, `{"bad":"2"}`, `{"` + device + `":false}`, `{"` + device + `":"0"}`, `{"` + device + `":"02"}`, strings.Repeat("x", 8193)} {
		os.WriteFile(path, []byte(raw), 0600)
		if _, e := crossLogoutHeld(path, device, "2"); e == nil {
			t.Fatal("malformed control accepted")
		}
	}
}

func TestCrossLogoutBackendFixture(t *testing.T) {
	t.Setenv("IM_TEST_CROSS_LOGOUT_RACE", "1")
	TestCrossDeviceBackendFixture(t)
}

func crossLogoutHeld(path, device, next string) (bool, error) {
	f, e := os.Open(path)
	if e != nil {
		return false, e
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, 8193))
	if e != nil || len(raw) > 8192 {
		return false, fmt.Errorf("invalid logout control size")
	}
	var targets map[string]string
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if decoder.Decode(&targets) != nil || targets == nil || len(targets) > 4 || decoder.Decode(new(any)) != io.EOF {
		return false, fmt.Errorf("invalid logout control JSON")
	}
	uuid := regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
	decimal := regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	for key, value := range targets {
		if !uuid.MatchString(key) || !decimal.MatchString(value) {
			return false, fmt.Errorf("invalid logout control entry")
		}
		if _, e := strconv.ParseInt(value, 10, 64); e != nil {
			return false, e
		}
	}
	wanted, ok := targets[device]
	if !ok {
		return false, nil
	}
	threshold, _ := strconv.ParseInt(wanted, 10, 64)
	actual, e := strconv.ParseInt(next, 10, 64)
	if e != nil || actual < 0 {
		return false, fmt.Errorf("invalid response sequence")
	}
	return actual >= threshold, nil
}

// Real upstream response bytes are held before downstream headers. Cancellation
// is observed, but the bounded fixture waits for host release to test teardown.
// No token/proof/body is persisted; only a response digest and sequence metadata.
func crossLogoutGate(next http.Handler, control, conversation string, observe func(map[string]any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/conversations/"+conversation+"/sync" {
			next.ServeHTTP(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		next.ServeHTTP(recorder, r)
		raw := recorder.Body.Bytes()
		copyResponse := func() (int, error) {
			for k, vs := range recorder.Header() {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(recorder.Code)
			return w.Write(raw)
		}
		if recorder.Code != 200 {
			_, _ = copyResponse()
			return
		}
		if len(raw) > 2*1024*1024 {
			http.Error(w, "logout fixture response too large", 502)
			return
		}
		var page struct {
			Next json.RawMessage `json:"next_after_seq"`
		}
		if json.Unmarshal(raw, &page) != nil {
			http.Error(w, "invalid upstream sync page", 502)
			return
		}
		seq := strings.Trim(string(page.Next), `"`)
		device := r.Header.Get("X-IM-Device-ID")
		held, e := crossLogoutHeld(control, device, seq)
		if e != nil {
			http.Error(w, "invalid logout fixture control", 500)
			return
		}
		if !held {
			_, _ = copyResponse()
			return
		}
		metadata := func(phase string) map[string]any {
			return map[string]any{"phase": phase, "method": r.Method, "path": r.URL.Path, "device": device, "device_headers": device != "" && r.Header.Get("X-IM-Device-Token") != "", "upstream_status": recorder.Code, "body_sha256": fmt.Sprintf("%x", sha256.Sum256(raw)), "next_seq": seq}
		}
		observe(metadata("held"))
		deadline := time.Now().Add(25 * time.Second)
		for held {
			if time.Now().After(deadline) {
				observe(metadata("timeout"))
				http.Error(w, "logout fixture release timeout", 504)
				return
			}
			time.Sleep(25 * time.Millisecond)
			held, e = crossLogoutHeld(control, device, seq)
			if e != nil {
				observe(metadata("invalid_control"))
				http.Error(w, "invalid logout fixture control", 500)
				return
			}
		}
		cancelled := r.Context().Err() != nil
		n, err := copyResponse()
		event := metadata("released")
		event["request_cancelled"] = cancelled
		event["write_error"] = err != nil
		event["write_bytes"] = n
		observe(event)
	})
}
