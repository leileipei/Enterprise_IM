//go:build nativeclientintegration && (darwin || linux)

package policystore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
)

// Pause only the test's scheduling boundary. The actual download service,
// repository, TLS stream and device API perform every business operation.
type deviceStreamGate struct {
	*filedownload.Service
	beforeFirst bool
	reached     chan struct{}
	resume      chan struct{}
	settled     chan error
	once        sync.Once
	mu          sync.Mutex
	checks      int
}

func (g *deviceStreamGate) wait(ctx context.Context) error {
	g.once.Do(func() { close(g.reached) })
	select {
	case <-g.resume:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (g *deviceStreamGate) Authorize(ctx context.Context, id access.TrustedIdentity, p *filedownload.Prepared) error {
	if g.beforeFirst {
		if e := g.wait(ctx); e != nil {
			return e
		}
	}
	return g.Service.Authorize(ctx, id, p)
}
func (g *deviceStreamGate) Check(ctx context.Context, id access.TrustedIdentity, p *filedownload.Prepared) error {
	g.mu.Lock()
	g.checks++
	n := g.checks
	g.mu.Unlock()
	if !g.beforeFirst && n == 2 {
		if e := g.wait(ctx); e != nil {
			return e
		}
	}
	return g.Service.Check(ctx, id, p)
}
func (g *deviceStreamGate) Finish(ctx context.Context, p *filedownload.Prepared, r filedownload.Result) error {
	e := g.Service.Finish(ctx, p, r)
	g.settled <- e
	return e
}

type streamDeviceGrant struct {
	ID    string `json:"device_id"`
	Token string `json:"device_token"`
}

func streamRegisterDevice(t *testing.T, f *fileMessageRealFixture, n int) streamDeviceGrant {
	t.Helper()
	body := fmt.Sprintf(`{"registration_id":"019a13c0-0000-7000-8000-%012d","platform":"android","name":"stream probe"}`, n)
	req, e := http.NewRequest("POST", f.api+"/api/v1/devices", bytes.NewBufferString(body))
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("X-Acting-Membership-ID", adminM)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	res, e := client.Do(req)
	if e != nil {
		t.Fatal("device registration transport", e)
	}
	defer res.Body.Close()
	if res.StatusCode != 201 {
		t.Fatalf("device registration status=%d", res.StatusCode)
	}
	var grant streamDeviceGrant
	if e = json.NewDecoder(res.Body).Decode(&grant); e != nil || grant.ID == "" || grant.Token == "" {
		t.Fatal("device grant absent", e)
	}
	return grant
}
func streamRevokeDevice(t *testing.T, f *fileMessageRealFixture, grant streamDeviceGrant) {
	t.Helper()
	req, e := http.NewRequest("DELETE", f.api+"/api/v1/devices/"+grant.ID, nil)
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	res, e := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if e != nil {
		t.Fatal("device revoke transport", e)
	}
	defer res.Body.Close()
	if res.StatusCode != 204 {
		t.Fatalf("device revoke status=%d", res.StatusCode)
	}
}
func streamDownloadRequest(t *testing.T, base, token, file string, device streamDeviceGrant) *http.Request {
	t.Helper()
	req := downloadRequest(t, base, token, adminM, file)
	req.Header.Set("X-IM-Device-ID", device.ID)
	req.Header.Set("X-IM-Device-Token", device.Token)
	return req
}

func TestFileDownloadNativeDeviceStreamRevocation(t *testing.T) {
	for _, before := range []bool{true, false} {
		name := "after-first-chunk"
		if before {
			name = "before-first-byte"
		}
		t.Run(name, func(t *testing.T) {
			dedicatedUpload(t)
			parent := filepath.Dir(os.Getenv("IM_TEST_MOBILE_FACTS"))
			if parent == "." {
				t.Fatal("explicit private evidence directory required")
			}
			out := filepath.Join(parent, name)
			if e := os.Mkdir(out, 0700); e != nil {
				t.Fatal(e)
			}
			t.Setenv("IM_TEST_MOBILE_FACTS", filepath.Join(out, "unused-facts.json"))
			f := realFileMessageFixture(t)
			// Retain the initial legacy fixture API log before its Go temporary directory
			// disappears; the device-enabled production API uses retained log helpers.
			t.Cleanup(func() {
				data, e := os.ReadFile(f.apiLog)
				if e != nil {
					t.Error(e)
					return
				}
				if e = os.WriteFile(filepath.Join(out, "bootstrap-api.log"), data, 0600); e != nil {
					t.Error(e)
				}
			})
			f.api = crossRetainedAPI(t, f.binary, append(f.apiEnv, "IM_RECEIPTS_ENABLED=true"))
			body := bytes.Repeat([]byte("x"), 8<<20)
			m := scanDownloadBody(t, f, body)
			old, other := streamRegisterDevice(t, f, 51), streamRegisterDevice(t, f, 52)
			spool := filepath.Join(t.TempDir(), "download")
			service, e := filedownload.NewService(f.repo, realTransferObjects(t), spool, uploadOwner)
			if e != nil {
				t.Fatal(e)
			}
			defer service.Close()
			gate := &deviceStreamGate{Service: service, beforeFirst: before, reached: make(chan struct{}), resume: make(chan struct{}), settled: make(chan error, 4)}
			var release sync.Once
			unblock := func() { release.Do(func() { close(gate.resume) }) }
			handler, e := httpserver.HandlerWithFileDownload(http.NotFoundHandler(), realDownloadAuth{fileMessageProductionAuth{api: f.api, client: &http.Client{Timeout: time.Second}}}, gate)
			if e != nil {
				t.Fatal(e)
			}
			server := httptest.NewTLSServer(handler)
			defer server.Close()
			defer unblock()
			client := server.Client()
			client.Timeout = 20 * time.Second
			defer client.CloseIdleConnections()
			type outcome struct {
				status int
				n      int
				err    error
				data   []byte
			}
			done := make(chan outcome, 1)
			go func() {
				res, e := client.Do(streamDownloadRequest(t, server.URL, f.token, m.ID, old))
				if e != nil {
					done <- outcome{err: e}
					return
				}
				defer res.Body.Close()
				data, e := io.ReadAll(res.Body)
				done <- outcome{res.StatusCode, len(data), e, data}
			}()
			select {
			case <-gate.reached:
			case <-time.After(12 * time.Second):
				t.Fatal("controlled real download boundary not reached")
			}
			// This is a separate actual production API request and committed revocation.
			streamRevokeDevice(t, f, old)
			unblock()
			var got outcome
			select {
			case got = <-done:
			case <-time.After(12 * time.Second):
				t.Fatal("download did not stop")
			}
			if before {
				if got.status != 401 || got.err != nil {
					t.Fatalf("before first byte status=%d err=%v", got.status, got.err)
				}
				var denied struct {
					Code string `json:"error_code"`
				}
				if json.Unmarshal(got.data, &denied) != nil || denied.Code != "invalid_device" {
					t.Fatal("revoked device denial missing")
				}
			} else if got.status != 200 || !errors.Is(got.err, io.ErrUnexpectedEOF) || got.n != 32768 || !bytes.Equal(got.data, body[:32768]) {
				t.Fatalf("partial download status=%d bytes=%d err=%v", got.status, got.n, got.err)
			}
			select {
			case e := <-gate.settled:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("terminal settlement missing")
			}
			var phase, reason string
			var written int64
			if e = f.conn.QueryRow(context.Background(), "SELECT phase,reason_code,bytes_written FROM file_download_sessions WHERE file_id=$1", m.ID).Scan(&phase, &reason, &written); e != nil {
				t.Fatal(e)
			}
			want := int64(32768)
			if before {
				want = 0
			}
			if phase != "interrupted" || reason != "authorization_revoked" || written != want {
				t.Fatalf("terminal phase=%s reason=%s bytes=%d", phase, reason, written)
			}
			if n, e := f.repo.RepairFileDownloadAudit(context.Background(), clientB, 20); e != nil || n != 1 {
				t.Fatalf("repair count=%d err=%v", n, e)
			}
			// A fresh request with the revoked grant must not read from an old spool.
			res, e := client.Do(streamDownloadRequest(t, server.URL, f.token, m.ID, old))
			if e != nil {
				t.Fatal(e)
			}
			denial, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 401 || !bytes.Contains(denial, []byte("invalid_device")) {
				t.Fatal("old device replay was not denied", res.StatusCode)
			}
			// The independent surviving device still downloads the complete real object.
			res, e = client.Do(streamDownloadRequest(t, server.URL, f.token, m.ID, other))
			if e != nil {
				t.Fatal(e)
			}
			complete, e := io.ReadAll(res.Body)
			res.Body.Close()
			if e != nil || res.StatusCode != 200 || !bytes.Equal(complete, body) {
				t.Fatal("surviving device integrity failed", res.StatusCode, e)
			}
			select {
			case e := <-gate.settled:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("survivor settlement missing")
			}
			if n, e := f.repo.RepairFileDownloadAudit(context.Background(), clientB, 20); e != nil || n != 1 {
				t.Fatalf("survivor audit count=%d err=%v", n, e)
			}
			var terminals, acked, revoked int
			if e = f.conn.QueryRow(context.Background(), "SELECT count(*),count(*) FILTER(WHERE audit_acked) FROM file_download_sessions WHERE file_id=$1", m.ID).Scan(&terminals, &acked); e != nil || terminals != 2 || acked != 2 {
				t.Fatal("audit terminal facts", terminals, acked, e)
			}
			if e = f.conn.QueryRow(context.Background(), "SELECT count(*) FROM user_devices WHERE revoked_at IS NOT NULL").Scan(&revoked); e != nil || revoked != 1 {
				t.Fatal("revocation fact", revoked, e)
			}
			entries, e := os.ReadDir(spool)
			if e != nil {
				t.Fatal(e)
			}
			for _, entry := range entries {
				if entry.IsDir() {
					t.Fatal("private download spool retained")
				}
			}
			sum := sha256.Sum256(complete)
			facts := map[string]any{"case": name, "response_status": got.status, "body_bytes_before_stop": want, "partial_body_unexpected_eof": !before, "phase": phase, "reason": reason, "terminal_sessions": terminals, "audited_sessions": acked, "revoked_devices": revoked, "old_grant_new_request_status": 401, "survivor_status": 200, "survivor_bytes": len(complete), "survivor_sha256": hex.EncodeToString(sum[:]), "private_spool_empty": true, "controlled_scheduling": true, "real_tls_stream": true}
			data, e := json.MarshalIndent(facts, "", "  ")
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(out, "facts.json"), data, 0600); e != nil {
				t.Fatal(e)
			}
			t.Logf("native device stream %s stopped bytes=%d; two audited terminals; survivor bytes=%d", name, want, len(complete))
		})
	}
}
