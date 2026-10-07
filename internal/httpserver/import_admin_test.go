package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	a "github.com/leileipei/Enterprise_IM/internal/importapply"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const importPath = "/api/admin/import-batches/90000000-0000-4000-8000-000000000004"

type importStub struct {
	pre   func(context.Context, access.ImportPrincipal) error
	apply func(context.Context, access.ImportPrincipal, string, []byte) (a.Result, error)
	get   func(context.Context, access.ImportPrincipal, string) (a.Result, error)
}

func (s importStub) Preauthorize(c context.Context, p access.ImportPrincipal) error {
	if s.pre != nil {
		return s.pre(c, p)
	}
	return nil
}
func (s importStub) Apply(c context.Context, p access.ImportPrincipal, id string, b []byte) (a.Result, error) {
	if s.apply != nil {
		return s.apply(c, p, id, b)
	}
	return a.Result{Receipt: a.Receipt{State: a.Applied}, Encoded: []byte(`{"state":"applied"}`)}, nil
}
func (s importStub) Get(c context.Context, p access.ImportPrincipal, id string) (a.Result, error) {
	if s.get != nil {
		return s.get(c, p, id)
	}
	return a.Result{Receipt: a.Receipt{State: a.Applied}, Encoded: []byte(`{"state":"applied"}`)}, nil
}
func importAuth(context.Context, string) (VerifiedIdentity, error) {
	return VerifiedIdentity{TenantID: "90000000-0000-4000-8000-000000000001", UserID: "90000000-0000-4000-8000-000000000002", ExpiresAt: time.Now().Add(time.Hour), Issuer: "https://sso.test", Subject: "actor"}, nil
}
func importRequest(method, path string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, path, body)
	r.Header.Set("Authorization", "Bearer verified")
	r.Header.Set("X-Acting-Membership-ID", "90000000-0000-4000-8000-000000000003")
	r.Header.Set("Content-Type", "application/json")
	return r
}
func TestAppendHTTPStatusContract(t *testing.T) {
	cases := []struct {
		name, method string
		err          error
		state        a.State
		replay       bool
		want         int
		code         string
	}{
		{"first", "POST", nil, a.Applied, false, 201, ""}, {"replay", "POST", nil, a.Applied, true, 200, ""}, {"rejected", "POST", nil, a.Rejected, false, 409, ""}, {"rejectedReplay", "POST", nil, a.Rejected, true, 409, ""}, {"getRejected", "GET", nil, a.Rejected, false, 200, ""},
		{"postBusy", "POST", a.ErrBusy, "", false, 409, "IMPORT_BUSY"}, {"getBusy", "GET", a.ErrBusy, "", false, 202, "IMPORT_BUSY"}, {"key", "POST", a.ErrKeyConflict, "", false, 409, "BATCH_KEY_CONFLICT"}, {"invalid", "POST", a.ErrInvalidInput, "", false, 422, "IMPORT_INPUT_INVALID"}, {"forbidden", "POST", a.ErrForbidden, "", false, 403, "IMPORT_FORBIDDEN"}, {"database", "POST", a.ErrDatabaseUnavailable, "", false, 503, "IMPORT_DATABASE_UNAVAILABLE"}, {"retry", "POST", a.ErrRetryable, "", false, 503, "IMPORT_RETRYABLE"}, {"audit", "POST", a.ErrAuditUnavailable, "", false, 503, "IMPORT_AUDIT_UNAVAILABLE"}, {"unknown", "POST", a.ErrCommitUnknown, "", false, 503, "COMMIT_OUTCOME_UNKNOWN"}, {"missing", "GET", a.ErrNotRecorded, "", false, 404, "IMPORT_NOT_RECORDED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := func() (a.Result, error) {
				return a.Result{Receipt: a.Receipt{State: tc.state}, Encoded: []byte(`{"state":"` + string(tc.state) + `"}`), Replay: tc.replay}, tc.err
			}
			s := importStub{apply: func(context.Context, access.ImportPrincipal, string, []byte) (a.Result, error) { return result() }, get: func(context.Context, access.ImportPrincipal, string) (a.Result, error) { return result() }}
			h, e := HandlerWithImports(http.NotFoundHandler(), authFunc(importAuth), s)
			if e != nil {
				t.Fatal(e)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, importRequest(tc.method, importPath, strings.NewReader(`{}`)))
			if w.Code != tc.want || w.Header().Get("Cache-Control") != "no-store" || tc.code != "" && !strings.Contains(w.Body.String(), tc.code) {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	h, _ := HandlerWithImports(http.NotFoundHandler(), authFunc(importAuth), importStub{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, importRequest("POST", "/api/admin/import-batches/not-a-uuid", strings.NewReader(`{}`)))
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}

type observedBody struct{ reads int }

func (b *observedBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (b *observedBody) Close() error             { return nil }
func TestAppendHTTPAuthBeforeRead(t *testing.T) {
	for _, unauth := range []bool{true, false} {
		b := &observedBody{}
		auth := authFunc(importAuth)
		if unauth {
			auth = authFunc(func(context.Context, string) (VerifiedIdentity, error) {
				return VerifiedIdentity{}, errors.New("bad token")
			})
		}
		h, _ := HandlerWithImports(http.NotFoundHandler(), auth, importStub{pre: func(context.Context, access.ImportPrincipal) error { return a.ErrForbidden }, apply: func(context.Context, access.ImportPrincipal, string, []byte) (a.Result, error) {
			t.Fatal("unauthorized busy probe")
			return a.Result{}, a.ErrBusy
		}})
		r := importRequest("POST", importPath, b)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 403
		if unauth {
			want = 401
		}
		if w.Code != want || b.reads != 0 {
			t.Fatal("unauthorized body read", w.Code, b.reads)
		}
	}
}
func TestAppendHTTPNoSecrets(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	marker := "NEVER_DISCLOSE_MARKER"
	h, _ := HandlerWithImports(http.NotFoundHandler(), authFunc(func(context.Context, string) (VerifiedIdentity, error) { return VerifiedIdentity{}, errors.New(marker) }), importStub{})
	r := importRequest("POST", "/api/admin/import-batches/"+marker, strings.NewReader(marker))
	r.Header.Set("Authorization", "Bearer "+marker)
	r.Header.Set("X-Request-ID", marker)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	h, _ = HandlerWithImports(http.NotFoundHandler(), authFunc(importAuth), importStub{apply: func(context.Context, access.ImportPrincipal, string, []byte) (a.Result, error) {
		return a.Result{}, errors.New(marker)
	}})
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, importRequest("POST", importPath, strings.NewReader(marker)))
	if strings.Contains(w.Body.String()+w2.Body.String()+logs.String(), marker) {
		t.Fatal("untrusted details leaked")
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatal("expected two safe rejection events", len(lines))
	}
	seen := map[string]bool{}
	for i, line := range lines {
		var event map[string]any
		if e := json.Unmarshal([]byte(line), &event); e != nil {
			t.Fatal("safe event missing", e)
		}
		id, ok := event["request_correlation_id"].(string)
		if !ok || len(id) != 32 || seen[id] {
			t.Fatal("unsafe or absent server correlation")
		}
		seen[id] = true
		code := []string{"IMPORT_UNAUTHORIZED", "IMPORT_DATABASE_UNAVAILABLE"}[i]
		if event["error_code"] != code {
			t.Fatal("unsafe event code")
		}
	}

}
func TestAppendHTTPIngressLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*http.Request)
		body   string
		want   int
	}{
		{"duplicateAuth", func(r *http.Request) { r.Header.Add("Authorization", "Bearer duplicate") }, "{}", 401},
		{"compressed", func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, "{}", 422},
		{"type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, "{}", 422},
		{"large", func(r *http.Request) {}, strings.Repeat(" ", 10*1024*1024+1), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := HandlerWithImports(http.NotFoundHandler(), authFunc(importAuth), importStub{})
			r := importRequest("POST", importPath, strings.NewReader(tc.body))
			tc.modify(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatal(w.Code)
			}
		})
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h, _ := HandlerWithImports(http.NotFoundHandler(), authFunc(importAuth), importStub{apply: func(context.Context, access.ImportPrincipal, string, []byte) (a.Result, error) {
		once.Do(func() { close(entered) })
		<-release
		return a.Result{Receipt: a.Receipt{State: a.Applied}, Encoded: []byte(`{}`)}, nil
	}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), importRequest("POST", importPath, strings.NewReader(`{}`)))
	}()
	<-entered
	w := httptest.NewRecorder()
	b := &observedBody{}
	h.ServeHTTP(w, importRequest("POST", importPath, b))
	if w.Code != 409 || b.reads != 0 {
		t.Fatal("POST admission queued/read")
	}
	g := httptest.NewRecorder()
	h.ServeHTTP(g, importRequest("GET", importPath, nil))
	if g.Code != 200 {
		t.Fatal("GET occupied POST slot")
	}
	close(release)
	<-done
}
