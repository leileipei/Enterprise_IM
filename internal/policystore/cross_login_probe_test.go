//go:build nativeclientintegration && (darwin || linux)

package policystore_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCrossMobileNotificationsBackendFixture(t *testing.T) {
	t.Setenv("IM_TEST_CROSS_MOBILE_NOTIFICATIONS", "1")
	TestCrossFreshLoginBackendFixture(t)
}

func TestCrossFreshLoginBackendFixture(t *testing.T) {
	t.Setenv("IM_TEST_CROSS_FRESH_LOGIN", "1")
	TestCrossDeviceBackendFixture(t)
}

func TestCrossReloginBackendFixture(t *testing.T) {
	t.Setenv("IM_TEST_CROSS_FRESH_LOGIN", "1")
	t.Setenv("IM_TEST_CROSS_RELOGIN", "1")
	TestCrossDeviceBackendFixture(t)
}

func TestCrossReloginRestartBackendFixture(t *testing.T) {
	t.Setenv("IM_TEST_CROSS_RELOGIN_RESTART", "1")
	TestCrossReloginBackendFixture(t)
}

type crossLoginGrant struct{ role, client, redirect, challenge, nonce string }
type crossLoginProvider struct {
	key     *rsa.PrivateKey
	issuer  string
	mu      sync.Mutex
	codes   map[string]crossLoginGrant
	refresh map[string]crossLoginGrant
}

func newCrossLoginProvider(t *testing.T) *crossLoginProvider {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	return &crossLoginProvider{key: key, codes: map[string]crossLoginGrant{}, refresh: map[string]crossLoginGrant{}}
}

var crossLoginRandom = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func (p *crossLoginProvider) authorize(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q, e := url.ParseQuery(r.URL.RawQuery)
	role := r.Header.Get("X-IM-Fixture-Role")
	platform := map[string]string{"alice": "android", "bob_android": "android", "bob_ios": "ios"}[role]
	good := e == nil && r.Method == "GET" && platform != "" && len(q) == 8
	for _, v := range q {
		good = good && len(v) == 1
	}
	good = good && q.Get("response_type") == "code" && q.Get("client_id") == "im-"+platform && q.Get("scope") == "openid profile" && q.Get("redirect_uri") == "com.enterpriseim.client:/oauth/callback" && q.Get("code_challenge_method") == "S256" && crossLoginRandom.MatchString(q.Get("code_challenge")) && crossLoginRandom.MatchString(q.Get("state")) && crossLoginRandom.MatchString(q.Get("nonce"))
	if !good {
		http.Error(w, "invalid_request", 400)
		return
	}
	code := freshFile().ID
	p.mu.Lock()
	p.codes[code] = crossLoginGrant{role, q.Get("client_id"), q.Get("redirect_uri"), q.Get("code_challenge"), q.Get("nonce")}
	p.mu.Unlock()
	redirect, _ := url.Parse(q.Get("redirect_uri"))
	redirect.RawQuery = url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
	w.Header().Set("Location", redirect.String())
	w.WriteHeader(http.StatusFound)
}
func (p *crossLoginProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/authorize" {
		p.authorize(w, r)
		return
	}
	if r.URL.Path == "/keys" {
		fmt.Fprintf(w, `{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"cross-login-key","n":"%s","e":"%s"}]}`, base64.RawURLEncoding.EncodeToString(p.key.N.Bytes()), base64.RawURLEncoding.EncodeToString(big.NewInt(int64(p.key.E)).Bytes()))
		return
	}
	if r.URL.Path != "/token" || r.Method != "POST" || r.ParseForm() != nil {
		http.NotFound(w, r)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var grant crossLoginGrant
	var ok bool
	includeID := false
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		code := r.PostForm.Get("code")
		grant, ok = p.codes[code]
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		ok = ok && crossLoginRandom.MatchString(r.PostForm.Get("code_verifier")) && base64.RawURLEncoding.EncodeToString(sum[:]) == grant.challenge && r.PostForm.Get("redirect_uri") == grant.redirect && r.PostForm.Get("client_id") == grant.client
		if ok {
			delete(p.codes, code)
			includeID = true
		}
	case "refresh_token":
		value := r.PostForm.Get("refresh_token")
		grant, ok = p.refresh[value]
		ok = ok && r.PostForm.Get("client_id") == grant.client
		if ok {
			delete(p.refresh, value)
		}
	}
	if !ok || r.PostForm.Get("client_secret") != "" {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":"invalid_grant"}`)
		return
	}
	subject := "native-cross-bob"
	if grant.role == "alice" {
		subject = "native-cross-alice"
	}
	sign := func(id bool) string {
		claims := jwt.MapClaims{"iss": p.issuer, "sub": subject, "aud": "enterprise-im-api", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Add(-time.Minute).Unix(), "jti": freshFile().ID, "client_id": grant.client}
		typ := "at+jwt"
		if id {
			claims["aud"] = grant.client
			claims["nonce"] = grant.nonce
			typ = "JWT"
		}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "cross-login-key"
		token.Header["typ"] = typ
		value, e := token.SignedString(p.key)
		if e != nil {
			panic("owned fixture signing failed")
		}
		return value
	}
	refresh := freshFile().ID
	p.refresh[refresh] = grant
	response := map[string]any{"access_token": sign(false), "token_type": "Bearer", "refresh_token": refresh}
	if includeID {
		response["id_token"] = sign(true)
	}
	json.NewEncoder(w).Encode(response)
}
func crossLoginIssuer(t *testing.T, conn *pgx.Conn) ([]string, map[string]string, http.Handler) {
	t.Helper()
	p := newCrossLoginProvider(t)
	s := httptest.NewTLSServer(p)
	p.issuer = s.URL
	t.Cleanup(s.Close)
	ca := filepath.Join(t.TempDir(), "cross-login-idp.pem")
	if e := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	run(t, conn, "INSERT INTO external_identities(issuer,subject,tenant_id,user_id) VALUES($1,'native-cross-alice',$2,$3),($1,'native-cross-bob',$2,$4)", s.URL, tenantA, adminA, personA)
	env := []string{"SSL_CERT_FILE=" + ca, "IM_OIDC_ENABLED=true", "IM_OIDC_ISSUER=" + s.URL, "IM_OIDC_AUDIENCE=enterprise-im-api", "IM_OIDC_JWKS_URL=" + s.URL + "/keys", "IM_OIDC_ALLOWED_CLIENT_IDS=im-windows,im-macos,im-android,im-ios", "IM_NATIVE_ENABLED=true", "IM_NATIVE_AUTHORIZATION_URL=" + s.URL + "/authorize", "IM_NATIVE_TOKEN_URL=" + s.URL + "/token"}
	for _, platform := range []string{"windows", "macos", "android", "ios"} {
		prefix := "IM_NATIVE_" + strings.ToUpper(platform) + "_"
		redirect := "com.enterpriseim.client:/oauth/callback"
		if platform == "windows" || platform == "macos" {
			redirect = "http://127.0.0.1/oauth/callback"
		}
		env = append(env, prefix+"CLIENT_ID=im-"+platform, prefix+"REDIRECT_URI="+redirect, prefix+"REFRESH_ENABLED=true")
	}
	// The app fetches this fixture-only bridge using the same exact CA as the API.
	// OS browser HTTPS trust is not changed, and this is not customer SSO.
	return env, map[string]string{}, http.HandlerFunc(p.authorize)
}
