package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
)

const (
	holdConversationID = "77777777-7777-4777-8777-777777777777"
	holdID             = "88888888-8888-4888-8888-888888888888"
	holdRequestID      = "99999999-9999-4999-8999-999999999999"
)

type legalHoldStub struct {
	list    func(context.Context, access.TrustedIdentity, string, string, int) (access.LegalHoldPage, error)
	place   func(context.Context, access.TrustedIdentity, string, string, string) (access.LegalHold, bool, error)
	release func(context.Context, access.TrustedIdentity, string, string, string, string) (access.LegalHold, error)
}

func (s legalHoldStub) ListLegalHolds(ctx context.Context, id access.TrustedIdentity, c, cursor string, limit int) (access.LegalHoldPage, error) {
	return s.list(ctx, id, c, cursor, limit)
}
func (s legalHoldStub) PlaceLegalHold(ctx context.Context, id access.TrustedIdentity, c, r, ref string) (access.LegalHold, bool, error) {
	return s.place(ctx, id, c, r, ref)
}
func (s legalHoldStub) ReleaseLegalHold(ctx context.Context, id access.TrustedIdentity, c, h, r, ref string) (access.LegalHold, error) {
	return s.release(ctx, id, c, h, r, ref)
}

func TestLegalHoldAdminRoutesAndStrictBodies(t *testing.T) {
	expected := access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}
	calls := 0
	places := 0
	service := legalHoldStub{
		list: func(_ context.Context, id access.TrustedIdentity, c, cursor string, limit int) (access.LegalHoldPage, error) {
			calls++
			if id != expected || c != holdConversationID || (limit != 100 && limit != 500) {
				t.Fatalf("list input: %+v %q %q %d", id, c, cursor, limit)
			}
			return access.LegalHoldPage{Holds: []access.LegalHold{}, NextCursor: "next"}, nil
		},
		place: func(_ context.Context, id access.TrustedIdentity, c, r, ref string) (access.LegalHold, bool, error) {
			calls++
			places++
			if id != expected || c != holdConversationID || r != holdRequestID || ref != "CASE-1" {
				t.Fatalf("place input: %+v %q %q %q", id, c, r, ref)
			}
			return access.LegalHold{ID: holdID, ConversationID: c, CaseReference: ref, PlacedAt: time.Now()}, places == 1, nil
		},
		release: func(_ context.Context, id access.TrustedIdentity, c, h, r, ref string) (access.LegalHold, error) {
			calls++
			if id != expected || c != holdConversationID || h != holdID || r != holdRequestID || ref != "APPROVAL-1" {
				t.Fatalf("release input: %+v %q %q %q %q", id, c, h, r, ref)
			}
			return access.LegalHold{ID: h, ConversationID: c, ReleaseApprovalReference: ref}, nil
		},
	}
	handler, err := HandlerWithLegalHolds(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/admin/conversations/" + holdConversationID + "/legal-holds"
	for _, path := range []string{base, base + "?limit=500&cursor=abc"} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(http.MethodGet, path))
		if res.Code != 200 || res.Header().Get("Cache-Control") != "no-store" || !strings.Contains(res.Body.String(), `"next_cursor":"next"`) {
			t.Fatalf("list %s: %d %s", path, res.Code, res.Body.String())
		}
	}
	post := func(path, body string) *httptest.ResponseRecorder {
		req := adminRequest(http.MethodPost, path)
		req.Header.Set("Content-Type", "application/json")
		req.Body = io.NopCloser(strings.NewReader(body))
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	res := post(base, `{"request_id":"`+holdRequestID+`","case_reference":"CASE-1"}`)
	if res.Code != 201 || !strings.Contains(res.Body.String(), `"id":"`+holdID+`"`) {
		t.Fatalf("place: %d %s", res.Code, res.Body.String())
	}
	res = post(base, `{"request_id":"`+holdRequestID+`","case_reference":"CASE-1"}`)
	if res.Code != 200 || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("placement replay: %d %s", res.Code, res.Body.String())
	}
	res = post(base+"/"+holdID+"/release", `{"request_id":"`+holdRequestID+`","approval_reference":"APPROVAL-1"}`)
	if res.Code != 200 {
		t.Fatalf("release: %d %s", res.Code, res.Body.String())
	}
	if calls != 5 {
		t.Fatalf("valid calls=%d", calls)
	}
	for _, body := range []string{
		`{}`, `null`, `{"request_id":"` + holdRequestID + `"}`,
		`{"request_id":"` + holdRequestID + `","case_reference":"CASE","tenant_id":"` + tenantID + `"}`,
		`{"request_id":"` + holdRequestID + `","case_reference":"CASE","case_reference":"OTHER"}`,
		`{"request_id":"` + holdRequestID + `","case_reference":"CASE","\u0063ase_reference":"OTHER"}`,
		`{"request_id":null,"case_reference":"CASE"}`,
		`{"request_id":"` + holdRequestID + `","case_reference":"\ud800"}`,
		`{"request_id":"` + holdRequestID + `","case_reference":"CASE"} {}`,
		strings.Repeat("x", 1100),
		string(append([]byte(`{"request_id":"`+holdRequestID+`","case_reference":"`), 0xff, '"', '}')),
	} {
		res = post(base, body)
		if res.Code != 400 {
			t.Fatalf("bad body %q: %d %s", body, res.Code, res.Body.String())
		}
	}
	for _, body := range []string{
		`{"request_id":"` + holdRequestID + `","approval_reference":"APPROVAL","tenant_id":"` + tenantID + `"}`,
		`{"request_id":"` + holdRequestID + `","approval_reference":null}`,
	} {
		res = post(base+"/"+holdID+"/release", body)
		if res.Code != 400 {
			t.Fatalf("bad release body %q: %d", body, res.Code)
		}
	}
	if calls != 5 {
		t.Fatalf("invalid request reached service: %d", calls)
	}
	for _, path := range []string{base + "?limit=0", base + "?limit=501", base + "?limit=no", base + "?limit=1&limit=2", base + "?tenant_id=" + tenantID, base + "?cursor=" + strings.Repeat("a", 1100)} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(http.MethodGet, path))
		if res.Code != 400 {
			t.Fatalf("bad query %s: %d", path, res.Code)
		}
	}
	if calls != 5 {
		t.Fatalf("invalid query reached service: %d", calls)
	}
	for _, input := range []struct {
		err  error
		code int
	}{
		{access.ErrInvalidLegalHold, 400}, {access.ErrInvalidIdentity, 403}, {access.ErrNotFound, 404}, {access.ErrConflict, 409}, {errors.New("db down"), 503},
	} {
		service.list = func(context.Context, access.TrustedIdentity, string, string, int) (access.LegalHoldPage, error) {
			return access.LegalHoldPage{}, input.err
		}
		mapped, _ := HandlerWithLegalHolds(Handler(nil), authFunc(verified), service)
		res := httptest.NewRecorder()
		mapped.ServeHTTP(res, adminRequest(http.MethodGet, base))
		if res.Code != input.code {
			t.Fatalf("mapped %v: %d", input.err, res.Code)
		}
	}
}

func TestLegalHoldFileCleanupConflictHTTP(t *testing.T) {
	svc := legalHoldStub{place: func(context.Context, access.TrustedIdentity, string, string, string) (access.LegalHold, bool, error) {
		return access.LegalHold{}, false, access.ErrFileCleanupInProgress
	}}
	h, e := HandlerWithLegalHolds(Handler(nil), authFunc(verified), svc)
	if e != nil {
		t.Fatal(e)
	}
	r := adminRequest("POST", "/api/v1/admin/conversations/"+holdConversationID+"/legal-holds")
	r.Header.Set("Content-Type", "application/json")
	r.Body = io.NopCloser(strings.NewReader(`{"request_id":"` + holdRequestID + `","case_reference":"CASE-NEW"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "file_cleanup_in_progress") || strings.Contains(w.Body.String(), `"id"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
