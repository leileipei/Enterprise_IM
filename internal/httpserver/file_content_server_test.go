package httpserver

import (
	"bufio"
	"context"
	"fmt"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/filetransfer"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type contentFunc func(context.Context, access.TrustedIdentity, string, io.Reader) (files.Metadata, error)

func (f contentFunc) Upload(ctx context.Context, id access.TrustedIdentity, s string, r io.Reader) (files.Metadata, error) {
	return f(ctx, id, s, r)
}
func contentRequest(path string, body io.Reader) *http.Request {
	r := adminRequest(http.MethodPut, path)
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Body = io.NopCloser(body)
	r.RequestURI = ""
	return r
}
func contentURL(s *httptest.Server) string { return s.URL + "/api/v1/files/" + actorID + "/content" }
func TestFileContentServerStrict(t *testing.T) {
	svc := contentFunc(func(_ context.Context, _ access.TrustedIdentity, _ string, r io.Reader) (files.Metadata, error) {
		b, e := io.ReadAll(r)
		if e != nil {
			return files.Metadata{}, e
		}
		if string(b) != "x" {
			return files.Metadata{}, files.ErrInvalidFileSize
		}
		return files.Metadata{ID: actorID, State: files.StateUploaded}, nil
	})
	h, e := HandlerWithFileContent(Handler(nil), authFunc(verified), svc)
	if e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	for _, tc := range []struct {
		method, query, typ, encoding, body string
		want                               int
	}{{"PUT", "", "application/octet-stream", "", "x", 200}, {"POST", "", "application/octet-stream", "", "x", 405}, {"PUT", "?unknown=1", "application/octet-stream", "", "x", 400}, {"PUT", "", "multipart/form-data", "", "x", 400}, {"PUT", "", "application/octet-stream", "gzip", "x", 400}, {"PUT", "", "application/octet-stream", "", "", 400}} {
		req := contentRequest(contentURL(srv)+tc.query, strings.NewReader(tc.body))
		req.Method = tc.method
		req.Header.Set("Content-Type", tc.typ)
		if tc.encoding != "" {
			req.Header.Set("Content-Encoding", tc.encoding)
		}
		resp, e := srv.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.want || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatal(tc, resp.StatusCode)
		}
	}
}
func TestFileContentServerDeadlines(t *testing.T) {
	if filetransfer.ReceiveTimeout != 60*time.Second || filetransfer.UploadTimeout != 150*time.Second {
		t.Fatal("changed default bounds")
	}
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(12 * time.Second)
		w.Write([]byte("json late"))
	})
	svc := contentFunc(func(_ context.Context, _ access.TrustedIdentity, _ string, r io.Reader) (files.Metadata, error) {
		if _, e := io.ReadAll(r); e != nil {
			return files.Metadata{}, e
		}
		time.Sleep(12 * time.Second)
		return files.Metadata{ID: actorID, State: files.StateUploaded}, nil
	})
	h, e := HandlerWithFileContent(fallback, authFunc(verified), svc)
	if e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Config.WriteTimeout = 10 * time.Second
	srv.Start()
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 40 * time.Second
	resp, e := client.Do(contentRequest(contentURL(srv), strings.NewReader("x")))
	if e != nil {
		t.Fatal("12s upload lost to old deadline", e)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	reused := false
	trace := &httptrace.ClientTrace{GotConn: func(c httptrace.GotConnInfo) { reused = reused || c.Reused }}
	req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), "POST", srv.URL+"/json", strings.NewReader("x"))
	resp, e = client.Do(req)
	if e == nil {
		resp.Body.Close()
		t.Fatal("JSON 10s WriteTimeout was widened")
	}
	if !reused {
		t.Fatal("next JSON did not use upload connection")
	}
}
func TestFileContentServerDefaultReceiveTimeout(t *testing.T) {
	svc := contentFunc(func(_ context.Context, _ access.TrustedIdentity, _ string, r io.Reader) (files.Metadata, error) {
		_, e := io.ReadAll(r)
		if e == nil {
			t.Error("unfinished request accepted")
		}
		return files.Metadata{}, files.ErrDependencyUnavailable
	})
	h, e := HandlerWithFileContent(Handler(nil), authFunc(verified), svc)
	if e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Config.WriteTimeout = 10 * time.Second
	srv.Start()
	defer srv.Close()
	conn, e := net.Dial("tcp", srv.Listener.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(70 * time.Second))
	req := adminRequest(http.MethodPut, "/api/v1/files/"+actorID+"/content")
	fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/octet-stream\r\nAuthorization: %s\r\nX-Acting-Membership-ID: %s\r\nContent-Length: 2\r\nConnection: close\r\n\r\nx", req.URL.Path, req.Header.Get("Authorization"), req.Header.Get("X-Acting-Membership-ID"))
	start := time.Now()
	resp, e := http.ReadResponse(bufio.NewReader(conn), req)
	elapsed := time.Since(start)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 || elapsed < 59*time.Second || elapsed > 67*time.Second {
		t.Fatal(resp.StatusCode, elapsed)
	}
}
func TestFileContentServerDefaultTotalTimeout(t *testing.T) {
	svc := contentFunc(func(ctx context.Context, _ access.TrustedIdentity, _ string, r io.Reader) (files.Metadata, error) {
		io.Copy(io.Discard, r)
		<-ctx.Done()
		return files.Metadata{}, ctx.Err()
	})
	h, e := HandlerWithFileContent(Handler(nil), authFunc(verified), svc)
	if e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Config.WriteTimeout = 10 * time.Second
	srv.Start()
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 160 * time.Second
	start := time.Now()
	resp, e := client.Do(contentRequest(contentURL(srv), strings.NewReader("x")))
	if resp != nil {
		resp.Body.Close()
	}
	elapsed := time.Since(start)
	if e == nil {
		t.Fatal("total deadline did not stop response")
	}
	if elapsed < 149*time.Second || elapsed > 158*time.Second {
		t.Fatal(elapsed, e)
	}
}

type blockedContentRepo struct {
	filetransfer.UploadRepository
	started chan struct{}
	release chan struct{}
}

func (s *blockedContentRepo) AcquireFileUpload(ctx context.Context, _ access.TrustedIdentity, _, _ string) (files.UploadTicket, error) {
	s.started <- struct{}{}
	select {
	case <-s.release:
		return files.UploadTicket{}, files.ErrInvalidIdentity
	case <-ctx.Done():
		return files.UploadTicket{}, ctx.Err()
	}
}

type unusedContentObjects struct{ objectstore.Store }
type observedContentBody struct {
	io.ReadCloser
	reads *atomic.Int32
}

func (b observedContentBody) Read(p []byte) (int, error) { b.reads.Add(1); return b.ReadCloser.Read(p) }
func TestFileContentNodeConcurrency(t *testing.T) {
	repo := &blockedContentRepo{started: make(chan struct{}, 4), release: make(chan struct{})}
	svc, e := filetransfer.NewService(repo, &unusedContentObjects{}, t.TempDir()+"/private", actorID)
	if e != nil {
		t.Fatal(e)
	}
	h, e := HandlerWithFileContent(Handler(nil), authFunc(verified), svc)
	if e != nil {
		t.Fatal(e)
	}
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = observedContentBody{r.Body, &reads}
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			resp, e := srv.Client().Do(contentRequest(contentURL(srv), strings.NewReader("x")))
			if resp != nil {
				resp.Body.Close()
			}
			done <- e
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-repo.started:
		case <-time.After(5 * time.Second):
			close(repo.release)
			t.Fatal("four slots not acquired")
		}
	}
	resp, e := srv.Client().Do(contentRequest(contentURL(srv), strings.NewReader("x")))
	if e != nil {
		close(repo.release)
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 429 || reads.Load() != 0 || resp.Header.Get("Retry-After") == "" {
		close(repo.release)
		t.Fatal(resp.StatusCode, reads.Load())
	}
	close(repo.release)
	for i := 0; i < 4; i++ {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
}
