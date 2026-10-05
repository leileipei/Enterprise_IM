package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fileSearchStub struct {
	calls int
	err   error
	kind  string
	page  policystore.FileSearchPage
}

func (s *fileSearchStub) SearchFileMessages(_ context.Context, id access.TrustedIdentity, cid, kind, q, cursor string, limit int) (policystore.FileSearchPage, error) {
	s.calls++
	s.kind = kind
	if id.TenantID != tenantID || id.UserID != actorID || id.ActingMembershipID != actingID {
		panic("untrusted identity")
	}
	return s.page, s.err
}
func (s *fileSearchStub) SearchAllFileMessages(ctx context.Context, id access.TrustedIdentity, q, kind, cursor string, limit int) (policystore.FileSearchPage, error) {
	return s.SearchFileMessages(ctx, id, "", kind, q, cursor, limit)
}
func TestFileSearchHTTPStrict(t *testing.T) {
	s := &fileSearchStub{}
	h, e := HandlerWithFileSearch(http.NotFoundHandler(), authFunc(verified), s)
	if e != nil {
		t.Fatal(e)
	}
	base := "/api/v1/conversations/" + actorID + "/files/search"
	paths := []string{base, base + "?", base + "?q=a", base + "?q=ab&q=cd", base + "?q=ab&unknown=x", base + "?q=ab&kind=direct", base + "?q=ab&limit=01", base + "?q=ab&limit=0", base + "?q=ab&limit=51", base + "?q=ab&limit=+20", base + "?q=ab&cursor=", base + "?q=ab&cursor=" + strings.Repeat("x", 2049), base + "?q=%00ab", base + "?q=" + strings.Repeat("a", 101), "/api/v1/groups/no/files/search?q=ab", "/api/v1/files/search?q=ab&kind=other"}
	for _, path := range paths {
		r := adminRequest("GET", path)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_file_search") || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal(path, w.Code, w.Body.String(), w.Header())
		}
	}
	for _, chunked := range []bool{false, true} {
		r := adminRequest("GET", base+"?q=ab")
		r.Body = forbiddenCapabilityBody{}
		if chunked {
			r.TransferEncoding = []string{"chunked"}
			r.ContentLength = -1
		} else {
			r.ContentLength = 1
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal("framed body", w.Code)
		}
	}
	for _, mutate := range []func(*http.Request){func(r *http.Request) { r.Header.Set("Content-Encoding", "identity") }, func(r *http.Request) { r.Method = "HEAD" }, func(r *http.Request) { r.Method = "POST" }, func(r *http.Request) { r.Header.Add("Authorization", "Bearer verified-token") }} {
		r := adminRequest("GET", base+"?q=ab")
		mutate(r)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == 200 {
			t.Fatal("invalid accepted")
		}
	}
	if s.calls != 0 {
		t.Fatal("invalid request queried store", s.calls)
	}
	for _, kind := range []string{"conversations", "groups"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, adminRequest("GET", "/api/v1/"+kind+"/"+actorID+"/files/search?q=ab"))
		want := "direct"
		if kind == "groups" {
			want = "group"
		}
		if w.Code != 200 || s.kind != want {
			t.Fatal(w.Code, s.kind)
		}
	}
	for _, tc := range []struct {
		e      error
		status int
		code   string
	}{{policystore.ErrInvalidFileSearch, 400, "invalid_file_search"}, {policystore.ErrForbidden, 403, "invalid_identity"}, {policystore.ErrMessageNotAvailable, 404, "not_found"}, {errors.New("secret store detail"), 503, "file_search_unavailable"}, {policystore.ErrAuditUnavailable, 503, "file_search_unavailable"}} {
		s.err = tc.e
		w := httptest.NewRecorder()
		h.ServeHTTP(w, adminRequest("GET", base+"?q=ab"))
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) || strings.Contains(w.Body.String(), "secret") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if _, e = HandlerWithFileSearch(nil, authFunc(verified), s); e == nil {
		t.Fatal("nil base")
	}
	if _, e = HandlerWithFileSearch(http.NotFoundHandler(), nil, s); e == nil {
		t.Fatal("nil auth")
	}
	if _, e = HandlerWithFileSearch(http.NotFoundHandler(), authFunc(verified), nil); e == nil {
		t.Fatal("nil store")
	}
}

type fileSearchMetadataSpy struct {
	metadataStub
	own int
}

func (s *fileSearchMetadataSpy) GetOwnFile(context.Context, access.TrustedIdentity, string) (files.Metadata, error) {
	s.own++
	return files.Metadata{}, nil
}
func TestFileSearchRoutePrecedence(t *testing.T) {
	m := &fileSearchMetadataSpy{}
	base, e := HandlerWithFileMetadata(http.NotFoundHandler(), authFunc(verified), m)
	if e != nil {
		t.Fatal(e)
	}
	s := &fileSearchStub{}
	h, e := HandlerWithFileSearch(base, authFunc(verified), s)
	if e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, adminRequest("GET", "/api/v1/files/search?q=ab"))
	if w.Code != 200 || m.own != 0 || s.calls != 1 {
		t.Fatal(w.Code, m.own, s.calls)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, adminRequest("GET", "/api/v1/files/"+actorID))
	if m.own != 1 {
		t.Fatal("nonsearch intercepted", m.own)
	}
}
func TestFileSearchDTOPrivacy(t *testing.T) {
	s := &fileSearchStub{page: policystore.FileSearchPage{Matches: []policystore.FileSearchMatch{{ConversationID: actorID, Kind: "direct", MessageID: actingID, SenderUserID: actorID, FileID: targetUserID, OriginalFilename: "中文<&.pdf", DetectedMediaType: "application/pdf", Seq: 9007199254740993, ActualSizeBytes: 1, ServerTime: time.Now()}}}}
	h, e := HandlerWithFileSearch(http.NotFoundHandler(), authFunc(verified), s)
	if e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, adminRequest("GET", "/api/v1/files/search?q=ab"))
	var p map[string]json.RawMessage
	if e = json.Unmarshal(w.Body.Bytes(), &p); e != nil || len(p) != 3 {
		t.Fatal(p, e)
	}
	var matches []map[string]any
	if e = json.Unmarshal(p["matches"], &matches); e != nil || len(matches) != 1 || len(matches[0]) != 10 || matches[0]["seq"] != "9007199254740993" || matches[0]["actual_size_bytes"] != "1" || matches[0]["original_filename"] != "中文<&.pdf" {
		t.Fatal(matches, e)
	}
	for _, name := range []string{"caption", "text", "object_key", "token", "sha256", "url"} {
		if _, ok := matches[0][name]; ok {
			t.Fatal("private field", name)
		}
	}
}
func TestFileSearchProductionClosed(t *testing.T) {
	h := HandlerWithClosedFileSearch(http.NotFoundHandler(), authFunc(verified))
	for _, path := range []string{"/api/v1/files/search?q=ab", "/api/v1/conversations/" + actorID + "/files/search?q=ab", "/api/v1/groups/" + actorID + "/files/search?q=ab"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, adminRequest("GET", path))
		if w.Code != 503 || !strings.Contains(w.Body.String(), "file_search_unavailable") {
			t.Fatal(w.Code, w.Body.String())
		}
		w = httptest.NewRecorder()
		r := adminRequest("GET", path)
		r.Header.Del("Authorization")
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("no auth", w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, adminRequest("GET", "/api/v1/files/search?q=a"))
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, adminRequest("HEAD", "/api/v1/files/search?q=ab"))
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, adminRequest("GET", "/api/v1/conversations/"+actorID+"/messages/search?q=ab"))
	if w.Code != 404 {
		t.Fatal("text route intercepted", w.Code)
	}
}
