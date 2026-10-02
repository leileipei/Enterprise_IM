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

type retentionStub struct {
	get func(context.Context, access.TrustedIdentity) (access.RetentionPolicy, error)
	set func(context.Context, access.TrustedIdentity, int64, int, string) (access.RetentionPolicy, error)
}

func (s retentionStub) GetRetentionPolicy(ctx context.Context, id access.TrustedIdentity) (access.RetentionPolicy, error) {
	return s.get(ctx, id)
}

func (s retentionStub) SetRetentionPolicy(ctx context.Context, id access.TrustedIdentity,
	version int64, days int, reference string) (access.RetentionPolicy, error) {
	return s.set(ctx, id, version, days, reference)
}

func TestRetentionAdminUsesVerifiedActorAndStrictVersionedBody(t *testing.T) {
	calledGet, calledSet := false, false
	approvedAt := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	service := retentionStub{
		get: func(_ context.Context, id access.TrustedIdentity) (access.RetentionPolicy, error) {
			calledGet = true
			if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) {
				t.Fatalf("unverified GET identity: %+v", id)
			}
			return access.RetentionPolicy{MessageBodyDays: 365}, nil
		},
		set: func(_ context.Context, id access.TrustedIdentity, version int64, days int, ref string) (access.RetentionPolicy, error) {
			calledSet = true
			if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) ||
				version != 0 || days != 730 || ref != "CAB-2026-01" {
				t.Fatalf("unverified PUT input: %+v %d %d %q", id, version, days, ref)
			}
			return access.RetentionPolicy{MessageBodyDays: 730, Version: 1,
				ApprovalReference: ref, ApprovedByUserID: actorID, ApprovedAt: &approvedAt}, nil
		},
	}
	handler, err := HandlerWithRetentionPolicy(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	get := adminRequest(http.MethodGet, "/api/v1/admin/retention-policy")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, get)
	if !calledGet || res.Code != http.StatusOK || res.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(res.Body.String(), `"message_body_days":365`) {
		t.Fatalf("retention GET: %d %s", res.Code, res.Body.String())
	}
	put := adminRequest(http.MethodPut, "/api/v1/admin/retention-policy")
	put.Body = io.NopCloser(strings.NewReader(`{"message_body_days":730,"expected_version":0,"approval_reference":"CAB-2026-01"}`))
	put.Header.Set("Content-Type", "application/json")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, put)
	if !calledSet || res.Code != http.StatusOK || res.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(res.Body.String(), `"version":1`) ||
		!strings.Contains(res.Body.String(), `"approved_by_user_id":"`+actorID+`"`) {
		t.Fatalf("retention PUT: %d %s", res.Code, res.Body.String())
	}
}

func TestRetentionAdminRejectsMalformedRequestsBeforeService(t *testing.T) {
	service := retentionStub{
		get: func(context.Context, access.TrustedIdentity) (access.RetentionPolicy, error) {
			t.Fatal("invalid GET reached service")
			return access.RetentionPolicy{}, nil
		},
		set: func(context.Context, access.TrustedIdentity, int64, int, string) (access.RetentionPolicy, error) {
			t.Fatal("invalid PUT reached service")
			return access.RetentionPolicy{}, nil
		},
	}
	handler, err := HandlerWithRetentionPolicy(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{}`, `{"message_body_days":730,"expected_version":0}`,
		`{"message_body_days":730,"expected_version":0,"approval_reference":"CAB","tenant_id":"` + tenantID + `"}`,
		`{"message_body_days":730,"expected_version":0,"approval_reference":"CAB"} {}`,
		`{"message_body_days":30,"expected_version":0,"approval_reference":"CAB","message_body_days":730}`,
		`{"message_body_days":30,"expected_version":0,"approval_reference":"CAB","\u006dessage_body_days":730}`,
		`{"message_body_days":null,"expected_version":0,"approval_reference":"CAB"}`,
		`{"message_body_days":730,"expected_version":0,"approval_reference":"CAB\ud800"}`,
		string(append([]byte(`{"message_body_days":730,"expected_version":0,"approval_reference":"CAB`),
			[]byte{0xff, '"', '}'}...)),
	} {
		req := adminRequest(http.MethodPut, "/api/v1/admin/retention-policy")
		req.Body = io.NopCloser(strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("invalid PUT %q: %d %s", body, res.Code, res.Body.String())
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(method, "/api/v1/admin/retention-policy"))
		if res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != "GET, PUT" {
			t.Fatalf("invalid method %s: %d", method, res.Code)
		}
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/admin/retention-policy?tenant_id="+tenantID))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("query spoofing: %d", res.Code)
	}
}

func TestRetentionAdminMapsConflictAndOutage(t *testing.T) {
	service := retentionStub{
		get: func(context.Context, access.TrustedIdentity) (access.RetentionPolicy, error) {
			return access.RetentionPolicy{}, access.ErrNotFound
		},
		set: func(context.Context, access.TrustedIdentity, int64, int, string) (access.RetentionPolicy, error) {
			return access.RetentionPolicy{}, access.ErrConflict
		},
	}
	handler, err := HandlerWithRetentionPolicy(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/admin/retention-policy"))
	if res.Code != http.StatusNotFound {
		t.Fatalf("not found: %d", res.Code)
	}
	req := adminRequest(http.MethodPut, "/api/v1/admin/retention-policy")
	req.Body = io.NopCloser(strings.NewReader(`{"message_body_days":730,"expected_version":0,"approval_reference":"CAB"}`))
	req.Header.Set("Content-Type", "application/json")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusConflict {
		t.Fatalf("version conflict: %d", res.Code)
	}
	service.get = func(context.Context, access.TrustedIdentity) (access.RetentionPolicy, error) {
		return access.RetentionPolicy{}, errors.New("db down")
	}
	handler, _ = HandlerWithRetentionPolicy(Handler(nil), authFunc(verified), service)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/admin/retention-policy"))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("storage outage: %d", res.Code)
	}
}
