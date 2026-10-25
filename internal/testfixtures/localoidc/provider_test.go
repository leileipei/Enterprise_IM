package localoidc

import (
	"bytes"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type browserFixture struct {
	server *httptest.Server
	client *http.Client
	now    time.Time
	cfg    Config
}

func fixture(t *testing.T, capacity int) *browserFixture {
	t.Helper()
	f := &browserFixture{now: time.Now().UTC().Truncate(time.Second)}
	f.server = httptest.NewUnstartedServer(nil)
	f.cfg = Config{Issuer: "https://" + f.server.Listener.Addr().String() + "/oidc", Audience: "enterprise-im-api", MaxPending: capacity, Now: func() time.Time { return f.now }, Clients: map[string]Client{"im-android": {ID: "im-android", RedirectURI: "com.enterpriseim.client:/oauth/callback"}, "im-ios": {ID: "im-ios", RedirectURI: "com.enterpriseim.client:/oauth/callback"}, "im-macos": {ID: "im-macos", RedirectURI: "http://127.0.0.1/oauth/callback", Loopback: true}}, Users: []User{{Subject: "local-alice", Name: "Alice"}, {Subject: "local-bob", Name: "Bob"}}}
	p, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.server.Config.Handler = p
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	f.client = f.server.Client()
	f.client.Jar, _ = cookiejar.New(nil)
	f.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return f
}
func (f *browserFixture) query() url.Values {
	v := strings.Repeat("v", 64)
	s := sha256.Sum256([]byte(v))
	random := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))
	return url.Values{"client_id": {"im-android"}, "redirect_uri": {"com.enterpriseim.client:/oauth/callback"}, "response_type": {"code"}, "scope": {"openid profile"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(s[:])}, "state": {random}, "nonce": {random}}
}
func body(t *testing.T, res *http.Response) []byte {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func (f *browserFixture) authorize(t *testing.T, q url.Values) (*http.Response, []byte) {
	t.Helper()
	r, e := f.client.Get(f.cfg.Issuer + "/authorize?" + q.Encode())
	if e != nil {
		t.Fatal(e)
	}
	return r, body(t, r)
}
func field(t *testing.T, b []byte, name string) string {
	t.Helper()
	m := regexp.MustCompile(`name="` + name + `" value="([A-Za-z0-9_-]+)"`).FindSubmatch(b)
	if len(m) != 2 {
		t.Fatal("missing selection field", name)
	}
	return string(m[1])
}
func (f *browserFixture) choose(t *testing.T, b []byte, user string) *http.Response {
	t.Helper()
	v := url.Values{"request_id": {field(t, b, "request_id")}, "selection_token": {field(t, b, "selection_token")}, "user": {user}}
	req, _ := http.NewRequest("POST", f.cfg.Issuer+"/authorize", strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", strings.TrimSuffix(f.cfg.Issuer, "/oidc"))
	r, e := f.client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func (f *browserFixture) code(t *testing.T, q url.Values) string {
	t.Helper()
	r, b := f.authorize(t, q)
	if r.StatusCode != 200 {
		t.Fatal("authorize denied", r.StatusCode)
	}
	r = f.choose(t, b, "local-alice")
	body(t, r)
	if r.StatusCode != 302 {
		t.Fatal("selection denied", r.StatusCode)
	}
	u, e := url.Parse(r.Header.Get("Location"))
	if e != nil || u.Query().Get("state") != q.Get("state") || u.Scheme != "com.enterpriseim.client" {
		t.Fatal("unsafe callback")
	}
	return u.Query().Get("code")
}
func (f *browserFixture) exchange(code string) url.Values {
	return url.Values{"grant_type": {"authorization_code"}, "client_id": {"im-android"}, "code": {code}, "redirect_uri": {"com.enterpriseim.client:/oauth/callback"}, "code_verifier": {strings.Repeat("v", 64)}}
}
func (f *browserFixture) token(t *testing.T, v url.Values) (int, map[string]any) {
	t.Helper()
	r, e := f.client.PostForm(f.cfg.Issuer+"/token", v)
	if e != nil {
		t.Fatal(e)
	}
	b := body(t, r)
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("tokens cacheable")
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		t.Fatal("non-json response")
	}
	return r.StatusCode, m
}
func (f *browserFixture) verified(t *testing.T, raw string, aud string) *jwt.Token {
	t.Helper()
	r, e := f.client.Get(f.cfg.Issuer + "/keys")
	if e != nil {
		t.Fatal(e)
	}
	var jwks struct{ Keys []struct{ N, E, Kid string } }
	if json.Unmarshal(body(t, r), &jwks) != nil || len(jwks.Keys) != 1 {
		t.Fatal("bad jwks")
	}
	n, _ := base64.RawURLEncoding.DecodeString(jwks.Keys[0].N)
	eb, _ := base64.RawURLEncoding.DecodeString(jwks.Keys[0].E)
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(eb).Int64())}
	token, e := jwt.Parse(raw, func(tk *jwt.Token) (any, error) {
		if tk.Header["kid"] != jwks.Keys[0].Kid {
			t.Fatal("kid mismatch")
		}
		return pub, nil
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(f.cfg.Issuer), jwt.WithAudience(aud), jwt.WithExpirationRequired(), jwt.WithTimeFunc(func() time.Time { return f.now }))
	if e != nil || !token.Valid {
		t.Fatal("unverified token")
	}
	return token
}
func TestBrowserCodePKCENonceAndRotatingRefresh(t *testing.T) {
	f := fixture(t, 256)
	q := f.query()
	r, b := f.authorize(t, q)
	if r.StatusCode != 200 || !bytes.Contains(b, []byte("本地合成身份源")) || !bytes.Contains(b, []byte("Alice")) || !bytes.Contains(b, []byte("Bob")) {
		t.Fatal("identity selector missing")
	}
	if r.Header.Get("Referrer-Policy") != "origin" || !strings.Contains(r.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("unsafe browser headers")
	}
	res := f.choose(t, b, "local-alice")
	body(t, res)
	u, _ := url.Parse(res.Header.Get("Location"))
	code := u.Query().Get("code")
	if res.StatusCode != 302 || code == "" {
		t.Fatal("no code")
	}
	status, tokens := f.token(t, f.exchange(code))
	if status != 200 {
		t.Fatal("exchange rejected", status)
	}
	access := f.verified(t, tokens["access_token"].(string), "enterprise-im-api")
	if access.Header["typ"] != "at+jwt" || access.Claims.(jwt.MapClaims)["client_id"] != "im-android" || access.Claims.(jwt.MapClaims)["sub"] != "local-alice" {
		t.Fatal("wrong access binding")
	}
	id := f.verified(t, tokens["id_token"].(string), "im-android")
	if id.Claims.(jwt.MapClaims)["nonce"] != q.Get("nonce") {
		t.Fatal("nonce lost")
	}
	if tokens["expires_in"] != float64(900) {
		t.Fatal("wrong access TTL")
	}
	status, _ = f.token(t, f.exchange(code))
	if status != 400 {
		t.Fatal("code replay accepted")
	}
	refresh := tokens["refresh_token"].(string)
	status, _ = f.token(t, url.Values{"grant_type": {"refresh_token"}, "client_id": {"im-ios"}, "refresh_token": {refresh}})
	if status != 400 {
		t.Fatal("cross-client refresh accepted")
	}
	status, rotated := f.token(t, url.Values{"grant_type": {"refresh_token"}, "client_id": {"im-android"}, "refresh_token": {refresh}})
	if status != 200 || rotated["refresh_token"] == refresh {
		t.Fatal("refresh did not rotate")
	}
	f.verified(t, rotated["access_token"].(string), "enterprise-im-api")
	status, _ = f.token(t, url.Values{"grant_type": {"refresh_token"}, "client_id": {"im-android"}, "refresh_token": {refresh}})
	if status != 400 {
		t.Fatal("refresh replay accepted")
	}
}
func TestRejectMalformedDuplicateAndWrongBinding(t *testing.T) {
	f := fixture(t, 256)
	for _, name := range []string{"duplicate-state", "foreign-client", "foreign-redirect", "query-redirect", "fragment-redirect", "plain-PKCE", "bad-nonce", "extra-parameter"} {
		t.Run(name, func(t *testing.T) {
			q := f.query()
			switch name {
			case "duplicate-state":
				q.Add("state", q.Get("state"))
			case "foreign-client":
				q.Set("client_id", "customer")
			case "foreign-redirect":
				q.Set("redirect_uri", "https://external.invalid/cb")
			case "query-redirect":
				q.Set("redirect_uri", "com.enterpriseim.client:/oauth/callback?x=1")
			case "fragment-redirect":
				q.Set("redirect_uri", "com.enterpriseim.client:/oauth/callback#x")
			case "plain-PKCE":
				q.Set("code_challenge_method", "plain")
			case "bad-nonce":
				q.Set("nonce", "short")
			case "extra-parameter":
				q.Set("user", "local-alice")
			}
			r, b := f.authorize(t, q)
			if r.StatusCode != 400 || bytes.Contains(b, []byte("selection_token")) {
				t.Fatal("unsafe authorization accepted")
			}
		})
	}
	for _, name := range []string{"wrong-verifier", "wrong-client", "wrong-redirect", "client-secret", "duplicate-grant"} {
		t.Run(name, func(t *testing.T) {
			code := f.code(t, f.query())
			v := f.exchange(code)
			switch name {
			case "wrong-verifier":
				v.Set("code_verifier", strings.Repeat("x", 64))
			case "wrong-client":
				v.Set("client_id", "im-ios")
			case "wrong-redirect":
				v.Set("redirect_uri", "com.enterpriseim.client:/else")
			case "client-secret":
				v.Set("client_secret", "not-allowed")
			case "duplicate-grant":
				v.Add("grant_type", "authorization_code")
			}
			status, m := f.token(t, v)
			if status != 400 || m["access_token"] != nil || m["refresh_token"] != nil {
				t.Fatal("unsafe grant accepted")
			}
		})
	}
}
func TestExpiryReplayAndCapacity(t *testing.T) {
	f := fixture(t, 1)
	r, b := f.authorize(t, f.query())
	if r.StatusCode != 200 {
		t.Fatal("initial denied")
	}
	r, _ = f.authorize(t, f.query())
	if r.StatusCode != 429 {
		t.Fatal("pending capacity unbounded")
	}
	f.now = f.now.Add(301 * time.Second)
	res := f.choose(t, b, "local-alice")
	body(t, res)
	if res.StatusCode != 400 {
		t.Fatal("expired selection accepted")
	}
	code := f.code(t, f.query())
	f.now = f.now.Add(61 * time.Second)
	status, _ := f.token(t, f.exchange(code))
	if status != 400 {
		t.Fatal("expired code accepted")
	}
	code = f.code(t, f.query())
	status, tokens := f.token(t, f.exchange(code))
	if status != 200 {
		t.Fatal("fresh grant denied")
	}
	f.now = f.now.Add(3601 * time.Second)
	status, _ = f.token(t, url.Values{"grant_type": {"refresh_token"}, "client_id": {"im-android"}, "refresh_token": {tokens["refresh_token"].(string)}})
	if status != 400 {
		t.Fatal("expired refresh accepted")
	}
}
func TestSelectionIsBrowserBoundAndSingleUse(t *testing.T) {
	f := fixture(t, 256)
	_, b := f.authorize(t, f.query())
	original := f.client.Jar
	f.client.Jar, _ = cookiejar.New(nil)
	r := f.choose(t, b, "local-alice")
	body(t, r)
	if r.StatusCode != 400 {
		t.Fatal("selection accepted from different browser")
	}
	f.client.Jar = original
	r = f.choose(t, b, "not-seeded")
	body(t, r)
	if r.StatusCode != 400 {
		t.Fatal("unknown subject accepted")
	}
	_, b = f.authorize(t, f.query())
	r = f.choose(t, b, "local-bob")
	body(t, r)
	if r.StatusCode != 302 {
		t.Fatal("selection denied")
	}
	r = f.choose(t, b, "local-bob")
	body(t, r)
	if r.StatusCode != 400 {
		t.Fatal("selection replay accepted")
	}
}
func TestConcurrentCodeSingleUse(t *testing.T) {
	f := fixture(t, 256)
	code := f.code(t, f.query())
	var wg sync.WaitGroup
	ch := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s, _ := f.token(t, f.exchange(code)); ch <- s }()
	}
	wg.Wait()
	close(ch)
	success := 0
	for s := range ch {
		if s == 200 {
			success++
		} else if s != 400 {
			t.Fatal("unexpected concurrent rejection", s)
		}
	}
	if success != 1 {
		t.Fatal("code not single-use", success)
	}
}
func TestRejectForeignConfigAndBodyBudget(t *testing.T) {
	f := fixture(t, 256)
	cfg := f.cfg
	cfg.Issuer = "https://customer.example/oidc"
	if _, e := New(cfg); e == nil {
		t.Fatal("external issuer accepted")
	}
	cfg = f.cfg
	cfg.Users = []User{{Subject: "a", Name: "a"}, {Subject: "a", Name: "b"}}
	if _, e := New(cfg); e == nil {
		t.Fatal("ambiguous users accepted")
	}
	r, e := f.client.Post(f.cfg.Issuer+"/token", "application/x-www-form-urlencoded", strings.NewReader(strings.Repeat("x", 4097)))
	if e != nil {
		t.Fatal(e)
	}
	body(t, r)
	if r.StatusCode != 413 {
		t.Fatal("body budget not enforced", r.StatusCode)
	}
}

func TestCookieOriginAndLoopbackCallbackBoundaries(t *testing.T) {
	f := fixture(t, 256)
	for _, origin := range []string{"", "https://external.invalid"} {
		_, b := f.authorize(t, f.query())
		v := url.Values{"request_id": {field(t, b, "request_id")}, "selection_token": {field(t, b, "selection_token")}, "user": {"local-alice"}}
		req, _ := http.NewRequest("POST", f.cfg.Issuer+"/authorize", strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, e := f.client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		body(t, res)
		if res.StatusCode != 400 {
			t.Fatal("foreign or missing Origin accepted")
		}
	}
	f.client.Jar, _ = cookiejar.New(nil)
	q := f.query()
	q.Set("client_id", "im-macos")
	q.Set("redirect_uri", "http://127.0.0.1:49152/oauth/callback")
	res, b := f.authorize(t, q)
	if res.StatusCode != 200 {
		t.Fatal("dynamic loopback port denied")
	}
	cookies := res.Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing secure session cookie")
	}
	if len(cookies) == 1 {
		c := cookies[0]
		if !c.HttpOnly || !c.Secure || c.Path != "/" || c.Domain != "" || c.SameSite != http.SameSiteLaxMode {
			t.Fatal("unsafe session cookie")
		}
	}
	res = f.choose(t, b, "local-bob")
	body(t, res)
	location, _ := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != 302 || location.Host != "127.0.0.1:49152" || location.Path != "/oauth/callback" {
		t.Fatal("loopback binding lost")
	}
	for _, redirect := range []string{"http://localhost:49152/oauth/callback", "http://127.0.0.1:80/oauth/callback", "http://user@127.0.0.1:49152/oauth/callback", "http://127.0.0.1:49152/oauth/%63allback", "http://127.0.0.1:49152/oauth/callback?"} {
		q.Set("redirect_uri", redirect)
		r, _ := f.authorize(t, q)
		if r.StatusCode != 400 {
			t.Fatal("noncanonical loopback accepted")
		}
	}
}
func TestGrantAndRefreshStateCapacityIsBounded(t *testing.T) {
	f := fixture(t, 1)
	code := f.code(t, f.query())
	_, pending := f.authorize(t, f.query())
	res := f.choose(t, pending, "local-alice")
	body(t, res)
	if res.StatusCode != 429 {
		t.Fatal("authorization code capacity unbounded")
	}
	s, tokens := f.token(t, f.exchange(code))
	if s != 200 {
		t.Fatal("initial exchange denied")
	}
	res = f.choose(t, pending, "local-alice")
	body(t, res)
	u, _ := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != 302 {
		t.Fatal("pending did not recover after code used")
	}
	s, _ = f.token(t, f.exchange(u.Query().Get("code")))
	if s != 429 {
		t.Fatal("refresh capacity unbounded")
	}
	s, _ = f.token(t, url.Values{"grant_type": {"refresh_token"}, "client_id": {"im-android"}, "refresh_token": {tokens["refresh_token"].(string)}})
	if s != 200 {
		t.Fatal("rotation at capacity denied")
	}
	f.now = f.now.Add(3601 * time.Second)
	code = f.code(t, f.query())
	s, _ = f.token(t, f.exchange(code))
	if s != 200 {
		t.Fatal("expired state did not release capacity")
	}
}
func TestConfigurationIsCopiedAndSelectorEscapesNames(t *testing.T) {
	f := fixture(t, 256)
	cfg := f.cfg
	cfg.Users = append([]User(nil), cfg.Users...)
	cfg.Users[0].Name = "<script>no</script>"
	p, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	cfg.Clients["im-android"] = Client{ID: "im-android", RedirectURI: "https://external.invalid"}
	cfg.Users[0].Name = "mutated"
	request := httptest.NewRequest("GET", f.cfg.Issuer+"/authorize?"+f.query().Encode(), nil)
	out := httptest.NewRecorder()
	p.ServeHTTP(out, request)
	if out.Code != 200 || bytes.Contains(out.Body.Bytes(), []byte("<script>")) || !bytes.Contains(out.Body.Bytes(), []byte("&lt;script&gt;")) {
		t.Fatal("config alias or HTML injection")
	}
}

// A browser form needs its tuple Origin, while its referrer must omit query.
// Keep rejecting null origins instead of accepting opaque cross-site posts.
func TestBrowserSelectionStillRejectsNullOrigin(t *testing.T) {
	f := fixture(t, 256)
	_, html := f.authorize(t, f.query())
	v := url.Values{"request_id": {field(t, html, "request_id")}, "selection_token": {field(t, html, "selection_token")}, "user": {"local-alice"}}
	req, _ := http.NewRequest("POST", f.cfg.Issuer+"/authorize", strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "null")
	res, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body(t, res)
	if res.StatusCode != 400 {
		t.Fatal("null Origin accepted", res.StatusCode)
	}
	valid := f.choose(t, html, "local-alice")
	body(t, valid)
	if valid.StatusCode != 302 {
		t.Fatal("valid same-origin selection rejected", valid.StatusCode)
	}
}
