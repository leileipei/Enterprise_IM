package nativeauth

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInvalidGrant = errors.New("invalid native grant")
	ErrUnavailable  = errors.New("native authentication unavailable")
	ErrRateLimited  = errors.New("native authentication rate exceeded")
)

type ExchangeRequest struct {
	Platform     string `json:"platform"`
	Code         string `json:"code"`
	CodeVerifier string `json:"code_verifier"`
	RedirectURI  string `json:"redirect_uri"`
	Nonce        string `json:"nonce"`
}
type RefreshRequest struct {
	Platform     string `json:"platform"`
	RefreshToken string `json:"refresh_token"`
}
type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
	ServerTime   time.Time `json:"server_time"`
}

// Runtime installs the configured JWT validator with local identity mapping.
// The IP limiter is persistent across API nodes. Neither dependency has an
// implicit allow implementation.
type TokenVerifier func(context.Context, string, string, string, string, bool) (time.Time, error)
type RequestLimiter func(context.Context, string) (time.Duration, error)
