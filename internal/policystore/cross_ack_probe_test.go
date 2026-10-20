//go:build nativeclientintegration && (darwin || linux)

package policystore_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestCrossAckObservation(t *testing.T) {
	seen := map[string]bool{}
	body := `{"message_id":"m","seq":1,"duplicate":false}`
	check := func(device, raw string, want bool) {
		t.Helper()
		response := &http.Response{Body: io.NopCloser(strings.NewReader(raw))}
		ack, drop, err := crossObserveACK(response, device, seen)
		if err != nil || drop != want || ack["message_id"] != "m" || ack["seq"] != "1" {
			t.Fatalf("ACK observation: %v %v %v", ack, drop, err)
		}
		restored, _ := io.ReadAll(response.Body)
		if string(restored) != raw {
			t.Fatal("body not restored")
		}
	}
	check("a", body, true)
	check("a", `{"message_id":"m","seq":"1","duplicate":true}`, false)
	check("a", body, false)
	check("b", body, true)
	for _, raw := range []string{`{}`, strings.Repeat("x", 4097), `{"message_id":"m","seq":0,"duplicate":false}`, `{"message_id":"m","seq":1.5,"duplicate":false}`} {
		_, _, err := crossObserveACK(&http.Response{Body: io.NopCloser(strings.NewReader(raw))}, "c", seen)
		if err == nil || seen["c"] {
			t.Fatal("invalid ACK accepted or consumed drop")
		}
	}
}

func TestCrossAckTransportAbort(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"message_id":"m","seq":"1","duplicate":false}`)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	proxy := httputil.NewSingleHostReverseProxy(u)
	committed := false
	proxy.ModifyResponse = func(r *http.Response) error {
		_, drop, err := crossObserveACK(r, "a", map[string]bool{})
		if err != nil {
			return err
		}
		committed = r.StatusCode == 200
		if drop {
			panic(http.ErrAbortHandler)
		}
		return nil
	}
	server := httptest.NewTLSServer(proxy)
	defer server.Close()
	response, err := server.Client().Post(server.URL, "application/json", strings.NewReader(`{}`))
	if response != nil {
		response.Body.Close()
	}
	if !committed || err == nil || response != nil {
		t.Fatal("upstream ACK did not become a downstream connection loss")
	}
}

func TestCrossAckRestartBackendFixture(t *testing.T) {
	t.Setenv("IM_TEST_CROSS_ACK_RESTART", "1")
	TestCrossDeviceBackendFixture(t)
}

// Fixture-only observer: records the real production ACK before a deliberate
// transport abort. Only the first committed nonduplicate ACK per device drops.
func crossObserveACK(response *http.Response, device string, seen map[string]bool) (map[string]any, bool, error) {
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil || len(raw) > 4096 || device == "" {
		return nil, false, fmt.Errorf("invalid bounded ACK")
	}
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return nil, false, fmt.Errorf("invalid ACK JSON")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, false, fmt.Errorf("trailing ACK JSON")
	}
	id, ok := value["message_id"].(string)
	duplicate, valid := value["duplicate"].(bool)
	var seq string
	switch n := value["seq"].(type) {
	case string:
		seq = n
	case json.Number:
		seq = string(n)
	}
	n, parse := strconv.ParseInt(seq, 10, 64)
	if !ok || id == "" || !valid || parse != nil || n <= 0 || strconv.FormatInt(n, 10) != seq {
		return nil, false, fmt.Errorf("invalid ACK fields")
	}
	drop := !duplicate && !seen[device]
	if drop {
		seen[device] = true
	}
	return map[string]any{"message_id": id, "seq": seq, "duplicate": duplicate}, drop, nil
}
