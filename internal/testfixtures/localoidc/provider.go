// Package localoidc is an isolated synthetic identity source for local testing.
// It must never be installed in a production API authentication path.
package localoidc

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Client struct {
	ID, RedirectURI string
	Loopback        bool
}
type User struct{ Subject, Name string }

// SelectionObservation deliberately contains classes only, never request values.
type SelectionObservation struct {
	Stage         string `json:"stage"`
	Origin        string `json:"origin"`
	CookiePresent bool   `json:"cookie_present"`
}
type Config struct {
	Issuer, Audience string
	Clients          map[string]Client
	Users            []User
	Now              func() time.Time
	Random           io.Reader
	MaxPending       int
	ObserveSelection func(SelectionObservation)
}
type request struct {
	client, redirect, state, nonce, challenge, selection, session string
	expires                                                       time.Time
}
type grant struct {
	client, redirect, subject, nonce, challenge string
	expires                                     time.Time
}
type Provider struct {
	mu             sync.Mutex
	cfg            Config
	key            *rsa.PrivateKey
	kid            string
	pending        map[string]request
	codes, refresh map[string]grant
}

var opaque = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
var verifier = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

const cookieName = "__Host-IMLocalSID"

func loopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
func validateIssuer(value string) error {
	u, e := url.Parse(value)
	if e != nil || u.Scheme != "https" || u.User != nil || !loopbackHost(u.Hostname()) || u.Port() == "" || u.Path != "/oidc" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.String() != value {
		return errors.New("local HTTPS issuer required")
	}
	p, e := strconv.Atoi(u.Port())
	if e != nil || p < 1024 || p > 65535 {
		return errors.New("local issuer port out of range")
	}
	return nil
}
func New(cfg Config) (*Provider, error) {
	if validateIssuer(cfg.Issuer) != nil || cfg.Audience != "enterprise-im-api" {
		return nil, errors.New("invalid local issuer or audience")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Random == nil {
		cfg.Random = rand.Reader
	}
	if cfg.MaxPending == 0 {
		cfg.MaxPending = 256
	}
	if cfg.MaxPending < 1 || cfg.MaxPending > 256 {
		return nil, errors.New("local state capacity out of range")
	}
	if len(cfg.Clients) < 1 || len(cfg.Clients) > 4 || len(cfg.Users) < 1 || len(cfg.Users) > 2 {
		return nil, errors.New("invalid local principals")
	}
	clients := make(map[string]Client, len(cfg.Clients))
	for id, c := range cfg.Clients {
		mobile := id == "im-android" || id == "im-ios"
		desktop := id == "im-macos" || id == "im-windows"
		if id != c.ID || (!mobile && !desktop) || (mobile && (c.Loopback || c.RedirectURI != "com.enterpriseim.client:/oauth/callback")) || (desktop && (!c.Loopback || c.RedirectURI != "http://127.0.0.1/oauth/callback")) {
			return nil, errors.New("invalid local public client")
		}
		clients[id] = c
	}
	seen := map[string]bool{}
	users := append([]User(nil), cfg.Users...)
	for _, u := range users {
		if (u.Subject != "local-alice" && u.Subject != "local-bob") || seen[u.Subject] || len(u.Name) < 1 || len(u.Name) > 64 || strings.ContainsAny(u.Name, "\r\n\x00") {
			return nil, errors.New("invalid synthetic user")
		}
		seen[u.Subject] = true
	}
	cfg.Clients = clients
	cfg.Users = users
	key, e := rsa.GenerateKey(cfg.Random, 2048)
	if e != nil {
		return nil, errors.New("local signing key unavailable")
	}
	pub, e := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if e != nil {
		return nil, errors.New("local signing key unavailable")
	}
	sum := sha256.Sum256(pub)
	return &Provider{cfg: cfg, key: key, kid: hex.EncodeToString(sum[:8]), pending: map[string]request{}, codes: map[string]grant{}, refresh: map[string]grant{}}, nil
}
func (p *Provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	switch r.URL.Path {
	case "/oidc/authorize":
		if r.Method == "GET" {
			p.authorize(w, r)
		} else if r.Method == "POST" {
			p.selectUser(w, r)
		} else {
			deny(w, 405, "invalid_request")
		}
	case "/oidc/token":
		if r.Method != "POST" {
			deny(w, 405, "invalid_request")
			return
		}
		p.token(w, r)
	case "/oidc/keys":
		if r.Method != "GET" {
			deny(w, 405, "invalid_request")
			return
		}
		p.keys(w)
	case "/oidc/.well-known/openid-configuration":
		if r.Method != "GET" {
			deny(w, 405, "invalid_request")
			return
		}
		writeJSON(w, 200, map[string]any{"issuer": p.cfg.Issuer, "authorization_endpoint": p.cfg.Issuer + "/authorize", "token_endpoint": p.cfg.Issuer + "/token", "jwks_uri": p.cfg.Issuer + "/keys", "response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
	default:
		deny(w, 404, "not_found")
	}
}
func deny(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func exact(v url.Values, keys ...string) bool {
	if len(v) != len(keys) {
		return false
	}
	for _, k := range keys {
		if len(v[k]) != 1 || v[k][0] == "" {
			return false
		}
	}
	return true
}
func form(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	if r.URL.RawQuery != "" {
		deny(w, 400, "invalid_request")
		return nil, false
	}
	kind, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || kind != "application/x-www-form-urlencoded" {
		deny(w, 400, "invalid_request")
		return nil, false
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, 4097))
	if e != nil {
		deny(w, 400, "invalid_request")
		return nil, false
	}
	if len(b) > 4096 {
		deny(w, 413, "invalid_request")
		return nil, false
	}
	v, e := url.ParseQuery(string(b))
	if e != nil {
		deny(w, 400, "invalid_request")
		return nil, false
	}
	return v, true
}
func (p *Provider) redirect(id, value string) bool {
	c, ok := p.cfg.Clients[id]
	if !ok {
		return false
	}
	if !c.Loopback {
		return value == c.RedirectURI
	}
	u, e := url.Parse(value)
	if e != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "/oauth/callback" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.String() != value {
		return false
	}
	port, e := strconv.Atoi(u.Port())
	return e == nil && port >= 1024 && port <= 65535
}
func (p *Provider) random() (string, error) {
	b := make([]byte, 32)
	if _, e := io.ReadFull(p.cfg.Random, b); e != nil {
		return "", errors.New("local random source unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func (p *Provider) sweep(now time.Time) {
	for k, v := range p.pending {
		if !now.Before(v.expires) {
			delete(p.pending, k)
		}
	}
	for k, v := range p.codes {
		if !now.Before(v.expires) {
			delete(p.codes, k)
		}
	}
	for k, v := range p.refresh {
		if !now.Before(v.expires) {
			delete(p.refresh, k)
		}
	}
}
func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.RawQuery) > 4096 {
		deny(w, 400, "invalid_request")
		return
	}
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil || !exact(q, "client_id", "redirect_uri", "response_type", "scope", "code_challenge_method", "code_challenge", "state", "nonce") || q.Get("response_type") != "code" || q.Get("scope") != "openid profile" || q.Get("code_challenge_method") != "S256" || !opaque.MatchString(q.Get("state")) || !opaque.MatchString(q.Get("nonce")) || !opaque.MatchString(q.Get("code_challenge")) || !p.redirect(q.Get("client_id"), q.Get("redirect_uri")) {
		deny(w, 400, "invalid_request")
		return
	}
	w.Header().Set("Content-Security-Policy", callbackCSP(q.Get("redirect_uri")))
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	p.sweep(now)
	if len(p.pending) >= p.cfg.MaxPending {
		deny(w, 429, "temporarily_unavailable")
		return
	}
	sid := ""
	if c, e := r.Cookie(cookieName); e == nil && opaque.MatchString(c.Value) {
		sid = c.Value
	}
	fresh := sid == ""
	if fresh {
		sid, e = p.random()
		if e != nil {
			deny(w, 503, "temporarily_unavailable")
			return
		}
	}
	id, e := p.random()
	if e != nil {
		deny(w, 503, "temporarily_unavailable")
		return
	}
	selection, e := p.random()
	if e != nil {
		deny(w, 503, "temporarily_unavailable")
		return
	}
	p.pending[id] = request{client: q.Get("client_id"), redirect: q.Get("redirect_uri"), state: q.Get("state"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), selection: selection, session: sid, expires: now.Add(300 * time.Second)}
	if fresh {
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: sid, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = selector.Execute(w, struct {
		ID, Token string
		Users     []User
	}{id, selection, p.cfg.Users})
}
func (p *Provider) selectUser(w http.ResponseWriter, r *http.Request) {
	observation := SelectionObservation{Stage: "origin", Origin: "other"}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		observation.Origin = "missing"
	} else if len(origins) == 1 {
		if origins[0] == strings.TrimSuffix(p.cfg.Issuer, "/oidc") {
			observation.Origin = "same"
		} else if origins[0] == "null" {
			observation.Origin = "null"
		}
	}
	cookie, cookieErr := r.Cookie(cookieName)
	observation.CookiePresent = cookieErr == nil && opaque.MatchString(cookie.Value)
	defer func() {
		if p.cfg.ObserveSelection != nil {
			p.cfg.ObserveSelection(observation)
		}
	}()
	if len(origins) != 1 || origins[0] != strings.TrimSuffix(p.cfg.Issuer, "/oidc") {
		deny(w, 400, "invalid_request")
		return
	}
	observation.Stage = "form"
	v, ok := form(w, r)
	if !ok {
		return
	}
	observation.Stage = "fields"
	if !exact(v, "request_id", "selection_token", "user") {
		deny(w, 400, "invalid_request")
		return
	}
	observation.Stage = "cookie"
	c, e := r.Cookie(cookieName)
	if e != nil || !opaque.MatchString(c.Value) {
		deny(w, 400, "invalid_request")
		return
	}
	observation.Stage = "binding"
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	p.sweep(now)
	req, ok := p.pending[v.Get("request_id")]
	if !ok || subtle.ConstantTimeCompare([]byte(req.session), []byte(c.Value)) != 1 || subtle.ConstantTimeCompare([]byte(req.selection), []byte(v.Get("selection_token"))) != 1 {
		deny(w, 400, "invalid_request")
		return
	}
	observation.Stage = "user"
	known := false
	for _, u := range p.cfg.Users {
		known = known || u.Subject == v.Get("user")
	}
	if !known {
		delete(p.pending, v.Get("request_id"))
		deny(w, 400, "invalid_request")
		return
	}
	observation.Stage = "capacity"
	if len(p.codes) >= p.cfg.MaxPending {
		deny(w, 429, "temporarily_unavailable")
		return
	}
	observation.Stage = "random"
	code, e := p.random()
	if e != nil {
		deny(w, 503, "temporarily_unavailable")
		return
	}
	delete(p.pending, v.Get("request_id"))
	p.codes[code] = grant{client: req.client, redirect: req.redirect, subject: v.Get("user"), nonce: req.nonce, challenge: req.challenge, expires: now.Add(60 * time.Second)}
	location, _ := url.Parse(req.redirect)
	location.RawQuery = url.Values{"code": {code}, "state": {req.state}}.Encode()
	observation.Stage = "accepted"
	w.Header().Set("Content-Security-Policy", callbackCSP(req.redirect))
	w.Header().Set("Location", location.String())
	w.WriteHeader(http.StatusFound)
}

var selector = template.Must(template.New("selector").Parse(`<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>本地合成身份源</title><style>body{font:18px system-ui;max-width:600px;margin:4rem auto;padding:1rem}button{display:block;margin:1rem 0;padding:1rem;width:100%}.note{color:#8a3300}</style><h1>本地合成身份源</h1><p class="note">仅供 Enterprise IM 本机测试，不是企业账号登录，不接收密码。</p><form method="post" action="/oidc/authorize"><input type="hidden" name="request_id" value="{{.ID}}"><input type="hidden" name="selection_token" value="{{.Token}}">{{range .Users}}<button type="submit" name="user" value="{{.Subject}}">{{.Name}}</button>{{end}}</form></html>`))

// Called only after the public-client redirect allowlist has accepted the URI.
func callbackCSP(redirect string) string {
	target := redirect
	if redirect == "com.enterpriseim.client:/oauth/callback" {
		target = "com.enterpriseim.client:"
	}
	return "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' " + target + "; frame-ancestors 'none'"
}
