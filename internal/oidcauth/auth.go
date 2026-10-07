// Package oidcauth validates API-bound JWT access tokens and maps external
// subjects to the local group identity. It does not implement browser login.
package oidcauth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"golang.org/x/time/rate"
)

var (
	ErrInvalidToken     = errors.New("invalid access token")
	ErrIdentityNotFound = errors.New("external identity not found")
)

type Config struct {
	Issuer           string
	Audience         string
	JWKSURL          string
	AllowedClientIDs []string
}

func (c Config) Validate() error {
	if !httpsURL(c.Issuer) || !httpsURL(c.JWKSURL) || strings.TrimSpace(c.Audience) != c.Audience || c.Audience == "" || len(c.AllowedClientIDs) == 0 {
		return errors.New("invalid OIDC configuration")
	}
	for _, id := range c.AllowedClientIDs {
		if id == "" || strings.TrimSpace(id) != id {
			return errors.New("invalid OIDC client list")
		}
	}
	return nil
}

func httpsURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.String() == value
}

type IdentityStore interface {
	Lookup(context.Context, string, string) (httpserver.VerifiedIdentity, error)
}

type Authenticator struct {
	config       Config
	clients      map[string]bool
	keyfunc      func(context.Context) jwt.Keyfunc
	store        IdentityStore
	jwksDegraded *atomic.Bool
}

// New fetches and refreshes the configured JWKS while ctx is alive. The
// issuer and JWKS endpoint are operator-provided, not read from a token.
func New(ctx context.Context, config Config, store IdentityStore) (*Authenticator, error) {
	return newWithHTTPClient(ctx, config, store, nil)
}

func newWithHTTPClient(ctx context.Context, config Config, store IdentityStore, client *http.Client) (*Authenticator, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errors.New("identity store is required")
	}
	ignoreInitialJWKSFailure := false
	degraded := &atomic.Bool{}
	keys, err := keyfunc.NewDefaultOverrideCtx(ctx, []string{config.JWKSURL}, keyfunc.Override{
		Client: jwksClient(client, degraded), HTTPTimeout: 2 * time.Second,
		NoErrorReturnFirstHTTPReq: &ignoreInitialJWKSFailure,
		RefreshUnknownKID:         rate.NewLimiter(rate.Every(time.Minute), 1),
		RateLimitWaitMax:          100 * time.Millisecond,
		RefreshErrorHandlerFunc: func(u string) func(context.Context, error) {
			return func(ctx context.Context, err error) {
				degraded.Store(true)
				slog.ErrorContext(ctx, "JWKS refresh failed", "url", u, "error", err)
			}
		},
	})
	if err != nil {
		return nil, err
	}
	verifyKeys, err := keyfunc.New(keyfunc.Options{
		Ctx: ctx, Storage: keys.Storage(), UseWhitelist: []jwkset.USE{jwkset.UseSig},
	})
	if err != nil {
		return nil, err
	}
	auth, err := newAuthenticatorWithKeyProvider(config, verifyKeys.KeyfuncCtx, store)
	if err != nil {
		return nil, err
	}
	auth.jwksDegraded = degraded
	return auth, nil
}

func newAuthenticator(config Config, key jwt.Keyfunc, store IdentityStore) (*Authenticator, error) {
	if key == nil {
		return nil, errors.New("token key is required")
	}
	return newAuthenticatorWithKeyProvider(config, func(context.Context) jwt.Keyfunc { return key }, store)
}

func newAuthenticatorWithKeyProvider(config Config, key func(context.Context) jwt.Keyfunc, store IdentityStore) (*Authenticator, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if key == nil || store == nil {
		return nil, errors.New("token key and identity store are required")
	}
	clients := make(map[string]bool, len(config.AllowedClientIDs))
	for _, id := range config.AllowedClientIDs {
		clients[id] = true
	}
	return &Authenticator{config: config, clients: clients, keyfunc: key, store: store}, nil
}

type jwksTransport struct {
	base     http.RoundTripper
	degraded *atomic.Bool
}

func (t jwksTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(r)
	if err == nil && response.StatusCode == http.StatusOK {
		t.degraded.Store(false)
	}
	return response, err
}

func jwksClient(base *http.Client, degraded *atomic.Bool) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	copy := *base
	transport := copy.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	copy.Transport = jwksTransport{base: transport, degraded: degraded}
	copy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("JWKS redirects are not allowed")
	}
	return &copy
}

type accessClaims struct {
	jwt.RegisteredClaims
	ClientID string `json:"client_id"`
}

func (a *Authenticator) Authenticate(ctx context.Context, raw string) (httpserver.VerifiedIdentity, error) {
	if a == nil || len(raw) == 0 || len(raw) > 8192 {
		return httpserver.VerifiedIdentity{}, ErrInvalidToken
	}
	claims := &accessClaims{}
	key := a.keyfunc(ctx)
	token, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, ErrInvalidToken
		}
		return key(token)
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithIssuer(a.config.Issuer),
		jwt.WithAudience(a.config.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
	)
	if err != nil {
		if a.jwksDegraded != nil && a.jwksDegraded.Load() && (errors.Is(err, jwkset.ErrKeyNotFound) || errors.Is(err, keyfunc.ErrKeyfunc) || errors.Is(err, context.DeadlineExceeded)) {
			return httpserver.VerifiedIdentity{}, httpserver.ErrAuthUnavailable
		}
		return httpserver.VerifiedIdentity{}, ErrInvalidToken
	}
	if token == nil || !token.Valid {
		return httpserver.VerifiedIdentity{}, ErrInvalidToken
	}
	typ, _ := token.Header["typ"].(string)
	if typ != "at+jwt" && typ != "application/at+jwt" {
		return httpserver.VerifiedIdentity{}, ErrInvalidToken
	}
	if claims.Subject == "" || claims.IssuedAt == nil || claims.ID == "" || !a.clients[claims.ClientID] {
		return httpserver.VerifiedIdentity{}, ErrInvalidToken
	}
	id, err := a.store.Lookup(ctx, a.config.Issuer, claims.Subject)
	if errors.Is(err, ErrIdentityNotFound) {
		return httpserver.VerifiedIdentity{}, ErrInvalidToken
	}
	if err != nil {
		return httpserver.VerifiedIdentity{}, errors.Join(httpserver.ErrAuthUnavailable, err)
	}
	id.ExpiresAt = claims.ExpiresAt.Time.UTC()
	id.Issuer = a.config.Issuer
	id.Subject = claims.Subject
	return id, nil
}
