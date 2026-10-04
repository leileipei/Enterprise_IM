package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
)

type fileRetentionPolicyStub struct {
	change   access.FileRetentionPolicyChange
	setCalls int
	err      error
}

func (s *fileRetentionPolicyStub) GetFileRetentionPolicy(context.Context, access.TrustedIdentity) (access.FileRetentionPolicyRecord, error) {
	return access.FileRetentionPolicyRecord{Policy: files.DefaultRetentionPolicy(), ApprovalReference: "private-approval", ActorUserID: actorID, ActorMembershipID: actingID, UpdatedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}, s.err
}
func (s *fileRetentionPolicyStub) SetFileRetentionPolicy(_ context.Context, id access.TrustedIdentity, c access.FileRetentionPolicyChange) (access.FileRetentionPolicyRecord, error) {
	s.change = c
	s.setCalls++
	if id.TenantID != tenantID || id.UserID != actorID || id.ActingMembershipID != actingID {
		panic("untrusted actor")
	}
	return access.FileRetentionPolicyRecord{Policy: c.Policy}, s.err
}
func (s *fileRetentionPolicyStub) ListFileRetentionPolicyHistory(context.Context, access.TrustedIdentity, string, int) (access.FileRetentionPolicyHistoryPage, error) {
	return access.FileRetentionPolicyHistoryPage{History: []access.FileRetentionPolicyRecord{}}, s.err
}

const fileRetentionPolicyJSON = `{"file_retention_days":"365","cleanup_enabled":true,"expected_version":"0","approval_reference":"CAB-1"}`

func TestFileRetentionPolicyHTTP(t *testing.T) {
	svc := &fileRetentionPolicyStub{}
	h, e := HandlerWithFileRetentionPolicy(Handler(nil), authFunc(verified), svc)
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"/api/v1/admin/file-retention-policy", "/api/v1/admin/file-retention-policy/history?limit=20"} {
		req := adminRequest(http.MethodGet, path)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != 200 || res.Header().Get("Cache-Control") != "no-store" || res.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal(res.Code, res.Header(), res.Body.String())
		}

	}
	req := adminRequest(http.MethodPut, "/api/v1/admin/file-retention-policy")
	req.Header.Set("Content-Type", "application/json")
	req.Body = io.NopCloser(strings.NewReader(fileRetentionPolicyJSON))
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != 200 || svc.setCalls != 1 || svc.change.ExpectedVersion != 0 || !svc.change.Policy.CleanupEnabled || svc.change.Policy.Days != 365 || svc.change.ApprovalReference != "CAB-1" {
		t.Fatal(res.Code, svc.change, res.Body.String())
	}
	var dto map[string]any
	if e = json.Unmarshal(res.Body.Bytes(), &dto); e != nil {
		t.Fatal(e)
	}
	if _, ok := dto["file_retention_days"].(string); !ok || dto["current_policy_applies_to_existing_files"] != true {
		t.Fatal("numeric precision unsafe", dto)
	}
}
func TestFileRetentionPolicyHTTPStrict(t *testing.T) {
	svc := &fileRetentionPolicyStub{}
	h, e := HandlerWithFileRetentionPolicy(Handler(nil), authFunc(verified), svc)
	if e != nil {
		t.Fatal(e)
	}
	for _, body := range []string{`{}`, strings.Replace(fileRetentionPolicyJSON, `"0"`, `0`, 1), strings.Replace(fileRetentionPolicyJSON, `"0"`, `"00"`, 1), strings.Replace(fileRetentionPolicyJSON, `"cleanup_enabled":true`, `"cleanup_enabled":true,"cleanup_enabled":false`, 1), strings.Replace(fileRetentionPolicyJSON, `"cleanup_enabled":true`, `"cleanup_enabled":null`, 1), strings.TrimSuffix(fileRetentionPolicyJSON, "}") + `,"tenant_id":"attacker"}`, fileRetentionPolicyJSON + ` {}`, strings.Replace(fileRetentionPolicyJSON, `"365"`, `"0"`, 1)} {
		req := adminRequest(http.MethodPut, "/api/v1/admin/file-retention-policy")
		req.Header.Set("Content-Type", "application/json")
		req.Body = io.NopCloser(strings.NewReader(body))
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != 400 {
			t.Fatal(body, res.Code, res.Body.String())
		}
	}
	if svc.setCalls != 0 {
		t.Fatal("invalid input reached service")
	}
	for _, path := range []string{"/api/v1/admin/file-retention-policy?tenant=x", "/api/v1/admin/file-retention-policy/history?limit=101", "/api/v1/admin/file-retention-policy/history?limit=1&limit=2", "/api/v1/admin/file-retention-policy/history?unknown=x"} {
		req := adminRequest(http.MethodGet, path)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != 400 {
			t.Fatal(path, res.Code)
		}
	}
	req := adminRequest(http.MethodPut, "/api/v1/admin/file-retention-policy")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Body = io.NopCloser(strings.NewReader(fileRetentionPolicyJSON))
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != 400 || svc.setCalls != 0 {
		t.Fatal("encoded input accepted")
	}
}
func TestFileRetentionPolicyHTTPErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{access.ErrInvalidIdentity, 403}, {access.ErrNotFound, 404}, {access.ErrConflict, 409}, {files.ErrInvalidRetentionPolicy, 400}, {access.ErrAuditUnavailable, 503}} {
		svc := &fileRetentionPolicyStub{err: tc.err}
		h, e := HandlerWithFileRetentionPolicy(Handler(nil), authFunc(verified), svc)
		if e != nil {
			t.Fatal(e)
		}
		res := httptest.NewRecorder()
		h.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/admin/file-retention-policy"))
		if res.Code != tc.status {
			t.Fatal(tc.err, res.Code, res.Body.String())
		}
	}
}

func TestFileRetentionPolicyNoEmployeeRoute(t *testing.T) {
	s := &fileRetentionPolicyStub{}
	h, e := HandlerWithFileRetentionPolicy(http.NotFoundHandler(), authFunc(verified), s)
	if e != nil {
		t.Fatal(e)
	}
	r := httptest.NewRecorder()
	h.ServeHTTP(r, adminRequest("GET", "/api/v1/file-retention-policy"))
	if r.Code != 404 {
		t.Fatal(r.Code)
	}
}
