package policystore_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/filescanner"
	"github.com/leileipei/Enterprise_IM/internal/filetransfer"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"io"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The bridge obtains the principal from the actual production /me verifier.
// It reads only exp from the exact token AFTER that verifier accepted its
// signature, issuer, audience, client and lifetime. Claims never select identity.
type realDownloadAuth struct{ fileMessageProductionAuth }

func (a realDownloadAuth) Authenticate(ctx context.Context, token string) (httpserver.VerifiedIdentity, error) {
	id, e := a.fileMessageProductionAuth.Authenticate(ctx, token)
	if e != nil {
		return id, e
	}
	claims := jwt.MapClaims{}
	if _, _, e = jwt.NewParser().ParseUnverified(token, claims); e != nil {
		return httpserver.VerifiedIdentity{}, e
	}
	expiry, e := claims.GetExpirationTime()
	if e != nil || expiry == nil {
		return httpserver.VerifiedIdentity{}, errors.New("verified expiry absent")
	}
	id.ExpiresAt = expiry.Time
	return id, nil
}
func dedicatedUpload(t *testing.T) {
	t.Helper()
	for _, pair := range [][2]string{{"IM_FILE_S3_ACCESS_KEY", "IM_TEST_FILE_UPLOAD_ACCESS_KEY"}, {"IM_FILE_S3_SECRET_KEY", "IM_TEST_FILE_UPLOAD_SECRET_KEY"}} {
		if os.Getenv(pair[1]) == "" {
			t.Fatal("dedicated upload role required")
		}
		t.Setenv(pair[0], os.Getenv(pair[1]))
	}
}
func scannerObjects(t *testing.T) objectstore.Store {
	t.Helper()
	key, secret := os.Getenv("IM_FILE_S3_ACCESS_KEY"), os.Getenv("IM_FILE_S3_SECRET_KEY")
	os.Setenv("IM_FILE_S3_ACCESS_KEY", os.Getenv("IM_TEST_FILE_WORKER_ACCESS_KEY"))
	os.Setenv("IM_FILE_S3_SECRET_KEY", os.Getenv("IM_TEST_FILE_WORKER_SECRET_KEY"))
	objects, e := objectstore.NewS3(objectstore.Config{Endpoint: os.Getenv("IM_TEST_S3_ENDPOINT"), Region: "us-east-1", Bucket: os.Getenv("IM_TEST_S3_BUCKET"), PathStyle: true, CredentialSource: "environment"})
	os.Setenv("IM_FILE_S3_ACCESS_KEY", key)
	os.Setenv("IM_FILE_S3_SECRET_KEY", secret)
	if e != nil {
		t.Fatal(e)
	}
	if e = objects.ValidateCapabilities(context.Background()); e != nil {
		t.Fatal(e)
	}
	return objects
}
func actualScanner(t *testing.T) *filescanner.Scanner {
	t.Helper()
	s, e := filescanner.New(filescanner.Config{QPDFPath: os.Getenv("IM_TEST_QPDF_PATH"), ClamdSocket: os.Getenv("IM_TEST_CLAMD_SOCKET"), RuntimeManifestPath: os.Getenv("IM_TEST_SCANNER_MANIFEST")})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ValidateRuntime(context.Background()); e != nil {
		t.Fatal("fresh runtime proof", e)
	}
	return s
}
func scanDownloadBody(t *testing.T, f *fileMessageRealFixture, body []byte) files.Metadata {
	t.Helper()
	ctx := context.Background()
	p := reservationParams()
	p.UploadRequestID = freshFile().ID
	p.OriginalFilename = "集团报告_日本語_😀.txt"
	p.DeclaredSizeBytes = int64(len(body))
	r, e := f.repo.ReserveFile(ctx, publisher(), p)
	if e != nil {
		t.Fatal(e)
	}
	upload, e := filetransfer.NewService(f.repo, realTransferObjects(t), t.TempDir()+"/upload", uploadOwner)
	if e != nil {
		t.Fatal(e)
	}
	defer upload.Close()
	if _, e = upload.Upload(ctx, publisher(), r.File.ID, bytes.NewReader(body)); e != nil {
		t.Fatal(e)
	}
	worker := filetransfer.ScanWorker{Repo: f.repo, Objects: scannerObjects(t), Scanner: actualScanner(t), SpoolDir: t.TempDir() + "/scan", OwnerID: uploadOwner}
	if found, e := worker.RunOnce(ctx); e != nil || !found {
		t.Fatal(found, e)
	}
	m, e := f.repo.GetOwnFile(ctx, publisher(), r.File.ID)
	if e != nil || m.State != files.StateReady || !bytes.Equal(m.SHA256, m.ScanSHA256) {
		t.Fatal("real ready seal", e)
	}
	if _, e = f.repo.SendMessage(ctx, publisher(), directA, policystore.MessageSendRequest{ClientMessageID: clientUUIDv7(func() time.Time {
		if f.repo.Now != nil {
			return f.repo.Now()
		}
		return time.Now()
	}(), 9910), MessageType: "file", FileID: m.ID, Caption: "本轮真实扫描附件"}); e != nil {
		t.Fatal(e)
	}
	return m
}
func downloadJWTIssuer(t *testing.T, f *fileMessageRealFixture) func(string, time.Time) string {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"test-key","n":"%s","e":"%s"}]}`, base64.RawURLEncoding.EncodeToString(key.N.Bytes()), base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()))
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/keys" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, jwks)
	}))
	t.Cleanup(issuer.Close)
	cert := filepath.Join(t.TempDir(), "ca.pem")
	if e = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	run(t, f.conn, `INSERT INTO external_identities(issuer,subject,tenant_id,user_id) VALUES($1,'admin',$2,$3),($1,'peer',$2,$4)`, issuer.URL, tenantA, adminA, personA)
	env := append(append([]string{}, f.apiEnv...), "SSL_CERT_FILE="+cert, "IM_OIDC_ISSUER="+issuer.URL, "IM_OIDC_JWKS_URL="+issuer.URL+"/keys", "IM_WEB_ENABLED=false")
	f.api, _ = startFileMessageProduction(t, f.binary, env)
	return func(subject string, expiry time.Time) string {
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": issuer.URL, "sub": subject, "aud": "enterprise-im-api", "exp": expiry.Unix(), "iat": time.Now().Add(-time.Minute).Unix(), "client_id": "enterprise-im-web", "jti": "p424-" + subject})
		token.Header["kid"] = "test-key"
		token.Header["typ"] = "at+jwt"
		v, e := token.SignedString(key)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
}
func explicitDownload(t *testing.T, f *fileMessageRealFixture, objects objectstore.Store, observe *tcpEvidence) (string, *filedownload.Service, string) {
	t.Helper()
	spool := t.TempDir() + "/download"
	svc, e := filedownload.NewService(f.repo, objects, spool, uploadOwner)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := svc.Close(); e != nil {
			t.Error(e)
		}
	})
	var consumer httpserver.FileDownloadService = svc
	if observe != nil {
		consumer = &observedDownload{Service: svc, evidence: observe}
	}
	h, e := httpserver.HandlerWithFileDownload(httpserver.Handler(nil), realDownloadAuth{fileMessageProductionAuth{api: f.api, client: &http.Client{Timeout: time.Second}}}, consumer)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewUnstartedServer(h)
	server.Config.WriteTimeout = 10 * time.Second
	if observe != nil {
		server.Listener = &evidenceListener{Listener: server.Listener, evidence: observe}
	}
	server.Start()
	t.Cleanup(server.Close)
	return server.URL, svc, spool
}
func downloadRequest(t *testing.T, base, token, member, file string) *http.Request {
	t.Helper()
	r, e := http.NewRequest("GET", base+"/api/v1/files/"+file+"/content", nil)
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Acting-Membership-ID", member)
	return r
}
func TestFileDownloadRealOIDCScan(t *testing.T) {
	dedicatedUpload(t)
	f := realFileMessageFixture(t)
	sign := downloadJWTIssuer(t, f)
	body := []byte("集团 private 日本語 😀\n")
	m := scanDownloadBody(t, f, body)
	evidence := &tcpEvidence{first: make(chan struct{}), settled: make(chan error, 64)}
	base, _, spool := explicitDownload(t, f, realTransferObjects(t), evidence)
	errs := make(chan error, 2)
	for _, who := range []struct{ subject, member string }{{"admin", adminM}, {"peer", targetM2}} {
		user := adminA
		if who.subject == "peer" {
			user = personA
		}
		go func() {
			lastStatus, lastCode := 0, ""
			operatorDeadline := time.Now().Add(45 * time.Second)
			for time.Now().Before(operatorDeadline) {
				r := downloadRequest(t, base, sign(who.subject, time.Now().Add(15*time.Second)), who.member, m.ID)
				res, e := (&http.Client{Timeout: 10 * time.Second}).Do(r)
				if e != nil {
					errs <- e
					return
				}

				data, e := io.ReadAll(res.Body)
				res.Body.Close()
				lastStatus = res.StatusCode
				if bytes.Contains(data, []byte("download_audit_pending")) {
					lastCode = "audit_pending"
				} else if bytes.Contains(data, []byte("download_in_progress")) {
					lastCode = "busy"
				} else {
					lastCode = "other"
				}
				if res.StatusCode == 409 || res.StatusCode == 503 {
					// This is explicit test-operator recovery. A failed settlement
					// cannot be repaired by guessing that the full HTTP body meant completion.
					var retryAt *time.Time
					var dbNow time.Time
					if qe := f.pool.QueryRow(context.Background(), `SELECT max(deadline),clock_timestamp() FROM file_download_sessions WHERE file_id=$1 AND requester_user_id=$2 AND NOT audit_acked`, m.ID, user).Scan(&retryAt, &dbNow); qe != nil {
						errs <- errors.New("operator session query failed")
						return
					}
					if retryAt != nil && retryAt.After(dbNow) {
						wait := retryAt.Sub(dbNow) + 10*time.Millisecond
						if wait > time.Until(operatorDeadline) {
							errs <- errors.New("operator recovery exceeded bound")
							return
						}
						time.Sleep(wait)
					} else {
						time.Sleep(20 * time.Millisecond)
					}
					if _, qe := f.repo.RepairFileDownloadAudit(context.Background(), clientB, 20); qe != nil {
						errs <- errors.New("operator audit repair failed")
						return
					}
					continue
				}
				_, params, headerErr := mime.ParseMediaType(res.Header.Get("Content-Disposition"))
				if e != nil || headerErr != nil || res.StatusCode != 200 || sha256.Sum256(data) != sha256.Sum256(body) || res.ContentLength != int64(len(body)) || params["filename"] != m.OriginalFilename || res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Location") != "" {
					errs <- fmt.Errorf("fixed private version/header mismatch status=%d bytes=%d expected=%d length=%d filenameEqual=%t dispositionError=%v readError=%v", res.StatusCode, len(data), len(body), res.ContentLength, params["filename"] == m.OriginalFilename, headerErr, e)
					return
				}
				errs <- nil
				return
			}
			errs <- fmt.Errorf("download admission retry exhausted status=%d code=%s", lastStatus, lastCode)
		}()
	}
	var admissionErr error
	for i := 0; i < 2; i++ {
		admissionErr = errors.Join(admissionErr, <-errs)
	}
	{
		if e := admissionErr; e != nil {
			var preparing, authorized, terminal int
			f.conn.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE phase='preparing'),count(*) FILTER (WHERE phase='authorized'),count(*) FILTER (WHERE phase IN ('completed','interrupted','unknown')) FROM file_download_sessions`).Scan(&preparing, &authorized, &terminal)
			select {
			case se := <-evidence.settled:
				var pe *pgconn.PgError
				code := "none"
				if errors.As(se, &pe) {
					code = pe.Code
				}
				t.Logf("settlement pgcode=%s busy=%t unavailable=%t", code, errors.Is(se, filedownload.ErrBusy), errors.Is(se, filedownload.ErrUnavailable))
			default:
			}
			t.Fatalf("%v sessions preparing=%d authorized=%d terminal=%d", e, preparing, authorized, terminal)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case <-evidence.settled:
		case <-time.After(8 * time.Second):
			t.Fatal("terminal attempt did not finish")
		}
	}
	run(t, f.conn, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ((SELECT max(deadline) FROM file_download_sessions)-clock_timestamp())))+0.01)`)
	if n, e := f.repo.RepairFileDownloadAudit(context.Background(), clientB, 20); e != nil || n < 0 {
		t.Fatal(n, e)
	}
	var complete int
	if e := f.conn.QueryRow(context.Background(), `SELECT count(DISTINCT requester_user_id) FROM file_download_sessions WHERE phase IN ('completed','unknown','interrupted') AND audit_acked`).Scan(&complete); e != nil || complete != 2 {
		t.Fatal(complete, e)
	}
	entries, e := os.ReadDir(spool)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range entries {
		if v.IsDir() {
			t.Fatal("completed spool retained")
		}
	}
}

type tcpEvidence struct {
	mu                      sync.Mutex
	first                   chan struct{}
	once                    sync.Once
	writeGate               time.Time
	writeGateOnce           sync.Once
	last, deadline, started time.Time
	accepted                int64
	timeout                 bool
	checks                  []time.Time
	settled                 chan error
}
type evidenceListener struct {
	net.Listener
	evidence *tcpEvidence
}

func (l *evidenceListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e != nil {
		return nil, e
	}
	if tcp, ok := c.(*net.TCPConn); ok {
		tcp.SetWriteBuffer(4096)
	}
	return &evidenceConn{Conn: c, e: l.evidence}, nil
}

type evidenceConn struct {
	net.Conn
	e *tcpEvidence
}

func (c *evidenceConn) SetWriteDeadline(at time.Time) error {
	c.e.mu.Lock()
	c.e.deadline = at
	c.e.mu.Unlock()
	return c.Conn.SetWriteDeadline(at)
}
func (c *evidenceConn) Write(p []byte) (int, error) {
	// Hold only the real wire write; object staging and authorization have
	// already completed. The underlying socket retains its actual deadline.
	c.e.writeGateOnce.Do(func() {
		if !c.e.writeGate.IsZero() {
			timer := time.NewTimer(time.Until(c.e.writeGate))
			defer timer.Stop()
			<-timer.C
		}
	})
	start := time.Now()
	n, e := c.Conn.Write(p)
	c.e.mu.Lock()
	if n > 0 {
		c.e.last = time.Now()
		c.e.accepted += int64(n)
	}
	var ne net.Error
	if errors.As(e, &ne) && ne.Timeout() {
		c.e.timeout = true
		c.e.started = start
	}
	c.e.mu.Unlock()
	c.e.once.Do(func() { close(c.e.first) })
	return n, e
}
func TestFileDownloadRealTokenExpiryBlockedWrite(t *testing.T) {
	testDownloadTokenExpiryBlockedWrite(t, 0)
}

func TestFileDownloadRealTokenExpiryPreparationBeforeWire(t *testing.T) {
	testDownloadTokenExpiryBlockedWrite(t, time.Second)
}

type delayedDownloadPreparationStore struct {
	objectstore.Store
	delay time.Duration
}

func (s *delayedDownloadPreparationStore) ReadVersion(ctx context.Context, v objectstore.VersionRef) (io.ReadCloser, error) {
	r, err := s.Store.ReadVersion(ctx, v)
	if err != nil {
		return nil, err
	}
	return &delayedDownloadPreparationReader{ReadCloser: r, ctx: ctx, delay: s.delay}, nil
}

type delayedDownloadPreparationReader struct {
	io.ReadCloser
	ctx   context.Context
	delay time.Duration
	once  sync.Once
	err   error
}

func (r *delayedDownloadPreparationReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		timer := time.NewTimer(r.delay)
		defer timer.Stop()
		select {
		case <-r.ctx.Done():
			r.err = r.ctx.Err()
		case <-timer.C:
		}
	})
	if r.err != nil {
		return 0, r.err
	}
	return r.ReadCloser.Read(p)
}

func testDownloadTokenExpiryBlockedWrite(t *testing.T, preparationDelay time.Duration) {
	dedicatedUpload(t)
	f := realFileMessageFixture(t)
	sign := downloadJWTIssuer(t, f)
	m := scanDownloadBody(t, f, bytes.Repeat([]byte("x"), 8<<20))
	expiry := time.Unix(time.Now().Add(4*time.Second).Unix(), 0)
	evidence := &tcpEvidence{first: make(chan struct{}), writeGate: expiry.Add(-800 * time.Millisecond)}
	objects := &delayedDownloadPreparationStore{Store: realTransferObjects(t), delay: preparationDelay}
	base, _, _ := explicitDownload(t, f, objects, evidence)
	token := sign("admin", expiry)
	address := strings.TrimPrefix(base, "http://")
	conn, e := dialSmallReceiveWindow(address)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "GET /api/v1/files/%s/content HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nX-Acting-Membership-ID: %s\r\n\r\n", m.ID, address, token, adminM)
	reader := bufio.NewReaderSize(conn, 1024)
	line, e := reader.ReadString('\n')
	if e != nil || !strings.Contains(line, "200") {
		var phase, reason string
		queryErr := f.conn.QueryRow(context.Background(), `SELECT phase,COALESCE(reason_code,'') FROM file_download_sessions WHERE file_id=$1`, m.ID).Scan(&phase, &reason)
		evidence.mu.Lock()
		t.Logf("initial-header diagnostic phase=%s reason=%s query-ok=%t token-remaining=%s tcp-accepted=%d write-deadline-set=%t", phase, reason, queryErr == nil, time.Until(expiry), evidence.accepted, !evidence.deadline.IsZero())
		evidence.mu.Unlock()
		t.Fatal("initial TCP headers", e, line)
	}
	for {
		line, e = reader.ReadString('\n')
		if e != nil {
			t.Fatal(e)
		}
		if line == "\r\n" {
			break
		}
	}
	// Stop reading the real TCP connection; the kernel buffer is deliberately small.
	deadline := time.Now().Add(8 * time.Second)
	var phase, reason string
	for time.Now().Before(deadline) {
		if e = f.conn.QueryRow(context.Background(), `SELECT phase,COALESCE(reason_code,'') FROM file_download_sessions WHERE file_id=$1`, m.ID).Scan(&phase, &reason); e != nil {
			t.Fatal(e)
		}
		if phase == "interrupted" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	evidence.mu.Lock()
	defer evidence.mu.Unlock()
	if reason != "token_expired" || !evidence.started.Before(expiry) || phase != "interrupted" || !evidence.timeout || !evidence.deadline.Equal(expiry) || evidence.last.After(expiry.Add(300*time.Millisecond)) || evidence.accepted >= 8<<20 {
		t.Fatal("blocked writer crossed bound", phase, reason, evidence.timeout, evidence.deadline.Sub(expiry), evidence.accepted, evidence.started.Sub(expiry))
	}
	t.Logf("TCP accepted=%d client-buffered=%d last-writer=%s timeout-start=%s exp=%s deadline=%s", evidence.accepted, reader.Buffered(), evidence.last.UTC().Format(time.RFC3339Nano), evidence.started.UTC().Format(time.RFC3339Nano), expiry.UTC().Format(time.RFC3339Nano), evidence.deadline.UTC().Format(time.RFC3339Nano))
}

type gatedObjects struct {
	objectstore.Store
	entered chan struct{}
	release chan struct{}
	delay   time.Duration
	once    sync.Once
}

func (o *gatedObjects) ReadVersion(ctx context.Context, v objectstore.VersionRef) (io.ReadCloser, error) {
	r, e := o.Store.ReadVersion(ctx, v)
	if e != nil {
		return nil, e
	}
	return &gatedReader{ReadCloser: r, ctx: ctx, objects: o}, nil
}

type gatedReader struct {
	io.ReadCloser
	ctx     context.Context
	objects *gatedObjects
	once    sync.Once
}

func (r *gatedReader) Read(p []byte) (int, error) {
	var e error
	r.once.Do(func() {
		r.objects.once.Do(func() { close(r.objects.entered) })
		if r.objects.delay > 0 {
			timer := time.NewTimer(r.objects.delay)
			defer timer.Stop()
			select {
			case <-r.ctx.Done():
				e = r.ctx.Err()
			case <-timer.C:
			}
		} else {
			select {
			case <-r.ctx.Done():
				e = r.ctx.Err()
			case <-r.objects.release:
			}
		}
	})
	if e != nil {
		return 0, e
	}
	return r.ReadCloser.Read(p)
}
func TestFileDownloadRealRevocation(t *testing.T) {
	for _, what := range []string{"membership", "hard-deny", "audit-fault", "ttl", "db-fault"} {
		t.Run(what, func(t *testing.T) {
			dedicatedUpload(t)
			f := realFileMessageFixture(t)
			sign := downloadJWTIssuer(t, f)
			if what == "ttl" {
				older := time.Now().Add(-48 * time.Hour)
				f.repo.Now = func() time.Time { return older }
			}
			m := scanDownloadBody(t, f, []byte("private revoked"))
			f.repo.Now = nil
			objects := &gatedObjects{Store: realTransferObjects(t), entered: make(chan struct{}), release: make(chan struct{})}
			base, _, _ := explicitDownload(t, f, objects, nil)
			out := make(chan *http.Response, 1)
			errc := make(chan error, 1)
			go func() {
				res, e := (&http.Client{Timeout: 15 * time.Second}).Do(downloadRequest(t, base, sign("admin", time.Now().Add(time.Minute)), adminM, m.ID))
				out <- res
				errc <- e
			}()
			select {
			case <-objects.entered:
			case <-time.After(8 * time.Second):
				t.Fatal("no actual fixed-version read")
			}
			switch what {
			case "membership":
				run(t, f.conn, "UPDATE user_organizations SET status='suspended' WHERE id=$1", adminM)
			case "hard-deny":
				grantPublisher(t, f.conn)
				r := policy.Rule{ID: "real-deny", TenantID: tenantA, Action: policy.ActionFileDownload, Effect: policy.EffectHardDeny, SourceOrganizationID: orgA, TargetOrganizationID: orgA, EffectiveFrom: time.Now().Add(-time.Hour), Reason: "real revocation"}
				if _, e := f.repo.Publish(context.Background(), publisher(), 0, []policy.Rule{r}, "CAB"); e != nil {
					t.Fatal(e)
				}
			case "ttl":
				setDownloadRetention(t, f.conn, 1)
			case "db-fault":
				run(t, f.conn, "ALTER TABLE tenant_file_retention_policy RENAME TO unavailable_retention_fixture")
				t.Cleanup(func() {
					run(t, f.conn, "ALTER TABLE unavailable_retention_fixture RENAME TO tenant_file_retention_policy")
				})
			case "audit-fault":
				run(t, f.conn, `CREATE FUNCTION reject_download_grant() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='file_download_authorize' THEN RAISE EXCEPTION 'owned audit fault';END IF;RETURN NEW;END $$;CREATE TRIGGER reject_download_grant BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_download_grant()`)
			}
			revoked := time.Now()
			close(objects.release)
			res := <-out
			if e := <-errc; e != nil {
				t.Fatal(e)
			}
			defer res.Body.Close()
			data, _ := io.ReadAll(res.Body)
			if res.StatusCode == 200 || bytes.Contains(data, []byte("private revoked")) {
				t.Fatal("payload survived real revocation", res.StatusCode)
			}
			t.Log("revocation committed before first payload:", revoked.UTC().Format(time.RFC3339Nano))
		})
	}
}
func TestFileDownloadRealAuditRepair(t *testing.T) {
	dedicatedUpload(t)
	f := realFileMessageFixture(t)
	sign := downloadJWTIssuer(t, f)
	m := scanDownloadBody(t, f, []byte("audit recovery"))
	base, _, _ := explicitDownload(t, f, realTransferObjects(t), nil)
	run(t, f.conn, `CREATE FUNCTION reject_download_terminal() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned terminal fault';END $$;CREATE TRIGGER reject_download_terminal BEFORE INSERT ON file_download_terminal_events FOR EACH ROW EXECUTE FUNCTION reject_download_terminal()`)
	res, e := (&http.Client{Timeout: 10 * time.Second}).Do(downloadRequest(t, base, sign("admin", time.Now().Add(3*time.Second)), adminM, m.ID))
	if e != nil {
		t.Fatal(e)
	}
	data, e := io.ReadAll(res.Body)
	res.Body.Close()
	if e != nil || string(data) != "audit recovery" {
		t.Fatal(e)
	}
	var session string
	var bound time.Time
	if e = f.conn.QueryRow(context.Background(), "SELECT id::text,deadline FROM file_download_sessions WHERE file_id=$1", m.ID).Scan(&session, &bound); e != nil {
		t.Fatal(e)
	}
	// An interrupted terminal persistence cannot be acknowledged from writer success.
	if _, e = f.repo.BeginFileDownload(context.Background(), publisher(), m.ID, clientB, time.Now().Add(30*time.Second)); e == nil {
		t.Fatal("audit gap admitted another download")
	}
	run(t, f.conn, "DROP TRIGGER reject_download_terminal ON file_download_terminal_events")
	run(t, f.conn, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp())))+0.01)`, bound)
	if n, e := f.repo.RepairFileDownloadAudit(context.Background(), clientB, 20); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	var phase string
	var ack bool
	if e = f.conn.QueryRow(context.Background(), "SELECT phase,audit_acked FROM file_download_sessions WHERE id=$1", session).Scan(&phase, &ack); e != nil || phase != "unknown" || !ack {
		t.Fatal(phase, ack, e)
	}
	if n, e := f.repo.RepairFileDownloadAudit(context.Background(), clientB, 20); e != nil || n != 0 {
		t.Fatal("repair duplicated", n, e)
	}
}
func TestFileDownloadRealTotalDeadline(t *testing.T) {
	dedicatedUpload(t)
	f := realFileMessageFixture(t)
	sign := downloadJWTIssuer(t, f)
	m := scanDownloadBody(t, f, []byte("total deadline"))
	objects := &gatedObjects{Store: realTransferObjects(t), entered: make(chan struct{}), delay: 55 * time.Second}
	base, _, spool := explicitDownload(t, f, objects, nil)
	run(t, f.conn, `CREATE FUNCTION delay_download_grant() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='file_download_authorize' THEN PERFORM pg_sleep(7);END IF;RETURN NEW;END $$;CREATE TRIGGER delay_download_grant BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION delay_download_grant()`)
	start := time.Now()
	res, e := (&http.Client{Timeout: 68 * time.Second}).Do(downloadRequest(t, base, sign("admin", time.Now().Add(2*time.Minute)), adminM, m.ID))
	elapsed := time.Since(start)
	if res != nil {
		data, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if bytes.Contains(data, []byte("total deadline")) {
			t.Fatal("late payload")
		}
	}
	if elapsed < 59*time.Second || elapsed > 66*time.Second {
		t.Fatal("total budget restarted", elapsed, e)
	}
	entries, readErr := os.ReadDir(spool)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatal("deadline retained content")
		}
	}
	t.Log("55s actual object read delay + 7s DB audit wait stopped within original total:", elapsed)
}
func TestFileDownloadProductionClosed(t *testing.T) {
	f := realFileMessageFixture(t)
	body := f.request(t, f.api, "GET", "/api/v1/files/"+freshFile().ID+"/content", nil, 503)
	if !bytes.Contains(body, []byte("file_download_unavailable")) {
		t.Fatal("production download opened")
	}
	TestFileMessageProductionClosed(t)
}

type observedDownload struct {
	*filedownload.Service
	evidence *tcpEvidence
}

func (o *observedDownload) Check(ctx context.Context, id access.TrustedIdentity, p *filedownload.Prepared) error {
	e := o.Service.Check(ctx, id, p)
	if e == nil {
		o.evidence.mu.Lock()
		o.evidence.checks = append(o.evidence.checks, time.Now())
		o.evidence.mu.Unlock()
	}
	return e
}
func TestFileDownloadRealTCPRevocation(t *testing.T) {
	dedicatedUpload(t)
	f := realFileMessageFixture(t)
	sign := downloadJWTIssuer(t, f)
	m := scanDownloadBody(t, f, bytes.Repeat([]byte("x"), 8<<20))
	evidence := &tcpEvidence{first: make(chan struct{})}
	base, _, _ := explicitDownload(t, f, realTransferObjects(t), evidence)
	address := strings.TrimPrefix(base, "http://")
	conn, e := net.Dial("tcp", address)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.(*net.TCPConn).SetReadBuffer(1024)
	fmt.Fprintf(conn, "GET /api/v1/files/%s/content HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nX-Acting-Membership-ID: %s\r\n\r\n", m.ID, address, sign("admin", time.Now().Add(time.Minute)), adminM)
	select {
	case <-evidence.first:
	case <-time.After(10 * time.Second):
		t.Fatal("real writer did not accept any output")
	}
	run(t, f.conn, "UPDATE user_organizations SET status='suspended' WHERE id=$1", adminM)
	revoked := time.Now()
	deadline := time.Now().Add(5 * time.Second)
	var phase string
	for time.Now().Before(deadline) {
		if e = f.conn.QueryRow(context.Background(), "SELECT phase FROM file_download_sessions WHERE file_id=$1", m.ID).Scan(&phase); e != nil {
			t.Fatal(e)
		}
		if phase == "interrupted" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	evidence.mu.Lock()
	defer evidence.mu.Unlock()
	if phase != "interrupted" || len(evidence.checks) == 0 || evidence.last.After(revoked.Add(time.Second+100*time.Millisecond)) || evidence.accepted >= 8<<20 {
		t.Fatal("TCP revocation not bounded", phase, evidence.accepted)
	}
	lastCheck := evidence.checks[len(evidence.checks)-1]
	if evidence.deadline.After(lastCheck.Add(time.Second + 10*time.Millisecond)) {
		t.Fatal("write deadline exceeded next review")
	}
	t.Logf("revocation=%s last-check=%s last-writer=%s writer-accepted=%d client-read=0 kernel-buffer-not-revocable=true", revoked.UTC().Format(time.RFC3339Nano), lastCheck.UTC().Format(time.RFC3339Nano), evidence.last.UTC().Format(time.RFC3339Nano), evidence.accepted)
}

func (o *observedDownload) Finish(ctx context.Context, p *filedownload.Prepared, r filedownload.Result) error {
	e := o.Service.Finish(ctx, p, r)
	if o.evidence.settled != nil {
		o.evidence.settled <- e
	}
	return e
}
