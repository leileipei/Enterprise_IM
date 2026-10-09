package nativeauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const nativeVerifierForTest = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~abcdefghijklmnopqrstuvwxyz"
const nativeNonceForTest = "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXpBQkNERUY"

func TestNativeOAuthNoArbitraryTokenDestination(t *testing.T) {
	var requests atomic.Int32
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "private provider error", http.StatusBadRequest)
	}))
	defer endpoint.Close()
	broker, err := NewBroker(nativeConfigForTest(endpoint.URL+"/token"), func(context.Context, string, string, string, string, bool) (time.Time, error) {
		return time.Time{}, ErrInvalidGrant
	}, endpoint.Client(), func(context.Context, string) (time.Duration, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	invalid := []ExchangeRequest{
		{Platform: "linux", Code: "code", CodeVerifier: nativeVerifierForTest, RedirectURI: "http://127.0.0.1:49152/oauth/callback", Nonce: nativeNonceForTest},
		{Platform: "macos", Code: "code", CodeVerifier: nativeVerifierForTest, RedirectURI: endpoint.URL + "/token", Nonce: nativeNonceForTest},
		{Platform: "macos", Code: "code", CodeVerifier: "short", RedirectURI: "http://127.0.0.1:49152/oauth/callback", Nonce: nativeNonceForTest},
		{Platform: "macos", Code: "code", CodeVerifier: nativeVerifierForTest, RedirectURI: "http://127.0.0.1:49152/oauth/callback", Nonce: ""},
	}
	for _, request := range invalid {
		if _, err = broker.Exchange(context.Background(), request); !errors.Is(err, ErrInvalidGrant) {
			t.Fatal("invalid input did not fail closed")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid input contacted token endpoint")
	}
}

func TestNativeOAuthRedirectUsesExactLoopbackAndMobileCallback(t *testing.T) {
	var calls atomic.Int32
	var lastRedirect atomic.Value
	lastRedirect.Store("")
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		lastRedirect.Store(r.FormValue("redirect_uri"))
		if r.Method != "POST" || r.URL.Path != "/token" || r.FormValue("grant_type") != "authorization_code" || r.FormValue("code_verifier") != nativeVerifierForTest || r.FormValue("client_secret") != "" {
			t.Error("incorrect public-client exchange")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"validated-access","id_token":"validated-id","token_type":"Bearer","expires_in":999999,"refresh_token":"rotated-refresh"}`))
	}))
	defer endpoint.Close()
	expiry := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	broker, err := NewBroker(nativeConfigForTest(endpoint.URL+"/token"), func(_ context.Context, access, id, client, nonce string, required bool) (time.Time, error) {
		if access != "validated-access" || id != "validated-id" || nonce != nativeNonceForTest || !required {
			return time.Time{}, ErrInvalidGrant
		}
		return expiry, nil
	}, endpoint.Client(), func(context.Context, string) (time.Duration, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	rejected := []struct{ platform, uri string }{
		{"macos", "http://localhost:49152/oauth/callback"}, {"macos", "http://127.0.0.1/oauth/callback"}, {"macos", "http://127.0.0.1:0/oauth/callback"}, {"macos", "http://127.0.0.1:65536/oauth/callback"}, {"macos", "http://127.0.0.1:49152/oauth/callback?next=other"}, {"macos", "http://127.0.0.1:49152/oauth/%63allback"}, {"macos", "http://127.0.0.1:49152/other"}, {"macos", "http://user@127.0.0.1:49152/oauth/callback"}, {"macos", "https://127.0.0.1:49152/oauth/callback"}, {"ios", "com.enterpriseim.client://oauth/callback"}, {"ios", "other:/oauth/callback"}, {"ios", "com.enterpriseim.client:/oauth/callback#fragment"},
	}
	for _, tc := range rejected {
		t.Run(tc.platform+"_"+tc.uri, func(t *testing.T) {
			_, err := broker.Exchange(context.Background(), ExchangeRequest{Platform: tc.platform, Code: "code", CodeVerifier: nativeVerifierForTest, RedirectURI: tc.uri, Nonce: nativeNonceForTest})
			if !errors.Is(err, ErrInvalidGrant) {
				t.Fatal("unsafe callback accepted")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("unsafe callback reached provider")
	}
	for _, tc := range []struct{ platform, uri string }{{"macos", "http://127.0.0.1:49152/oauth/callback"}, {"windows", "http://127.0.0.1:60000/oauth/callback"}, {"ios", "com.enterpriseim.client:/oauth/callback"}, {"android", "com.enterpriseim.client:/oauth/callback"}} {
		tokens, err := broker.Exchange(context.Background(), ExchangeRequest{Platform: tc.platform, Code: "code", CodeVerifier: nativeVerifierForTest, RedirectURI: tc.uri, Nonce: nativeNonceForTest})
		if err != nil || tokens.AccessToken != "validated-access" || !tokens.ExpiresAt.Equal(expiry) || lastRedirect.Load().(string) != tc.uri || tokens.ServerTime.IsZero() {
			t.Fatal("valid exact callback or verified expiry failed")
		}
	}
}

func TestNativeOAuthRefreshRotationAndInvalidGrant(t *testing.T) {
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("grant_type") != "refresh_token" || r.FormValue("client_id") != "im-macos" || r.FormValue("client_secret") != "" {
			t.Error("incorrect refresh public client")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.FormValue("refresh_token") {
		case "first":
			_, _ = w.Write([]byte(`{"access_token":"new-access","token_type":"Bearer","refresh_token":"second"}`))
		case "second":
			_, _ = w.Write([]byte(`{"access_token":"new-access","token_type":"Bearer"}`))
		default:
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"PRIVATE_PROVIDER_DETAIL"}`))
		}
	}))
	defer endpoint.Close()
	broker, err := NewBroker(nativeConfigForTest(endpoint.URL+"/token"), func(_ context.Context, access, id, client, nonce string, required bool) (time.Time, error) {
		if access != "new-access" || id != "" || required || nonce != "" || client != "im-macos" {
			return time.Time{}, ErrInvalidGrant
		}
		return time.Now().Add(time.Minute), nil
	}, endpoint.Client(), func(context.Context, string) (time.Duration, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := broker.Refresh(context.Background(), RefreshRequest{Platform: "macos", RefreshToken: "first"})
	if err != nil || tokens.RefreshToken != "second" {
		t.Fatal("rotation failed")
	}
	tokens, err = broker.Refresh(context.Background(), RefreshRequest{Platform: "macos", RefreshToken: "second"})
	if err != nil || tokens.RefreshToken != "" {
		t.Fatal("missing refresh must preserve existing client credential")
	}
	_, err = broker.Refresh(context.Background(), RefreshRequest{Platform: "macos", RefreshToken: "invalid"})
	if !errors.Is(err, ErrInvalidGrant) || strings.Contains(err.Error(), "PRIVATE_PROVIDER_DETAIL") {
		t.Fatal("invalid grant or provider detail leaked")
	}
}

func TestNativeOAuthTimeoutAndSensitiveLogRedaction(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(200) }))
	defer destination.Close()
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("code") == "redirect" {
			w.Header().Set("Location", destination.URL+"/stolen")
			w.WriteHeader(307)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"PRIVATE_ACCESS_VALUE","id_token":"PRIVATE_ID_VALUE","token_type":"Bearer"}`))
	}))
	defer provider.Close()
	broker, err := NewBroker(nativeConfigForTest(provider.URL+"/token"), func(ctx context.Context, _ string, _ string, _ string, _ string, _ bool) (time.Time, error) {
		<-ctx.Done()
		return time.Now().Add(time.Minute), nil
	}, provider.Client(), func(context.Context, string) (time.Duration, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = broker.Exchange(ctx, ExchangeRequest{Platform: "macos", Code: "timeout", CodeVerifier: nativeVerifierForTest, RedirectURI: "http://127.0.0.1:49152/oauth/callback", Nonce: nativeNonceForTest})
	if !errors.Is(err, ErrUnavailable) || time.Since(started) > time.Second {
		t.Fatal("caller timeout not unavailable within budget", err)
	}
	if strings.Contains(err.Error(), "PRIVATE_ACCESS_VALUE") || strings.Contains(err.Error(), "PRIVATE_ID_VALUE") {
		t.Fatal("private token exposed in error")
	}
	_, err = broker.Exchange(context.Background(), ExchangeRequest{Platform: "macos", Code: "redirect", CodeVerifier: nativeVerifierForTest, RedirectURI: "http://127.0.0.1:49152/oauth/callback", Nonce: nativeNonceForTest})
	if !errors.Is(err, ErrUnavailable) || redirected.Load() != 0 {
		t.Fatal("provider redirect followed or exposed")
	}
}
func TestNativeOAuthAdmissionIsBoundedAndCancellationDoesNotConsumeSlots(t *testing.T) {
	broker, err := NewBroker(nativeConfigForTest("https://sso.example.test/group/token"), func(context.Context, string, string, string, string, bool) (time.Time, error) {
		return time.Time{}, ErrInvalidGrant
	}, nil, func(context.Context, string) (time.Duration, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 20; i++ {
		_, release, err := broker.BeginRequest(cancelled)
		if release != nil {
			release()
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Fatal("cancelled request admitted")
		}
	}
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for i := 0; i < 20; i++ {
		ctx, release, err := broker.BeginRequest(context.Background())
		if err != nil {
			t.Fatal("available admission rejected")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("total budget missing")
		}
		releases = append(releases, release)
	}
	if _, release, err := broker.BeginRequest(context.Background()); !errors.Is(err, ErrUnavailable) {
		if release != nil {
			release()
		}
		t.Fatal("more than twenty requests admitted")
	}
}
