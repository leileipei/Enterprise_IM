package nativeauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Broker struct {
	config Config
	verify TokenVerifier
	limit  RequestLimiter
	client *http.Client
	slots  chan struct{}
}
type requestSlotKey struct{}

func NewBroker(config Config, verify TokenVerifier, client *http.Client, limit RequestLimiter) (*Broker, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if verify == nil || limit == nil {
		return nil, errors.New("native validation and rate limiter required")
	}
	copyConfig := config
	copyConfig.Platforms = make(map[string]PlatformConfig, len(config.Platforms))
	for k, v := range config.Platforms {
		copyConfig.Platforms[k] = v
	}
	if client == nil {
		client = http.DefaultClient
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return ErrUnavailable }
	return &Broker{config: copyConfig, verify: verify, limit: limit, client: &copyClient, slots: make(chan struct{}, 20)}, nil
}
func (b *Broker) ConfigFor(platform string) (PublicConfig, error) {
	p, ok := b.config.Platforms[platform]
	if !ok {
		return PublicConfig{}, ErrInvalidGrant
	}
	return PublicConfig{Platform: platform, Issuer: b.config.Issuer, AuthorizationURL: b.config.AuthorizationURL, ClientID: p.ClientID, Scope: b.config.Scope, RedirectURI: p.RedirectURI, Loopback: p.Loopback, RefreshEnabled: p.RefreshEnabled, ServerTime: time.Now().UTC()}, nil
}

// BeginRequest bounds decoding, quota checks, and the exchange together. The
// context carries a private broker-bound slot so nested Exchange does not take
// a second semaphore permit. Direct callers receive the same bound.
func (b *Broker) BeginRequest(parent context.Context) (context.Context, func(), error) {
	if parent.Err() != nil {
		return nil, nil, ErrUnavailable
	}
	if parent.Value(requestSlotKey{}) == b {
		return parent, func() {}, nil
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	select {
	case b.slots <- struct{}{}:
	case <-ctx.Done():
		cancel()
		return nil, nil, ErrUnavailable
	default:
		cancel()
		return nil, nil, ErrUnavailable
	}
	if ctx.Err() != nil {
		cancel()
		<-b.slots
		return nil, nil, ErrUnavailable
	}
	var once sync.Once
	release := func() { once.Do(func() { cancel(); <-b.slots }) }
	return context.WithValue(ctx, requestSlotKey{}, b), release, nil
}
func (b *Broker) AllowRequest(ctx context.Context, ip string) (time.Duration, error) {
	delay, e := b.limit(ctx, ip)
	if e == nil {
		return 0, nil
	}
	if errors.Is(e, ErrRateLimited) {
		return delay, ErrRateLimited
	}
	return 0, ErrUnavailable
}
func (b *Broker) Exchange(parent context.Context, request ExchangeRequest) (Tokens, error) {
	p, ok := b.config.Platforms[request.Platform]
	if !ok || !p.allowsRedirect(request.RedirectURI) || !safeText(request.Code, 2048) || !verifierPattern.MatchString(request.CodeVerifier) || !validNonce(request.Nonce) {
		return Tokens{}, ErrInvalidGrant
	}
	ctx, release, err := b.BeginRequest(parent)
	if err != nil {
		return Tokens{}, err
	}
	defer release()
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {p.ClientID}, "code": {request.Code}, "code_verifier": {request.CodeVerifier}, "redirect_uri": {request.RedirectURI}}
	return b.exchange(ctx, p, form, request.Nonce, true)
}
func (b *Broker) Refresh(parent context.Context, request RefreshRequest) (Tokens, error) {
	p, ok := b.config.Platforms[request.Platform]
	if !ok || !p.RefreshEnabled || !safeText(request.RefreshToken, 4096) {
		return Tokens{}, ErrInvalidGrant
	}
	ctx, release, err := b.BeginRequest(parent)
	if err != nil {
		return Tokens{}, err
	}
	defer release()
	return b.exchange(ctx, p, url.Values{"grant_type": {"refresh_token"}, "client_id": {p.ClientID}, "refresh_token": {request.RefreshToken}}, "", false)
}
func (b *Broker) exchange(ctx context.Context, platform PlatformConfig, form url.Values, nonce string, requireID bool) (Tokens, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, b.config.TokenURL, strings.NewReader(form.Encode()))
	if e != nil {
		return Tokens{}, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	response, e := b.client.Do(req)
	if e != nil {
		return Tokens{}, ErrUnavailable
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, 32769))
	if e != nil || len(raw) > 32768 {
		return Tokens{}, ErrUnavailable
	}
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		if (response.StatusCode == 400 || response.StatusCode == 401) && json.Unmarshal(raw, &failure) == nil && failure.Error == "invalid_grant" {
			return Tokens{}, ErrInvalidGrant
		}
		return Tokens{}, ErrUnavailable
	}
	var body struct {
		AccessToken  string `json:"access_token"`
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF || !strings.EqualFold(body.TokenType, "Bearer") || !safeText(body.AccessToken, 8192) || len(body.IDToken) > 8192 || len(body.RefreshToken) > 4096 || (requireID && body.IDToken == "") {
		return Tokens{}, ErrUnavailable
	}
	expiry, e := b.verify(ctx, body.AccessToken, body.IDToken, platform.ClientID, nonce, requireID)
	if ctx.Err() != nil {
		return Tokens{}, ErrUnavailable
	}
	if e != nil {
		if errors.Is(e, ErrUnavailable) {
			return Tokens{}, ErrUnavailable
		}
		return Tokens{}, ErrInvalidGrant
	}
	now := time.Now().UTC()
	if !expiry.After(now) {
		return Tokens{}, ErrInvalidGrant
	}
	if !platform.RefreshEnabled {
		body.RefreshToken = ""
	}
	return Tokens{AccessToken: body.AccessToken, RefreshToken: body.RefreshToken, TokenType: "Bearer", ExpiresAt: expiry.UTC(), ServerTime: now}, nil
}
