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
	"github.com/leileipei/Enterprise_IM/internal/testfixtures"
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

// The real backend, MinIO and bound ClamAV worker are never replaced. The host
// Flutter runner uses an in-memory vault; installed native plugins are a separate gate.
func TestNativeFilesAndDevicesAgainstProductionAPI(t *testing.T) {
	for _, k := range []string{"IM_TEST_DATABASE_URL", "IM_TEST_NATIVE_DART", "IM_TEST_NATIVE_FLUTTER_SNAPSHOT", "IM_TEST_INTEGRATION_PROBE_VERSION"} {
		if os.Getenv(k) == "" {
			t.Fatalf("explicit gate requires %s", k)
		}
	}
	ctx := context.Background()
	conn := db(t)
	seedDirectConversation(t, conn)
	runtimePolicyEdit(t, conn)
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
	env, refresh := nativeFilesRefreshIssuer(t, conn)
	storage, _ := url.Parse(os.Getenv("IM_TEST_S3_ENDPOINT"))
	var scanFault, downloadFault atomic.Bool
	makeGateway := func(fault *atomic.Bool) *httptest.Server {
		p := httputil.NewSingleHostReverseProxy(storage)
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fault.Load() && r.Method == "GET" && strings.Contains(r.URL.Path, "/files/") {
				http.Error(w, "owned storage outage", 503)
				return
			}
			p.ServeHTTP(w, r)
		}))
		t.Cleanup(s.Close)
		return s
	}
	workerObjects := makeGateway(&scanFault)
	apiObjects := makeGateway(&downloadFault)
	apiEnv := append(fileRuntimeEnv(t, dsn, t.TempDir()+"/upload"), env...)
	apiEnv = append(apiEnv, "IM_HTTP_ADDR=127.0.0.1:0", "IM_RECEIPTS_ENABLED=true", "IM_FILE_UPLOAD_ENABLED=true", "IM_FILE_BUSINESS_ENABLED=true", "IM_FILE_S3_ENDPOINT="+apiObjects.URL, "IM_FILE_DOWNLOAD_S3_ACCESS_KEY="+os.Getenv("IM_TEST_FILE_DOWNLOAD_ACCESS_KEY"), "IM_FILE_DOWNLOAD_S3_SECRET_KEY="+os.Getenv("IM_TEST_FILE_DOWNLOAD_SECRET_KEY"), "IM_FILE_DOWNLOAD_SPOOL_DIR="+t.TempDir()+"/download", "IM_FILE_DOWNLOAD_OWNER_ID="+freshFile().ID, "IM_FILE_READ_PROBE_VERSION_ID="+os.Getenv("IM_TEST_INTEGRATION_PROBE_VERSION"))
	upstream, _ := url.Parse(startProductionAPI(t, productionTestBinary(t, "im-api"), apiEnv))
	proxy, ca := nativeProbeTLS(t, httputil.NewSingleHostReverseProxy(upstream))
	workerEnv := append(fileRuntimeEnv(t, dsn, t.TempDir()+"/worker"), "IM_FILE_S3_ENDPOINT="+workerObjects.URL, "IM_FILE_WORKER_ENABLED=true", "IM_FILE_WORKER_ID="+freshFile().ID, "IM_FILE_QPDF_PATH="+os.Getenv("IM_TEST_QPDF_PATH"), "IM_FILE_CLAMD_SOCKET="+os.Getenv("IM_TEST_CLAMD_SOCKET"), "IM_FILE_SCANNER_MANIFEST="+os.Getenv("IM_TEST_SCANNER_MANIFEST"), "IM_FILE_S3_ACCESS_KEY="+os.Getenv("IM_TEST_FILE_WORKER_ACCESS_KEY"), "IM_FILE_S3_SECRET_KEY="+os.Getenv("IM_TEST_FILE_WORKER_SECRET_KEY"))
	startFileWorkerProcess(t, productionTestBinary(t, "im-file-worker"), workerEnv)
	// A terminal download is intentionally blocked until the official repair
	// process writes and acknowledges its machine audit. Never mutate that flag.
	repairLog, e := os.CreateTemp(t.TempDir(), "audit-repair-*.log")
	if e != nil {
		t.Fatal(e)
	}
	repair := fileProductCommand(productionTestBinary(t, "im-file-cleaner"), []string{"IM_DATABASE_URL=" + dsn})
	repair.Args = append(repair.Args, "--execute", "--repair-only")
	repair.Stdout, repair.Stderr = repairLog, repairLog
	if e = repair.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { repairLog.Close() })
	repairProof := trackFileProductProcess(t, repair)
	deadline := time.Now().Add(20 * time.Second)
	for {
		b, _ := os.ReadFile(repairLog.Name())
		if strings.Contains(string(b), `"status":"file_download_audit_repair"`) {
			if e = repairProof.Ready(); e != nil {
				t.Fatal(e)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("official audit repair did not start: %s", b)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var flutter atomic.Pointer[exec.Cmd]
	var runtimePID atomic.Int64
	controlKey := freshFile().ID
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("X-Owned-Fixture") != controlKey {
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
				t.Error("runtime identity registration failed", e)
				w.WriteHeader(500)
				return
			}
			if e = proof.Ready(); e != nil {
				t.Error(e)
				w.WriteHeader(500)
				return
			}
			runtimePID.Store(int64(d.PID))

		case "/scan-fault":
			scanFault.Store(true)
		case "/scan-healthy":
			scanFault.Store(false)
		case "/download-fault":
			downloadFault.Store(true)
		case "/download-healthy":
			downloadFault.Store(false)
		case "/settled":
			deadline := time.Now().Add(15 * time.Second)
			for {
				var pending int
				e := conn.QueryRow(r.Context(), "SELECT count(*) FROM file_download_sessions WHERE tenant_id=$1 AND NOT audit_acked", tenantA).Scan(&pending)
				if e != nil || time.Now().After(deadline) {
					w.WriteHeader(500)
					return
				}
				if pending == 0 {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		case "/expire":
			var d struct {
				ID string `json:"file_id"`
			}
			if json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&d) != nil {
				w.WriteHeader(400)
				return
			}
			tag, e := conn.Exec(ctx, "UPDATE messages m SET accepted_at=clock_timestamp()-(p.file_retention_days+1)*interval '24 hours' FROM message_attachments a,tenant_file_retention_policy p WHERE m.tenant_id=$1 AND a.tenant_id=m.tenant_id AND a.file_id=$2 AND a.message_id=m.id AND p.tenant_id=m.tenant_id", tenantA, d.ID)
			if e != nil || tag.RowsAffected() != 1 {
				w.WriteHeader(500)
				return
			}
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
	for _, part := range []string{"integration_test/files_and_devices_test.dart", "test/native_files_backend_probe.dart", ".dart_tool/package_graph.json"} {
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
	input := map[string]any{"origin": proxy.URL, "ca": caPath, "storage": filepath.Join(dir, "storage"), "tenant": tenantA, "user": adminA, "membership": adminM, "conversation": directA, "refresh": refresh, "control": control.URL, "control_key": controlKey}
	b, _ := json.Marshal(input)
	config := filepath.Join(dir, "input.json")
	if e := os.WriteFile(config, b, 0600); e != nil {
		t.Fatal(e)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, os.Getenv("IM_TEST_NATIVE_DART"), os.Getenv("IM_TEST_NATIVE_FLUTTER_SNAPSHOT"), "test", "--no-pub", "--reporter", "expanded", "--dart-define=IM_FILES_FIXTURE="+config, "test/native_files_backend_probe.dart")
	flutter.Store(cmd)
	cmd.Dir = client
	cmd.Env = processChildEnv(map[string]string{"HOME": os.Getenv("HOME"), "CI": "true", "FLUTTER_SUPPRESS_ANALYTICS": "true", "PUB_CACHE": os.Getenv("IM_TEST_NATIVE_PUB_CACHE")})
	logPath := filepath.Join(dir, "flutter.log")
	log, e := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { log.Close() })
	cmd.Stdout = log
	cmd.Stderr = log
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	registration := trackFileProductProcess(t, cmd)
	if e = registration.Ready(); e != nil {
		t.Fatal(e)
	}
	e = cmd.Wait()
	output, _ := os.ReadFile(logPath)
	if e != nil {
		t.Fatalf("Flutter real files probe failed: %v\n%s", e, output)
	}
	if runtimePID.Load() == 0 {
		t.Fatal("native test runtime never registered")
	}
	if b, e := exec.Command("/bin/ps", "-p", fmt.Sprint(runtimePID.Load()), "-o", "stat=").Output(); e == nil && strings.TrimSpace(string(b)) != "" {
		t.Fatal("Flutter runtime still present after parent Wait")
	}
	// Parent Wait is observed. Native tester absence is audited, but its OS exit
	// code was reaped by Flutter tooling; never invent an independent exit code.
	var report map[string]any
	for _, line := range strings.Split(string(output), "\n") {
		if p := strings.Index(line, "NATIVE_FILES_REPORT "); p >= 0 {
			if e = json.Unmarshal([]byte(line[p+len("NATIVE_FILES_REPORT "):]), &report); e != nil {
				t.Fatal(e)
			}
		}
	}
	if report == nil || report["status"] != "passed" || report["clean"] != true || report["eicar"] != true || report["scan_failed"] != true || report["download_failed"] != true || report["expired"] != true || report["revoked"] != true {
		t.Fatal("complete real files report missing")
	}
	var ready, rejected, failed, bound, revoked int
	if e = conn.QueryRow(ctx, "SELECT count(*) FILTER (WHERE state='ready'),count(*) FILTER (WHERE state='rejected'),count(*) FILTER (WHERE state='scan_failed') FROM file_objects WHERE tenant_id=$1", tenantA).Scan(&ready, &rejected, &failed); e != nil {
		t.Fatal(e)
	}
	if e = conn.QueryRow(ctx, "SELECT count(*) FROM message_attachments WHERE tenant_id=$1", tenantA).Scan(&bound); e != nil {
		t.Fatal(e)
	}
	if e = conn.QueryRow(ctx, "SELECT count(*) FROM user_devices WHERE tenant_id=$1 AND user_id=$2 AND revoked_at IS NOT NULL", tenantA, adminA).Scan(&revoked); e != nil {
		t.Fatal(e)
	}
	if ready != 1 || rejected != 1 || failed != 1 || bound != 1 || revoked != 1 {
		t.Fatalf("independent facts ready=%d rejected=%d failed=%d bound=%d revoked=%d", ready, rejected, failed, bound, revoked)
	}
	var sealed int
	if e = conn.QueryRow(ctx, "SELECT count(*) FROM file_objects WHERE tenant_id=$1 AND state='ready' AND scan_sha256=sha256 AND scan_engine IS NOT NULL AND scan_definition_version IS NOT NULL AND scanned_at IS NOT NULL", tenantA).Scan(&sealed); e != nil || sealed != 1 {
		t.Fatal("scanner provenance missing", e)
	}
	t.Logf("NATIVE_FILES_LIFECYCLE runtime_registered=true runtime_absent_after_parent_wait=true runtime_os_exit_independently_observed=false")
	t.Logf("NATIVE_FILES_DB ready=%d rejected=%d scan_failed=%d bound=%d revoked=%d scanner_sealed=%d", ready, rejected, failed, bound, revoked, sealed)
	t.Log("real Flutter host files/devices: native refresh/me and admission; ready/rejected/scan_failed; streamed download/fresh export/expiry; own revoke; independent DB facts")
}

func nativeFilesRefreshIssuer(t *testing.T, conn *pgx.Conn) ([]string, string) {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"test-key","n":"%s","e":"%s"}]}`, base64.RawURLEncoding.EncodeToString(key.N.Bytes()), base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()))
	var issuer string
	var mutex sync.Mutex
	refresh := freshFile().ID
	initial := refresh
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
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != "im-macos" || r.Form.Get("refresh_token") != refresh || r.Form.Get("client_secret") != "" {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		claims := jwt.MapClaims{"iss": issuer, "sub": "native-files-admin", "aud": "enterprise-im-api", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Add(-time.Minute).Unix(), "jti": freshFile().ID, "client_id": "im-macos"}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "test-key"
		token.Header["typ"] = "at+jwt"
		access, err := token.SignedString(key)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		refresh = freshFile().ID
		json.NewEncoder(w).Encode(map[string]any{"access_token": access, "token_type": "Bearer", "refresh_token": refresh})
	}))
	issuer = idp.URL
	t.Cleanup(idp.Close)
	ca := filepath.Join(t.TempDir(), "idp-ca.pem")
	if e = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	run(t, conn, "INSERT INTO external_identities(issuer,subject,tenant_id,user_id) VALUES($1,'native-files-admin',$2,$3)", issuer, tenantA, adminA)
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
