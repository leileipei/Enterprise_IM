package policystore_test

// These fixtures use official binaries and real dependencies. The two HTTP
// proxies forward bytes and may inject transport failures; neither mounts a
// business handler, replaces a repository, or supplies a successful response.
import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
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
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/leileipei/Enterprise_IM/internal/testfixtures"
	"github.com/redis/go-redis/v9"
)

type nodeRuntimeConfig struct{ URL, OwnerID, SpoolDir string }
type fileBusinessProcessFixture struct {
	privateRoot, apiURL, webURL, buildSHA, schema                              string
	binaries                                                                   map[string]string
	processes                                                                  map[string]*exec.Cmd
	integrationProcesses                                                       map[string]*testfixtures.IntegrationProcess
	pool                                                                       *pgxpool.Pool
	oidc, proxy                                                                *httptest.Server
	nodes                                                                      map[string]nodeRuntimeConfig
	conn                                                                       *pgx.Conn
	waits                                                                      map[string]chan error
	logs                                                                       map[string]*os.File
	apiDSN, repairDSN, adminDSN, apiRole, repairRole, ca, probeVersion, stream string
	key                                                                        *rsa.PrivateKey
	backend                                                                    atomic.Pointer[httputil.ReverseProxy]
	dbRelay                                                                    *processDBRelay
	objectGateway                                                              *httptest.Server
	objectFault                                                                atomic.Int32
	objectWrites                                                               atomic.Int64
	objectRequests                                                             atomic.Int64
	webFaultCounts                                                             [6]atomic.Int64
	objectGate                                                                 atomic.Pointer[processObjectGate]
	client                                                                     *http.Client
	redis                                                                      *redis.Client
	browserSubject                                                             atomic.Value
	expectedExitCodes                                                          map[string]int
	processPIDs                                                                map[string]int
}

var fileBusinessBuild struct {
	sync.Once
	root     string
	binaries map[string]string
	sha      string
	err      error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 && fileBusinessBuild.root != "" {
		_ = os.RemoveAll(fileBusinessBuild.root)
	}
	os.Exit(code)
}
func processPrivateFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if e := os.WriteFile(path, b, 0600); e != nil {
		t.Fatal("private fixture write failed")
	}
}
func processRandom(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		t.Fatal(e)
	}
	return hex.EncodeToString(b[:])
}
func requireBusinessProcess(t *testing.T) {
	t.Helper()
	for _, key := range []string{"IM_TEST_DATABASE_URL", "IM_TEST_REDIS_URL", "IM_TEST_S3_ENDPOINT", "IM_TEST_S3_BUCKET", "IM_TEST_S3_POLICY_BUCKET", "IM_TEST_FILE_UPLOAD_ACCESS_KEY", "IM_TEST_FILE_UPLOAD_SECRET_KEY", "IM_TEST_FILE_WORKER_ACCESS_KEY", "IM_TEST_FILE_WORKER_SECRET_KEY", "IM_TEST_FILE_DOWNLOAD_ACCESS_KEY", "IM_TEST_FILE_DOWNLOAD_SECRET_KEY", "IM_FILE_CLEANUP_S3_ACCESS_KEY", "IM_FILE_CLEANUP_S3_SECRET_KEY", "IM_TEST_FILE_BOOTSTRAP_ACCESS_KEY", "IM_TEST_FILE_BOOTSTRAP_SECRET_KEY", "IM_TEST_QPDF_PATH", "IM_TEST_CLAMD_SOCKET", "IM_TEST_SCANNER_MANIFEST", "IM_TEST_BROWSER_NODE", "CHROMIUM_EXECUTABLE"} {
		if os.Getenv(key) == "" {
			t.Fatal("required process fixture missing", key)
		}
	}
	d, e := url.Parse(os.Getenv("IM_TEST_DATABASE_URL"))
	if e != nil || (d.Hostname() != "127.0.0.1" && d.Hostname() != "localhost") || d.Path != "/enterprise_im_files" {
		t.Fatal("dedicated local fixture database required")
	}
	for _, key := range []string{"IM_TEST_S3_BUCKET", "IM_TEST_S3_POLICY_BUCKET"} {
		if !strings.HasPrefix(os.Getenv(key), "p426-") {
			t.Fatal("P4-26 owned bucket prefix required")
		}
	}
	keys := map[string]bool{}
	for _, n := range []string{"IM_TEST_FILE_UPLOAD_ACCESS_KEY", "IM_TEST_FILE_WORKER_ACCESS_KEY", "IM_TEST_FILE_DOWNLOAD_ACCESS_KEY", "IM_FILE_CLEANUP_S3_ACCESS_KEY"} {
		v := os.Getenv(n)
		if keys[v] {
			t.Fatal("four distinct IAM principals required")
		}
		keys[v] = true
	}
}
func fileBusinessBinaries(t *testing.T) (map[string]string, string) {
	t.Helper()
	fileBusinessBuild.Do(func() {
		root, e := os.MkdirTemp("", "im-p426-binaries-")
		if e != nil {
			fileBusinessBuild.err = e
			return
		}
		fileBusinessBuild.root = root
		fileBusinessBuild.sha = os.Getenv("IM_TEST_FILE_BUSINESS_BUILD_SHA")
		if fileBusinessBuild.sha == "" {
			b, e := exec.Command("git", "rev-parse", "HEAD").Output()
			if e != nil {
				fileBusinessBuild.err = e
				return
			}
			fileBusinessBuild.sha = strings.TrimSpace(string(b))
		}
		fileBusinessBuild.binaries = map[string]string{}
		for _, name := range []string{"im-api", "im-file-worker", "im-outbox-worker", "im-file-cleaner"} {
			path := filepath.Join(root, name)
			c := exec.Command("go", "build", "-o", path, "../../cmd/"+name)
			if b, e := c.CombinedOutput(); e != nil {
				processPrivateFile(t, filepath.Join(root, "build.log"), b)
				fileBusinessBuild.err = e
				return
			}
			fileBusinessBuild.binaries[name] = path
		}
	})
	if fileBusinessBuild.err != nil {
		t.Fatal("official process binary build failed")
	}
	return fileBusinessBuild.binaries, fileBusinessBuild.sha
}
func newFileBusinessProcessFixture(t *testing.T) *fileBusinessProcessFixture {
	t.Helper()
	requireBusinessProcess(t)
	parent := os.Getenv("IM_TEST_FILE_BUSINESS_OUTPUT_DIR")
	if parent != "" {
		if !filepath.IsAbs(parent) {
			t.Fatal("absolute private evidence directory required")
		}
		info, e := os.Lstat(parent)
		if e != nil || info.Mode().Perm() != 0700 || !info.IsDir() || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
			t.Fatal("private evidence directory required")
		}
	}
	root, e := os.MkdirTemp(parent, "im-p426-process-")
	if e != nil {
		t.Fatal(e)
	}
	f := &fileBusinessProcessFixture{privateRoot: root, processes: map[string]*exec.Cmd{}, waits: map[string]chan error{}, logs: map[string]*os.File{}, nodes: map[string]nodeRuntimeConfig{}, client: &http.Client{Timeout: 70 * time.Second}, expectedExitCodes: map[string]int{}, processPIDs: map[string]int{}}
	f.binaries, f.buildSHA = fileBusinessBinaries(t)
	f.conn = db(t)
	seedDirectConversation(t, f.conn)
	seedThirdGroupMember(t, f.conn)
	runtimePolicyEdit(t, f.conn)
	run(t, f.conn, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES($1,$2,$3,$4,'group_admin','2020-01-01')`, freshFile().ID, tenantA, adminM, orgA)
	if e = f.conn.QueryRow(context.Background(), "SELECT current_schema()").Scan(&f.schema); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile("../../db/migrations/000004_external_identities.up.sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.conn.PgConn().Exec(context.Background(), string(raw)).ReadAll(); e != nil {
		t.Fatal(e)
	}
	f.adminDSN = processDatabaseURL(t, f.schema)
	f.apiRole = "p426_api_" + processRandom(t)
	f.repairRole = "p426_repair_" + processRandom(t)
	t.Cleanup(func() {
		f.stopOwned(t)
		if f.proxy != nil {
			f.proxy.Close()
		}
		if f.oidc != nil {
			f.oidc.Close()
		}
		if f.dbRelay != nil {
			f.dbRelay.Close()
		}
		if f.objectGateway != nil {
			f.objectGateway.Close()
		}
		if f.pool != nil {
			f.pool.Close()
		}
		if f.redis != nil {
			f.redis.Del(context.Background(), f.stream, outbox.PublisherPresenceKey(f.stream))
			f.redis.Close()
		}
		for _, role := range []string{f.apiRole, f.repairRole} {
			f.conn.Exec(context.Background(), "DROP OWNED BY "+role)
			f.conn.Exec(context.Background(), "DROP ROLE "+role)
		}
	})
	f.apiDSN = f.createRole(t, f.apiRole, false)
	f.repairDSN = f.createRole(t, f.repairRole, true)
	cfg, e := pgxpool.ParseConfig(f.adminDSN)
	if e != nil {
		t.Fatal("fixture pool configuration failed")
	}
	f.pool, e = pgxpool.NewWithConfig(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	opts, e := redis.ParseURL(os.Getenv("IM_TEST_REDIS_URL"))
	if e != nil {
		t.Fatal("fixture Redis configuration failed")
	}
	f.redis = redis.NewClient(opts)
	f.stream = "enterprise-im:test:p426:" + processRandom(t)

	f.dbRelay = newProcessDBRelay(t, f.apiDSN)
	u, _ := url.Parse(f.apiDSN)
	u.Host = f.dbRelay.listener.Addr().String()
	f.apiDSN = u.String()
	f.startOIDC(t)
	target, e := url.Parse(os.Getenv("IM_TEST_S3_ENDPOINT"))
	if e != nil {
		t.Fatal("invalid fixture endpoint")
	}
	objects := httputil.NewSingleHostReverseProxy(target)
	objects.ModifyResponse = func(res *http.Response) error {
		if g := f.objectGate.Load(); g != nil && res.StatusCode == 200 && res.Request.Method == "GET" && strings.Contains(res.Request.URL.Path, "/files/"+g.file) {
			res.Body = &processGatedBody{res.Body, res.Request.Context(), g}
		}
		return nil
	}
	deadTarget, _ := url.Parse("http://127.0.0.1:1")
	deadObjects := httputil.NewSingleHostReverseProxy(deadTarget)
	deadObjects.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) { http.Error(w, "storage unreachable", 503) }
	f.objectGateway = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.objectRequests.Add(1)
		if r.Method == "PUT" || r.Method == "DELETE" {
			f.objectWrites.Add(1)
		}
		if f.objectFault.Load() != 0 {
			deadObjects.ServeHTTP(w, r)
			return
		}
		objects.ServeHTTP(w, r)
	}))
	bootstrap := f.s3Client("BOOTSTRAP", os.Getenv("IM_TEST_S3_ENDPOINT"))
	probe, e := bootstrap.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String(os.Getenv("IM_TEST_S3_BUCKET")), Key: aws.String("_im_runtime/read-probe/v1"), Body: strings.NewReader("enterprise-im-file-read-probe-v1\n")})
	if e != nil || aws.ToString(probe.VersionId) == "" {
		t.Fatal("administrator probe bootstrap failed")
	}
	f.probeVersion = aws.ToString(probe.VersionId)
	for _, node := range []string{"api-a", "api-b"} {
		f.nodes[node] = nodeRuntimeConfig{OwnerID: freshFile().ID, SpoolDir: filepath.Join(root, node+"-download")}
	}
	return f
}
func (f *fileBusinessProcessFixture) createRole(t *testing.T, name string, repair bool) string {
	t.Helper()
	password := processRandom(t)
	run(t, f.conn, "CREATE ROLE "+name+" LOGIN PASSWORD '"+password+"'")
	run(t, f.conn, "GRANT USAGE ON SCHEMA "+f.schema+" TO "+name)
	if !repair {
		run(t, f.conn, "GRANT SELECT,INSERT,UPDATE ON ALL TABLES IN SCHEMA "+f.schema+" TO "+name)
		run(t, f.conn, "GRANT USAGE ON ALL SEQUENCES IN SCHEMA "+f.schema+" TO "+name)
	} else {
		for _, table := range []string{"conversations", "file_objects", "file_lifecycle_events", "messages", "message_attachments", "users", "user_organizations", "file_download_sessions", "file_download_terminal_events", "file_worker_audit_events", "audit_events"} {
			run(t, f.conn, "GRANT SELECT ON "+table+" TO "+name)
		}
		for _, table := range []string{"conversations", "file_objects", "file_download_sessions"} {
			run(t, f.conn, "GRANT UPDATE ON "+table+" TO "+name)
		}
		run(t, f.conn, "GRANT INSERT,UPDATE ON file_download_terminal_events TO "+name)
		run(t, f.conn, "GRANT INSERT ON file_worker_audit_events TO "+name)
		run(t, f.conn, "GRANT USAGE ON SEQUENCE file_worker_audit_events_id_seq TO "+name)
	}
	u, e := url.Parse(f.adminDSN)
	if e != nil {
		t.Fatal("fixture role DSN failed")
	}
	u.User = url.UserPassword(name, password)
	return u.String()
}
func processChildEnv(values map[string]string) []string {
	result := []string{}
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "LANG", "TZ"} {
		if v := os.Getenv(k); v != "" {
			result = append(result, k+"="+v)
		}
	}
	for k, v := range values {
		result = append(result, k+"="+v)
	}
	return result
}
func (f *fileBusinessProcessFixture) startOIDC(t *testing.T) {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	f.key = key
	f.browserSubject.Store("admin")
	f.proxy = browserTestTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := f.backend.Load(); p != nil {
			p.ServeHTTP(w, r)
		} else {
			http.Error(w, "starting", 503)
		}
	}))
	f.webURL = f.proxy.URL
	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"test-key","n":"%s","e":"%s"}]}`, base64.RawURLEncoding.EncodeToString(key.N.Bytes()), base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()))
	var codes sync.Map
	f.oidc = browserTestTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/keys":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, jwks)
		case "/authorize":
			q := r.URL.Query()
			if r.Method != "GET" || q.Get("response_type") != "code" || q.Get("client_id") != "enterprise-im-web" || q.Get("redirect_uri") != f.webURL+"/web/" || q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || q.Get("state") == "" {
				http.Error(w, "invalid authorization", 400)
				return
			}
			var b [24]byte
			if _, e := rand.Read(b[:]); e != nil {
				http.Error(w, "unavailable", 503)
				return
			}
			code := base64.RawURLEncoding.EncodeToString(b[:])
			codes.Store(code, q.Get("code_challenge"))
			u, _ := url.Parse(f.webURL + "/web/")
			v := u.Query()
			v.Set("code", code)
			v.Set("state", q.Get("state"))
			u.RawQuery = v.Encode()
			http.Redirect(w, r, u.String(), 302)
		case "/token":
			if r.Method != "POST" || r.ParseForm() != nil || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("client_id") != "enterprise-im-web" || r.Form.Get("redirect_uri") != f.webURL+"/web/" {
				http.Error(w, "invalid token request", 400)
				return
			}
			challenge, ok := codes.LoadAndDelete(r.Form.Get("code"))
			digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if !ok || r.Form.Get("code_verifier") == "" || base64.RawURLEncoding.EncodeToString(digest[:]) != challenge {
				http.Error(w, "invalid code", 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			token, e := f.sign(f.browserSubject.Load().(string), time.Now().Add(time.Hour))
			if e != nil {
				http.Error(w, "unavailable", 503)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": 3600})
		default:
			http.NotFound(w, r)
		}
	}))
	f.ca = filepath.Join(f.privateRoot, "oidc-ca.pem")
	processPrivateFile(t, f.ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.oidc.Certificate().Raw}))
	run(t, f.conn, `INSERT INTO external_identities(issuer,subject,tenant_id,user_id) VALUES($1,'admin',$2,$3),($1,'peer',$2,$4),($1,'outsider',$2,$5),($1,'foreign',$6,$7)`, f.oidc.URL, tenantA, adminA, personA, groupUserC, tenantB, personB)
}
func (f *fileBusinessProcessFixture) sign(subject string, expiry time.Time) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": f.oidc.URL, "sub": subject, "aud": "enterprise-im-api", "exp": expiry.Unix(), "iat": time.Now().Add(-time.Minute).Unix(), "client_id": "enterprise-im-web", "jti": "p426-" + subject})
	token.Header["kid"] = "test-key"
	token.Header["typ"] = "at+jwt"
	return token.SignedString(f.key)
}
func (f *fileBusinessProcessFixture) token(t *testing.T, subject string) string {
	t.Helper()
	s, e := f.sign(subject, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal("fixture token signing failed")
	}
	return s
}
func (f *fileBusinessProcessFixture) s3Client(role, endpoint string) *s3.Client {
	prefix := "IM_TEST_FILE_" + role
	if role == "CLEANUP" {
		prefix = "IM_FILE_CLEANUP_S3"
	}
	c := aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider(os.Getenv(prefix+"_ACCESS_KEY"), os.Getenv(prefix+"_SECRET_KEY"), ""), Retryer: func() aws.Retryer { return aws.NopRetryer{} }, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired}
	return s3.NewFromConfig(c, func(o *s3.Options) { o.BaseEndpoint = aws.String(endpoint); o.UsePathStyle = true })
}
func (f *fileBusinessProcessFixture) apiEnvironment(upload, business bool, node string) map[string]string {
	n := f.nodes[node]
	e := map[string]string{"IM_DATABASE_URL": f.apiDSN, "IM_HTTP_ADDR": "127.0.0.1:0", "SSL_CERT_FILE": f.ca, "IM_OIDC_ENABLED": "true", "IM_OIDC_ISSUER": f.oidc.URL, "IM_OIDC_AUDIENCE": "enterprise-im-api", "IM_OIDC_JWKS_URL": f.oidc.URL + "/keys", "IM_OIDC_ALLOWED_CLIENT_IDS": "enterprise-im-web", "IM_WEB_ENABLED": "true", "IM_WEB_AUTHORIZATION_URL": f.oidc.URL + "/authorize", "IM_WEB_TOKEN_URL": f.oidc.URL + "/token", "IM_WEB_CLIENT_ID": "enterprise-im-web", "IM_WEB_REDIRECT_URL": f.webURL + "/web/", "IM_FILE_UPLOAD_ENABLED": fmt.Sprint(upload), "IM_FILE_BUSINESS_ENABLED": fmt.Sprint(business)}
	if upload || business {
		e["IM_FILE_S3_ENDPOINT"] = f.objectGateway.URL
		e["IM_FILE_S3_REGION"] = "us-east-1"
		e["IM_FILE_S3_BUCKET"] = os.Getenv("IM_TEST_S3_BUCKET")
		e["IM_FILE_S3_PATH_STYLE"] = "true"
	}
	if upload {
		e["IM_FILE_S3_ACCESS_KEY"] = os.Getenv("IM_TEST_FILE_UPLOAD_ACCESS_KEY")
		e["IM_FILE_S3_SECRET_KEY"] = os.Getenv("IM_TEST_FILE_UPLOAD_SECRET_KEY")
		e["IM_FILE_S3_CREDENTIAL_SOURCE"] = "environment"
		e["IM_FILE_SPOOL_DIR"] = filepath.Join(f.privateRoot, node+"-upload")
	}
	if business {
		e["IM_FILE_DOWNLOAD_S3_ACCESS_KEY"] = os.Getenv("IM_TEST_FILE_DOWNLOAD_ACCESS_KEY")
		e["IM_FILE_DOWNLOAD_S3_SECRET_KEY"] = os.Getenv("IM_TEST_FILE_DOWNLOAD_SECRET_KEY")
		e["IM_FILE_DOWNLOAD_SPOOL_DIR"] = n.SpoolDir
		e["IM_FILE_DOWNLOAD_OWNER_ID"] = n.OwnerID
		e["IM_FILE_READ_PROBE_VERSION_ID"] = f.probeVersion
	}
	if f.processes["outbox"] != nil {
		e["IM_REALTIME_REDIS_URL"] = os.Getenv("IM_TEST_REDIS_URL")
		e["IM_REALTIME_STREAM"] = f.stream
	}
	return e
}
func (f *fileBusinessProcessFixture) launch(t *testing.T, name, binary string, env map[string]string, args ...string) {
	t.Helper()
	f.launchProcess(t, name, binary, env, false, args...)
}
func (f *fileBusinessProcessFixture) launchProcess(t *testing.T, name, binary string, env map[string]string, allowCompleted bool, args ...string) {
	t.Helper()
	if f.processes[name] != nil {
		t.Fatal("fixture process already active")
	}
	log, e := os.OpenFile(filepath.Join(f.privateRoot, name+"-"+fmt.Sprint(time.Now().UnixNano())+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(f.binaries[binary], args...)
	cmd.Env = processChildEnv(env)
	cmd.Stdout = log
	cmd.Stderr = log
	if e = cmd.Start(); e != nil {
		log.Close()
		t.Fatal("official process failed to start")
	}
	registration, registerErr := testfixtures.RegisterIntegrationProcess(cmd, os.Getenv("IM_TEST_INTEGRATION_GATE"), t.Name())
	done := make(chan error, 1)
	go func(result chan error) { result <- cmd.Wait(); close(result) }(done)
	if registerErr != nil && allowCompleted {
		select {
		case waitErr := <-done:
			// Actual Wait proves the fast child has terminated. A terminal record
			// does not claim a live UID/start/pgid or allow a Ready proof.
			registration, registerErr = testfixtures.RegisterIntegrationProcess(cmd, os.Getenv("IM_TEST_INTEGRATION_GATE"), t.Name())
			done = make(chan error, 1)
			done <- waitErr
			close(done)
		case <-time.After(35 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			_ = log.Close()
			t.Fatal("invalid startup did not terminate after registration failure")
		}
	}
	if registerErr != nil {
		_ = cmd.Process.Kill()
		<-done
		_ = log.Close()
		t.Fatal(registerErr)
	}
	if f.integrationProcesses == nil {
		f.integrationProcesses = map[string]*testfixtures.IntegrationProcess{}
	}
	f.integrationProcesses[name] = registration
	f.processes[name] = cmd
	f.processPIDs[name] = cmd.Process.Pid
	f.logs[name] = log
	f.waits[name] = done
}
func (f *fileBusinessProcessFixture) await(t *testing.T, name string, timeout time.Duration, predicate func([]byte) bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-f.waits[name]:
			t.Fatal("official process exited during startup; private log retained")
		default:
		}
		b, e := os.ReadFile(f.logs[name].Name())
		if e == nil && predicate(b) {
			if err := f.integrationProcesses[name].Ready(); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("official process startup budget exhausted; private log retained")
}
func (f *fileBusinessProcessFixture) startAPI(t *testing.T, upload, business bool, node string) {
	t.Helper()
	if _, ok := f.nodes[node]; !ok {
		t.Fatal("unknown fixture node")
	}
	f.launch(t, node, "im-api", f.apiEnvironment(upload, business, node))
	f.await(t, node, 35*time.Second, func(b []byte) bool {
		address := productionAPIAddress(b)
		if address == "" {
			return false
		}
		f.nodes[node] = nodeRuntimeConfig{URL: "http://" + address, OwnerID: f.nodes[node].OwnerID, SpoolDir: f.nodes[node].SpoolDir}
		r, e := f.client.Get("http://" + address + "/health/ready")
		if e != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == 200
	})
	if node == "api-a" {
		f.apiURL = f.nodes[node].URL
		u, _ := url.Parse(f.apiURL)
		proxy := httputil.NewSingleHostReverseProxy(u)
		proxy.ModifyResponse = f.webTransport
		proxy.FlushInterval = -1
		f.backend.Store(proxy)
	}
}
func (f *fileBusinessProcessFixture) stopProcess(t *testing.T, name string, kill bool) {
	t.Helper()
	p := f.processes[name]
	if p == nil {
		return
	}
	if kill {
		_ = p.Process.Kill()
	} else {
		_ = p.Process.Signal(syscall.SIGTERM)
	}
	select {
	case err := <-f.waits[name]:
		expected := fmt.Sprintf("exit:%d", f.expectedExitCodes[name])
		if kill {
			expected = "signal:killed"
		}
		if proofErr := f.integrationProcesses[name].Exited(expected, integrationProcessExit(err)); proofErr != nil {
			t.Error(proofErr)
		}
		if !kill {
			code := 0
			if err != nil {
				code = -1
				if exit, ok := err.(*exec.ExitError); ok {
					code = exit.ExitCode()
				}
			}
			if code != f.expectedExitCodes[name] {
				t.Errorf("owned process %s exit=%d want=%d", name, code, f.expectedExitCodes[name])
			}
		}
		delete(f.expectedExitCodes, name)
	case <-time.After(22 * time.Second):
		_ = p.Process.Kill()
		waitErr := <-f.waits[name]
		if proofErr := f.integrationProcesses[name].Exited("bounded_shutdown", integrationProcessExit(waitErr)); proofErr != nil {
			t.Error(proofErr)
		}
		t.Error("owned process failed bounded shutdown")
	}
	_ = f.logs[name].Close()
	delete(f.logs, name)
	delete(f.processes, name)
	delete(f.integrationProcesses, name)
	delete(f.waits, name)
}
func (f *fileBusinessProcessFixture) restartAPI(t *testing.T, node string, upload, business bool) {
	t.Helper()
	f.stopProcess(t, node, false)
	f.startAPI(t, upload, business, node)
}
func (f *fileBusinessProcessFixture) startWorkers(t *testing.T) {
	t.Helper()
	if f.processes["outbox"] == nil {
		f.launch(t, "outbox", "im-outbox-worker", map[string]string{"IM_DATABASE_URL": f.adminDSN, "IM_OUTBOX_REDIS_URL": os.Getenv("IM_TEST_REDIS_URL"), "IM_OUTBOX_STREAM": f.stream})
		f.await(t, "outbox", 10*time.Second, func([]byte) bool {
			return f.redis.Exists(context.Background(), outbox.PublisherPresenceKey(f.stream)).Val() == 1
		})
	}
	if f.processes["scanner"] == nil {
		f.launch(t, "scanner", "im-file-worker", map[string]string{"IM_DATABASE_URL": f.adminDSN, "IM_FILE_WORKER_ENABLED": "true", "IM_FILE_WORKER_ID": freshFile().ID, "IM_FILE_SPOOL_DIR": filepath.Join(f.privateRoot, "scan"), "IM_FILE_S3_ENDPOINT": os.Getenv("IM_TEST_S3_ENDPOINT"), "IM_FILE_S3_REGION": "us-east-1", "IM_FILE_S3_BUCKET": os.Getenv("IM_TEST_S3_BUCKET"), "IM_FILE_S3_PATH_STYLE": "true", "IM_FILE_S3_ACCESS_KEY": os.Getenv("IM_TEST_FILE_WORKER_ACCESS_KEY"), "IM_FILE_S3_SECRET_KEY": os.Getenv("IM_TEST_FILE_WORKER_SECRET_KEY"), "IM_FILE_QPDF_PATH": os.Getenv("IM_TEST_QPDF_PATH"), "IM_FILE_CLAMD_SOCKET": os.Getenv("IM_TEST_CLAMD_SOCKET"), "IM_FILE_SCANNER_MANIFEST": os.Getenv("IM_TEST_SCANNER_MANIFEST")})
		f.await(t, "scanner", 95*time.Second, func(b []byte) bool { return bytes.Contains(b, []byte("file worker started")) })
	}
}
func (f *fileBusinessProcessFixture) stopOwned(t *testing.T) {
	t.Helper()
	for _, name := range []string{"api-b", "api-a", "repair", "scanner", "outbox"} {
		f.stopProcess(t, name, false)
	}
	for name := range f.processes {
		f.stopProcess(t, name, true)
	}
}
func (f *fileBusinessProcessFixture) assertEvidence(t *testing.T) {
	t.Helper()
	hashes := map[string]string{}
	for name, path := range f.binaries {
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal("binary evidence missing")
		}
		h := sha256.Sum256(b)
		hashes[name] = hex.EncodeToString(h[:])
	}
	if b, e := os.ReadFile(filepath.Join(f.privateRoot, "im-api-linux")); e == nil {
		h := sha256.Sum256(b)
		hashes["im-api-linux"] = hex.EncodeToString(h[:])
	}
	record := map[string]any{"stage": "P4-26", "test": t.Name(), "source_sha": f.buildSHA, "schema": f.schema, "probe_version_id": f.probeVersion, "process_pids": f.processPIDs, "binary_sha256": hashes, "nodes": f.nodes, "business_handlers_in_fixture": false, "repository_replacements": false}
	b, e := json.MarshalIndent(record, "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	processPrivateFile(t, filepath.Join(f.privateRoot, "evidence.json"), b)
}
func (f *fileBusinessProcessFixture) request(t *testing.T, method, node, path, subject, member string, body []byte, typ string) (int, http.Header, []byte) {
	t.Helper()
	base := f.nodes[node].URL
	if base == "" {
		t.Fatal("fixture node not listening")
	}
	req, e := http.NewRequest(method, base+path, bytes.NewReader(body))
	if e != nil {
		t.Fatal("fixture request invalid")
	}
	if subject != "" {
		req.Header.Set("Authorization", "Bearer "+f.token(t, subject))
	}
	if member != "" {
		req.Header.Set("X-Acting-Membership-ID", member)
	}
	if typ != "" {
		req.Header.Set("Content-Type", typ)
	}
	res, e := f.client.Do(req)
	if e != nil {
		t.Fatal("official API transport failed")
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, 27<<20))
	if e != nil {
		t.Fatal("official API body transport failed")
	}
	return res.StatusCode, res.Header, b
}
func businessJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if json.Unmarshal(b, &v) != nil {
		t.Fatal("invalid official response")
	}
	return v
}
func businessStatus(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("official status=%d want=%d", got, want)
	}
}
func (f *fileBusinessProcessFixture) allocate(t *testing.T, node, cid, name, media string, size int64) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"upload_request_id": freshFile().ID, "original_filename": name, "declared_media_type": media, "declared_size_bytes": fmt.Sprint(size)})
	code, _, b := f.request(t, "POST", node, "/api/v1/conversations/"+cid+"/files", "admin", adminM, body, "application/json")
	businessStatus(t, code, 201)
	id, ok := businessJSON(t, b)["file_id"].(string)
	if !ok || id == "" {
		t.Fatal("allocation identity absent")
	}
	return id
}
func (f *fileBusinessProcessFixture) upload(t *testing.T, node, cid, name, media string, body []byte, ready bool) string {
	t.Helper()
	id := f.allocate(t, node, cid, name, media, int64(len(body)))
	code, _, _ := f.request(t, "PUT", node, "/api/v1/files/"+id+"/content", "admin", adminM, body, "application/octet-stream")
	businessStatus(t, code, 200)
	if ready {
		f.waitState(t, node, id, "ready")
	}
	return id
}
func (f *fileBusinessProcessFixture) waitState(t *testing.T, node, id, want string) {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		code, _, b := f.request(t, "GET", node, "/api/v1/files/"+id, "admin", adminM, nil, "")
		businessStatus(t, code, 200)
		state, _ := businessJSON(t, b)["state"].(string)
		if state == want {
			return
		}
		if state == "rejected" || state == "scan_failed" {
			t.Fatal("scanner terminal state differs from required state")
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("actual scan state deadline exhausted")
}
func (f *fileBusinessProcessFixture) expectStartupFailure(t *testing.T, settings map[string]string) {
	t.Helper()
	name := "negative-" + processRandom(t)
	f.launchProcess(t, name, "im-api", settings, true)
	select {
	case e := <-f.waits[name]:
		if proofErr := f.integrationProcesses[name].Exited("exit:1", integrationProcessExit(e)); proofErr != nil {
			t.Error(proofErr)
		}
		if e == nil {
			t.Fatal("invalid startup returned success")
		}
	case <-time.After(35 * time.Second):
		t.Fatal("invalid startup exceeded initialization budget")
	}
	b, e := os.ReadFile(f.logs[name].Name())
	if e != nil || productionAPIAddress(b) != "" {
		t.Fatal("invalid startup opened listener")
	}
	f.logs[name].Close()
	delete(f.logs, name)
	delete(f.processes, name)
	delete(f.integrationProcesses, name)
	delete(f.waits, name)
}
