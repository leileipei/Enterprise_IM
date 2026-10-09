package httpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/leileipei/Enterprise_IM/internal/nativeauth"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func nativeHTTPConfig(tokenURL string) nativeauth.Config {
	return nativeauth.Config{Issuer: "https://sso.example.test/group", AuthorizationURL: "https://sso.example.test/group/authorize", TokenURL: tokenURL, Audience: "enterprise-im-api", Scope: "openid profile", Platforms: map[string]nativeauth.PlatformConfig{
		"windows": {Platform: "windows", ClientID: "im-windows", RedirectURI: "http://127.0.0.1/oauth/callback", Loopback: true, RefreshEnabled: true},
		"macos":   {Platform: "macos", ClientID: "im-macos", RedirectURI: "http://127.0.0.1/oauth/callback", Loopback: true, RefreshEnabled: true},
		"android": {Platform: "android", ClientID: "im-android", RedirectURI: "com.enterpriseim.client:/oauth/callback", RefreshEnabled: true},
		"ios":     {Platform: "ios", ClientID: "im-ios", RedirectURI: "com.enterpriseim.client:/oauth/callback", RefreshEnabled: true},
	}}
}
func TestNativeHTTPConfigurationAndStrictTokenBody(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fixture-access","id_token":"fixture-id","token_type":"Bearer","refresh_token":"fixture-refresh"}`))
	}))
	defer provider.Close()
	broker, err := nativeauth.NewBroker(nativeHTTPConfig(provider.URL+"/token"), func(context.Context, string, string, string, string, bool) (time.Time, error) {
		return time.Now().Add(time.Minute), nil
	}, provider.Client(), func(context.Context, string) (time.Duration, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	handler, err := HandlerWithNativeAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), broker)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/native/config?platform=macos", nil))
	var data map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &data) != nil || data["client_id"] != "im-macos" || data["redirect_uri"] != "http://127.0.0.1/oauth/callback" || data["loopback"] != true || data["server_time"] == nil || strings.Contains(w.Body.String(), provider.URL) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("public native configuration is incomplete or leaks token destination")
	}
	for _, path := range []string{"/api/v1/native/config", "/api/v1/native/config?platform=macos&platform=ios", "/api/v1/native/config?platform=linux", "/api/v1/native/config?platform=macos&token_url=other"} {
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 400 {
			t.Fatal("invalid platform query accepted")
		}
	}
	valid := `{"platform":"macos","code":"fixture-code","code_verifier":"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~abcdefghijklmnopqrstuvwxyz","redirect_uri":"http://127.0.0.1:49152/oauth/callback","nonce":"YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXpBQkNERUY"}`
	for _, tc := range []struct {
		body   string
		status int
	}{{valid[:len(valid)-1] + `,"token_url":"https://other.example.test/token"}`, 400}, {valid + ` {}`, 400}, {strings.Repeat("a", 4097), 413}} {
		w = httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/v1/native/oauth/token", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(w, r)
		if w.Code != tc.status || strings.Contains(w.Body.String(), "fixture-access") {
			t.Fatal("invalid body accepted or token exposed", w.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid body reached provider")
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/native/oauth/token", strings.NewReader(valid))
	r.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, r)
	if w.Code != 200 || calls.Load() != 1 || strings.Contains(w.Body.String(), "fixture-id") || !strings.Contains(w.Body.String(), "fixture-access") || !strings.Contains(w.Body.String(), "expires_at") || !strings.Contains(w.Body.String(), "server_time") {
		t.Fatal("native token contract failed")
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/native/oauth/token", nil))
	if w.Code != 405 || w.Header().Get("Allow") != "POST" {
		t.Fatal("native token method is open")
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/health/live", nil))
	if w.Code != 204 {
		t.Fatal("native handler intercepted existing route")
	}
}

func TestNativeHTTPRejectedBodyCannotOutliveTotalBudget(t *testing.T) {
	for _, tc := range []struct {
		name           string
		length, status int
		deny           bool
	}{{"rate_denial", 4096, 429, true}, {"oversized_body", 4097, 413, false}} {
		t.Run(tc.name, func(t *testing.T) {
			broker, err := nativeauth.NewBroker(nativeHTTPConfig("https://sso.example.test/group/token"), func(context.Context, string, string, string, string, bool) (time.Time, error) {
				return time.Time{}, nativeauth.ErrInvalidGrant
			}, nil, func(context.Context, string) (time.Duration, error) {
				if tc.deny {
					return time.Minute, nativeauth.ErrRateLimited
				}
				return 0, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			handler, err := HandlerWithNativeAuth(http.NotFoundHandler(), broker)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(6 * time.Second)); err != nil {
				t.Fatal(err)
			}
			_, err = fmt.Fprintf(conn, "POST /api/v1/native/oauth/token HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", tc.length)
			if err != nil {
				t.Fatal(err)
			}
			// Deliberately send no body: even an early denial must not cause net/http
			// to drain this request forever before publishing its closed error.
			started := time.Now()
			response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "POST"})
			if err != nil || response.StatusCode != tc.status || time.Since(started) > 5500*time.Millisecond {
				t.Fatal("rejected body escaped native request budget", err)
			}
			response.Body.Close()
		})
	}
}
