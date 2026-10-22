//go:build nativeclientintegration && (darwin || linux)

package policystore_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/leileipei/Enterprise_IM/internal/receiptoutbox"
	"github.com/redis/go-redis/v9"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCrossDeviceBackendFixture(t *testing.T) {
	for _, key := range []string{"IM_TEST_DATABASE_URL", "IM_TEST_REDIS_URL", "IM_TEST_MOBILE_CONFIG", "IM_TEST_MOBILE_FACTS", "IM_TEST_MOBILE_STOP"} {
		if os.Getenv(key) == "" {
			t.Fatalf("explicit cross-process fixture requires %s", key)
		}
	}
	ctx := context.Background()
	conn := db(t)
	seedDirectConversation(t, conn)
	freshLogin := os.Getenv("IM_TEST_CROSS_FRESH_LOGIN") == "1"
	logoutNavigation := os.Getenv("IM_TEST_CROSS_LOGOUT_NAVIGATION") == "1"
	logoutRace := os.Getenv("IM_TEST_CROSS_LOGOUT_RACE") == "1"
	permissionChange := os.Getenv("IM_TEST_CROSS_PERMISSION_CHANGE") == "1"
	if permissionChange {
		grantPublisher(t, conn)
	}
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
	options, e := redis.ParseURL(os.Getenv("IM_TEST_REDIS_URL"))
	if e != nil {
		t.Fatal(e)
	}
	cache := redis.NewClient(options)
	t.Cleanup(func() { _ = cache.Close() })
	stream := "enterprise-im:test:mobile:" + schema
	receipts := stream + ":receipts"
	t.Cleanup(func() {
		if e := cache.Del(ctx, stream, receipts, outbox.PublisherPresenceKey(stream), receiptoutbox.PublisherPresenceKey(receipts)).Err(); e != nil {
			t.Error("owned Redis cleanup failed")
		}
	})
	ackRestart := os.Getenv("IM_TEST_CROSS_ACK_RESTART") == "1"
	startRealtime := startRealtimeProcess
	startAPI := startProductionAPI
	receiptRetry := os.Getenv("IM_TEST_CROSS_RECEIPT_RETRY") == "1"
	if ackRestart || receiptRetry || permissionChange || logoutRace || logoutNavigation || freshLogin {
		startRealtime = crossRetainedRealtime
		startAPI = crossRetainedAPI
	}
	worker := fileProductCommand(productionTestBinary(t, "im-outbox-worker"), []string{"IM_DATABASE_URL=" + dsn, "IM_OUTBOX_REDIS_URL=" + os.Getenv("IM_TEST_REDIS_URL"), "IM_OUTBOX_STREAM=" + stream})
	startRealtime(t, worker, func() bool { return cache.Exists(ctx, outbox.PublisherPresenceKey(stream)).Val() == 1 })
	receiptWorker := fileProductCommand(productionTestBinary(t, "im-receipt-worker"), []string{"IM_DATABASE_URL=" + dsn, "IM_RECEIPT_REDIS_URL=" + os.Getenv("IM_TEST_REDIS_URL"), "IM_RECEIPT_STREAM=" + receipts})
	startRealtime(t, receiptWorker, func() bool { return cache.Exists(ctx, receiptoutbox.PublisherPresenceKey(receipts)).Val() == 1 })
	// Real broker refresh uses the platform-bound synthetic identity issuer.
	var env []string
	var refresh map[string]string
	var authorize http.Handler
	if freshLogin {
		env, refresh, authorize = crossLoginIssuer(t, conn)
	} else {
		env, refresh = crossRefreshIssuer(t, conn)
	}
	upstream, _ := url.Parse(startAPI(t, productionTestBinary(t, "im-api"), append(env, "IM_DATABASE_URL="+dsn, "IM_HTTP_ADDR=127.0.0.1:0", "IM_RECEIPTS_ENABLED=true", "IM_RECEIPT_WRITES_PER_SECOND=100", "IM_REALTIME_REDIS_URL="+os.Getenv("IM_TEST_REDIS_URL"), "IM_REALTIME_STREAM="+stream, "IM_RECEIPT_STREAM="+receipts)))

	var mu sync.Mutex
	events := []map[string]any{}
	eventPath := filepath.Join(filepath.Dir(os.Getenv("IM_TEST_MOBILE_FACTS")), "backend-api-events.json")
	saveEvents := func() {
		b, e := json.MarshalIndent(events, "", "  ")
		if e == nil {
			e = os.WriteFile(eventPath+".new", b, 0600)
		}
		if e == nil {
			e = os.Rename(eventPath+".new", eventPath)
		}
		if e != nil {
			t.Error("cross event persistence failed", e)
		}
	}
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	seenACK := map[string]bool{}
	reverse.ModifyResponse = func(r *http.Response) error {
		mu.Lock()
		defer mu.Unlock()
		event := map[string]any{"n": len(events) + 1, "method": r.Request.Method, "path": r.Request.URL.Path, "status": r.StatusCode, "device": r.Request.Header.Get("X-IM-Device-ID"), "device_headers": r.Request.Header.Get("X-IM-Device-ID") != "" && r.Request.Header.Get("X-IM-Device-Token") != ""}
		if meta, ok := r.Request.Context().Value(crossReceiptContextKey{}).(map[string]any); ok {
			for k, v := range meta {
				event[k] = v
			}
			event["upstream_status"] = r.StatusCode
		}
		drop := false
		if ackRestart && r.Request.Method == "POST" && r.Request.URL.Path == "/api/v1/conversations/"+directA+"/messages" && r.StatusCode == 200 {
			ack, lost, err := crossObserveACK(r, r.Request.Header.Get("X-IM-Device-ID"), seenACK)
			if err != nil {
				return err
			}
			drop = lost
			event["ack"] = ack
			event["upstream_status"] = r.StatusCode
			event["downstream_dropped"] = drop
		}
		events = append(events, event)
		saveEvents()
		if drop {
			panic(http.ErrAbortHandler)
		}
		return nil
	}
	var handler http.Handler = reverse
	if receiptRetry {
		control := filepath.Join(filepath.Dir(os.Getenv("IM_TEST_MOBILE_CONFIG")), "receipt-control.json")
		if err := os.WriteFile(control, []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		handler = crossReceiptGate(reverse, control, directA, time.Now(), func(event map[string]any) {
			mu.Lock()
			defer mu.Unlock()
			event["n"] = len(events) + 1
			events = append(events, event)
			saveEvents()
		})
	}
	if logoutRace {
		control := filepath.Join(filepath.Dir(os.Getenv("IM_TEST_MOBILE_CONFIG")), "logout-control.json")
		if err := os.WriteFile(control, []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		handler = crossLogoutGate(reverse, control, directA, func(event map[string]any) {
			mu.Lock()
			defer mu.Unlock()
			event["n"] = len(events) + 1
			events = append(events, event)
			saveEvents()
		})
	}
	if freshLogin {
		next := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/fixture/authorize" {
				authorize.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	proxy, ca := nativeProbeTLS(t, handler)
	actors := map[string]any{}
	for role, platform := range map[string]string{"alice": "android", "bob_ios": "ios", "bob_android": "android"} {
		user, membership := personA, targetM2
		if role == "alice" {
			user, membership = adminA, adminM
		}
		actors[role] = map[string]any{"platform": platform, "user": user, "membership": membership, "refresh": refresh[role]}
	}
	input := map[string]any{"origin": proxy.URL, "ca_pem": string(ca), "tenant": tenantA, "conversation": directA, "schema": schema, "actors": actors}
	if permissionChange {
		input["permission_other_membership"] = targetM
	}
	b, e := json.Marshal(input)
	if e != nil {
		t.Fatal(e)
	}
	path := os.Getenv("IM_TEST_MOBILE_CONFIG")
	if e = os.WriteFile(path+".tmp", b, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(path+".tmp", path); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(18 * time.Minute)
	for {
		if _, e = os.Stat(os.Getenv("IM_TEST_MOBILE_STOP")); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cross-process scenario deadline")
		}
		time.Sleep(100 * time.Millisecond)
	}
	var messages, devices, delivered, reads, userReads, aliceSends, bobSends, minSeq, maxSeq, distinctIDs int
	queries := []struct {
		sql  string
		dest []any
	}{
		{"SELECT count(*),COALESCE(min(seq),0),COALESCE(max(seq),0),count(DISTINCT client_msg_id),count(*) FILTER(WHERE sender_user_id='" + adminA + "'),count(*) FILTER(WHERE sender_user_id='" + personA + "') FROM messages", []any{&messages, &minSeq, &maxSeq, &distinctIDs, &aliceSends, &bobSends}},
		{"SELECT count(*) FROM user_devices", []any{&devices}},
		{"SELECT count(*),count(*) FILTER(WHERE read_at IS NOT NULL) FROM message_device_receipts", []any{&delivered, &reads}},
		{"SELECT count(*) FROM message_user_receipts WHERE read_at IS NOT NULL", []any{&userReads}},
	}
	for _, q := range queries {
		if e = conn.QueryRow(ctx, q.sql).Scan(q.dest...); e != nil {
			t.Fatal(e)
		}
	}
	facts := map[string]any{"messages": messages, "devices": devices, "device_delivered": delivered, "device_read": reads, "user_read": userReads, "alice_sends": aliceSends, "bob_sends": bobSends, "min_seq": minSeq, "max_seq": maxSeq, "distinct_client_ids": distinctIDs}
	b, _ = json.MarshalIndent(facts, "", "  ")
	if e = os.WriteFile(os.Getenv("IM_TEST_MOBILE_FACTS"), b, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(filepath.Dir(path), "scenario-done")); e != nil {
		t.Fatal("cross scenario stopped before all checkpoints")
	}
	wantMessages, wantReceipts, wantUserReads, wantAliceSends := 3, 5, 3, 2
	if permissionChange {
		wantMessages, wantReceipts, wantUserReads, wantAliceSends = 2, 3, 2, 1
	}
	if logoutRace {
		wantMessages, wantReceipts, wantUserReads, wantAliceSends = 5, 4, 3, 4
	}
	if logoutNavigation {
		wantMessages, wantReceipts, wantUserReads, wantAliceSends = 4, 4, 3, 3
	}
	wantDevices, wantBobSends := 3, 1
	if os.Getenv("IM_TEST_CROSS_RELOGIN") == "1" {
		wantMessages, wantReceipts, wantUserReads, wantAliceSends, wantDevices, wantBobSends = 5, 11, 5, 3, 5, 2
	}
	if os.Getenv("IM_TEST_CROSS_RELOGIN_RESTART") == "1" {
		wantMessages, wantReceipts, wantUserReads, wantAliceSends, wantDevices, wantBobSends = 8, 15, 8, 4, 5, 4
	}
	if messages != wantMessages || devices != wantDevices || delivered != wantReceipts || reads != wantReceipts || userReads != wantUserReads || aliceSends != wantAliceSends || bobSends != wantBobSends || minSeq != 1 || maxSeq != wantMessages || distinctIDs != wantMessages {
		t.Fatalf("cross final database mismatch: %s", b)
	}
	mu.Lock()
	defer mu.Unlock()
	saveEvents()
	for _, event := range events {
		if event["method"] == "POST" && event["path"] == "/api/v1/conversations/"+directA+"/messages" && event["status"] == 200 && event["device_headers"] != true {
			t.Fatal("cross native sender omitted device credentials")
		}
	}
	t.Log("CROSS_PROCESS_FINAL independent database facts passed")
}

func crossRefreshIssuer(t *testing.T, conn *pgx.Conn) ([]string, map[string]string) {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"cross-key","n":"%s","e":"%s"}]}`, base64.RawURLEncoding.EncodeToString(key.N.Bytes()), base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()))
	platforms := map[string]string{"alice": "android", "bob_ios": "ios", "bob_android": "android"}
	tokens := map[string]string{}
	initial := map[string]string{}
	for role := range platforms {
		tokens[role] = freshFile().ID
		initial[role] = tokens[role]
	}
	var issuer string
	var mu sync.Mutex
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/keys" {
			io.WriteString(w, jwks)
			return
		}
		if r.URL.Path != "/token" || r.Method != "POST" || r.ParseForm() != nil {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		role := ""
		for name, token := range tokens {
			if token == r.Form.Get("refresh_token") {
				role = name
			}
		}
		platform := platforms[role]
		if role == "" || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != "im-"+platform || r.Form.Get("client_secret") != "" {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		subject := "native-cross-bob"
		if role == "alice" {
			subject = "native-cross-alice"
		}
		claims := jwt.MapClaims{"iss": issuer, "sub": subject, "aud": "enterprise-im-api", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Add(-time.Minute).Unix(), "jti": freshFile().ID, "client_id": "im-" + platform}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "cross-key"
		token.Header["typ"] = "at+jwt"
		access, e := token.SignedString(key)
		if e != nil {
			w.WriteHeader(500)
			return
		}
		tokens[role] = freshFile().ID
		json.NewEncoder(w).Encode(map[string]any{"access_token": access, "token_type": "Bearer", "refresh_token": tokens[role]})
	}))
	issuer = server.URL
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "cross-idp.pem")
	if e = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	run(t, conn, "INSERT INTO external_identities(issuer,subject,tenant_id,user_id) VALUES($1,'native-cross-alice',$2,$3),($1,'native-cross-bob',$2,$4)", issuer, tenantA, adminA, personA)
	env := []string{"SSL_CERT_FILE=" + ca, "IM_OIDC_ENABLED=true", "IM_OIDC_ISSUER=" + issuer, "IM_OIDC_AUDIENCE=enterprise-im-api", "IM_OIDC_JWKS_URL=" + issuer + "/keys", "IM_OIDC_ALLOWED_CLIENT_IDS=im-windows,im-macos,im-android,im-ios", "IM_NATIVE_ENABLED=true", "IM_NATIVE_AUTHORIZATION_URL=" + issuer + "/authorize", "IM_NATIVE_TOKEN_URL=" + issuer + "/token"}
	for _, platform := range []string{"windows", "macos", "android", "ios"} {
		prefix := "IM_NATIVE_" + strings.ToUpper(platform) + "_"
		redirect := "com.enterpriseim.client:/oauth/callback"
		if platform == "windows" || platform == "macos" {
			redirect = "http://127.0.0.1/oauth/callback"
		}
		env = append(env, prefix+"CLIENT_ID=im-"+platform, prefix+"REDIRECT_URI="+redirect, prefix+"REFRESH_ENABLED=true")
	}
	return env, initial
}
