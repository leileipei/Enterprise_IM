package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
)

type retentionBatchesFunc func(context.Context, access.TrustedIdentity, string, string, string, int) (access.RetentionBatchPage, error)

func (f retentionBatchesFunc) ListRetentionBatches(ctx context.Context, id access.TrustedIdentity, c, k, cur string, l int) (access.RetentionBatchPage, error) {
	return f(ctx, id, c, k, cur, l)
}

// Catches wrong trusted identity, wrong defaults, DTO leakage and lost evidence fields.
func TestRetentionBatchAdminResponse(t *testing.T) {
	when := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	days := 365
	for _, kind := range []string{"body", "digest"} {
		service := retentionBatchesFunc(func(_ context.Context, id access.TrustedIdentity, c, k, cur string, l int) (access.RetentionBatchPage, error) {
			if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || c != holdConversationID || k != kind || cur != "next" || l != 2 {
				t.Fatalf("input: %+v %s %s %s %d", id, c, k, cur, l)
			}
			b := access.RetentionBatch{ID: holdID, ConversationID: c, Kind: k, ProcessedAt: when, ProcessedCount: 2, FirstSeq: 1, LastSeq: 3}
			if k == "body" {
				b.RetentionDays = &days
				b.CutoffAt = &when
			} else {
				b.MinExpiresAt = &when
				b.MaxExpiresAt = &when
			}
			return access.RetentionBatchPage{Batches: []access.RetentionBatch{b}, NextCursor: "more"}, nil
		})
		h, err := HandlerWithRetentionBatches(Handler(nil), authFunc(verified), service)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRecorder()
		h.ServeHTTP(r, adminRequest(http.MethodGet, "/api/v1/admin/conversations/"+holdConversationID+"/retention-batches?kind="+kind+"&limit=2&cursor=next"))
		if r.Code != 200 || r.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response: %d %s", r.Code, r.Body.String())
		}
		var p struct {
			Batches []map[string]any `json:"batches"`
			Next    string           `json:"next_cursor"`
		}
		if json.Unmarshal(r.Body.Bytes(), &p) != nil || len(p.Batches) != 1 || p.Next != "more" {
			t.Fatalf("page: %s", r.Body.String())
		}
		b := p.Batches[0]
		if b["kind"] != kind || b["id"] != holdID || b["processed_count"] != float64(2) || b["first_seq"] != float64(1) || b["last_seq"] != float64(3) || b["processed_at"] != "2026-10-03T00:00:00Z" {
			t.Fatalf("batch: %+v", b)
		}
		if kind == "body" {
			if b["retention_days"] != float64(365) || b["cutoff_at"] == nil || b["min_expires_at"] != nil {
				t.Fatalf("body: %+v", b)
			}
		} else if b["retention_days"] != nil || b["cutoff_at"] != nil || b["min_expires_at"] == nil || b["max_expires_at"] == nil {
			t.Fatalf("digest: %+v", b)
		}
		for _, key := range []string{"text", "content_digest", "tenant_id", "sender_user_id"} {
			if _, ok := b[key]; ok {
				t.Fatalf("leaked %s", key)
			}
		}
	}
	service := retentionBatchesFunc(func(_ context.Context, _ access.TrustedIdentity, _, k, cur string, l int) (access.RetentionBatchPage, error) {
		if k != "body" || cur != "" || l != 100 {
			t.Fatal("defaults")
		}
		return access.RetentionBatchPage{}, nil
	})
	h, _ := HandlerWithRetentionBatches(Handler(nil), authFunc(verified), service)
	r := httptest.NewRecorder()
	h.ServeHTTP(r, adminRequest("GET", "/api/v1/admin/conversations/"+holdConversationID+"/retention-batches?kind=body"))
	if r.Code != 200 || r.Body.String() != "{\"batches\":[],\"next_cursor\":\"\"}\n" {
		t.Fatalf("empty page: %d %s", r.Code, r.Body.String())
	}
}

// Catches parser bypass and accidentally accepting unauthenticated or mutating requests.
func TestRetentionBatchAdminRejectsInvalidRequests(t *testing.T) {
	called := 0
	service := retentionBatchesFunc(func(context.Context, access.TrustedIdentity, string, string, string, int) (access.RetentionBatchPage, error) {
		called++
		return access.RetentionBatchPage{}, nil
	})
	h, _ := HandlerWithRetentionBatches(Handler(nil), authFunc(verified), service)
	base := "/api/v1/admin/conversations/" + holdConversationID + "/retention-batches"
	for _, query := range []string{"", "?kind=all", "?kind=BODY", "?kind=body&kind=digest", "?kind=body&tenant_id=x", "?kind=body&limit=0", "?kind=body&limit=501", "?kind=body&limit=no", "?kind=body&limit=1&limit=2", "?kind=body&cursor=", "?kind=body&cursor=" + strings.Repeat("a", 1025), "?kind=body&cursor=x&cursor=y", "?kind=%zz"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, adminRequest("GET", base+query))
		if r.Code != 400 || r.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("query %s: %d", query, r.Code)
		}
	}
	for _, input := range []struct {
		method, path, body string
		code               int
	}{{"POST", base + "?kind=body", "", 405}, {"GET", base + "?kind=body", "{}", 400}, {"GET", "/api/v1/admin/conversations/bad/retention-batches?kind=body", "", 400}} {
		r := httptest.NewRecorder()
		req := adminRequest(input.method, input.path)
		req.Body = io.NopCloser(strings.NewReader(input.body))
		h.ServeHTTP(r, req)
		if r.Code != input.code {
			t.Fatalf("request: %+v %d", input, r.Code)
		}
	}
	for _, header := range []string{"Authorization", "X-Acting-Membership-ID"} {
		r := httptest.NewRecorder()
		req := adminRequest("GET", base+"?kind=body")
		req.Header.Del(header)
		h.ServeHTTP(r, req)
		want := 401
		if header != "Authorization" {
			want = 400
		}
		if r.Code != want {
			t.Fatalf("missing %s: %d", header, r.Code)
		}
	}
	if called != 0 {
		t.Fatalf("invalid request reached store: %d", called)
	}
	r := httptest.NewRecorder()
	h.ServeHTTP(r, adminRequest("GET", "/health/live"))
	if r.Code != 200 {
		t.Fatalf("fallthrough: %d", r.Code)
	}
}

func TestRetentionBatchAdminErrorMapping(t *testing.T) {
	for _, input := range []struct {
		err  error
		code int
		key  string
	}{{access.ErrInvalidRetentionQuery, 400, "invalid_retention_query"}, {access.ErrInvalidIdentity, 403, "invalid_identity"}, {access.ErrNotFound, 404, "not_found"}, {access.ErrAuditUnavailable, 503, "unavailable"}, {errors.New("postgres://secret-body-digest"), 503, "unavailable"}} {
		h, _ := HandlerWithRetentionBatches(Handler(nil), authFunc(verified), retentionBatchesFunc(func(context.Context, access.TrustedIdentity, string, string, string, int) (access.RetentionBatchPage, error) {
			return access.RetentionBatchPage{}, input.err
		}))
		r := httptest.NewRecorder()
		h.ServeHTTP(r, adminRequest("GET", "/api/v1/admin/conversations/"+holdConversationID+"/retention-batches?kind=body"))
		if r.Code != input.code || !strings.Contains(r.Body.String(), `"error_code":"`+input.key+`"`) || strings.Contains(r.Body.String(), "secret-body-digest") || strings.Contains(r.Body.String(), "batches") {
			t.Fatalf("mapped: %d %s", r.Code, r.Body.String())
		}
	}
}
