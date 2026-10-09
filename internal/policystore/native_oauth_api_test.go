//go:build darwin || linux

package policystore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/nativeauth"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestNativeOAuthExchangeThroughRealAPIIdentityMapping(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	ctx := context.Background()
	for _, name := range []string{"000004_external_identities", "000026_native_oauth_rate"} {
		raw, e := os.ReadFile("../../db/migrations/" + name + ".up.sql")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = conn.PgConn().Exec(ctx, string(raw)).ReadAll(); e != nil {
			t.Fatal("native fixture migration failed")
		}
	}
	var schema string
	if e := conn.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); e != nil {
		t.Fatal(e)
	}
	databaseURL := processDatabaseURL(t, schema)
	parsed, e := url.Parse(databaseURL)
	if e != nil {
		t.Fatal("invalid fixture database URL")
	}
	run(t, conn, "GRANT DELETE ON native_oauth_request_windows TO "+pgx.Identifier{parsed.User.Username()}.Sanitize())
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"test-key","n":"%s","e":"%s"}]}`, base64.RawURLEncoding.EncodeToString(key.N.Bytes()), base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()))
	nonce := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	verifier := strings.Repeat("v", 64)
	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	var issuer atomic.Value
	var exchanges atomic.Int32
	sign := func(subject, client string, id bool) string {
		claims := jwt.MapClaims{"iss": issuer.Load().(string), "sub": subject, "aud": "enterprise-im-api", "exp": expiry.Unix(), "iat": time.Now().Add(-time.Minute).Unix(), "jti": "native-owned-fixture", "client_id": client}
		typ := "at+jwt"
		if id {
			claims["aud"] = client
			claims["nonce"] = nonce
			typ = "JWT"
		}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "test-key"
		token.Header["typ"] = typ
		value, err := token.SignedString(key)
		if err != nil {
			t.Error("fixture signing failed")
		}
		return value
	}
	idp := browserTestTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/keys" {
			io.WriteString(w, jwks)
			return
		}
		if r.URL.Path != "/token" || r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		if r.ParseForm() != nil || r.Form.Get("client_id") != "im-macos" || r.Form.Get("client_secret") != "" {
			w.WriteHeader(400)
			return
		}
		subject, client := "native-admin", "im-macos"
		includeID := false
		refresh := "native-refresh-1"
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code_verifier") != verifier || r.Form.Get("redirect_uri") != "http://127.0.0.1:49152/oauth/callback" {
				w.WriteHeader(400)
				return
			}
			includeID = true
			switch r.Form.Get("code") {
			case "valid":
			case "wrong-client":
				client = "im-windows"
			case "unbound":
				subject = "native-unbound"
			default:
				w.WriteHeader(400)
				io.WriteString(w, `{"error":"invalid_grant","error_description":"PRIVATE_PROVIDER_DETAIL"}`)
				return
			}
		case "refresh_token":
			if r.Form.Get("refresh_token") != "native-refresh-1" {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			refresh = "native-refresh-2"
		default:
			w.WriteHeader(400)
			return
		}
		exchanges.Add(1)
		reply := map[string]any{"access_token": sign(subject, client, false), "token_type": "Bearer", "refresh_token": refresh, "expires_in": 999999999}
		if includeID {
			reply["id_token"] = sign(subject, client, true)
		}
		json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(idp.Close)
	issuer.Store(idp.URL)
	cert := filepath.Join(t.TempDir(), "native-idp-ca.pem")
	if e = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	run(t, conn, "INSERT INTO external_identities(issuer,subject,tenant_id,user_id) VALUES($1,'native-admin',$2,$3)", idp.URL, tenantA, adminA)
	env := []string{"IM_DATABASE_URL=" + databaseURL, "IM_HTTP_ADDR=127.0.0.1:0", "SSL_CERT_FILE=" + cert, "IM_OIDC_ENABLED=true", "IM_OIDC_ISSUER=" + idp.URL, "IM_OIDC_AUDIENCE=enterprise-im-api", "IM_OIDC_JWKS_URL=" + idp.URL + "/keys", "IM_OIDC_ALLOWED_CLIENT_IDS=im-windows,im-macos,im-android,im-ios", "IM_RECEIPTS_ENABLED=true", "IM_NATIVE_ENABLED=true", "IM_NATIVE_AUTHORIZATION_URL=" + idp.URL + "/authorize", "IM_NATIVE_TOKEN_URL=" + idp.URL + "/token"}
	for _, platform := range []string{"windows", "macos", "android", "ios"} {
		prefix := "IM_NATIVE_" + strings.ToUpper(platform) + "_"
		redirect := "com.enterpriseim.client:/oauth/callback"
		if platform == "windows" || platform == "macos" {
			redirect = "http://127.0.0.1/oauth/callback"
		}
		env = append(env, prefix+"CLIENT_ID=im-"+platform, prefix+"REDIRECT_URI="+redirect, prefix+"REFRESH_ENABLED=true")
	}
	binary := productionTestBinary(t, "im-api")
	address := startProductionAPI(t, binary, env)
	clientHTTP := &http.Client{Timeout: 8 * time.Second}
	request := func(addr, method, path string, body any, bearer string, want int, target any) {
		t.Helper()
		var raw []byte
		if body != nil {
			var err error
			raw, err = json.Marshal(body)
			if err != nil {
				t.Fatal("request encoding failed")
			}
		}
		req := productionRequest(t, method, addr, path, bearer, raw, false)
		res, err := clientHTTP.Do(req)
		if err != nil {
			t.Fatal("native API request failed")
		}
		defer res.Body.Close()
		reply, err := io.ReadAll(io.LimitReader(res.Body, 32769))
		if err != nil || len(reply) > 32768 {
			t.Fatal("native API response budget failed")
		}
		if res.StatusCode != want {
			t.Fatalf("native API status=%d want=%d path=%s", res.StatusCode, want, path)
		}
		if strings.HasPrefix(path, "/api/v1/native/") && res.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("native response permits caching")
		}
		if want != 200 && (bytes.Contains(reply, []byte("PRIVATE_PROVIDER_DETAIL")) || bytes.Contains(reply, []byte("access_token")) || bytes.Contains(reply, []byte("refresh_token"))) {
			t.Fatal("sensitive denial response")
		}
		if target != nil && json.Unmarshal(reply, target) != nil {
			t.Fatal("native response shape failed")
		}
	}
	var config nativeauth.PublicConfig
	request(address, "GET", "/api/v1/native/config?platform=macos", nil, "", 200, &config)
	if config.ClientID != "im-macos" || config.Issuer != idp.URL || !config.RefreshEnabled || !config.Loopback {
		t.Fatal("native public configuration mismatch")
	}
	exchange := nativeauth.ExchangeRequest{Platform: "macos", Code: "valid", CodeVerifier: verifier, RedirectURI: "http://127.0.0.1:49152/oauth/callback", Nonce: nonce}
	var tokens nativeauth.Tokens
	request(address, "POST", "/api/v1/native/oauth/token", exchange, "", 200, &tokens)
	if tokens.AccessToken == "" || tokens.RefreshToken != "native-refresh-1" || !tokens.ExpiresAt.Equal(expiry) || tokens.ServerTime.IsZero() {
		t.Fatal("verified native expiry/token response mismatch")
	}
	var self policystore.SelfContext
	request(address, "GET", "/api/v1/me", nil, tokens.AccessToken, 200, &self)
	if self.UserID != adminA || self.TenantID != tenantA {
		t.Fatal("native identity mapped to wrong local principal")
	}
	var refreshed nativeauth.Tokens
	request(address, "POST", "/api/v1/native/oauth/refresh", nativeauth.RefreshRequest{Platform: "macos", RefreshToken: tokens.RefreshToken}, "", 200, &refreshed)
	if refreshed.RefreshToken != "native-refresh-2" || refreshed.AccessToken == "" {
		t.Fatal("native refresh rotation failed")
	}
	for _, code := range []string{"wrong-client", "unbound", "invalid"} {
		rejected := exchange
		rejected.Code = code
		request(address, "POST", "/api/v1/native/oauth/token", rejected, "", 401, nil)
	}
	wrongNonce := exchange
	wrongNonce.Nonce = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
	request(address, "POST", "/api/v1/native/oauth/token", wrongNonce, "", 401, nil)
	run(t, conn, "UPDATE users SET status='frozen' WHERE tenant_id=$1 AND id=$2", tenantA, adminA)
	request(address, "POST", "/api/v1/native/oauth/token", exchange, "", 401, nil)
	request(address, "POST", "/api/v1/native/oauth/refresh", nativeauth.RefreshRequest{Platform: "macos", RefreshToken: tokens.RefreshToken}, "", 401, nil)
	run(t, conn, "UPDATE users SET status='active' WHERE tenant_id=$1 AND id=$2", tenantA, adminA)
	second := startProductionAPI(t, binary, env)
	run(t, conn, "DELETE FROM native_oauth_request_windows")
	for i := 0; i < 20; i++ {
		node := address
		if i%2 == 1 {
			node = second
		}
		request(node, "POST", "/api/v1/native/oauth/token", exchange, "", 200, nil)
	}
	before := exchanges.Load()
	request(address, "POST", "/api/v1/native/oauth/token", exchange, "", 429, nil)
	request(second, "POST", "/api/v1/native/oauth/token", exchange, "", 429, nil)
	if exchanges.Load() != before {
		t.Fatal("shared quota denial reached IdP")
	}
	disabledEnv := append([]string{}, env...)
	for i, v := range disabledEnv {
		if v == "IM_NATIVE_ENABLED=true" {
			disabledEnv[i] = "IM_NATIVE_ENABLED=false"
		}
	}
	disabled := startProductionAPI(t, binary, disabledEnv)
	request(disabled, "GET", "/api/v1/native/config?platform=macos", nil, "", 404, nil)
	request(disabled, "GET", "/api/v1/me", nil, tokens.AccessToken, 200, &self)
}
