//go:build nativeclientintegration && (darwin || linux)

package policystore_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"github.com/leileipei/Enterprise_IM/internal/receiptoutbox"
	"github.com/leileipei/Enterprise_IM/internal/testfixtures"
	"github.com/redis/go-redis/v9"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeMessagingUIAgainstProductionAPI(t *testing.T) {
	for _, k := range []string{"IM_TEST_DATABASE_URL", "IM_TEST_REDIS_URL", "IM_TEST_NATIVE_DART", "IM_TEST_NATIVE_FLUTTER_SNAPSHOT", "IM_TEST_NATIVE_TESTER"} {
		if os.Getenv(k) == "" {
			t.Fatalf("explicit gate requires %s", k)
		}
	}
	ctx := context.Background()
	conn := db(t)
	seedDirectConversation(t, conn)
	grantPublisher(t, conn)
	for _, n := range []string{"000004_external_identities", "000026_native_oauth_rate"} {
		b, e := os.ReadFile("../../db/migrations/" + n + ".up.sql")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = conn.PgConn().Exec(ctx, string(b)).ReadAll(); e != nil {
			t.Fatal(e)
		}
	}
	var schema string
	if e := conn.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); e != nil {
		t.Fatal(e)
	}
	dsn := processDatabaseURL(t, schema)
	parsed, _ := url.Parse(dsn)
	run(t, conn, "GRANT DELETE ON native_oauth_request_windows TO "+pgx.Identifier{parsed.User.Username()}.Sanitize())
	version := int64(0)
	publish := func(deny bool) error {
		rules := []policy.Rule{}
		now := time.Now().UTC()
		for _, action := range []policy.Action{policy.ActionDirectoryView, policy.ActionStartChat, policy.ActionCreateGroup, policy.ActionSendMessage} {
			rules = append(rules, policy.Rule{ID: "native-ui-" + string(action), TenantID: tenantA, Effect: policy.EffectAllow, Action: action, SourceOrganizationID: orgA, TargetOrganizationID: orgA2, Bidirectional: true, RequestedBy: adminA, ApprovedBy: adminA, Reason: "owned UI fixture", EffectiveFrom: now.Add(-time.Hour), EffectiveTo: now.Add(time.Hour)})
		}
		if deny {
			rules = append(rules, policy.Rule{ID: "native-ui-deny", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionCreateGroup, SourceOrganizationID: orgA, TargetOrganizationID: orgA2, Bidirectional: true, Reason: "owned group denial", EffectiveFrom: now.Add(-time.Hour)})
		}
		_, e := (policystore.Service{DB: conn}).Publish(ctx, publisher(), version, rules, "owned native UI fixture")
		if e == nil {
			version++
		}
		return e
	}
	if e := publish(false); e != nil {
		t.Fatal(e)
	}
	options, err := redis.ParseURL(os.Getenv("IM_TEST_REDIS_URL"))
	if err != nil {
		t.Fatal(err)
	}
	redisClient := redis.NewClient(options)
	t.Cleanup(func() { _ = redisClient.Close() })
	stream := "enterprise-im:test:native-ui:" + schema
	receiptStream := stream + ":receipts"
	t.Cleanup(func() {
		if err := redisClient.Del(ctx, stream, receiptStream, outbox.PublisherPresenceKey(stream), receiptoutbox.PublisherPresenceKey(receiptStream)).Err(); err != nil {
			t.Error("Redis cleanup failed")
		}
	})
	worker := fileProductCommand(productionTestBinary(t, "im-outbox-worker"), []string{"IM_DATABASE_URL=" + dsn, "IM_OUTBOX_REDIS_URL=" + os.Getenv("IM_TEST_REDIS_URL"), "IM_OUTBOX_STREAM=" + stream})
	startRealtimeProcess(t, worker, func() bool { return redisClient.Exists(ctx, outbox.PublisherPresenceKey(stream)).Val() == 1 })
	receiptWorker := fileProductCommand(productionTestBinary(t, "im-receipt-worker"), []string{"IM_DATABASE_URL=" + dsn, "IM_RECEIPT_REDIS_URL=" + os.Getenv("IM_TEST_REDIS_URL"), "IM_RECEIPT_STREAM=" + receiptStream})
	startRealtimeProcess(t, receiptWorker, func() bool {
		return redisClient.Exists(ctx, receiptoutbox.PublisherPresenceKey(receiptStream)).Val() == 1
	})
	env, refresh := nativeMessagingRefreshIssuer(t, conn)
	upstream, _ := url.Parse(startProductionAPI(t, productionTestBinary(t, "im-api"), append(env, "IM_DATABASE_URL="+dsn, "IM_HTTP_ADDR=127.0.0.1:0", "IM_RECEIPTS_ENABLED=true", "IM_RECEIPT_WRITES_PER_SECOND=100", "IM_REALTIME_REDIS_URL="+os.Getenv("IM_TEST_REDIS_URL"), "IM_REALTIME_STREAM="+stream, "IM_RECEIPT_STREAM="+receiptStream)))
	var dropAck atomic.Bool
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	reverse.ModifyResponse = func(r *http.Response) error {
		t.Logf("OWNED_API method=%s path=%s status=%d", r.Request.Method, r.Request.URL.Path, r.StatusCode)
		if r.Request.Method == "POST" && r.Request.URL.Path == "/api/v1/conversations/"+directA+"/messages" && r.StatusCode == 200 && dropAck.CompareAndSwap(true, false) {
			r.Body.Close()
			return errors.New("owned ACK interruption")
		}
		return nil
	}
	reverse.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) {
		http.Error(w, "owned transport interruption", 502)
	}
	proxy, ca := nativeProbeTLS(t, reverse)
	var flutter atomic.Pointer[exec.Cmd]
	var runtimePID atomic.Int64
	key := freshFile().ID
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("X-Owned-Fixture") != key {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/runtime":
			var d struct {
				PID int `json:"runtime_pid"`
			}
			if json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&d) != nil {
				w.WriteHeader(400)
				return
			}
			proof, e := testfixtures.RegisterIntegrationBrowserChild(flutter.Load(), d.PID, os.Getenv("IM_TEST_NATIVE_TESTER"), os.Getenv("IM_TEST_INTEGRATION_GATE"), t.Name()+"/flutter_runtime")
			if e != nil {
				t.Error(e)
				w.WriteHeader(500)
				return
			}
			if e = proof.Ready(); e != nil {
				t.Error(e)
				w.WriteHeader(500)
				return
			}
			runtimePID.Store(int64(d.PID))
		case "/deny-group":
			if e := publish(true); e != nil {
				t.Error(e)
				w.WriteHeader(500)
				return
			}
		case "/allow-group":
			if e := publish(false); e != nil {
				t.Error(e)
				w.WriteHeader(500)
				return
			}
		case "/drop-ack":
			dropAck.Store(true)
		default:
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(204)
	}))
	t.Cleanup(control.Close)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if e := os.WriteFile(caPath, ca, 0600); e != nil {
		t.Fatal(e)
	}
	client := nativeProbeClientCopy(t, filepath.Join(dir, "client"))
	for _, part := range []string{"integration_test/messaging_test.dart", "integration_test/files_and_devices_test.dart", "test/native_messaging_backend_probe.dart", ".dart_tool/package_graph.json"} {
		b, e := os.ReadFile("../../clients/enterprise_im/" + part)
		if e != nil {
			t.Fatal(e)
		}
		target := filepath.Join(client, part)
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(target, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	input := map[string]any{"origin": proxy.URL, "ca": caPath, "storage": filepath.Join(dir, "storage"), "tenant": tenantA, "conversation": directA, "alice_user": adminA, "alice_membership": adminM, "alice_refresh": refresh["alice"], "bob_user": personA, "bob_membership": targetM2, "bob_group_membership": targetM, "bob_refresh": refresh["bob"], "control": control.URL, "control_key": key}
	b, _ := json.Marshal(input)
	config := filepath.Join(dir, "input.json")
	if e := os.WriteFile(config, b, 0600); e != nil {
		t.Fatal(e)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, os.Getenv("IM_TEST_NATIVE_DART"), os.Getenv("IM_TEST_NATIVE_FLUTTER_SNAPSHOT"), "test", "--no-pub", "--reporter", "expanded", "--dart-define=IM_MESSAGING_FIXTURE="+config, "test/native_messaging_backend_probe.dart")
	flutter.Store(cmd)
	cmd.Dir = client
	cmd.Env = processChildEnv(map[string]string{"HOME": os.Getenv("HOME"), "CI": "true", "FLUTTER_SUPPRESS_ANALYTICS": "true", "PUB_CACHE": os.Getenv("IM_TEST_NATIVE_PUB_CACHE")})
	log, e := os.CreateTemp(dir, "flutter-*.log")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { log.Close() })
	cmd.Stdout, cmd.Stderr = log, log
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	proof := trackFileProductProcess(t, cmd)
	if e = proof.Ready(); e != nil {
		t.Fatal(e)
	}
	e = cmd.Wait()
	output, _ := os.ReadFile(log.Name())
	if e != nil {
		t.Fatalf("real messaging UI failed: %v\n%s", e, output)
	}
	if runtimePID.Load() == 0 {
		t.Fatal("runtime not registered")
	}
	if b, e := exec.Command("/bin/ps", "-p", fmt.Sprint(runtimePID.Load()), "-o", "stat=").Output(); e == nil && strings.TrimSpace(string(b)) != "" {
		t.Fatal("runtime still present")
	}
	var report map[string]any
	for _, line := range strings.Split(string(output), "\n") {
		if p := strings.Index(line, "NATIVE_MESSAGING_REPORT "); p >= 0 {
			if e = json.Unmarshal([]byte(line[p+len("NATIVE_MESSAGING_REPORT "):]), &report); e != nil {
				t.Fatal(e)
			}
		}
	}
	if report == nil || report["status"] != "passed" || report["messages"] != float64(3) || report["denied_group"] != true || report["cross_organization_group"] != true || report["lost_ack_retry"] != true || report["visible_read"] != true || report["left"] != true {
		t.Fatal("complete UI report missing")
	}
	var directMessages, groupMessages, groups, read, left, intact int
	if e = conn.QueryRow(ctx, "SELECT count(*) FILTER (WHERE conversation_id=$2),count(*) FILTER (WHERE conversation_id<>$2) FROM messages WHERE tenant_id=$1", tenantA, directA).Scan(&directMessages, &groupMessages); e != nil {
		t.Fatal(e)
	}
	if e = conn.QueryRow(ctx, "SELECT count(*) FROM conversations WHERE tenant_id=$1 AND kind='group'", tenantA).Scan(&groups); e != nil {
		t.Fatal(e)
	}
	if e = conn.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts WHERE tenant_id=$1 AND user_id=$2 AND read_at IS NOT NULL", tenantA, personA).Scan(&read); e != nil {
		t.Fatal(e)
	}
	if e = conn.QueryRow(ctx, "SELECT count(*) FILTER (WHERE status='left'),count(*) FILTER (WHERE status='active' AND role='owner') FROM conversation_membership_intervals WHERE tenant_id=$1", tenantA).Scan(&left, &intact); e != nil {
		t.Fatal(e)
	}
	if directMessages != 2 || groupMessages != 1 || groups != 1 || read != 3 || left != 1 || intact != 1 {
		t.Fatalf("independent facts direct=%d groupMessages=%d groups=%d read=%d left=%d owner=%d", directMessages, groupMessages, groups, read, left, intact)
	}
	t.Log("NATIVE_MESSAGING_LIFECYCLE runtime_registered=true runtime_absent_after_parent_wait=true runtime_os_exit_independently_observed=false")
	t.Log("NATIVE_MESSAGING_DB direct_messages=2 group_messages=1 groups=1 user_reads=3 left=1 active_owner=1")
}

func nativeMessagingRefreshIssuer(t *testing.T, conn *pgx.Conn) ([]string, map[string]string) {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"test-key","n":"%s","e":"%s"}]}`, base64.RawURLEncoding.EncodeToString(key.N.Bytes()), base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()))
	var issuer string
	var mutex sync.Mutex
	refresh := map[string]string{"alice": freshFile().ID, "bob": freshFile().ID}
	initial := map[string]string{"alice": refresh["alice"], "bob": refresh["bob"]}
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/keys" {
			io.WriteString(w, jwks)
			return
		}
		if r.URL.Path != "/token" || r.Method != "POST" || r.ParseForm() != nil {
			http.NotFound(w, r)
			return
		}
		mutex.Lock()
		defer mutex.Unlock()
		name := ""
		for who, token := range refresh {
			if token == r.Form.Get("refresh_token") {
				name = who
			}
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != "im-macos" || name == "" || r.Form.Get("client_secret") != "" {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		claims := jwt.MapClaims{"iss": issuer, "sub": "native-messaging-" + name, "aud": "enterprise-im-api", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Add(-time.Minute).Unix(), "jti": freshFile().ID, "client_id": "im-macos"}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "test-key"
		token.Header["typ"] = "at+jwt"
		access, err := token.SignedString(key)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		refresh[name] = freshFile().ID
		json.NewEncoder(w).Encode(map[string]any{"access_token": access, "token_type": "Bearer", "refresh_token": refresh[name]})
	}))
	issuer = idp.URL
	t.Cleanup(idp.Close)
	ca := filepath.Join(t.TempDir(), "idp-ca.pem")
	if e = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	run(t, conn, "INSERT INTO external_identities(issuer,subject,tenant_id,user_id) VALUES($1,'native-messaging-alice',$2,$3),($1,'native-messaging-bob',$2,$4)", issuer, tenantA, adminA, personA)
	env := []string{"SSL_CERT_FILE=" + ca, "IM_OIDC_ENABLED=true", "IM_OIDC_ISSUER=" + issuer, "IM_OIDC_AUDIENCE=enterprise-im-api", "IM_OIDC_JWKS_URL=" + issuer + "/keys", "IM_OIDC_ALLOWED_CLIENT_IDS=im-windows,im-macos,im-android,im-ios", "IM_NATIVE_ENABLED=true", "IM_NATIVE_AUTHORIZATION_URL=" + issuer + "/authorize", "IM_NATIVE_TOKEN_URL=" + issuer + "/token"}
	for _, p := range []string{"windows", "macos", "android", "ios"} {
		prefix := "IM_NATIVE_" + strings.ToUpper(p) + "_"
		redirect := "com.enterpriseim.client:/oauth/callback"
		if p == "windows" || p == "macos" {
			redirect = "http://127.0.0.1/oauth/callback"
		}
		env = append(env, prefix+"CLIENT_ID=im-"+p, prefix+"REDIRECT_URI="+redirect, prefix+"REFRESH_ENABLED=true")
	}
	return env, initial
}
