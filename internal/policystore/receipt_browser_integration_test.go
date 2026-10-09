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
	"github.com/leileipei/Enterprise_IM/internal/testfixtures"
	"math/big"
	"net/http"
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

	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"github.com/leileipei/Enterprise_IM/internal/receiptoutbox"
	"github.com/redis/go-redis/v9"
)

func TestTwoRealBrowsersDeliveryReadAndRecovery(t *testing.T) {
	node := os.Getenv("IM_TEST_BROWSER_NODE")
	if node == "" {
		t.Skip("set IM_TEST_BROWSER_NODE to run real browser integration test")
	}
	if os.Getenv("IM_TEST_REDIS_URL") == "" {
		t.Skip("set IM_TEST_REDIS_URL to run real browser integration test")
	}
	conn := db(t)
	seedDirectConversation(t, conn)
	ctx := context.Background()
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn}
	rules := []policy.Rule{}
	for _, action := range []policy.Action{policy.ActionDirectoryView, policy.ActionSendMessage, policy.ActionCreateGroup, policy.ActionStartChat} {
		rules = append(rules, policy.Rule{ID: "receipt-web-" + string(action), TenantID: tenantA, Effect: policy.EffectAllow, Action: action, SourceOrganizationID: orgA, TargetOrganizationID: orgA2, Bidirectional: true, Reason: "owned receipt browser fixture", RequestedBy: adminA, ApprovedBy: adminA, EffectiveTo: time.Now().Add(time.Hour), EffectiveFrom: time.Now().Add(-time.Hour)})
	}
	if _, err := svc.Publish(ctx, publisher(), 0, rules, "receipt browser cross-org fixture"); err != nil {
		t.Fatal(err)
	}
	cross, err := svc.CreateGroup(ctx, publisher(), policystore.CreateGroupRequest{ClientRequestID: clientUUIDv7(time.Now(), 601), Name: "跨组织回执验收", MemberMembershipIDs: []string{targetM}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SendGroupTextMessage(ctx, publisher(), cross.ID, clientUUIDv7(time.Now(), 602), "跨组织群消息"); err != nil {
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
	var authorizations, exchanges atomic.Int64
	var idpIssuer atomic.Value
	idp := browserTestTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/keys":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(jwks))
		case "/choose":
			subject := r.URL.Query().Get("as")
			if subject != "browser-admin" && subject != "browser-bob" {
				http.Error(w, "invalid fixture subject", 400)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "receipt-fixture-subject", Value: subject, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			w.WriteHeader(204)
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
			subject := "browser-admin"
			if cookie, err := r.Cookie("receipt-fixture-subject"); err == nil && cookie.Value == "browser-bob" {
				subject = "browser-bob"
			}
			loginCodes.Store(code, []string{challenge, subject})
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
			if !ok || verifier == "" || base64.RawURLEncoding.EncodeToString(digest[:]) != challenge.([]string)[0] {
				http.Error(w, "invalid authorization code", http.StatusBadRequest)
				return
			}
			exchanges.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": productionJWT(t, key, idpIssuer.Load().(string), challenge.([]string)[1]), "token_type": "Bearer", "expires_in": 300,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(idp.Close)
	idpIssuer.Store(idp.URL)
	certFile := filepath.Join(t.TempDir(), "local-idp-ca.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	run(t, conn, `INSERT INTO external_identities (issuer,subject,tenant_id,user_id) VALUES ($1,'browser-admin',$2,$3),($1,'browser-bob',$2,$4)`,
		idp.URL, tenantA, adminA, personA)

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
	receiptStream := stream + ":receipts"
	t.Cleanup(func() { client.Del(ctx, receiptStream, receiptoutbox.PublisherPresenceKey(receiptStream)) })
	receiptWorker := fileProductCommand(productionTestBinary(t, "im-receipt-worker"), []string{"IM_DATABASE_URL=" + databaseURL, "IM_RECEIPT_REDIS_URL=" + os.Getenv("IM_TEST_REDIS_URL"), "IM_RECEIPT_STREAM=" + receiptStream})
	startRealtimeProcess(t, receiptWorker, func() bool { return client.Exists(ctx, receiptoutbox.PublisherPresenceKey(receiptStream)).Val() == 1 })
	worker := fileProductCommand(productionTestBinary(t, "im-outbox-worker"), []string{"IM_DATABASE_URL=" + databaseURL,
		"IM_OUTBOX_REDIS_URL=" + os.Getenv("IM_TEST_REDIS_URL"), "IM_OUTBOX_STREAM=" + stream})
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
		"IM_RECEIPTS_ENABLED=true", "IM_RECEIPT_STREAM=" + receiptStream, "IM_RECEIPT_WRITES_PER_SECOND=100",
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

	input := filepath.Join(t.TempDir(), "receipt-web.json")
	privateInput, _ := json.Marshal(map[string]string{"baseURL": proxy.URL, "idpURL": idp.URL, "direct": directA, "group": cross.ID, "adminMembership": adminM, "bobDirectMembership": targetM2, "bobGroupMembership": targetM})
	if err := os.WriteFile(input, privateInput, 0600); err != nil {
		t.Fatal(err)
	}
	browser := exec.Command(node, "../webclient/e2e/receipt_real_backend.cjs")
	browser.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	browser.Env = testfixtures.BrowserEnvironment(map[string]string{"WEB_FILE_INPUT": input, "IM_TEST_WEB_URL": proxy.URL, "NODE_PATH": os.Getenv("NODE_PATH"), "CHROMIUM_EXECUTABLE": os.Getenv("CHROMIUM_EXECUTABLE")})
	var output bytes.Buffer
	browser.Stdout, browser.Stderr = &output, &output
	lifecycle, startErr := testfixtures.StartIntegrationBrowser(browser, t.Name(), true)
	if startErr != nil {
		t.Fatal(startErr)
	}
	done := make(chan error, 1)
	go func() { done <- lifecycle.Wait() }()
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

	if authorizations.Load() < 4 || authorizations.Load() != exchanges.Load() {
		t.Fatal("actual PKCE flows incomplete", authorizations.Load(), exchanges.Load())
	}
	var devices, users int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM message_device_receipts WHERE tenant_id=$1 AND conversation_id=$2 AND seq=1 AND user_id=$3 AND read_at IS NOT NULL", tenantA, directA, personA).Scan(&devices); err != nil || devices != 3 {
		t.Fatal("three actual Bob device read facts including modal recovery", devices, err)
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts WHERE tenant_id=$1 AND conversation_id=$2 AND seq=1 AND user_id=$3 AND read_at IS NOT NULL", tenantA, directA, personA).Scan(&users); err != nil || users != 1 {
		t.Fatal("one Bob user read fact", users, err)
	}
	var groupRead bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM message_user_receipts WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3 AND read_at IS NOT NULL)", tenantA, cross.ID, personA).Scan(&groupRead); err != nil || !groupRead {
		t.Fatal("cross-org group read", groupRead, err)
	}
	var onlineRead int
	if e := conn.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts WHERE tenant_id=$1 AND conversation_id=$2 AND seq=2 AND user_id=$3 AND read_at IS NOT NULL", tenantA, cross.ID, personA).Scan(&onlineRead); e != nil || onlineRead != 1 {
		t.Fatal("zero-cache real user read", onlineRead, e)
	}
	t.Log("actual OIDC/PG/Redis browsers:", output.String())
}
