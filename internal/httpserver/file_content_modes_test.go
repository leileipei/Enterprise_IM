package httpserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFileContentModeMatrix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		u, b  bool
		allow string
	}{
		{"off", false, false, "GET"}, {"upload", true, false, "GET, PUT"}, {"read", false, true, "GET"}, {"both", true, true, "GET, PUT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{"GET", "PUT", "HEAD", "OPTIONS", "POST"} {
				calls := 0
				h := HandlerWithFileContentModes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }), tc.u, tc.b)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(method, "/api/v1/files/"+actorID+"/content", nil))
				want := 405
				callWant := 0
				if method == "GET" {
					if tc.b {
						want = 204
						callWant = 1
					} else {
						want = 503
					}
				}
				if method == "PUT" && tc.u {
					want = 204
					callWant = 1
				}
				if w.Code != want || calls != callWant {
					t.Fatal("wrong assembled method", method, w.Code, calls)
				}
				if want == 405 && w.Header().Get("Allow") != tc.allow {
					t.Fatal("wrong Allow", method, w.Header())
				}
				if want != 204 && (w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff") {
					t.Fatal("closed content response not private")
				}
			}
		})
	}
}

type contentModeWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *contentModeWriter) SetWriteDeadline(v time.Time) error { w.deadline = v; return nil }
func TestFileContentModesResponseController(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	h := HandlerWithFileContentModes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := http.NewResponseController(w)
		if c.SetWriteDeadline(deadline) != nil || c.Flush() != nil {
			t.Error("binary controller/flush blocked by mode wrapper")
		}
		io.WriteString(w, "bytes")
	}), true, true)
	w := &contentModeWriter{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/files/"+actorID+"/content", nil))
	if !w.deadline.Equal(deadline) || !w.Flushed || w.Body.String() != "bytes" {
		t.Fatal("stream response changed")
	}
}

func TestFileStatusWithoutReservation(t *testing.T) {
	s := &metadataStub{}
	h, e := HandlerWithFileStatus(Handler(nil), authFunc(verified), s)
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/api/v1/files/" + actorID, "", 200},
		{"GET", "/api/v1/files/" + actorID + "?q=x", "", 400},
		{"GET", "/api/v1/files/" + actorID + "?", "", 400},
		{"GET", "/api/v1/files/" + actorID, "x", 400},
		{"POST", "/api/v1/conversations/" + actorID + "/files", reservationJSON, 503},
	} {
		r := adminRequest(tc.method, tc.path)
		r.Header.Set("Content-Type", "application/json")
		r.Body = io.NopCloser(strings.NewReader(tc.body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatal("status-only route disagrees", tc.method, w.Code, w.Body.String())
		}
	}
	if s.calls != 0 {
		t.Fatal("disabled reservation called upload service")
	}
	r := httptest.NewRequest("GET", "/api/v1/files/"+actorID, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("status bypassed identity", w.Code)
	}
	if _, e := HandlerWithFileStatus(nil, authFunc(verified), s); e == nil {
		t.Fatal("incomplete status assembly accepted")
	}
}
