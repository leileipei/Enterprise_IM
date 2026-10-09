package oidcauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"github.com/leileipei/Enterprise_IM/internal/nativeauth"
	"testing"
	"time"
)

func TestNativeOAuthRejectsWrongIssuerNonceAudienceAndRedirect(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	c := config()
	c.AllowedClientIDs = append(c.AllowedClientIDs, "im-macos", "im-ios")
	auth, err := newAuthenticator(c, func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, lookupFunc(func(_ context.Context, issuer, sub string) (httpserver.VerifiedIdentity, error) {
		if issuer != testIssuer || sub != testSubject {
			return httpserver.VerifiedIdentity{}, ErrIdentityNotFound
		}
		return httpserver.VerifiedIdentity{TenantID: testTenant, UserID: testUser}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	accessClaims := testClaims()
	accessClaims.ClientID = "im-macos"
	access := signToken(t, key, accessClaims, "at+jwt", jwt.SigningMethodRS256)
	nonce := "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXpBQkNERUY"
	makeClaims := func() jwt.MapClaims {
		return jwt.MapClaims{"iss": testIssuer, "sub": testSubject, "aud": "im-macos", "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Add(-time.Second).Unix(), "nonce": nonce}
	}
	signID := func(claims jwt.MapClaims, k *rsa.PrivateKey, typ string) string {
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "test-key"
		token.Header["typ"] = typ
		raw, err := token.SignedString(k)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	expiry, err := auth.VerifyNativeTokens(context.Background(), access, signID(makeClaims(), key, "JWT"), "im-macos", nonce, true)
	if err != nil || !expiry.Equal(accessClaims.ExpiresAt.Time) {
		t.Fatal("valid native tokens or verified access expiry rejected")
	}
	cases := []struct {
		name   string
		change func(jwt.MapClaims)
		key    *rsa.PrivateKey
		typ    string
	}{
		{"wrong_issuer", func(c jwt.MapClaims) { c["iss"] = "https://other.example.test" }, key, "JWT"},
		{"wrong_nonce", func(c jwt.MapClaims) { c["nonce"] = "another-login" }, key, "JWT"},
		{"wrong_audience", func(c jwt.MapClaims) { c["aud"] = "im-ios" }, key, "JWT"},
		{"wrong_subject", func(c jwt.MapClaims) { c["sub"] = "other-user" }, key, "JWT"},
		{"expired_id", func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() }, key, "JWT"},
		{"future_issued", func(c jwt.MapClaims) { c["iat"] = time.Now().Add(time.Hour).Unix() }, key, "JWT"},
		{"missing_issued", func(c jwt.MapClaims) { delete(c, "iat") }, key, "JWT"},
		{"multi_aud_no_azp", func(c jwt.MapClaims) { c["aud"] = []string{"im-macos", "other"} }, key, "JWT"},
		{"wrong_azp", func(c jwt.MapClaims) { c["azp"] = "im-ios" }, key, "JWT"},
		{"wrong_signature", func(jwt.MapClaims) {}, other, "JWT"},
		{"access_type_as_id", func(jwt.MapClaims) {}, key, "at+jwt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims := makeClaims()
			tc.change(claims)
			_, err := auth.VerifyNativeTokens(context.Background(), access, signID(claims, tc.key, tc.typ), "im-macos", nonce, true)
			if !errors.Is(err, nativeauth.ErrInvalidGrant) {
				t.Fatal("invalid native identity token accepted")
			}
		})
	}
	claims := makeClaims()
	claims["aud"] = []string{"im-macos", "other"}
	claims["azp"] = "im-macos"
	if _, err := auth.VerifyNativeTokens(context.Background(), access, signID(claims, key, "JWT"), "im-macos", nonce, true); err != nil {
		t.Fatal("valid multi-audience authorized party rejected")
	}
	// A valid API token for another allowed client is still wrong for this exchange.
	for _, client := range []string{"enterprise-im-web", "im-ios"} {
		wrong := testClaims()
		wrong.ClientID = client
		raw := signToken(t, key, wrong, "at+jwt", jwt.SigningMethodRS256)
		if _, err := auth.VerifyNativeTokens(context.Background(), raw, signID(makeClaims(), key, "JWT"), "im-macos", nonce, true); !errors.Is(err, nativeauth.ErrInvalidGrant) {
			t.Fatal("another allowed client accepted by native exchange")
		}
	}
	if _, err := auth.VerifyNativeTokens(context.Background(), access, "", "im-macos", nonce, true); !errors.Is(err, nativeauth.ErrInvalidGrant) {
		t.Fatal("first exchange without ID token accepted")
	}
	if _, err := auth.VerifyNativeTokens(context.Background(), access, "", "im-macos", "", false); err != nil {
		t.Fatal("refresh without ID token rejected")
	}
	claims = makeClaims()
	claims["nonce"] = "original-login-nonce"
	if _, err := auth.VerifyNativeTokens(context.Background(), access, signID(claims, key, "JWT"), "im-macos", "", false); err != nil {
		t.Fatal("refresh ID token incorrectly requires a new nonce")
	}
}
