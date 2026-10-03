//go:build darwin || linux

package policystore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"github.com/redis/go-redis/v9"
)

func browserTestTLSServer(handler http.Handler) *httptest.Server {
	server := httptest.NewUnstartedServer(handler)
	// Chrome may probe self-signed fixture certificates before the test context
	// applies ignoreHTTPSErrors. Failed probes are expected and not requests.
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	return server
}

func TestRealBrowserLoginRealtimeAndOfflinePull(t *testing.T) {
	node := os.Getenv("IM_TEST_BROWSER_NODE")
	if node == "" {
		t.Skip("set IM_TEST_BROWSER_NODE to run real browser integration test")
	}
	if os.Getenv("IM_TEST_REDIS_URL") == "" {
		t.Skip("set IM_TEST_REDIS_URL to run real browser integration test")
	}
	conn := db(t)
	seedDirectConversation(t, conn)
	// Read-only evidence fixtures exercise the real authenticated admin API;
	// they do not run or enable either cleanup worker.
	run(t, conn, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ('00000000-0000-4000-8000-000000009a07',$1,$2,$3,'group_admin','2020-01-01')`, tenantA, adminM, orgA)
	run(t, conn, `INSERT INTO message_body_clear_batches(id,tenant_id,conversation_id,retention_days,cutoff_at,cleared_at,first_seq,last_seq,cleared_count)
 VALUES ('00000000-0000-4000-8000-000000009b07',$1,$2,365,'2025-10-03T10:00:00Z','2026-10-03T10:00:00Z',5,11,2)`, tenantA, directA)
	run(t, conn, `INSERT INTO message_digest_retirement_batches(id,tenant_id,conversation_id,retired_at,retired_count,first_seq,last_seq,min_expires_at,max_expires_at)
 VALUES ('00000000-0000-4000-8000-000000009d07',$1,$2,'2026-10-03T10:00:00Z',2,5,11,'2026-10-01T10:00:00Z','2026-10-03T10:00:00Z')`, tenantA, directA)
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	holdService := access.Service{DB: conn}
	hold, _, err := holdService.PlaceLegalHold(context.Background(), admin, directA,
		"00000000-0000-4000-8000-00000000ca08", "CASE-BROWSER-RELEASED")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holdService.ReleaseLegalHold(context.Background(), admin, directA, hold.ID,
		"00000000-0000-4000-8000-00000000cb08", "CAB-BROWSER-RELEASE"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := holdService.PlaceLegalHold(context.Background(), admin, directA,
		"00000000-0000-4000-8000-00000000cc08", "CASE-BROWSER-ACTIVE"); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../db/migrations/000004_external_identities.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(migration)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := (policystore.Service{DB: conn}).SendTextMessage(context.Background(), publisher(), directA,
		clientUUIDv7(time.Now(), 1601), "已有消息"); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := conn.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	databaseURL := processDatabaseURL(t, schema)

	var backend atomic.Pointer[httputil.ReverseProxy]
	proxy := browserTestTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream := backend.Load()
		if upstream == nil {
			http.Error(w, "API is starting", http.StatusServiceUnavailable)
			return
		}
		upstream.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	redirectURL := proxy.URL + "/web/"
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes())
	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"test-key","n":"%s","e":"%s"}]}`, n, e)
	var loginCodes sync.Map
	var accessToken atomic.Value
	var authorizations, exchanges atomic.Int64
	idp := browserTestTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/keys":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(jwks))
		case "/authorize":
			values := r.URL.Query()
			challenge := values.Get("code_challenge")
			if r.Method != http.MethodGet || values.Get("response_type") != "code" ||
				values.Get("client_id") != "enterprise-im-web" || values.Get("redirect_uri") != redirectURL ||
				values.Get("code_challenge_method") != "S256" || values.Get("state") == "" ||
				len(challenge) != 43 {
				http.Error(w, "invalid authorization request", http.StatusBadRequest)
				return
			}
			bytes := make([]byte, 24)
			if _, err := rand.Read(bytes); err != nil {
				http.Error(w, "code unavailable", http.StatusServiceUnavailable)
				return
			}
			code := base64.RawURLEncoding.EncodeToString(bytes)
			loginCodes.Store(code, challenge)
			authorizations.Add(1)
			callback, _ := url.Parse(redirectURL)
			query := callback.Query()
			query.Set("code", code)
			query.Set("state", values.Get("state"))
			callback.RawQuery = query.Encode()
			http.Redirect(w, r, callback.String(), http.StatusFound)
		case "/token":
			if r.Method != http.MethodPost || r.ParseForm() != nil ||
				r.Form.Get("grant_type") != "authorization_code" ||
				r.Form.Get("client_id") != "enterprise-im-web" || r.Form.Get("redirect_uri") != redirectURL {
				http.Error(w, "invalid token request", http.StatusBadRequest)
				return
			}
			challenge, ok := loginCodes.LoadAndDelete(r.Form.Get("code"))
			verifier := r.Form.Get("code_verifier")
			digest := sha256.Sum256([]byte(verifier))
			if !ok || verifier == "" || base64.RawURLEncoding.EncodeToString(digest[:]) != challenge {
				http.Error(w, "invalid authorization code", http.StatusBadRequest)
				return
			}
			exchanges.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": accessToken.Load().(string), "token_type": "Bearer", "expires_in": 300,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(idp.Close)
	accessToken.Store(productionJWT(t, key, idp.URL, "browser-admin"))
	certFile := filepath.Join(t.TempDir(), "local-idp-ca.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	run(t, conn, `INSERT INTO external_identities (issuer,subject,tenant_id,user_id) VALUES ($1,'browser-admin',$2,$3)`,
		idp.URL, tenantA, adminA)

	options, err := redis.ParseURL(os.Getenv("IM_TEST_REDIS_URL"))
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	stream := fmt.Sprintf("enterprise-im:test:browser:%d", time.Now().UnixNano())
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		var published bool
		if err := conn.QueryRow(context.Background(), `SELECT published_at IS NOT NULL FROM outbox_events
 WHERE tenant_id=$1 AND conversation_id=$2 AND seq=1`, tenantA, directA).Scan(&published); err != nil {
			t.Fatal(err)
		}
		if published {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed message was not published before API startup")
		}
		time.Sleep(25 * time.Millisecond)
	}
	apiEnv := []string{
		"IM_DATABASE_URL=" + databaseURL, "IM_HTTP_ADDR=127.0.0.1:0", "SSL_CERT_FILE=" + certFile,
		"IM_OIDC_ENABLED=true", "IM_OIDC_ISSUER=" + idp.URL,
		"IM_OIDC_AUDIENCE=enterprise-im-api", "IM_OIDC_JWKS_URL=" + idp.URL + "/keys",
		"IM_OIDC_ALLOWED_CLIENT_IDS=enterprise-im-web",
		"IM_REALTIME_REDIS_URL=" + os.Getenv("IM_TEST_REDIS_URL"), "IM_REALTIME_STREAM=" + stream,
		"IM_WEB_ENABLED=true", "IM_WEB_AUTHORIZATION_URL=" + idp.URL + "/authorize",
		"IM_WEB_TOKEN_URL=" + idp.URL + "/token", "IM_WEB_CLIENT_ID=enterprise-im-web",
		"IM_WEB_REDIRECT_URL=" + redirectURL,
	}
	apiURL := startProductionAPI(t, productionTestBinary(t, "im-api"), apiEnv)
	target, err := url.Parse(apiURL)
	if err != nil {
		t.Fatal(err)
	}
	backend.Store(httputil.NewSingleHostReverseProxy(target))

	browser := exec.Command(node, "../webclient/e2e/real_backend.cjs")
	browser.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	browser.Env = append(os.Environ(), "IM_TEST_WEB_URL="+proxy.URL)
	var output bytes.Buffer
	browser.Stdout, browser.Stderr = &output, &output
	if err := browser.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- browser.Wait() }()
	timer := time.NewTimer(90 * time.Second)
	defer timer.Stop()
	select {
	case err = <-done:
	case <-timer.C:
		_ = syscall.Kill(-browser.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatalf("real browser flow timed out: %s", output.String())
	}
	// The browser belongs to the test's process group. Clean up even if Node
	// exited without closing every Chromium child.
	_ = syscall.Kill(-browser.Process.Pid, syscall.SIGKILL)
	if err != nil {
		t.Fatalf("real browser flow: %v authorizations=%d exchanges=%d: %s",
			err, authorizations.Load(), exchanges.Load(), output.String())
	}
	if authorizations.Load() != 2 || exchanges.Load() != 2 {
		t.Fatalf("expected two real PKCE logins: authorizations=%d exchanges=%d", authorizations.Load(), exchanges.Load())
	}
	var bodyReads, digestReads int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE reason='listed_body'),
 count(*) FILTER (WHERE reason='listed_digest') FROM audit_events
 WHERE tenant_id=$1 AND action='retention_batches_list' AND outcome='allow'`, tenantA).Scan(&bodyReads, &digestReads); err != nil || bodyReads != 1 || digestReads != 1 {
		t.Fatalf("browser evidence audits body=%d digest=%d err=%v", bodyReads, digestReads, err)
	}
	var holdReads int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM audit_events
 WHERE tenant_id=$1 AND action='legal_hold_list' AND outcome='allow'`, tenantA).Scan(&holdReads); err != nil || holdReads != 3 {
		t.Fatalf("browser legal hold audit=%d err=%v", holdReads, err)
	}
	var webHolds, webEvents int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM conversation_legal_holds
 WHERE tenant_id=$1 AND conversation_id=$2 AND case_reference='CASE-BROWSER-WEB-NEW'
 AND release_approval_reference='CAB-BROWSER-WEB-RELEASE' AND released_at IS NOT NULL`, tenantA, directA).Scan(&webHolds); err != nil || webHolds != 1 {
		t.Fatalf("browser administered holds=%d err=%v", webHolds, err)
	}
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM conversation_legal_hold_events e
 JOIN conversation_legal_holds h ON h.tenant_id=e.tenant_id AND h.id=e.hold_id
 WHERE h.tenant_id=$1 AND h.case_reference='CASE-BROWSER-WEB-NEW'`, tenantA).Scan(&webEvents); err != nil || webEvents != 2 {
		t.Fatalf("browser hold events=%d err=%v", webEvents, err)
	}
	var placedAudits, releasedAudits int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE action='legal_hold_place' AND reason='case_placed'),
 count(*) FILTER (WHERE action='legal_hold_release' AND reason='case_released') FROM audit_events
 WHERE tenant_id=$1 AND outcome='allow'`, tenantA).Scan(&placedAudits, &releasedAudits); err != nil || placedAudits != 3 || releasedAudits != 2 {
		t.Fatalf("browser hold write audits place=%d release=%d err=%v", placedAudits, releasedAudits, err)
	}
	var policyDays, policyVersion, otherDays, otherVersion, historyCount, updateAudits, extensionDenials int
	if err := conn.QueryRow(context.Background(), `SELECT message_body_retention_days,retention_version
 FROM tenants WHERE id=$1`, tenantA).Scan(&policyDays, &policyVersion); err != nil || policyDays != 180 || policyVersion != 1 {
		t.Fatalf("browser retention policy days=%d version=%d err=%v", policyDays, policyVersion, err)
	}
	if err := conn.QueryRow(context.Background(), `SELECT message_body_retention_days,retention_version
 FROM tenants WHERE id=$1`, tenantB).Scan(&otherDays, &otherVersion); err != nil || otherDays != 365 || otherVersion != 0 {
		t.Fatalf("other tenant policy changed days=%d version=%d err=%v", otherDays, otherVersion, err)
	}
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM tenant_retention_policy_history
 WHERE tenant_id=$1 AND version=1 AND message_body_retention_days=180 AND approval_reference='CAB-BROWSER-RETENTION'
 AND approved_by_user_id=$2`, tenantA, adminA).Scan(&historyCount); err != nil || historyCount != 1 {
		t.Fatalf("browser approval history=%d err=%v", historyCount, err)
	}
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE outcome='allow'),
 count(*) FILTER (WHERE outcome='deny' AND reason='retention_extension_requires_empty_history')
 FROM audit_events WHERE tenant_id=$1 AND action='retention_policy_update'`, tenantA).Scan(&updateAudits, &extensionDenials); err != nil || updateAudits != 1 || extensionDenials != 1 {
		t.Fatalf("browser policy audits allow=%d extension denied=%d err=%v", updateAudits, extensionDenials, err)
	}
	var historyReads int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM audit_events
 WHERE tenant_id=$1 AND action='retention_policy_history_list' AND outcome='allow'`, tenantA).Scan(&historyReads); err != nil || historyReads != 1 {
		t.Fatalf("browser history audits=%d err=%v", historyReads, err)
	}
	var auditReads int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM audit_events
 WHERE tenant_id=$1 AND action='audit_events_list' AND outcome='allow'`, tenantA).Scan(&auditReads); err != nil || auditReads != 3 {
		t.Fatalf("browser audit reads=%d err=%v", auditReads, err)
	}
	var persisted int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM messages WHERE tenant_id=$1 AND conversation_id=$2
 AND text_body IN ('已有消息','来自真实浏览器一','断线期间来自浏览器一')`, tenantA, directA).Scan(&persisted); err != nil || persisted != 3 {
		t.Fatalf("browser messages persisted=%d err=%v", persisted, err)
	}
}
