package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fileIdentityStub struct {
	err   error
	calls int
}

func (s *fileIdentityStub) ValidateFileIdentity(context.Context, access.TrustedIdentity) error {
	s.calls++
	return s.err
}
func TestFileCapabilitiesStrictRequest(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body, encoding string
		want                               int
	}{
		{"valid", "GET", "/api/v1/file-capabilities", "", "", 200},
		{"head", "HEAD", "/api/v1/file-capabilities", "", "", 405},
		{"put", "PUT", "/api/v1/file-capabilities", "", "", 405},
		{"query", "GET", "/api/v1/file-capabilities?x=1", "", "", 400},
		{"force query", "GET", "/api/v1/file-capabilities?", "", "", 400},
		{"body", "GET", "/api/v1/file-capabilities", "x", "", 400},
		{"encoding", "GET", "/api/v1/file-capabilities", "", "gzip", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &fileIdentityStub{}
			h, e := HandlerWithFileCapabilities(Handler(nil), authFunc(verified), s, FileCapabilities{UploadEnabled: true})
			if e != nil {
				t.Fatal(e)
			}
			r := adminRequest(tc.method, tc.path)
			r.Body = io.NopCloser(strings.NewReader(tc.body))
			r.Header.Set("Content-Encoding", tc.encoding)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatal(w.Code, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal(w.Header())
			}
			if tc.want == 200 {
				var v map[string]bool
				if json.Unmarshal(w.Body.Bytes(), &v) != nil || len(v) != 4 || !v["upload_enabled"] || v["message_send_enabled"] || v["download_enabled"] || v["filename_search_enabled"] {
					t.Fatal(v)
				}
				if s.calls != 1 {
					t.Fatal(s.calls)
				}
			} else if s.calls != 0 {
				t.Fatal("invalid request reached identity store")
			}
		})
	}
	if _, e := HandlerWithFileCapabilities(nil, authFunc(verified), &fileIdentityStub{}, FileCapabilities{}); e == nil {
		t.Fatal("nil base accepted")
	}
	if _, e := HandlerWithFileCapabilities(Handler(nil), nil, &fileIdentityStub{}, FileCapabilities{}); e == nil {
		t.Fatal("nil auth accepted")
	}
	if _, e := HandlerWithFileCapabilities(Handler(nil), authFunc(verified), nil, FileCapabilities{}); e == nil {
		t.Fatal("nil identity accepted")
	}
}
func TestFileCapabilitiesCurrentIdentity(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{{access.ErrInvalidIdentity, 403}, {errors.New("private dependency detail"), 503}} {
		s := &fileIdentityStub{err: tc.err}
		h, _ := HandlerWithFileCapabilities(Handler(nil), authFunc(verified), s, FileCapabilities{MessageSendEnabled: true})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, adminRequest("GET", "/api/v1/file-capabilities"))
		if w.Code != tc.want || strings.Contains(w.Body.String(), "private dependency detail") || strings.Contains(w.Body.String(), "message_send_enabled") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, header := range []string{"Authorization", "X-Acting-Membership-ID"} {
		s := &fileIdentityStub{}
		h, _ := HandlerWithFileCapabilities(Handler(nil), authFunc(verified), s, FileCapabilities{})
		r := adminRequest("GET", "/api/v1/file-capabilities")
		r.Header.Add(header, r.Header.Get(header))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == 200 || s.calls != 0 {
			t.Fatal("duplicate identity header accepted", header)
		}
	}
	h, _ := HandlerWithFileCapabilities(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(418) }), authFunc(verified), &fileIdentityStub{}, FileCapabilities{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, adminRequest("GET", "/other"))
	if w.Code != 418 {
		t.Fatal(w.Code)
	}
}
