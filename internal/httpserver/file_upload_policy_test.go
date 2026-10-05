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

type filePolicyStub struct {
	change   access.FileUploadPolicyChange
	setCalls int
	err      error
}

func (s *filePolicyStub) GetFileUploadPolicy(context.Context, access.TrustedIdentity) (access.FileUploadPolicyRecord, error) {
	return access.FileUploadPolicyRecord{Policy: files.DefaultUploadPolicy(), ApprovalReference: "private-approval", ActorUserID: actorID, ActorMembershipID: actingID, UpdatedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}, s.err
}
func (s *filePolicyStub) SetFileUploadPolicy(_ context.Context, id access.TrustedIdentity, c access.FileUploadPolicyChange) (access.FileUploadPolicyRecord, error) {
	s.change = c
	s.setCalls++
	if id.TenantID != tenantID || id.UserID != actorID || id.ActingMembershipID != actingID {
		panic("untrusted actor")
	}
	return access.FileUploadPolicyRecord{Policy: c.Policy}, s.err
}
func (s *filePolicyStub) ListFileUploadPolicyHistory(context.Context, access.TrustedIdentity, string, int) (access.FileUploadPolicyHistoryPage, error) {
	return access.FileUploadPolicyHistoryPage{History: []access.FileUploadPolicyRecord{}}, s.err
}
func (s *filePolicyStub) GetEffectiveFileUploadPolicy(context.Context, access.TrustedIdentity) (files.UploadPolicy, error) {
	return files.DefaultUploadPolicy(), s.err
}

const filePolicyJSON = `{"enabled":true,"max_size_bytes":"26214400","allowed_media_types":["text/plain"],"upload_ttl_seconds":"900","tenant_storage_budget_bytes":"1073741824","expected_version":"0","approval_reference":"CAB-1"}`

func TestFileUploadPolicyHTTP(t *testing.T) {
	svc := &filePolicyStub{}
	h, e := HandlerWithFileUploadPolicy(Handler(nil), authFunc(verified), svc)
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"/api/v1/file-upload-policy", "/api/v1/admin/file-upload-policy", "/api/v1/admin/file-upload-policy/history?limit=20"} {
		req := adminRequest(http.MethodGet, path)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != 200 || res.Header().Get("Cache-Control") != "no-store" || res.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal(res.Code, res.Header(), res.Body.String())
		}
		if path == "/api/v1/file-upload-policy" && (strings.Contains(res.Body.String(), "approval") || strings.Contains(res.Body.String(), "budget") || strings.Contains(res.Body.String(), "version")) {
			t.Fatal("public policy leak", res.Body.String())
		}
	}
	req := adminRequest(http.MethodPut, "/api/v1/admin/file-upload-policy")
	req.Header.Set("Content-Type", "application/json")
	req.Body = io.NopCloser(strings.NewReader(filePolicyJSON))
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != 200 || svc.setCalls != 1 || svc.change.ExpectedVersion != 0 || !svc.change.Policy.Enabled || svc.change.Policy.MaxSizeBytes != 26214400 || svc.change.ApprovalReference != "CAB-1" {
		t.Fatal(res.Code, svc.change, res.Body.String())
	}
	var dto map[string]any
	if e = json.Unmarshal(res.Body.Bytes(), &dto); e != nil {
		t.Fatal(e)
	}
	if _, ok := dto["max_size_bytes"].(string); !ok {
		t.Fatal("numeric precision unsafe", dto)
	}
}
func TestFileUploadPolicyHTTPStrict(t *testing.T) {
	svc := &filePolicyStub{}
	h, e := HandlerWithFileUploadPolicy(Handler(nil), authFunc(verified), svc)
	if e != nil {
		t.Fatal(e)
	}
	for _, body := range []string{`{}`, strings.Replace(filePolicyJSON, `"0"`, `0`, 1), strings.Replace(filePolicyJSON, `"0"`, `"00"`, 1), strings.Replace(filePolicyJSON, `"enabled":true`, `"enabled":true,"enabled":false`, 1), strings.Replace(filePolicyJSON, `"enabled":true`, `"enabled":null`, 1), strings.TrimSuffix(filePolicyJSON, "}") + `,"tenant_id":"attacker"}`, filePolicyJSON + ` {}`, strings.Replace(filePolicyJSON, `"text/plain"`, `null`, 1)} {
		req := adminRequest(http.MethodPut, "/api/v1/admin/file-upload-policy")
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
	for _, path := range []string{"/api/v1/file-upload-policy?tenant=x", "/api/v1/admin/file-upload-policy/history?limit=101", "/api/v1/admin/file-upload-policy/history?limit=1&limit=2", "/api/v1/admin/file-upload-policy/history?unknown=x"} {
		req := adminRequest(http.MethodGet, path)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != 400 {
			t.Fatal(path, res.Code)
		}
	}
	req := adminRequest(http.MethodPut, "/api/v1/admin/file-upload-policy")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Body = io.NopCloser(strings.NewReader(filePolicyJSON))
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != 400 || svc.setCalls != 0 {
		t.Fatal("encoded input accepted")
	}
}
func TestFileUploadPolicyHTTPErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{access.ErrInvalidIdentity, 403}, {access.ErrNotFound, 404}, {access.ErrConflict, 409}, {files.ErrStorageBudgetExceeded, 409}, {files.ErrInvalidUploadPolicy, 400}, {access.ErrAuditUnavailable, 503}} {
		svc := &filePolicyStub{err: tc.err}
		h, e := HandlerWithFileUploadPolicy(Handler(nil), authFunc(verified), svc)
		if e != nil {
			t.Fatal(e)
		}
		res := httptest.NewRecorder()
		h.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/admin/file-upload-policy"))
		if res.Code != tc.status {
			t.Fatal(tc.err, res.Code, res.Body.String())
		}
	}
}
