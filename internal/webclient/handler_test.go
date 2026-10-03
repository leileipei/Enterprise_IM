package webclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leileipei/Enterprise_IM/internal/httpserver"
)

type verifierFunc func(context.Context, string) (httpserver.VerifiedIdentity, error)

func (f verifierFunc) Authenticate(ctx context.Context, token string) (httpserver.VerifiedIdentity, error) {
	return f(ctx, token)
}

const redirectURL = "https://im.example.test/web/"
const validVerifier = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~abcdefghijklmnopqrstuvwxyz"

func webConfig(tokenURL string) Config {
	return Config{Issuer: "https://sso.example.test/group", AuthorizationURL: "https://sso.example.test/group/authorize",
		TokenURL: tokenURL, ClientID: "enterprise-im-web", RedirectURL: redirectURL, Scope: "openid profile"}
}

func TestConfigRejectsUnsafeBrowserLoginSettings(t *testing.T) {
	valid := webConfig("https://sso.example.test/group/token")
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	invalid := []Config{
		{Issuer: valid.Issuer, AuthorizationURL: "http://sso.example.test/authorize", TokenURL: valid.TokenURL, ClientID: valid.ClientID, RedirectURL: valid.RedirectURL, Scope: valid.Scope},
		{Issuer: valid.Issuer, AuthorizationURL: valid.AuthorizationURL, TokenURL: "https://sso.example.test/token#fragment", ClientID: valid.ClientID, RedirectURL: valid.RedirectURL, Scope: valid.Scope},
		{Issuer: valid.Issuer, AuthorizationURL: valid.AuthorizationURL, TokenURL: valid.TokenURL, ClientID: valid.ClientID, RedirectURL: "http://im.example.test/web/", Scope: valid.Scope},
		{Issuer: valid.Issuer, AuthorizationURL: valid.AuthorizationURL, TokenURL: valid.TokenURL, ClientID: valid.ClientID, RedirectURL: "https://im.example.test/other", Scope: valid.Scope},
		{Issuer: valid.Issuer, AuthorizationURL: valid.AuthorizationURL, TokenURL: valid.TokenURL, ClientID: "", RedirectURL: valid.RedirectURL, Scope: valid.Scope},
		{Issuer: valid.Issuer, AuthorizationURL: valid.AuthorizationURL, TokenURL: valid.TokenURL, ClientID: valid.ClientID, RedirectURL: valid.RedirectURL, Scope: "profile"},
	}
	for _, cfg := range invalid {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("unsafe config accepted: %+v", cfg)
		}
	}
}

func TestTokenExchangeUsesConfiguredEndpointAndVerifiesAccessToken(t *testing.T) {
	called := 0
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.Method != http.MethodPost || r.URL.Path != "/token" || r.FormValue("grant_type") != "authorization_code" ||
			r.FormValue("client_id") != "enterprise-im-web" || r.FormValue("redirect_uri") != redirectURL ||
			r.FormValue("code") != "one-time-code" || r.FormValue("code_verifier") != validVerifier {
			t.Fatalf("unexpected token request: %s %s %v", r.Method, r.URL.Path, r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"verified-access-token","token_type":"Bearer","expires_in":300,"id_token":"must-not-return","refresh_token":"must-not-return"}`))
	}))
	defer idp.Close()
	cfg := webConfig(idp.URL + "/token")
	verified := 0
	handler, err := NewHandler(http.NotFoundHandler(), verifierFunc(func(_ context.Context, token string) (httpserver.VerifiedIdentity, error) {
		verified++
		if token != "verified-access-token" {
			t.Fatalf("unexpected access token: %q", token)
		}
		return httpserver.VerifiedIdentity{TenantID: "00000000-0000-4000-8000-000000000001", UserID: "00000000-0000-4000-8000-000000000002"}, nil
	}), cfg, idp.Client())
	if err != nil {
		t.Fatal(err)
	}
	request := exchangeRequest(`{"code":"one-time-code","code_verifier":"` + validVerifier + `"}`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || called != 1 || verified != 1 || response.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(response.Body.String(), `"access_token":"verified-access-token"`) ||
		strings.Contains(response.Body.String(), "must-not-return") {
		t.Fatalf("token exchange: %d %s called=%d verified=%d", response.Code, response.Body.String(), called, verified)
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result["expires_in"] != float64(300) {
		t.Fatalf("token response: %+v %v", result, err)
	}
}

func exchangeRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/web/oauth/token", strings.NewReader(body))
	r.Header.Set("Origin", "https://im.example.test")
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestTokenExchangeRejectsUntrustedInputs(t *testing.T) {
	called := 0
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called++ }))
	defer idp.Close()
	handler, err := NewHandler(http.NotFoundHandler(), verifierFunc(func(context.Context, string) (httpserver.VerifiedIdentity, error) {
		return httpserver.VerifiedIdentity{}, nil
	}), webConfig(idp.URL+"/token"), idp.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		body   string
		origin string
		path   string
		status int
	}{
		{`{"code":"c","code_verifier":"` + validVerifier + `"}`, "https://evil.example.test", "/web/oauth/token", http.StatusForbidden},
		{`{"code":"c","code_verifier":"` + validVerifier + `"}`, "", "/web/oauth/token", http.StatusForbidden},
		{`{"code":"c","code_verifier":"bad"}`, "https://im.example.test", "/web/oauth/token", http.StatusBadRequest},
		{`{"code":"c","code":"other","code_verifier":"` + validVerifier + `"}`, "https://im.example.test", "/web/oauth/token", http.StatusBadRequest},
		{`{"code":"c","code_verifier":"` + validVerifier + `","token_url":"https://evil.example.test"}`, "https://im.example.test", "/web/oauth/token", http.StatusBadRequest},
		{`{"code":"c","code_verifier":"` + validVerifier + `"}`, "https://im.example.test", "/web/oauth/token?redirect_uri=https://evil.example.test", http.StatusBadRequest},
	} {
		r := exchangeRequest(test.body)
		r.Header.Set("Origin", test.origin)
		r.URL.Path = "/web/oauth/token"
		if strings.Contains(test.path, "?") {
			r.URL.RawQuery = strings.SplitN(test.path, "?", 2)[1]
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		if response.Code != test.status {
			t.Fatalf("input %s %q: %d %s", test.path, test.origin, response.Code, response.Body.String())
		}
	}
	if called != 0 {
		t.Fatalf("untrusted input reached IdP: %d", called)
	}
}

func TestWebPageAndConfigHaveSafeHeaders(t *testing.T) {
	handler, err := NewHandler(http.NotFoundHandler(), verifierFunc(func(context.Context, string) (httpserver.VerifiedIdentity, error) {
		return httpserver.VerifiedIdentity{}, nil
	}), webConfig("https://sso.example.test/group/token"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/web/", "/web/app.js", "/web/retention.js", "/web/style.css", "/web/config"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" ||
			response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("unsafe web resource %s: %d %v", path, response.Code, response.Header())
		}
	}
}

func TestWebCallbackPageAcceptsOnlyOIDCQuery(t *testing.T) {
	handler, err := NewHandler(http.NotFoundHandler(), verifierFunc(func(context.Context, string) (httpserver.VerifiedIdentity, error) {
		return httpserver.VerifiedIdentity{}, nil
	}), webConfig("https://sso.example.test/group/token"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path string
		want int
	}{
		{"/web/?code=one&state=two", http.StatusOK},
		{"/web/?code=one&state=two&iss=https%3A%2F%2Fsso.example.test", http.StatusOK},
		{"/web/?error=access_denied&state=two", http.StatusOK},
		{"/web/?code=one", http.StatusBadRequest},
		{"/web/?code=one&code=two&state=two", http.StatusBadRequest},
		{"/web/?code=one&state=two&state=three", http.StatusBadRequest},
		{"/web/?code=one&state=two&error=access_denied", http.StatusBadRequest},
		{"/web/?code=one&state=two&unexpected=one", http.StatusBadRequest},
		{"/web/app.js?code=one&state=two", http.StatusBadRequest},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.want {
			t.Errorf("callback path %s: got %d want %d", test.path, response.Code, test.want)
		}
	}
}

func TestTokenExchangeRejectsDuplicateTokenFields(t *testing.T) {
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"first","access_token":"second","token_type":"Bearer","expires_in":300}`))
	}))
	defer idp.Close()
	verified := false
	handler, err := NewHandler(http.NotFoundHandler(), verifierFunc(func(context.Context, string) (httpserver.VerifiedIdentity, error) {
		verified = true
		return httpserver.VerifiedIdentity{}, nil
	}), webConfig(idp.URL+"/token"), idp.Client())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, exchangeRequest(`{"code":"one-time-code","code_verifier":"`+validVerifier+`"}`))
	if response.Code != http.StatusServiceUnavailable || verified {
		t.Fatalf("ambiguous token response accepted: %d %s", response.Code, response.Body.String())
	}
}

func TestTokenExchangeAcceptsBrowserOriginWithExplicitDefaultPort(t *testing.T) {
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("redirect_uri") != "https://im.example.test:443/web/" {
			t.Fatalf("redirect URI changed: %q", r.FormValue("redirect_uri"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"verified-access-token","token_type":"Bearer","expires_in":300}`))
	}))
	defer idp.Close()
	cfg := webConfig(idp.URL + "/token")
	cfg.RedirectURL = "https://im.example.test:443/web/"
	handler, err := NewHandler(http.NotFoundHandler(), verifierFunc(func(context.Context, string) (httpserver.VerifiedIdentity, error) {
		return httpserver.VerifiedIdentity{}, nil
	}), cfg, idp.Client())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, exchangeRequest(`{"code":"one-time-code","code_verifier":"`+validVerifier+`"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("default HTTPS port rejected: %d %s", response.Code, response.Body.String())
	}
}

func TestTokenExchangeRejectsInvalidMappedIdentity(t *testing.T) {
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"unknown-token","token_type":"Bearer","expires_in":300}`))
	}))
	defer idp.Close()
	handler, err := NewHandler(http.NotFoundHandler(), verifierFunc(func(context.Context, string) (httpserver.VerifiedIdentity, error) {
		return httpserver.VerifiedIdentity{}, errors.New("unmapped user")
	}), webConfig(idp.URL+"/token"), idp.Client())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, exchangeRequest(`{"code":"one-time-code","code_verifier":"`+validVerifier+`"}`))
	if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), "unknown-token") {
		t.Fatalf("unmapped identity returned: %d %s", response.Code, response.Body.String())
	}
}

func TestTokenExchangeDoesNotFollowIdPRedirect(t *testing.T) {
	forwarded := 0
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { forwarded++ }))
	defer destination.Close()
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer idp.Close()
	handler, err := NewHandler(http.NotFoundHandler(), verifierFunc(func(context.Context, string) (httpserver.VerifiedIdentity, error) {
		return httpserver.VerifiedIdentity{}, nil
	}), webConfig(idp.URL+"/token"), idp.Client())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, exchangeRequest(`{"code":"one-time-code","code_verifier":"`+validVerifier+`"}`))
	if response.Code != http.StatusServiceUnavailable || forwarded != 0 {
		t.Fatalf("redirect followed: %d forwarded=%d", response.Code, forwarded)
	}
}
