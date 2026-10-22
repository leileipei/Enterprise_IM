//go:build nativeclientintegration && (darwin || linux)

package policystore_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A fixture that accepts another verifier or replay would give a false native
// login verdict. Exercise the HTTP provider, without needing a database.
func TestCrossLoginProviderBindsCodeToPKCEClientRedirectAndNonce(t *testing.T) {
	p := newCrossLoginProvider(t)
	s := httptest.NewTLSServer(p)
	defer s.Close()
	p.issuer = s.URL
	c := s.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	verifier := strings.Repeat("v", 43)
	hash := sha256.Sum256([]byte(verifier))
	query := url.Values{"response_type": {"code"}, "client_id": {"im-ios"}, "scope": {"openid profile"}, "redirect_uri": {"com.enterpriseim.client:/oauth/callback"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}, "state": {strings.Repeat("s", 43)}, "nonce": {strings.Repeat("n", 43)}}
	authorize := func(q url.Values, role string, want int) string {
		t.Helper()
		req, _ := http.NewRequest("GET", s.URL+"/authorize?"+q.Encode(), nil)
		req.Header.Set("X-IM-Fixture-Role", role)
		res, e := c.Do(req)
		if e != nil {
			t.Fatal("authorize transport")
		}
		defer res.Body.Close()
		if res.StatusCode != want || res.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("authorize contract")
		}
		if want != 302 {
			return ""
		}
		u, e := url.Parse(res.Header.Get("Location"))
		if e != nil || u.Query().Get("state") != strings.Repeat("s", 43) {
			t.Fatal("state not preserved")
		}
		return u.Query().Get("code")
	}
	authorize(query, "bob_android", 400)
	query.Add("state", "duplicate")
	authorize(query, "bob_ios", 400)
	query["state"] = []string{strings.Repeat("s", 43)}
	code := authorize(query, "bob_ios", 302)
	exchange := func(code, client, redirect, v string, want int) map[string]any {
		t.Helper()
		res, e := c.PostForm(s.URL+"/token", url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {client}, "redirect_uri": {redirect}, "code_verifier": {v}})
		if e != nil {
			t.Fatal("token transport")
		}
		defer res.Body.Close()
		var body map[string]any
		if res.StatusCode != want || res.Header.Get("Cache-Control") != "no-store" || json.NewDecoder(res.Body).Decode(&body) != nil {
			t.Fatal("token contract")
		}
		return body
	}
	exchange(code, "im-android", query.Get("redirect_uri"), verifier, 400)
	exchange(code, "im-ios", "com.enterpriseim.client:/wrong", verifier, 400)
	exchange(code, "im-ios", query.Get("redirect_uri"), strings.Repeat("x", 43), 400)
	body := exchange(code, "im-ios", query.Get("redirect_uri"), verifier, 200)
	for _, name := range []string{"access_token", "id_token", "refresh_token"} {
		if _, ok := body[name].(string); !ok {
			t.Fatal("missing token")
		}
	}
	parts := strings.Split(body["id_token"].(string), ".")
	raw, e := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if e != nil || json.Unmarshal(raw, &claims) != nil || claims["nonce"] != strings.Repeat("n", 43) || claims["aud"] != "im-ios" || claims["sub"] != "native-cross-bob" {
		t.Fatal("ID token binding")
	}
	exchange(code, "im-ios", query.Get("redirect_uri"), verifier, 400)
}
