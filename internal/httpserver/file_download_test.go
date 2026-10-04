package httpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type downloadHTTPRepo struct {
	data             []byte
	name             string
	authErr          error
	beginErr         error
	results          []filedownload.Result
	prepares, checks int
	checkErrAt       int
}

func (r *downloadHTTPRepo) BeginFileDownload(_ context.Context, id access.TrustedIdentity, fid, owner string, deadline time.Time) (filedownload.Ticket, error) {
	r.prepares++
	if r.beginErr != nil {
		return filedownload.Ticket{}, r.beginErr
	}
	at := time.Now().UTC().Add(-time.Minute)
	up := at.Add(time.Second)
	scan := at.Add(2 * time.Second)
	n := int64(len(r.data))
	sha := sha256.Sum256(r.data)
	m := files.Metadata{CreateParams: files.CreateParams{TenantID: id.TenantID, ConversationID: actorID, UploaderUserID: id.UserID, UploaderMembershipID: id.ActingMembershipID, UploadRequestID: actorID, OriginalFilename: r.name, DeclaredMediaType: "text/plain", DeclaredSizeBytes: n}, ID: fid, RequestDigest: make([]byte, 32), State: files.StateReady, StateVersion: 3, CreatedAt: at, UpdatedAt: scan, UploadExpiresAt: at.Add(15 * time.Minute), ObjectKey: "tenants/" + id.TenantID + "/files/" + fid, ObjectVersionID: "sealed-v1", DetectedMediaType: "text/plain", ActualSizeBytes: &n, SHA256: sha[:], UploadedAt: &up, ScanJobID: actorID, ScanEngine: "clamav", ScanDefinitionVersion: "fresh", ScannedAt: &scan, ScanSHA256: sha[:]}
	return filedownload.Ticket{SessionID: actorID, OwnerID: owner, LeaseToken: actingID, Identity: id, File: m, MessageID: actorID, MessageSeq: 1, Deadline: deadline, LeaseExpiresAt: deadline}, nil
}
func (r *downloadHTTPRepo) AuthorizeFileDownload(context.Context, access.TrustedIdentity, filedownload.Ticket) error {
	return r.authErr
}
func (r *downloadHTTPRepo) CheckFileDownload(context.Context, access.TrustedIdentity, filedownload.Ticket) error {
	r.checks++
	if r.checkErrAt > 0 && r.checks >= r.checkErrAt {
		return filedownload.ErrNotFound
	}
	return nil
}
func (r *downloadHTTPRepo) FinishFileDownload(_ context.Context, _ filedownload.Ticket, result filedownload.Result) error {
	r.results = append(r.results, result)
	return nil
}

type downloadHTTPObjects struct{ data []byte }

func (o downloadHTTPObjects) ValidateCapabilities(context.Context) error { return nil }
func (o downloadHTTPObjects) ReadVersion(context.Context, objectstore.VersionRef) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(o.data)), nil
}
func (o downloadHTTPObjects) PutVersion(context.Context, objectstore.Location, string, files.Measurement, io.ReadSeeker) (objectstore.VersionRef, error) {
	return objectstore.VersionRef{}, errors.New("unused")
}
func (o downloadHTTPObjects) FindAttemptVersions(context.Context, objectstore.Location, string, int) ([]objectstore.VersionRef, error) {
	return nil, errors.New("unused")
}

type downloadHTTPWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	partial   int
	onWrite   func()
}

func (w *downloadHTTPWriter) SetWriteDeadline(at time.Time) error {
	w.deadlines = append(w.deadlines, at)
	return nil
}
func (w *downloadHTTPWriter) SetReadDeadline(time.Time) error { return nil }
func (w *downloadHTTPWriter) Write(b []byte) (int, error) {
	if w.onWrite != nil {
		w.onWrite()
	}
	if w.partial > 0 && len(b) > w.partial {
		n, e := w.ResponseRecorder.Write(b[:w.partial])
		if e != nil {
			return n, e
		}
		return n, io.ErrShortWrite
	}
	return w.ResponseRecorder.Write(b)
}
func downloadHTTPAuth(_ context.Context, _ string) (VerifiedIdentity, error) {
	return VerifiedIdentity{TenantID: tenantID, UserID: actorID, ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func downloadHTTPHandler(t *testing.T, r *downloadHTTPRepo, auth Authenticator) http.Handler {
	t.Helper()
	s, e := filedownload.NewService(r, downloadHTTPObjects{r.data}, filepath.Join(t.TempDir(), "spool"), actorID)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	put, e := HandlerWithFileContent(Handler(nil), auth, &contentStub{})
	if e != nil {
		t.Fatal(e)
	}
	h, e := HandlerWithFileDownload(put, auth, s)
	if e != nil {
		t.Fatal(e)
	}
	return h
}
func downloadHTTPReq(method string) *http.Request {
	return adminRequest(method, "/api/v1/files/"+actorID+"/content")
}
func downloadHTTPServe(h http.Handler, w http.ResponseWriter, r *http.Request) (aborted bool) {
	defer func() {
		if v := recover(); v != nil {
			if v != http.ErrAbortHandler {
				panic(v)
			}
			aborted = true
		}
	}()
	h.ServeHTTP(w, r)
	return false
}
func TestFileDownloadHTTPMethods(t *testing.T) {
	r := &downloadHTTPRepo{data: []byte("secret payload"), name: "报告.txt"}
	h := downloadHTTPHandler(t, r, authFunc(downloadHTTPAuth))
	for _, method := range []string{"HEAD", "POST", "OPTIONS", "PATCH", "DELETE"} {
		w := &downloadHTTPWriter{ResponseRecorder: httptest.NewRecorder()}
		h.ServeHTTP(w, downloadHTTPReq(method))
		if w.Code != 405 || w.Header().Get("Allow") != "GET, PUT" {
			t.Fatal(method, w.Code, w.Header())
		}
	}
	for _, header := range []string{"Range", "If-Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "Content-Encoding"} {
		req := downloadHTTPReq("GET")
		req.Header.Set(header, "anything")
		w := &downloadHTTPWriter{ResponseRecorder: httptest.NewRecorder()}
		h.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatal(header, w.Code)
		}
	}
	for _, kind := range []string{"query", "empty_query", "body"} {
		req := downloadHTTPReq("GET")
		switch kind {
		case "query":
			req.URL.RawQuery = "tenant=x"
		case "empty_query":
			req.URL.ForceQuery = true
		case "body":
			req.Body = io.NopCloser(strings.NewReader("x"))
			req.ContentLength = 1
		}
		w := &downloadHTTPWriter{ResponseRecorder: httptest.NewRecorder()}
		h.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatal(kind, w.Code)
		}
	}
	if r.prepares != 0 {
		t.Fatal("invalid request reached download", r.prepares)
	}
	put := downloadHTTPReq("PUT")
	put.Header.Set("Content-Type", "application/octet-stream")
	put.Body = io.NopCloser(strings.NewReader("x"))
	w := &downloadHTTPWriter{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, put)
	if w.Code != 200 {
		t.Fatal("PUT changed", w.Code, w.Body.String())
	}
	w = &downloadHTTPWriter{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, downloadHTTPReq("GET"))
	if w.Code != 200 || w.Body.String() != string(r.data) {
		t.Fatal("GET intercepted", w.Code, w.Body.String())
	}
}
func TestFileDownloadDispositionSafety(t *testing.T) {
	for _, name := range []string{"中文报告.txt", strings.Repeat("x", 255), "../../secret.txt", "evil\r\nInjected: yes.txt", strings.Repeat("x", 256), "invalid\x00.txt"} {
		t.Run(name, func(t *testing.T) {
			r := &downloadHTTPRepo{data: []byte("file contents"), name: name}
			h := downloadHTTPHandler(t, r, authFunc(downloadHTTPAuth))
			w := &downloadHTTPWriter{ResponseRecorder: httptest.NewRecorder()}
			h.ServeHTTP(w, downloadHTTPReq("GET"))
			if name == "中文报告.txt" || len(name) == 255 {
				typ, p, e := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
				if e != nil || typ != "attachment" || p["filename"] != name || w.Code != 200 || w.Header().Get("Content-Type") != "application/octet-stream" {
					t.Fatal(w.Code, w.Header(), e)
				}
			} else if w.Code != 503 || strings.Contains(w.Body.String(), string(r.data)) || w.Header().Get("Content-Disposition") != "" {
				t.Fatal("invalid filename exposed", w.Code, w.Header(), w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Location") != "" || w.Header().Get("ETag") != "" || w.Header().Get("Accept-Ranges") != "" {
				t.Fatal(w.Header())
			}
		})
	}
}
func TestFileDownloadZeroBytesBeforeAudit(t *testing.T) {
	r := &downloadHTTPRepo{data: []byte("private content"), name: "private.txt", authErr: policystore.ErrAuditUnavailable}
	h := downloadHTTPHandler(t, r, authFunc(downloadHTTPAuth))
	w := &downloadHTTPWriter{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, downloadHTTPReq("GET"))
	if w.Code != 503 || strings.Contains(w.Body.String(), string(r.data)) || w.Header().Get("Content-Disposition") != "" || len(r.results) != 1 || r.results[0].BytesWritten != 0 || r.results[0].Outcome != "interrupted" {
		t.Fatal(w.Code, w.Body.String(), r.results)
	}
}
func TestFileDownloadDeadlineUnsupported(t *testing.T) {
	r := &downloadHTTPRepo{data: []byte("x"), name: "a.txt"}
	h := downloadHTTPHandler(t, r, authFunc(downloadHTTPAuth))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, downloadHTTPReq("GET"))
	if w.Code != 503 || r.prepares != 0 {
		t.Fatal(w.Code, r.prepares)
	}
}
func TestFileDownloadInFlightReauth(t *testing.T) {
	for _, why := range []string{"token", "identity", "db"} {
		t.Run(why, func(t *testing.T) {
			r := &downloadHTTPRepo{data: bytes.Repeat([]byte("x"), 65536), name: "a.txt"}
			calls := 0
			auth := authFunc(func(ctx context.Context, token string) (VerifiedIdentity, error) {
				calls++
				v, e := downloadHTTPAuth(ctx, token)
				if calls >= 3 {
					if why == "token" {
						return VerifiedIdentity{}, errors.New("revoked token")
					}
					if why == "identity" {
						v.UserID = actingID
					}
				}
				return v, e
			})
			if why == "db" {
				r.checkErrAt = 2
			}
			h := downloadHTTPHandler(t, r, auth)
			w := &downloadHTTPWriter{ResponseRecorder: httptest.NewRecorder()}
			aborted := downloadHTTPServe(h, w, downloadHTTPReq("GET"))
			if !aborted || w.Body.Len() != 32768 || len(r.results) != 1 || r.results[0].BytesWritten != 32768 || r.results[0].Outcome != "interrupted" || strings.Contains(w.Body.String(), "error_code") {
				t.Fatal(aborted, w.Body.Len(), r.results)
			}
		})
	}
}
func TestFileDownloadTokenExpiry(t *testing.T) {
	r := &downloadHTTPRepo{data: bytes.Repeat([]byte("x"), 65536), name: "a.txt"}
	exp := time.Now().Add(150 * time.Millisecond)
	auth := authFunc(func(context.Context, string) (VerifiedIdentity, error) {
		return VerifiedIdentity{TenantID: tenantID, UserID: actorID, ExpiresAt: exp}, nil
	})
	h := downloadHTTPHandler(t, r, auth)
	w := &downloadHTTPWriter{ResponseRecorder: httptest.NewRecorder()}
	w.onWrite = func() {
		if w.Body.Len() == 0 {
			time.Sleep(time.Until(exp) + 10*time.Millisecond)
		}
	}
	aborted := downloadHTTPServe(h, w, downloadHTTPReq("GET"))
	if !aborted || len(r.results) != 1 || r.results[0].Outcome != "interrupted" || r.results[0].Reason != "token_expired" {
		t.Fatal(aborted, r.results)
	}
	for _, d := range w.deadlines[1:] {
		if d.After(exp) {
			t.Fatal("deadline exceeds exp", d, exp)
		}
	}
}
func TestFileDownloadPartialWrite(t *testing.T) {
	r := &downloadHTTPRepo{data: []byte("partial content"), name: "a.txt"}
	h := downloadHTTPHandler(t, r, authFunc(downloadHTTPAuth))
	w := &downloadHTTPWriter{ResponseRecorder: httptest.NewRecorder(), partial: 5}
	aborted := downloadHTTPServe(h, w, downloadHTTPReq("GET"))
	if !aborted || w.Body.Len() != 5 || len(r.results) != 1 || r.results[0].BytesWritten != 5 || r.results[0].Outcome != "interrupted" || strings.Contains(w.Body.String(), "error_code") {
		t.Fatal(aborted, w.Body.String(), r.results)
	}
}
func TestFileDownloadMissingTrustedExpiry(t *testing.T) {
	r := &downloadHTTPRepo{data: []byte("x"), name: "a.txt"}
	h := downloadHTTPHandler(t, r, authFunc(verified))
	w := &downloadHTTPWriter{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, downloadHTTPReq("GET"))
	if w.Code != 503 || r.prepares != 0 {
		t.Fatal(w.Code, r.prepares)
	}
}
func TestFileDownloadProductionClosed(t *testing.T) {
	for _, put := range []bool{false, true} {
		h := HandlerWithClosedFileDownload(Handler(nil), put)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, downloadHTTPReq("GET"))
		if w.Code != 503 || !strings.Contains(w.Body.String(), "file_download_unavailable") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestFileDownloadAdmissionErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{filedownload.ErrBusy, 409}, {filedownload.ErrAuditPending, 503}, {filedownload.ErrLimit, 429}, {filedownload.ErrNotFound, 404}, {filedownload.ErrInvalidIdentity, 403},
	} {
		r := &downloadHTTPRepo{data: []byte("private"), name: "a.txt", beginErr: tc.err}
		h := downloadHTTPHandler(t, r, authFunc(downloadHTTPAuth))
		w := &downloadHTTPWriter{ResponseRecorder: httptest.NewRecorder()}
		h.ServeHTTP(w, downloadHTTPReq("GET"))
		if w.Code != tc.status || w.Header().Get("Content-Disposition") != "" || strings.Contains(w.Body.String(), "private") {
			t.Fatal(tc.err, w.Code, w.Body.String())
		}
	}
}
