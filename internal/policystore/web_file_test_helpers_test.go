package policystore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/testfixtures"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"github.com/leileipei/Enterprise_IM/internal/filetransfer"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/leileipei/Enterprise_IM/internal/realtime"
	"github.com/redis/go-redis/v9"
)

type webFileFixture struct {
	real    *fileMessageRealFixture
	baseURL string
	server  *httptest.Server
	handler http.Handler
	private []string
	redis   *redis.Client
	stream  string
}

func requireWebFiles(t *testing.T) {
	t.Helper()
	for _, name := range []string{"IM_TEST_DATABASE_URL", "IM_TEST_REDIS_URL", "IM_TEST_S3_ENDPOINT", "IM_TEST_S3_BUCKET", "IM_TEST_FILE_UPLOAD_ACCESS_KEY", "IM_TEST_FILE_UPLOAD_SECRET_KEY", "IM_TEST_FILE_WORKER_ACCESS_KEY", "IM_TEST_FILE_WORKER_SECRET_KEY", "IM_TEST_QPDF_PATH", "IM_TEST_CLAMD_SOCKET", "IM_TEST_SCANNER_MANIFEST", "IM_TEST_BROWSER_NODE", "CHROMIUM_EXECUTABLE"} {
		if os.Getenv(name) == "" {
			t.Fatal("required runtime missing:", name)
		}
	}
}
func newWebFileFixture(t *testing.T) *webFileFixture {
	t.Helper()
	requireWebFiles(t)
	dedicatedUpload(t)
	real := realFileMessageFixture(t)
	run(t, real.conn, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES($1,$2,$3,$4,'group_admin','2020-01-01')`, freshFile().ID, tenantA, adminM, orgA)
	f := &webFileFixture{real: real, baseURL: real.web}
	// Each fixture gets a fresh schema. The versioned bucket is P4-25 owned;
	// its existing role credentials are never printed or written to evidence.
	upload, e := filetransfer.NewService(real.repo, realTransferObjects(t), filepath.Join(t.TempDir(), "upload"), uploadOwner)
	if e != nil {
		t.Fatal(e)
	}
	workerObjects := scannerObjects(t)
	download, e := filedownload.NewService(real.repo, workerObjects, filepath.Join(t.TempDir(), "download"), uploadOwner)
	if e != nil {
		t.Fatal(e)
	}
	scanner := actualScanner(t)
	auth := realDownloadAuth{fileMessageProductionAuth{api: real.api, client: &http.Client{Timeout: 3 * time.Second}}}
	fallbackURL, _ := url.Parse(real.api)
	fallback := httputil.NewSingleHostReverseProxy(fallbackURL)
	h, e := httpserver.HandlerWithFileMessages(fallback, auth, real.repo, real.repo)
	if e != nil {
		t.Fatal(e)
	}
	h, e = httpserver.HandlerWithMessageSearch(h, auth, real.repo)
	if e != nil {
		t.Fatal(e)
	}
	h, e = httpserver.HandlerWithCrossMessageSearch(h, auth, real.repo)
	if e != nil {
		t.Fatal(e)
	}
	h, e = httpserver.HandlerWithFileMetadata(h, auth, real.repo)
	if e != nil {
		t.Fatal(e)
	}
	h, e = httpserver.HandlerWithFileContent(h, auth, upload)
	if e != nil {
		t.Fatal(e)
	}
	h, e = httpserver.HandlerWithFileDownload(h, auth, download)
	if e != nil {
		t.Fatal(e)
	}
	admin := access.Service{DB: real.pool}
	h, e = httpserver.HandlerWithFileUploadPolicy(h, auth, admin)
	if e != nil {
		t.Fatal(e)
	}
	h, e = httpserver.HandlerWithFileRetentionPolicy(h, auth, admin)
	if e != nil {
		t.Fatal(e)
	}
	h, e = httpserver.HandlerWithFileSearch(h, auth, real.repo)
	if e != nil {
		t.Fatal(e)
	}
	h, e = httpserver.HandlerWithFileCapabilities(h, auth, admin, httpserver.FileCapabilities{UploadEnabled: true, MessageSendEnabled: true, DownloadEnabled: true, FilenameSearchEnabled: true})
	if e != nil {
		t.Fatal(e)
	}
	opts, e := redis.ParseURL(os.Getenv("IM_TEST_REDIS_URL"))
	if e != nil {
		t.Fatal(e)
	}
	f.redis = redis.NewClient(opts)
	if e = f.redis.Ping(context.Background()).Err(); e != nil {
		t.Fatal("real Redis unavailable")
	}
	f.stream = fmt.Sprintf("enterprise-im:test:p425:%d", time.Now().UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	if e = outbox.RefreshPublisherPresence(ctx, f.redis, f.stream); e != nil {
		t.Fatal("real publisher presence")
	}
	fanout, e := realtime.StartStreamFanout(ctx, f.redis, f.stream, real.repo)
	if e != nil {
		t.Fatal("real fanout unavailable")
	}
	h, e = httpserver.HandlerWithRealtimeNotifications(h, auth, real.repo, realtime.RedisTickets{Client: f.redis}, ctx, fanout)
	if e != nil {
		t.Fatal(e)
	}
	fileRoutes := h
	h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__p425/download-idle" {
			var pending int
			e := real.pool.QueryRow(r.Context(), "SELECT count(*) FROM file_download_sessions WHERE NOT audit_acked").Scan(&pending)
			w.Header().Set("Cache-Control", "no-store")
			json.NewEncoder(w).Encode(map[string]bool{"idle": e == nil && pending == 0})
			return
		}
		fileRoutes.ServeHTTP(w, r)
	})
	f.handler = h
	f.server = httptest.NewUnstartedServer(h)
	f.server.Config.WriteTimeout = 160 * time.Second
	f.server.Start()
	target, _ := url.Parse(f.server.URL)
	real.webBackend.Store(httputil.NewSingleHostReverseProxy(target))
	done := make(chan struct{})
	workerErrors := make(chan error, 1)
	worker := filetransfer.ScanWorker{Repo: real.repo, Objects: workerObjects, Scanner: scanner, SpoolDir: filepath.Join(t.TempDir(), "scan"), OwnerID: uploadOwner}
	publisher := outbox.Worker{DB: real.pool, Publisher: outbox.RedisPublisher{Client: f.redis, Stream: f.stream}}
	go func() {
		defer close(done)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := outbox.RefreshPublisherPresence(ctx, f.redis, f.stream); err != nil && ctx.Err() == nil {
					select {
					case workerErrors <- err:
					default:
					}
					return
				}
				if _, err := worker.RunOnce(ctx); err != nil && ctx.Err() == nil {
					select {
					case workerErrors <- err:
					default:
					}
					return
				}
				// Actual machine-audit repair makes repeated downloads admissible.
				if _, err := real.repo.RepairFileDownloadAudit(ctx, uploadOwner, 20); err != nil && ctx.Err() == nil && !errors.Is(err, filedownload.ErrBusy) && !errors.Is(err, filedownload.ErrUnavailable) {
					select {
					case workerErrors <- err:
					default:
					}
					return
				}
				if _, err := publisher.ProcessOne(ctx); err != nil && ctx.Err() == nil {
					select {
					case workerErrors <- err:
					default:
					}
					return
				}
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		<-fanout.Done()
		f.server.Close()
		if e := upload.Close(); e != nil {
			t.Error("upload cleanup", e)
		}
		if e := download.Close(); e != nil {
			t.Error("download cleanup", e)
		}
		f.redis.Del(context.Background(), f.stream, outbox.PublisherPresenceKey(f.stream))
		f.redis.Close()
		select {
		case <-workerErrors:
			t.Error("real file worker failed")
		default:
		}
	})
	return f
}
func (f *webFileFixture) group(t *testing.T) string {
	t.Helper()
	g, e := f.real.repo.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2, groupMemberC))
	if e != nil {
		t.Fatal(e)
	}
	return g.ID
}

type webFileSample struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	MIME      string `json:"mime"`
	Outcome   string `json:"outcome,omitempty"`
	SavedName string `json:"savedName,omitempty"`
}

func webPDF() []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 20 20] /Resources << >> /Contents 4 0 R >>", "<< /Length 0 >>\nstream\nendstream"}
	offsets := []int{0}
	for i, s := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, s)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 5\n0000000000 65535 f \n")
	for _, o := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size 5 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return b.Bytes()
}
func (f *webFileFixture) samples(t *testing.T, rejected bool) []webFileSample {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	picture := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			picture.Set(x, y, color.RGBA{20, 40, 60, 255})
		}
	}
	var pngBytes, jpegBytes bytes.Buffer
	if e := png.Encode(&pngBytes, picture); e != nil {
		t.Fatal(e)
	}
	if e := jpeg.Encode(&jpegBytes, picture, nil); e != nil {
		t.Fatal(e)
	}
	type sample struct {
		name, mime string
		body       []byte
		outcome    string
	}
	data := []sample{{"集团报告.txt", "text/plain", []byte("集团 日本語 😀\n"), ""}, {"集团报告.pdf", "application/pdf", webPDF(), ""}, {"集团图片.png", "image/png", pngBytes.Bytes(), ""}, {"集团照片.jpg", "image/jpeg", jpegBytes.Bytes(), ""}}
	if rejected {
		plain := filepath.Join(dir, "plain.pdf")
		encrypted := filepath.Join(dir, "encrypted.pdf")
		if e := os.WriteFile(plain, webPDF(), 0600); e != nil {
			t.Fatal(e)
		}
		if e := exec.Command(os.Getenv("IM_TEST_QPDF_PATH"), "--encrypt", "user", "owner", "256", "--", plain, encrypted).Run(); e != nil {
			t.Fatal("real encrypted PDF fixture", e)
		}
		cipher, e := os.ReadFile(encrypted)
		if e != nil {
			t.Fatal(e)
		}
		data = []sample{{"拒绝病毒.txt", "text/plain", []byte("X5O!P%@AP[4\\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*"), "scan"}, {"拒绝损坏.pdf", "application/pdf", []byte("%PDF-1.4\ninvalid\n"), "scan"}, {"拒绝加密.pdf", "application/pdf", cipher, "scan"}, {"拒绝伪装.txt", "text/plain", pngBytes.Bytes(), "scan"}, {"拒绝超限.txt", "text/plain", bytes.Repeat([]byte("x"), 26214401), "select"}}
	}
	samples := make([]webFileSample, 0, len(data))
	for i, s := range data {
		p := filepath.Join(dir, fmt.Sprintf("sample-%d", i))
		if e := os.WriteFile(p, s.body, 0600); e != nil {
			t.Fatal(e)
		}
		samples = append(samples, webFileSample{Path: p, Name: s.name, MIME: s.mime, Outcome: s.outcome})
		f.private = append(f.private, s.name)
	}
	return samples
}
func (f *webFileFixture) browser(t *testing.T, name string, data map[string]any) {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	data["baseURL"] = f.baseURL
	data["evidenceDir"] = dir
	p := filepath.Join(dir, "input.json")
	b, e := json.Marshal(data)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	script, e := filepath.Abs("../webclient/e2e/" + name + ".cjs")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Getenv("IM_TEST_BROWSER_NODE"), script)
	cmd.Env = testfixtures.BrowserEnvironment(map[string]string{"WEB_FILE_INPUT": p})
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	out, e := cmd.CombinedOutput()
	if e != nil {
		var states string
		f.real.pool.QueryRow(context.Background(), `SELECT COALESCE(string_agg(state::text,','),'') FROM file_objects`).Scan(&states)
		t.Fatal("real browser flow failed", name, e, string(out), "DB states:", states)
	}
	var facts map[string]bool
	if e = json.Unmarshal(bytes.TrimSpace(out), &facts); e != nil || len(facts) == 0 {
		t.Fatal("browser evidence invalid")
	}
	for k, v := range facts {
		if !v {
			t.Fatal("browser evidence false", k)
		}
	}
	t.Log("real browser evidence", name, facts)
}
func (f *webFileFixture) assertPrivate(t *testing.T) {
	t.Helper()
	logs, e := os.ReadFile(f.real.apiLog)
	if e != nil {
		t.Fatal(e)
	}
	assertFileMessagePrivate(t, logs, append(f.private, f.real.token)...)
}
func (f *webFileFixture) request(t *testing.T, method, path string, body any, want int) []byte {
	t.Helper()
	var data []byte
	if body != nil {
		var e error
		data, e = json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
	}
	r := productionRequest(t, method, f.server.URL, path, f.real.token, data, true)
	res, e := (&http.Client{Timeout: 10 * time.Second}).Do(r)
	if e != nil {
		t.Fatal("fixture HTTP transport failed")
	}
	defer res.Body.Close()
	b, e := io.ReadAll(res.Body)
	if e != nil || res.StatusCode != want {
		t.Fatal("fixture HTTP unexpected status", method, strings.Split(path, "?")[0], res.StatusCode, want)
	}
	return b
}

// This issuer seam uses the same real JWKS and production verification process.
func (f *webFileFixture) peerToken(t *testing.T) string {
	t.Helper()
	run(t, f.real.conn, `INSERT INTO external_identities(issuer,subject,tenant_id,user_id) VALUES($1,'p425-peer',$2,$3)`, f.real.issuer, tenantA, personA)
	token := f.real.issueToken("p425-peer")
	f.private = append(f.private, token)
	return token
}
func (f *webFileFixture) requestAs(t *testing.T, token, member, method, path string, body any, want int) []byte {
	t.Helper()
	var data []byte
	var e error
	if body != nil {
		data, e = json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
	}
	r, e := http.NewRequest(method, f.server.URL+path, bytes.NewReader(data))
	if e != nil {
		t.Fatal("request creation")
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Acting-Membership-ID", member)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	res, e := (&http.Client{Timeout: 10 * time.Second}).Do(r)
	if e != nil {
		t.Fatal("actual request failed")
	}
	defer res.Body.Close()
	b, e := io.ReadAll(res.Body)
	if e != nil || res.StatusCode != want {
		t.Fatal("actual request status", method, strings.Split(path, "?")[0], res.StatusCode, want)
	}
	return b
}
func (f *webFileFixture) uploadReady(t *testing.T, cid, name string, body []byte) files.Metadata {
	t.Helper()
	var dto struct {
		FileID string `json:"file_id"`
	}
	if e := json.Unmarshal(f.request(t, "POST", "/api/v1/conversations/"+cid+"/files", map[string]any{"upload_request_id": freshFile().ID, "original_filename": name, "declared_media_type": "text/plain", "declared_size_bytes": fmt.Sprint(len(body))}, 201), &dto); e != nil || dto.FileID == "" {
		t.Fatal("reservation DTO")
	}
	r, e := http.NewRequest("PUT", f.server.URL+"/api/v1/files/"+dto.FileID+"/content", bytes.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Authorization", "Bearer "+f.real.token)
	r.Header.Set("X-Acting-Membership-ID", adminM)
	r.Header.Set("Content-Type", "application/octet-stream")
	res, e := (&http.Client{Timeout: 15 * time.Second}).Do(r)
	if e != nil {
		t.Fatal("real PUT failed")
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal("real PUT status", res.StatusCode)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		m, e := f.real.repo.GetOwnFile(context.Background(), publisher(), dto.FileID)
		if e == nil && m.State == files.StateReady {
			f.private = append(f.private, name, string(body))
			return m
		}
		if e == nil && (m.State == files.StateRejected || m.State == files.StateScanFailed) {
			t.Fatal("actual scanner did not accept TXT", m.State)
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("real scanner readiness deadline")
	return files.Metadata{}
}
func (f *webFileFixture) sendFile(t *testing.T, cid, kind, id, caption string) {
	t.Helper()
	prefix := "conversations"
	if kind == "group" {
		prefix = "groups"
	}
	f.request(t, "POST", "/api/v1/"+prefix+"/"+cid+"/messages", map[string]string{"client_msg_id": clientUUIDv7(time.Now(), 8700+int(time.Now().UnixNano()%100000)), "message_type": "file", "file_id": id, "caption": caption}, 200)
	if caption != "" {
		f.private = append(f.private, caption)
	}
}

// Faults are applied only after the actual authenticated S3 download completes.
// The temporary same-origin proxy never bypasses identity, scan or visibility.
func (f *webFileFixture) replaceHandler(t *testing.T, h http.Handler) {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.FlushInterval = -1 // Expose the deliberately flushed real 200 before the injected cut.
	f.real.webBackend.Store(proxy)
}
func (f *webFileFixture) content(t *testing.T, r *http.Request) (http.Header, []byte, int) {
	req, e := http.NewRequestWithContext(r.Context(), "GET", f.server.URL+r.URL.RequestURI(), nil)
	if e != nil {
		return nil, nil, 503
	}
	req.Header = r.Header.Clone()
	res, e := (&http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if e != nil {
		return nil, nil, 503
	}
	defer res.Body.Close()
	body, e := io.ReadAll(res.Body)
	if e != nil {
		return nil, nil, 503
	}
	if res.StatusCode == 200 {
		// The source is fully read. Observe actual machine-audit completion
		// before faulting the separate client leg; do not guess from EOF.
		deadline := time.Now().Add(5 * time.Second)
		for {
			var pending int
			err := f.real.pool.QueryRow(r.Context(), "SELECT count(*) FROM file_download_sessions WHERE NOT audit_acked").Scan(&pending)
			if err != nil {
				return nil, nil, 503
			}
			if pending == 0 {
				break
			}
			if time.Now().After(deadline) {
				return nil, nil, 503
			}
			select {
			case <-r.Context().Done():
				return nil, nil, 503
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	return res.Header, body, res.StatusCode
}
func copyWebHeaders(w http.ResponseWriter, h http.Header) {
	for k, vs := range h {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}
func (f *webFileFixture) installDownloadFaults(t *testing.T) {
	var n atomic.Int64
	u, _ := url.Parse(f.server.URL)
	fallback := httputil.NewSingleHostReverseProxy(u)
	f.replaceHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__p425/download-fault-state" {
			json.NewEncoder(w).Encode(map[string]int64{"count": n.Load()})
			return
		}
		if r.URL.Path == "/__p425/download-idle" {
			var pending int
			err := f.real.pool.QueryRow(r.Context(), "SELECT count(*) FROM file_download_sessions WHERE NOT audit_acked").Scan(&pending)
			json.NewEncoder(w).Encode(map[string]bool{"idle": err == nil && pending == 0})
			return
		}
		if r.Method != "GET" || !strings.HasPrefix(r.URL.Path, "/api/v1/files/") || !strings.HasSuffix(r.URL.Path, "/content") {
			fallback.ServeHTTP(w, r)
			return
		}
		h, b, status := f.content(t, r)
		if status != 200 {
			var dto struct {
				Code string `json:"error_code"`
			}
			if json.Unmarshal(b, &dto) == nil {
				for _, code := range []string{"download_audit_pending", "download_in_progress", "file_download_unavailable", "not_found", "unauthorized"} {
					if dto.Code == code {
						t.Log("actual upstream fault admission", status, code)
					}
				}
			}
			copyWebHeaders(w, h)
			w.WriteHeader(status)
			w.Write(b)
			return
		}
		i := n.Add(1)
		copyWebHeaders(w, h)
		switch i {
		case 1:
			w.WriteHeader(200)
			w.Write(b[:len(b)/2])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		case 2:
			w.Header().Set("Content-Length", fmt.Sprint(len(b)+3))
			w.WriteHeader(200)
			w.Write(b)
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		case 3:
			w.Header().Del("Content-Length")
			w.Header().Set("Location", r.URL.Path)
			w.WriteHeader(302)
		case 4:
			w.Header().Set("Content-Length", "26214401")
			w.WriteHeader(200)
			w.Write(b)
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		default:
			http.Error(w, "fault fixture exhausted", 500)
		}
	}))
	t.Cleanup(func() {
		if n.Load() != 4 {
			t.Error("not all real download faults exercised", n.Load())
		}
	})
}
func (f *webFileFixture) installContextStream(t *testing.T) {
	var armed, reached atomic.Bool
	u, _ := url.Parse(f.server.URL)
	fallback := httputil.NewSingleHostReverseProxy(u)
	f.replaceHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__p425/read-arm" {
			armed.Store(true)
			w.WriteHeader(204)
			return
		}
		if r.URL.Path == "/__p425/read-state" {
			json.NewEncoder(w).Encode(map[string]bool{"reached": reached.Load()})
			return
		}
		if armed.Load() && r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/files/") && strings.HasSuffix(r.URL.Path, "/content") {
			h, b, status := f.content(t, r)
			copyWebHeaders(w, h)
			w.WriteHeader(status)
			if status != 200 {
				w.Write(b)
				return
			}
			armed.Store(false)
			w.Write(b[:len(b)/2])
			w.(http.Flusher).Flush()
			reached.Store(true)
			select {
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
			panic(http.ErrAbortHandler)
		}
		fallback.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		if !reached.Load() {
			t.Error("real partial stream never reached")
		}
	})
}
func (f *webFileFixture) installPolicyConflict(t *testing.T) {
	var injected atomic.Bool
	u, _ := url.Parse(f.server.URL)
	fallback := httputil.NewSingleHostReverseProxy(u)
	f.replaceHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && r.URL.Path == "/api/v1/admin/file-upload-policy" && injected.CompareAndSwap(false, true) {
			s := access.Service{DB: f.real.pool}
			current, e := s.GetFileUploadPolicy(r.Context(), publisher())
			if e == nil {
				_, e = s.SetFileUploadPolicy(r.Context(), publisher(), access.FileUploadPolicyChange{Policy: current.Policy, ExpectedVersion: current.Policy.Version, ApprovalReference: "P425-CONCURRENT-APPROVED"})
			}
			if e != nil {
				http.Error(w, "concurrent approved update failed", 503)
				return
			}
		}
		fallback.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		if !injected.Load() {
			t.Error("concurrent CAS was not exercised")
		}
	})
}

func webFileAwaitCondition(t *testing.T) {
	f := newWebFileFixture(t)
	var calls atomic.Int64
	routes := f.handler
	f.replaceHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__p425/poll-proof" {
			w.Header().Set("Cache-Control", "no-store")
			json.NewEncoder(w).Encode(map[string]bool{"ready": calls.Add(1) >= 2})
			return
		}
		routes.ServeHTTP(w, r)
	}))
	f.browser(t, "file_poll", map[string]any{})
	if calls.Load() < 2 {
		t.Fatal("false predicate was not retried")
	}
}
