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
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/filescanner"
	"github.com/leileipei/Enterprise_IM/internal/filetransfer"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"github.com/leileipei/Enterprise_IM/internal/realtime"
	"github.com/redis/go-redis/v9"
)

type fileMessageRealFixture struct {
	webBackend        *atomic.Pointer[httputil.ReverseProxy]
	issuer            string
	issueToken        func(string) string
	conn              *pgx.Conn
	pool              *pgxpool.Pool
	repo              policystore.Service
	api, web, token   string
	binary            string
	apiEnv            []string
	apiLog            string
	logins, exchanges atomic.Int64
}

func realFileMessageFixture(t *testing.T) *fileMessageRealFixture {
	t.Helper()
	c := fileMessageDB(t)
	seedDirectConversation(t, c)
	seedThirdGroupMember(t, c)
	runtimePolicyEdit(t, c)
	var schema string
	if e := c.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); e != nil {
		t.Fatal(e)
	}
	cfg, e := pgxpool.ParseConfig(processDatabaseURL(t, schema))
	if e != nil {
		t.Fatal(e)
	}
	pool, e := pgxpool.NewWithConfig(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(pool.Close)
	f := &fileMessageRealFixture{conn: c, pool: pool, repo: policystore.Service{DB: pool}}
	var exists bool
	if e = c.QueryRow(context.Background(), "SELECT to_regclass('external_identities') IS NOT NULL").Scan(&exists); e != nil {
		t.Fatal(e)
	}
	if !exists {
		b, e := os.ReadFile("../../db/migrations/000004_external_identities.up.sql")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = c.PgConn().Exec(context.Background(), string(b)).ReadAll(); e != nil {
			t.Fatal(e)
		}
	}
	var backend atomic.Pointer[httputil.ReverseProxy]
	f.webBackend = &backend
	proxy := browserTestTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := backend.Load(); p != nil {
			p.ServeHTTP(w, r)
		} else {
			http.Error(w, "starting", 503)
		}
	}))
	t.Cleanup(proxy.Close)
	f.web = proxy.URL
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"test-key","n":"%s","e":"%s"}]}`, base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()), base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()))
	redirect := proxy.URL + "/web/"
	var codes sync.Map
	var token atomic.Value
	idp := browserTestTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/keys":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(jwks))
		case "/authorize":
			q := r.URL.Query()
			if r.Method != "GET" || q.Get("response_type") != "code" || q.Get("client_id") != "enterprise-im-web" || q.Get("redirect_uri") != redirect || q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || q.Get("state") == "" {
				http.Error(w, "invalid authorization", 400)
				return
			}
			b := make([]byte, 24)
			if _, e := rand.Read(b); e != nil {
				http.Error(w, "unavailable", 503)
				return
			}
			code := base64.RawURLEncoding.EncodeToString(b)
			codes.Store(code, q.Get("code_challenge"))
			f.logins.Add(1)
			u, _ := url.Parse(redirect)
			v := u.Query()
			v.Set("code", code)
			v.Set("state", q.Get("state"))
			u.RawQuery = v.Encode()
			http.Redirect(w, r, u.String(), 302)
		case "/token":
			if r.Method != "POST" || r.ParseForm() != nil || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("client_id") != "enterprise-im-web" || r.Form.Get("redirect_uri") != redirect {
				http.Error(w, "invalid token request", 400)
				return
			}
			challenge, ok := codes.LoadAndDelete(r.Form.Get("code"))
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if !ok || r.Form.Get("code_verifier") == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				http.Error(w, "invalid code", 400)
				return
			}
			f.exchanges.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": token.Load(), "token_type": "Bearer", "expires_in": 300})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(idp.Close)
	f.issuer = idp.URL
	f.issueToken = func(subject string) string { return productionJWT(t, key, idp.URL, subject) }
	f.token = f.issueToken("file-message-admin")
	token.Store(f.token)
	cert := filepath.Join(t.TempDir(), "idp-ca.pem")
	if e = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	run(t, c, `INSERT INTO external_identities(issuer,subject,tenant_id,user_id) VALUES($1,'file-message-admin',$2,$3)`, idp.URL, tenantA, adminA)
	f.binary = productionTestBinary(t, "im-api")
	f.apiEnv = []string{"IM_DATABASE_URL=" + processDatabaseURL(t, schema), "IM_HTTP_ADDR=127.0.0.1:0", "SSL_CERT_FILE=" + cert, "IM_OIDC_ENABLED=true", "IM_OIDC_ISSUER=" + idp.URL, "IM_OIDC_AUDIENCE=enterprise-im-api", "IM_OIDC_JWKS_URL=" + idp.URL + "/keys", "IM_OIDC_ALLOWED_CLIENT_IDS=enterprise-im-web", "IM_WEB_ENABLED=true", "IM_WEB_AUTHORIZATION_URL=" + idp.URL + "/authorize", "IM_WEB_TOKEN_URL=" + idp.URL + "/token", "IM_WEB_CLIENT_ID=enterprise-im-web", "IM_WEB_REDIRECT_URL=" + redirect, "IM_FILE_UPLOAD_ENABLED=false"}
	f.api, f.apiLog = startFileMessageProduction(t, f.binary, f.apiEnv)
	u, _ := url.Parse(f.api)
	backend.Store(httputil.NewSingleHostReverseProxy(u))
	return f
}
func startFileMessageProduction(t *testing.T, binary string, environment []string) (string, string) {
	t.Helper()
	log, e := os.CreateTemp(t.TempDir(), "p423-api-*.log")
	if e != nil {
		t.Fatal(e)
	}
	os.Chmod(log.Name(), 0600)
	cmd := fileProductCommand(binary, environment)
	cmd.Stdout = log
	cmd.Stderr = log
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait(); log.Close() })
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(log.Name())
		if addr := productionAPIAddress(b); addr != "" {
			res, e := client.Get("http://" + addr + "/health/ready")
			if e == nil {
				res.Body.Close()
				if res.StatusCode == 200 {
					return "http://" + addr, log.Name()
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("production API failed readiness; private log retained at", log.Name())
	return "", ""
}

// JWT verification and external identity mapping run in the actual production
// OIDC process, using its TLS JWKS fixture. This test bridge forwards only the
// verified identity to the explicit file-message assembly, never token claims.
type fileMessageProductionAuth struct {
	api    string
	client *http.Client
}

func (a fileMessageProductionAuth) Authenticate(ctx context.Context, token string) (httpserver.VerifiedIdentity, error) {
	req, e := http.NewRequestWithContext(ctx, "GET", a.api+"/api/v1/me", nil)
	if e != nil {
		return httpserver.VerifiedIdentity{}, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, e := a.client.Do(req)
	if e != nil {
		return httpserver.VerifiedIdentity{}, e
	}
	defer res.Body.Close()
	var id struct {
		TenantID string `json:"tenant_id"`
		UserID   string `json:"user_id"`
	}
	if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&id) != nil || id.TenantID == "" || id.UserID == "" {
		return httpserver.VerifiedIdentity{}, errors.New("OIDC rejected")
	}
	return httpserver.VerifiedIdentity{TenantID: id.TenantID, UserID: id.UserID}, nil
}
func (f *fileMessageRealFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	auth := fileMessageProductionAuth{api: f.api, client: &http.Client{Timeout: 3 * time.Second}}
	h, e := httpserver.HandlerWithFileMessages(httpserver.Handler(nil), auth, f.repo, f.repo)
	if e != nil {
		t.Fatal(e)
	}
	h, e = httpserver.HandlerWithMessageSearch(h, auth, f.repo)
	if e != nil {
		t.Fatal(e)
	}
	return h
}
func (f *fileMessageRealFixture) scanned(t *testing.T, cid string) files.Metadata {
	t.Helper()
	if os.Getenv("IM_TEST_SCANNER_MANIFEST") == "" {
		t.Skip("requires controlled scanner runtime")
	}
	ctx := context.Background()
	body := "P423 clean private content"
	p := reservationParams()
	p.ConversationID = cid
	p.UploadRequestID = clientUUIDv7(time.Now(), 8601)
	p.OriginalFilename = fmt.Sprintf("P423_PRIVATE_NAME_%d.txt", time.Now().UnixNano())
	p.DeclaredSizeBytes = int64(len(body))
	r, e := f.repo.ReserveFile(ctx, publisher(), p)
	if e != nil {
		t.Fatal(e)
	}
	objects := realTransferObjects(t)
	upload, e := filetransfer.NewService(f.repo, objects, t.TempDir()+"/upload", uploadOwner)
	if e != nil {
		t.Fatal(e)
	}
	m, e := upload.Upload(ctx, publisher(), r.File.ID, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	scanner, e := filescanner.New(filescanner.Config{QPDFPath: os.Getenv("IM_TEST_QPDF_PATH"), ClamdSocket: os.Getenv("IM_TEST_CLAMD_SOCKET"), RuntimeManifestPath: os.Getenv("IM_TEST_SCANNER_MANIFEST")})
	if e != nil {
		t.Fatal(e)
	}
	if e = scanner.ValidateRuntime(ctx); e != nil {
		t.Fatal("trusted runtime failed attestation", e)
	}
	worker := filetransfer.ScanWorker{Repo: f.repo, Objects: objects, Scanner: scanner, SpoolDir: t.TempDir() + "/scan", OwnerID: uploadOwner}
	if found, e := worker.RunOnce(ctx); e != nil || !found {
		t.Fatal(found, e)
	}
	var state string
	var sealed, scan []byte
	if e = f.conn.QueryRow(ctx, "SELECT state,sha256,scan_sha256 FROM file_objects WHERE id=$1", m.ID).Scan(&state, &sealed, &scan); e != nil || state != "ready" || !bytes.Equal(sealed, scan) {
		t.Fatal("no trusted clean scan proof", state, e)
	}
	m.State = files.StateReady
	return m
}
func (f *fileMessageRealFixture) request(t *testing.T, base, method, path string, body any, want int) []byte {
	t.Helper()
	var data []byte
	if body != nil {
		var e error
		data, e = json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
	}
	r := productionRequest(t, method, base, path, f.token, data, true)
	res, e := (&http.Client{Timeout: 5 * time.Second}).Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(res.Body)
	if res.StatusCode != want {
		t.Fatalf("%s status %d want %d: %s", path, res.StatusCode, want, b.String())
	}
	return b.Bytes()
}
func fileMessageJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if e := json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func assertFileMessagePrivate(t *testing.T, b []byte, forbidden ...string) {
	t.Helper()
	for _, s := range forbidden {
		if s != "" && bytes.Contains(b, []byte(s)) {
			t.Fatal("private file value leaked")
		}
	}
}
func TestFileMessageRealScanSendPull(t *testing.T) {
	for _, group := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "group"}[group], func(t *testing.T) {
			f := realFileMessageFixture(t)
			cid := directA
			route := "/api/v1/conversations/"
			if group {
				g, e := f.repo.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2, groupMemberC))
				if e != nil {
					t.Fatal(e)
				}
				cid = g.ID
				route = "/api/v1/groups/"
			}
			m := f.scanned(t, cid)
			server := httptest.NewServer(f.handler(t))
			defer server.Close()
			path := route + cid + "/messages"
			caption := fmt.Sprintf("P423_PRIVATE_CAPTION_%d", time.Now().UnixNano())
			req := map[string]string{"client_msg_id": clientUUIDv7(time.Now(), 8602), "message_type": "file", "file_id": m.ID, "caption": caption}
			ackRaw := f.request(t, server.URL, "POST", path, req, 200)
			ack := fileMessageJSON(t, ackRaw)
			if len(ack) != 5 || ack["seq"] != float64(1) || ack["duplicate"] != false {
				t.Fatal(ack)
			}
			assertFileMessagePrivate(t, ackRaw, caption, m.OriginalFilename, m.ObjectKey, m.ObjectVersionID)
			var mid string
			if e := f.conn.QueryRow(context.Background(), "SELECT id::text FROM messages WHERE conversation_id=$1", cid).Scan(&mid); e != nil || mid != ack["message_id"] {
				t.Fatal(mid, e)
			}
			legacy := f.request(t, server.URL, "GET", path+"?after_seq=0&limit=10", nil, 200)
			assertFileMessagePrivate(t, legacy, caption, m.OriginalFilename, m.ID)
			item := fileMessageJSON(t, legacy)["messages"].([]any)[0].(map[string]any)
			if len(item) != 5 || item["text"] != "附件消息（当前客户端不支持查看）" {
				t.Fatal(item)
			}
			typed := fileMessageJSON(t, f.request(t, server.URL, "GET", path+"?after_seq=0&limit=10&message_format=typed_v1", nil, 200))["messages"].([]any)[0].(map[string]any)
			card := typed["attachment"].(map[string]any)
			if typed["message_type"] != "file" || typed["caption"] != caption || card["download_available"] != false || card["available"] != true || card["original_filename"] != m.OriginalFilename {
				t.Fatal(typed)
			}
			search := f.request(t, server.URL, "GET", route+cid+"/messages/search?q="+url.QueryEscape(caption)+"&limit=10", nil, 200)
			assertFileMessagePrivate(t, search, caption, m.OriginalFilename)
			if len(fileMessageJSON(t, search)["messages"].([]any)) != 0 {
				t.Fatal("attachment matched search")
			}
			var audit string
			if e := f.conn.QueryRow(context.Background(), "SELECT COALESCE(jsonb_agg(to_jsonb(a))::text,'[]') FROM audit_events a").Scan(&audit); e != nil {
				t.Fatal(e)
			}
			assertFileMessagePrivate(t, []byte(audit), caption, m.OriginalFilename)
			logs, _ := os.ReadFile(f.apiLog)
			assertFileMessagePrivate(t, logs, caption, m.OriginalFilename)
			tx, e := f.conn.Begin(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(context.Background())
			stamp := time.Now().UTC().Add(31 * 24 * time.Hour)
			for _, q := range []string{"UPDATE messages SET text_body=NULL,body_cleared_at=$2 WHERE id=$1", "UPDATE messages SET content_digest=NULL,digest_retired_at=$2 WHERE id=$1", "UPDATE message_idempotency SET content_digest=NULL,digest_retired_at=$2 WHERE message_id=$1", "UPDATE message_attachments SET sealed_sha256=NULL,fingerprint_retired_at=$2 WHERE message_id=$1"} {
				if _, e = tx.Exec(context.Background(), q, mid, stamp); e != nil {
					t.Fatal(e)
				}
			}
			if e = tx.Commit(context.Background()); e != nil {
				t.Fatal(e)
			}
			retry := fileMessageJSON(t, f.request(t, server.URL, "POST", path, req, 410))
			if retry["error_code"] != "retry_window_expired" {
				t.Fatal(retry)
			}
		})
	}
}
func TestFileMessageRealRealtime(t *testing.T) {
	if os.Getenv("IM_TEST_REDIS_URL") == "" {
		t.Skip("requires dedicated Redis")
	}
	f := realFileMessageFixture(t)
	m := f.scanned(t, directA)
	opts, e := redis.ParseURL(os.Getenv("IM_TEST_REDIS_URL"))
	if e != nil {
		t.Fatal(e)
	}
	client := redis.NewClient(opts)
	t.Cleanup(func() { client.Close() })
	stream := fmt.Sprintf("enterprise-im:test:p423:%d", time.Now().UnixNano())
	t.Cleanup(func() { client.Del(context.Background(), stream, outbox.PublisherPresenceKey(stream)) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if e = outbox.RefreshPublisherPresence(ctx, client, stream); e != nil {
		t.Fatal(e)
	}
	fanout, e := realtime.StartStreamFanout(ctx, client, stream, f.repo)
	if e != nil {
		t.Fatal(e)
	}
	auth := fileMessageProductionAuth{api: f.api, client: &http.Client{Timeout: 3 * time.Second}}
	h, e := httpserver.HandlerWithRealtimeNotifications(f.handler(t), auth, f.repo, realtime.RedisTickets{Client: client}, ctx, fanout)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	ticket := fileMessageJSON(t, f.request(t, server.URL, "POST", "/api/v1/realtime/tickets", nil, 200))["ticket"].(string)
	ws, _, e := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/realtime", &websocket.DialOptions{Subprotocols: []string{"enterprise-im.v1", "ticket." + ticket}})
	if e != nil {
		t.Fatal(e)
	}
	defer ws.CloseNow()
	realtimeE2EFrame(t, ws, `{"type":"ready","resync_required":true}`)
	caption := "P423_PRIVATE_REALTIME_CAPTION"
	ack := f.request(t, server.URL, "POST", "/api/v1/conversations/"+directA+"/messages", map[string]string{"client_msg_id": clientUUIDv7(time.Now(), 8610), "message_type": "file", "file_id": m.ID, "caption": caption}, 200)
	assertFileMessagePrivate(t, ack, caption, m.OriginalFilename)
	worker := outbox.Worker{DB: f.pool, Publisher: outbox.RedisPublisher{Client: client, Stream: stream}}
	if processed, e := worker.ProcessOne(ctx); e != nil || !processed {
		t.Fatal(processed, e)
	}
	realtimeE2EFrame(t, ws, `{"type":"sync_required"}`)
	events, e := client.XRange(ctx, stream, "-", "+").Result()
	if e != nil || len(events) != 1 {
		t.Fatal(len(events), e)
	}
	b, _ := json.Marshal(events)
	assertFileMessagePrivate(t, b, caption, m.OriginalFilename, m.ID, m.ObjectKey, m.ObjectVersionID)
	f.request(t, server.URL, "GET", "/api/v1/conversations/"+directA+"/messages?after_seq=0&limit=10&message_format=typed_v1", nil, 200)
}
func TestFileMessageProductionClosed(t *testing.T) {
	f := realFileMessageFixture(t)
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown flag", true: "upload enabled"}[enabled], func(t *testing.T) {
			env := append([]string{}, f.apiEnv...)
			env = append(env, "IM_FILE_MESSAGE_ENABLED=true")
			if enabled {
				env = append(env, "IM_FILE_UPLOAD_ENABLED=true", "IM_FILE_S3_ENDPOINT="+os.Getenv("IM_TEST_S3_ENDPOINT"), "IM_FILE_S3_REGION=us-east-1", "IM_FILE_S3_BUCKET="+os.Getenv("IM_TEST_S3_BUCKET"), "IM_FILE_S3_PATH_STYLE=true", "IM_FILE_SPOOL_DIR="+t.TempDir()+"/upload")
			}
			api, log := startFileMessageProduction(t, f.binary, env)
			path := "/api/v1/conversations/" + directA + "/messages"
			b := f.request(t, api, "POST", path, map[string]string{"client_msg_id": clientUUIDv7(time.Now(), 8620), "message_type": "file", "file_id": "00000000-0000-4000-8000-000000001234", "caption": "P423_PRIVATE_CLOSED"}, 503)
			if fileMessageJSON(t, b)["error_code"] != "file_message_unavailable" {
				t.Fatal(string(b))
			}
			f.request(t, api, "POST", path, map[string]string{"client_msg_id": clientUUIDv7(time.Now(), 8621+int(time.Now().UnixNano()%100000)), "text": "P423 production text remains enabled"}, 200)
			logs, _ := os.ReadFile(log)
			assertFileMessagePrivate(t, logs, "P423_PRIVATE_CLOSED")
		})
	}
	var filesCount int
	if e := f.conn.QueryRow(context.Background(), "SELECT count(*) FROM message_attachments").Scan(&filesCount); e != nil || filesCount != 0 {
		t.Fatal(filesCount, e)
	}
}

func TestFileMessageRetiredHTTP(t *testing.T) {
	f := realFileMessageFixture(t)
	// This is a SQL-boundary replay fixture; it does not count as trusted scan
	// acceptance. The separate RealScanSendPull test requires actual ClamAV.
	m := fileMessageFixture(t, f.conn, directA, adminA, adminM)
	server := httptest.NewServer(f.handler(t))
	defer server.Close()
	path := "/api/v1/conversations/" + directA + "/messages"
	req := map[string]string{"client_msg_id": clientUUIDv7(time.Now(), 8640), "message_type": "file", "file_id": m.ID, "caption": ""}
	ack := fileMessageJSON(t, f.request(t, server.URL, "POST", path, req, 200))
	mid := ack["message_id"].(string)
	tx, e := f.conn.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	stamp := time.Now().UTC().Add(31 * 24 * time.Hour)
	for _, q := range []string{"UPDATE messages SET text_body=NULL,body_cleared_at=$2 WHERE id=$1", "UPDATE messages SET content_digest=NULL,digest_retired_at=$2 WHERE id=$1", "UPDATE message_idempotency SET content_digest=NULL,digest_retired_at=$2 WHERE message_id=$1", "UPDATE message_attachments SET sealed_sha256=NULL,fingerprint_retired_at=$2 WHERE message_id=$1"} {
		if _, e = tx.Exec(context.Background(), q, mid, stamp); e != nil {
			t.Fatal(e)
		}
	}
	if e = tx.Commit(context.Background()); e != nil {
		t.Fatal(e)
	}
	body := fileMessageJSON(t, f.request(t, server.URL, "POST", path, req, 410))
	if body["error_code"] != "retry_window_expired" {
		t.Fatal(body)
	}
	assertFileMessageWrites(t, f.conn, directA, 1)
}
