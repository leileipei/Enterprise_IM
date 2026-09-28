package oidcauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
)

const (
	testIssuer   = "https://sso.example.test/group"
	testAudience = "enterprise-im-api"
	testSubject  = "directory-unique-123"
	testTenant   = "11111111-1111-4111-8111-111111111111"
	testUser     = "22222222-2222-4222-8222-222222222222"
)

type lookupFunc func(context.Context, string, string) (httpserver.VerifiedIdentity, error)

func (f lookupFunc) Lookup(ctx context.Context, issuer, subject string) (httpserver.VerifiedIdentity, error) {
	return f(ctx, issuer, subject)
}

func testClaims() accessClaims {
	return accessClaims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer: testIssuer, Subject: testSubject, Audience: jwt.ClaimStrings{testAudience},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-time.Minute)),
		ID:        "token-123",
	}, ClientID: "enterprise-im-web"}
}

func signToken(t *testing.T, key *rsa.PrivateKey, claims accessClaims, typ string, method jwt.SigningMethod) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	token.Header["typ"] = typ
	token.Header["kid"] = "test-key"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func config() Config {
	return Config{Issuer: testIssuer, Audience: testAudience, JWKSURL: "https://sso.example.test/group/keys", AllowedClientIDs: []string{"enterprise-im-web"}}
}

func TestValidAccessTokenMapsOnlyIssuerAndSubject(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	store := lookupFunc(func(_ context.Context, issuer, subject string) (httpserver.VerifiedIdentity, error) {
		called = true
		if issuer != testIssuer || subject != testSubject {
			t.Fatalf("unexpected lookup: %s %s", issuer, subject)
		}
		return httpserver.VerifiedIdentity{TenantID: testTenant, UserID: testUser}, nil
	})
	auth, err := newAuthenticator(config(), func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, store)
	if err != nil {
		t.Fatal(err)
	}
	id, err := auth.Authenticate(context.Background(), signToken(t, key, testClaims(), "at+jwt", jwt.SigningMethodRS256))
	if err != nil || !called || id.TenantID != testTenant || id.UserID != testUser {
		t.Fatalf("identity=%+v called=%v err=%v", id, called, err)
	}
}

func TestRejectsInvalidAccessTokensBeforeMapping(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		claims accessClaims
		typ    string
		key    *rsa.PrivateKey
		method jwt.SigningMethod
	}{}
	base := testClaims()
	add := func(name string, change func(*accessClaims), typ string, signingKey *rsa.PrivateKey, method jwt.SigningMethod) {
		claims := base
		change(&claims)
		cases = append(cases, struct {
			name   string
			claims accessClaims
			typ    string
			key    *rsa.PrivateKey
			method jwt.SigningMethod
		}{name, claims, typ, signingKey, method})
	}
	add("ID token type", func(*accessClaims) {}, "JWT", key, jwt.SigningMethodRS256)
	add("wrong issuer", func(c *accessClaims) { c.Issuer = "https://attacker.example.test" }, "at+jwt", key, jwt.SigningMethodRS256)
	add("wrong audience", func(c *accessClaims) { c.Audience = jwt.ClaimStrings{"other-api"} }, "at+jwt", key, jwt.SigningMethodRS256)
	add("expired", func(c *accessClaims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute)) }, "at+jwt", key, jwt.SigningMethodRS256)
	add("future issued at", func(c *accessClaims) { c.IssuedAt = jwt.NewNumericDate(time.Now().Add(time.Hour)) }, "at+jwt", key, jwt.SigningMethodRS256)
	add("no issued at", func(c *accessClaims) { c.IssuedAt = nil }, "at+jwt", key, jwt.SigningMethodRS256)
	add("no subject", func(c *accessClaims) { c.Subject = "" }, "at+jwt", key, jwt.SigningMethodRS256)
	add("no token ID", func(c *accessClaims) { c.ID = "" }, "at+jwt", key, jwt.SigningMethodRS256)
	add("wrong client", func(c *accessClaims) { c.ClientID = "other-client" }, "at+jwt", key, jwt.SigningMethodRS256)
	add("invalid signature", func(*accessClaims) {}, "at+jwt", other, jwt.SigningMethodRS256)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			store := lookupFunc(func(context.Context, string, string) (httpserver.VerifiedIdentity, error) {
				called = true
				return httpserver.VerifiedIdentity{}, nil
			})
			auth, err := newAuthenticator(config(), func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, store)
			if err != nil {
				t.Fatal(err)
			}
			_, err = auth.Authenticate(context.Background(), signToken(t, tc.key, tc.claims, tc.typ, tc.method))
			if err == nil || called {
				t.Fatalf("invalid token accepted or mapped: err=%v called=%v", err, called)
			}
		})
	}
}

func TestMissingMappingAndStoreFailure(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	valid := signToken(t, key, testClaims(), "application/at+jwt", jwt.SigningMethodRS256)
	for _, tc := range []struct {
		name        string
		lookupErr   error
		unavailable bool
	}{
		{"unmapped", ErrIdentityNotFound, false},
		{"database unavailable", errors.New("database secret"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth, err := newAuthenticator(config(), func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, lookupFunc(func(context.Context, string, string) (httpserver.VerifiedIdentity, error) {
				return httpserver.VerifiedIdentity{}, tc.lookupErr
			}))
			if err != nil {
				t.Fatal(err)
			}
			_, err = auth.Authenticate(context.Background(), valid)
			if err == nil || errors.Is(err, httpserver.ErrAuthUnavailable) != tc.unavailable {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRejectsUnsafeOrIncompleteOIDCConfiguration(t *testing.T) {
	bad := []Config{
		{},
		{Issuer: "http://sso.example.test", Audience: testAudience, JWKSURL: "https://sso.example.test/keys", AllowedClientIDs: []string{"web"}},
		{Issuer: testIssuer, Audience: testAudience, JWKSURL: "http://sso.example.test/keys", AllowedClientIDs: []string{"web"}},
		{Issuer: testIssuer, Audience: testAudience, JWKSURL: "https://sso.example.test/keys", AllowedClientIDs: nil},
	}
	for _, cfg := range bad {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("unsafe config accepted: %+v", cfg)
		}
	}
}

func TestRejectsSymmetricAlgorithmBeforeIdentityLookup(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	auth, err := newAuthenticator(config(), func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, lookupFunc(func(context.Context, string, string) (httpserver.VerifiedIdentity, error) {
		called = true
		return httpserver.VerifiedIdentity{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, testClaims())
	token.Header["typ"] = "at+jwt"
	signed, err := token.SignedString([]byte("attacker-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(context.Background(), signed); err == nil || called {
		t.Fatalf("symmetric token accepted or mapped: err=%v called=%v", err, called)
	}
}

func TestRejectsTokenWithoutKeyIDBeforeKeyLookup(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	auth, err := newAuthenticator(config(), func(*jwt.Token) (any, error) { called = true; return &key.PublicKey, nil }, lookupFunc(func(context.Context, string, string) (httpserver.VerifiedIdentity, error) {
		return httpserver.VerifiedIdentity{TenantID: testTenant, UserID: testUser}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, testClaims())
	token.Header["typ"] = "at+jwt"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(context.Background(), signed); err == nil || called {
		t.Fatalf("missing kid accepted or key lookup called: err=%v called=%v", err, called)
	}
}

func TestFetchesConfiguredJWKSAndVerifiesSignedAccessToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := testJWKS(key, "sig")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/keys" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(jwks))
	}))
	defer server.Close()
	cfg := config()
	cfg.Issuer = server.URL
	cfg.JWKSURL = server.URL + "/keys"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	auth, err := newWithHTTPClient(ctx, cfg, lookupFunc(func(_ context.Context, issuer, subject string) (httpserver.VerifiedIdentity, error) {
		if issuer != server.URL || subject != testSubject {
			t.Fatalf("unexpected external identity: %s %s", issuer, subject)
		}
		return httpserver.VerifiedIdentity{TenantID: testTenant, UserID: testUser}, nil
	}), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	claims := testClaims()
	claims.Issuer = server.URL
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["typ"] = "at+jwt"
	token.Header["kid"] = "test-key"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	id, err := auth.Authenticate(ctx, signed)
	if err != nil || id.UserID != testUser {
		t.Fatalf("JWKS verification failed: identity=%+v err=%v", id, err)
	}
}

func testJWKS(key *rsa.PrivateKey, use string) string {
	n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes())
	return fmt.Sprintf(`{"keys":[{"kty":"RSA","use":%q,"alg":"RS256","kid":"test-key","n":"%s","e":"%s"}]}`, use, n, e)
}

func TestRejectsJWKSRedirectEvenFromHTTPS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://untrusted.example.test/keys", http.StatusFound)
	}))
	defer server.Close()
	cfg := config()
	cfg.JWKSURL = server.URL + "/keys"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := newWithHTTPClient(ctx, cfg, lookupFunc(func(context.Context, string, string) (httpserver.VerifiedIdentity, error) {
		return httpserver.VerifiedIdentity{}, nil
	}), server.Client()); err == nil {
		t.Fatal("JWKS redirect accepted")
	}
}

func TestRejectsEncryptionKeyAsSignatureKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testJWKS(key, "enc")))
	}))
	defer server.Close()
	cfg := config()
	cfg.JWKSURL = server.URL + "/keys"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	auth, err := newWithHTTPClient(ctx, cfg, lookupFunc(func(context.Context, string, string) (httpserver.VerifiedIdentity, error) {
		return httpserver.VerifiedIdentity{TenantID: testTenant, UserID: testUser}, nil
	}), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, testClaims())
	token.Header["typ"] = "at+jwt"
	token.Header["kid"] = "test-key"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(ctx, signed); err == nil {
		t.Fatal("encryption key accepted for signature")
	}
	delete(token.Header, "kid")
	signedWithoutKid, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(ctx, signedWithoutKid); err == nil {
		t.Fatal("encryption key accepted without kid")
	}
}

func TestJWKSRefreshFailureReportsUnavailableForRotatedKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var unavailable atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if unavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(testJWKS(key, "sig")))
	}))
	defer server.Close()
	cfg := config()
	cfg.JWKSURL = server.URL + "/keys"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	auth, err := newWithHTTPClient(ctx, cfg, lookupFunc(func(context.Context, string, string) (httpserver.VerifiedIdentity, error) {
		return httpserver.VerifiedIdentity{TenantID: testTenant, UserID: testUser}, nil
	}), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	unavailable.Store(true)
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, testClaims())
	token.Header["typ"] = "at+jwt"
	token.Header["kid"] = "rotated-key"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = auth.Authenticate(ctx, signed)
	if !errors.Is(err, httpserver.ErrAuthUnavailable) {
		t.Fatalf("JWKS outage reported as %v", err)
	}
	_, err = auth.Authenticate(ctx, signed)
	if !errors.Is(err, httpserver.ErrAuthUnavailable) {
		t.Fatalf("second JWKS outage reported as %v", err)
	}
	_, err = auth.Authenticate(ctx, signed)
	if !errors.Is(err, httpserver.ErrAuthUnavailable) {
		t.Fatalf("continued JWKS outage reported as %v", err)
	}
}

func TestUnknownKeyRefreshIsBoundedByRequestContext(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) > 1 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		_, _ = w.Write([]byte(testJWKS(key, "sig")))
	}))
	defer server.Close()
	cfg := config()
	cfg.JWKSURL = server.URL + "/keys"
	serviceCtx, stop := context.WithCancel(context.Background())
	defer stop()
	auth, err := newWithHTTPClient(serviceCtx, cfg, lookupFunc(func(context.Context, string, string) (httpserver.VerifiedIdentity, error) {
		return httpserver.VerifiedIdentity{}, nil
	}), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, testClaims())
	token.Header["typ"] = "at+jwt"
	token.Header["kid"] = "unknown-key"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := auth.Authenticate(requestCtx, signed); err == nil {
		t.Fatal("unknown key accepted")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("unknown key blocked for %s", elapsed)
	}
}

func TestSuccessfulJWKSFetchClearsDegradedState(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	degraded := &atomic.Bool{}
	degraded.Store(true)
	client := jwksClient(server.Client(), degraded)
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if degraded.Load() {
		t.Fatal("successful JWKS response did not clear outage state")
	}
}

func TestOIDCStartupFailsWhenJWKSUnavailable(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	cfg := config()
	cfg.JWKSURL = server.URL + "/keys"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := newWithHTTPClient(ctx, cfg, lookupFunc(func(context.Context, string, string) (httpserver.VerifiedIdentity, error) {
		return httpserver.VerifiedIdentity{}, nil
	}), server.Client())
	if err == nil {
		t.Fatal("unavailable JWKS accepted at startup")
	}
}
