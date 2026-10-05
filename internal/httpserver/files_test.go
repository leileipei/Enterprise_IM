package httpserver

import (
	"context"
	"encoding/json"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type metadataStub struct {
	calls int
	p     files.CreateParams
}

func (s *metadataStub) ReserveFile(_ context.Context, id access.TrustedIdentity, p files.CreateParams) (files.Reservation, error) {
	s.calls++
	s.p = p
	return files.Reservation{File: files.Metadata{CreateParams: p, ID: actorID, State: files.StateAllocated}}, nil
}
func (s *metadataStub) GetOwnFile(context.Context, access.TrustedIdentity, string) (files.Metadata, error) {
	return files.Metadata{ID: actorID, State: files.StateAllocated}, nil
}

const reservationJSON = `{"upload_request_id":"00000000-0000-4000-8000-000000000501","original_filename":"test.txt","declared_media_type":"text/plain","declared_size_bytes":"1"}`

func TestFileMetadataHTTP(t *testing.T) {
	s := &metadataStub{}
	h, e := HandlerWithFileMetadata(Handler(nil), authFunc(verified), s)
	if e != nil {
		t.Fatal(e)
	}
	path := "/api/v1/conversations/" + actorID + "/files"
	for _, body := range []string{reservationJSON, strings.Replace(reservationJSON, `"1"`, `1`, 1), strings.Replace(reservationJSON, `"1"`, `"01"`, 1), strings.TrimSuffix(reservationJSON, "}") + `,"object_key":"evil"}`, strings.Replace(reservationJSON, `"declared_size_bytes":"1"`, `"declared_size_bytes":"1","declared_size_bytes":"2"`, 1)} {
		req := adminRequest(http.MethodPost, path)
		req.Header.Set("Content-Type", "application/json")
		req.Body = io.NopCloser(strings.NewReader(body))
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if body == reservationJSON {
			if res.Code != 201 || s.p.TenantID != tenantID || s.p.UploaderMembershipID != actingID {
				t.Fatal(res.Code, s.p, res.Body.String())
			}
			var v map[string]any
			if e = json.Unmarshal(res.Body.Bytes(), &v); e != nil || len(v) != 9 {
				t.Fatal(v, e)
			}
			if _, ok := v["declared_size_bytes"].(string); !ok {
				t.Fatal(v)
			}
		} else if res.Code != 400 {
			t.Fatal(res.Code, res.Body.String())
		}
	}
	if s.calls != 1 {
		t.Fatal(s.calls)
	}
	req := adminRequest(http.MethodGet, "/api/v1/files/"+actorID)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != 200 || res.Header().Get("Cache-Control") != "no-store" || res.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal(res.Code, res.Header())
	}
}
