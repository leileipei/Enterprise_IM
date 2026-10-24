package localoidc

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"math/big"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	v, ok := form(w, r)
	if !ok {
		return
	}
	kind := v.Get("grant_type")
	if (kind == "authorization_code" && !exact(v, "grant_type", "client_id", "code", "redirect_uri", "code_verifier")) || (kind == "refresh_token" && !exact(v, "grant_type", "client_id", "refresh_token")) || (kind != "authorization_code" && kind != "refresh_token") {
		deny(w, 400, "invalid_request")
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	p.sweep(now)
	var g grant
	if kind == "authorization_code" {
		value, exists := p.codes[v.Get("code")]
		if exists {
			delete(p.codes, v.Get("code"))
		}
		sum := sha256.Sum256([]byte(v.Get("code_verifier")))
		challenge := base64.RawURLEncoding.EncodeToString(sum[:])
		if !exists || value.client != v.Get("client_id") || value.redirect != v.Get("redirect_uri") || !verifier.MatchString(v.Get("code_verifier")) || subtle.ConstantTimeCompare([]byte(value.challenge), []byte(challenge)) != 1 {
			deny(w, 400, "invalid_grant")
			return
		}
		g = value
		if len(p.refresh) >= p.cfg.MaxPending {
			deny(w, 429, "temporarily_unavailable")
			return
		}
	} else {
		value, exists := p.refresh[v.Get("refresh_token")]
		if !exists || value.client != v.Get("client_id") {
			deny(w, 400, "invalid_grant")
			return
		}
		delete(p.refresh, v.Get("refresh_token"))
		g = value
	}
	refresh, e := p.random()
	if e != nil {
		deny(w, 503, "temporarily_unavailable")
		return
	}
	jti, e := p.random()
	if e != nil {
		deny(w, 503, "temporarily_unavailable")
		return
	}
	accessClaims := jwt.MapClaims{"iss": p.cfg.Issuer, "sub": g.subject, "aud": p.cfg.Audience, "client_id": g.client, "iat": now.Unix(), "exp": now.Add(900 * time.Second).Unix(), "jti": jti}
	access := jwt.NewWithClaims(jwt.SigningMethodRS256, accessClaims)
	access.Header["typ"] = "at+jwt"
	access.Header["kid"] = p.kid
	accessText, e := access.SignedString(p.key)
	if e != nil {
		deny(w, 503, "temporarily_unavailable")
		return
	}
	response := map[string]any{"access_token": accessText, "token_type": "Bearer", "expires_in": 900, "refresh_token": refresh}
	if kind == "authorization_code" {
		id := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": p.cfg.Issuer, "sub": g.subject, "aud": g.client, "nonce": g.nonce, "iat": now.Unix(), "exp": now.Add(900 * time.Second).Unix()})
		id.Header["kid"] = p.kid
		idText, e := id.SignedString(p.key)
		if e != nil {
			deny(w, 503, "temporarily_unavailable")
			return
		}
		response["id_token"] = idText
	}
	g.expires = now.Add(3600 * time.Second)
	p.refresh[refresh] = g
	writeJSON(w, 200, response)
}
func (p *Provider) keys(w http.ResponseWriter) {
	writeJSON(w, 200, map[string]any{"keys": []map[string]string{{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": p.kid, "n": base64.RawURLEncoding.EncodeToString(p.key.PublicKey.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(p.key.PublicKey.E)).Bytes())}}})
}
