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
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/redis/go-redis/v9"
)

func productionTestBinary(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", path, "../../cmd/"+name)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v: %s", name, err, output)
	}
	return path
}

func productionAPIAddress(log []byte) string {
	for _, line := range bytes.Split(log, []byte{'\n'}) {
		var event struct {
			Msg     string `json:"msg"`
			Address string `json:"address"`
		}
		if json.Unmarshal(line, &event) == nil && event.Msg == "im api listening" {
			return event.Address
		}
	}
	return ""
}

func startProductionAPI(t *testing.T, binary string, environment []string) string {
	t.Helper()
	logFile, err := os.CreateTemp(t.TempDir(), "api-*.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), environment...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logFile.Close()
	})
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		log, _ := os.ReadFile(logFile.Name())
		address := productionAPIAddress(log)
		if address != "" {
			url := "http://" + address
			res, err := client.Get(url + "/health/ready")
			if err == nil {
				res.Body.Close()
				if res.StatusCode == http.StatusOK {
					return url
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	log, _ := os.ReadFile(logFile.Name())
	t.Fatalf("production API did not become ready: %s", log)
	return ""
}

func productionJWT(t *testing.T, key *rsa.PrivateKey, issuer, subject string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss": issuer, "sub": subject, "aud": "enterprise-im-api",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Add(-time.Minute).Unix(),
		"jti": "production-api-test-" + subject, "client_id": "enterprise-im-web",
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["typ"] = "at+jwt"
	token.Header["kid"] = "test-key"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func productionRequest(t *testing.T, method, address, path, token string, body []byte, membership bool) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, address+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if membership {
		req.Header.Set("X-Acting-Membership-ID", adminM)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func productionResponse(t *testing.T, client *http.Client, req *http.Request, wantStatus int, body any) {
	t.Helper()
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != wantStatus {
		var detail bytes.Buffer
		_, _ = detail.ReadFrom(res.Body)
		t.Fatalf("%s %s: status=%d want=%d body=%s", req.Method, req.URL, res.StatusCode, wantStatus, detail.String())
	}
	if body != nil {
		if err := json.NewDecoder(res.Body).Decode(body); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProductionAPIWithOIDCAndRealtimeProcesses(t *testing.T) {
	if os.Getenv("IM_TEST_REDIS_URL") == "" {
		t.Skip("set IM_TEST_REDIS_URL for production API integration test")
	}
	conn := db(t)
	seedDirectConversation(t, conn)
	migration, err := os.ReadFile("../../db/migrations/000004_external_identities.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(migration)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := conn.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	databaseURL := processDatabaseURL(t, schema)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes())
	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"test-key","n":"%s","e":"%s"}]}`, n, e)
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/keys" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(jwks))
	}))
	t.Cleanup(idp.Close)
	certFile := filepath.Join(t.TempDir(), "test-idp-ca.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	run(t, conn, `INSERT INTO external_identities (issuer,subject,tenant_id,user_id) VALUES ($1,'admin-fixture',$2,$3)`,
		idp.URL, tenantA, adminA)
	token := productionJWT(t, key, idp.URL, "admin-fixture")
	unboundToken := productionJWT(t, key, idp.URL, "unbound-fixture")
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	wrongSignatureToken := productionJWT(t, wrongKey, idp.URL, "admin-fixture")

	options, err := redis.ParseURL(os.Getenv("IM_TEST_REDIS_URL"))
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	stream := fmt.Sprintf("enterprise-im:test:production-api:%d", time.Now().UnixNano())
	t.Cleanup(func() {
		if err := client.Del(context.Background(), stream, outbox.PublisherPresenceKey(stream)).Err(); err != nil {
			t.Errorf("clean Redis test keys: %v", err)
		}
	})
	worker := exec.Command(productionTestBinary(t, "im-outbox-worker"))
	worker.Env = append(os.Environ(), "IM_DATABASE_URL="+databaseURL,
		"IM_OUTBOX_REDIS_URL="+os.Getenv("IM_TEST_REDIS_URL"), "IM_OUTBOX_STREAM="+stream)
	startRealtimeProcess(t, worker, func() bool {
		return client.Exists(context.Background(), outbox.PublisherPresenceKey(stream)).Val() == 1
	})
	apiBinary := productionTestBinary(t, "im-api")
	apiEnv := []string{
		"IM_DATABASE_URL=" + databaseURL, "IM_HTTP_ADDR=127.0.0.1:0", "SSL_CERT_FILE=" + certFile,
		"IM_OIDC_ENABLED=true", "IM_OIDC_ISSUER=" + idp.URL,
		"IM_OIDC_AUDIENCE=enterprise-im-api", "IM_OIDC_JWKS_URL=" + idp.URL + "/keys",
		"IM_OIDC_ALLOWED_CLIENT_IDS=enterprise-im-web",
		"IM_REALTIME_REDIS_URL=" + os.Getenv("IM_TEST_REDIS_URL"), "IM_REALTIME_STREAM=" + stream,
	}
	firstURL := startProductionAPI(t, apiBinary, apiEnv)
	secondURL := startProductionAPI(t, apiBinary, apiEnv)
	if firstURL == secondURL {
		t.Fatalf("production APIs reported the same address: %s", firstURL)
	}
	httpClient := &http.Client{Timeout: 5 * time.Second}
	assertProductionRetentionBatches(t, conn, httpClient, firstURL, secondURL, token)
	assertProductionRetentionHistory(t, conn, httpClient, firstURL, secondURL, token)
	productionResponse(t, httpClient, productionRequest(t, http.MethodGet, firstURL, "/api/v1/me", "invalid", nil, false), http.StatusUnauthorized, nil)
	productionResponse(t, httpClient, productionRequest(t, http.MethodGet, firstURL, "/api/v1/me", wrongSignatureToken, nil, false), http.StatusUnauthorized, nil)
	productionResponse(t, httpClient, productionRequest(t, http.MethodGet, firstURL, "/api/v1/me", unboundToken, nil, false), http.StatusUnauthorized, nil)
	var self struct {
		TenantID string `json:"tenant_id"`
		UserID   string `json:"user_id"`
	}
	productionResponse(t, httpClient, productionRequest(t, http.MethodGet, firstURL, "/api/v1/me", token, nil, false), http.StatusOK, &self)
	if self.TenantID != tenantA || self.UserID != adminA {
		t.Fatalf("unexpected OIDC mapping: %+v", self)
	}
	issueTicket := func(address string) string {
		t.Helper()
		var result struct {
			Ticket string `json:"ticket"`
		}
		productionResponse(t, httpClient, productionRequest(t, http.MethodPost, address, "/api/v1/realtime/tickets", token, nil, true), http.StatusOK, &result)
		if len(result.Ticket) != 43 {
			t.Fatalf("invalid realtime ticket: %q", result.Ticket)
		}
		return result.Ticket
	}
	first := realtimeE2EConnect(t, firstURL, issueTicket(firstURL))
	second := realtimeE2EConnect(t, secondURL, issueTicket(secondURL))
	send := func(address, id, message string) {
		t.Helper()
		body, err := json.Marshal(map[string]string{"client_msg_id": id, "text": message})
		if err != nil {
			t.Fatal(err)
		}
		var ack struct {
			Seq int64 `json:"seq"`
		}
		productionResponse(t, httpClient, productionRequest(t, http.MethodPost, address,
			"/api/v1/conversations/"+directA+"/messages", token, body, true), http.StatusOK, &ack)
		if ack.Seq <= 0 {
			t.Fatalf("invalid message ACK: %+v", ack)
		}
	}
	pull := func(address string, after, want int64, message string) {
		t.Helper()
		var page struct {
			Messages []struct {
				Seq  int64  `json:"seq"`
				Text string `json:"text"`
			} `json:"messages"`
		}
		path := fmt.Sprintf("/api/v1/conversations/%s/messages?after_seq=%d&limit=10", directA, after)
		productionResponse(t, httpClient, productionRequest(t, http.MethodGet, address, path, token, nil, true), http.StatusOK, &page)
		if len(page.Messages) != 1 || page.Messages[0].Seq != want || page.Messages[0].Text != message {
			t.Fatalf("unexpected message page: %+v", page)
		}
	}
	send(firstURL, clientUUIDv7(time.Now(), 1401), "生产 API 消息一")
	realtimeE2EFrame(t, first, `{"type":"sync_required"}`)
	realtimeE2EFrame(t, second, `{"type":"sync_required"}`)
	pull(firstURL, 0, 1, "生产 API 消息一")
	pull(secondURL, 0, 1, "生产 API 消息一")
	first.CloseNow()
	send(secondURL, clientUUIDv7(time.Now(), 1402), "生产 API 消息二")
	realtimeE2EFrame(t, second, `{"type":"sync_required"}`)
	reconnected := realtimeE2EConnect(t, firstURL, issueTicket(firstURL))
	pull(firstURL, 1, 2, "生产 API 消息二")
	reconnected.CloseNow()
}
