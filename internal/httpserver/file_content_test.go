package httpserver

import (
	"context"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type contentStub struct {
	calls int
	body  string
	err   error
}

func (s *contentStub) Upload(_ context.Context, _ access.TrustedIdentity, _ string, r io.Reader) (files.Metadata, error) {
	s.calls++
	b, e := io.ReadAll(r)
	s.body = string(b)
	if e != nil {
		return files.Metadata{}, e
	}
	return files.Metadata{ID: actorID, State: files.StateUploaded}, s.err
}
func TestFileContentUnsupportedDeadlines(t *testing.T) {
	s := &contentStub{}
	h, e := HandlerWithFileContent(Handler(nil), authFunc(verified), s)
	if e != nil {
		t.Fatal(e)
	}
	req := adminRequest(http.MethodPut, "/api/v1/files/"+actorID+"/content")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Body = io.NopCloser(strings.NewReader("x"))
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != 503 || s.calls != 0 {
		t.Fatal(res.Code, s.calls)
	}
}
