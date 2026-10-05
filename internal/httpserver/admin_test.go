package httpserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leileipei/Enterprise_IM/internal/access"
)

const (
	tenantID     = "11111111-1111-4111-8111-111111111111"
	actorID      = "22222222-2222-4222-8222-222222222222"
	actingID     = "33333333-3333-4333-8333-333333333333"
	targetUserID = "44444444-4444-4444-8444-444444444444"
	targetMemID  = "55555555-5555-4555-8555-555555555555"
)

type authFunc func(context.Context, string) (VerifiedIdentity, error)

func (f authFunc) Authenticate(ctx context.Context, token string) (VerifiedIdentity, error) {
	return f(ctx, token)
}

type adminStub struct {
	get    func(context.Context, access.TrustedIdentity, string) (access.Person, error)
	end    func(context.Context, access.TrustedIdentity, string) error
	search func(context.Context, access.TrustedIdentity, string, int) (access.SearchPage, error)
}

func (s adminStub) GetManagedPerson(ctx context.Context, id access.TrustedIdentity, target string) (access.Person, error) {
	return s.get(ctx, id, target)
}

func (s adminStub) EndMembership(ctx context.Context, id access.TrustedIdentity, target string) error {
	return s.end(ctx, id, target)
}

func (s adminStub) SearchManagedPeople(ctx context.Context, id access.TrustedIdentity, query string, limit int) (access.SearchPage, error) {
	return s.search(ctx, id, query, limit)
}

func verified(_ context.Context, token string) (VerifiedIdentity, error) {
	if token != "verified-token" {
		return VerifiedIdentity{}, errors.New("secret verifier detail")
	}
	return VerifiedIdentity{TenantID: tenantID, UserID: actorID}, nil
}

func testAdmin() adminStub {
	return adminStub{
		get: func(context.Context, access.TrustedIdentity, string) (access.Person, error) {
			return access.Person{ID: targetUserID, DisplayName: "目标用户", Memberships: []access.Membership{{ID: targetMemID, OrganizationID: tenantID, OrganizationName: "总部", Departments: []access.Department{{ID: actingID, Name: "信息中心"}}}}}, nil
		},
		end: func(context.Context, access.TrustedIdentity, string) error { return nil },
		search: func(context.Context, access.TrustedIdentity, string, int) (access.SearchPage, error) {
			return access.SearchPage{People: []access.SearchPerson{}}, nil
		},
	}
}

func TestAdminSearchUsesVerifiedIdentityAndBoundedQuery(t *testing.T) {
	service := testAdmin()
	service.search = func(_ context.Context, id access.TrustedIdentity, query string, limit int) (access.SearchPage, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || query != "双任职" || limit != 1 {
			t.Fatalf("unexpected search input: %+v %q %d", id, query, limit)
		}
		return access.SearchPage{People: []access.SearchPerson{{ID: targetUserID, EmployeeNo: "A002", DisplayName: "双任职人员"}}, HasMore: true}, nil
	}
	handler, err := HandlerWithAdmin(nil, authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/admin/users?q=%E5%8F%8C%E4%BB%BB%E8%81%8C&limit=1"))
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"employee_no":"A002"`) || !strings.Contains(res.Body.String(), `"has_more":true`) || strings.Contains(res.Body.String(), "TenantID") {
		t.Fatalf("search response: %d %s", res.Code, res.Body.String())
	}
}

func TestAdminSearchRejectsMalformedParameters(t *testing.T) {
	handler, err := HandlerWithAdmin(nil, authFunc(verified), testAdmin())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/v1/admin/users", "/api/v1/admin/users?q=%20", "/api/v1/admin/users?q=a&limit=0",
		"/api/v1/admin/users?q=a&limit=51", "/api/v1/admin/users?q=a&limit=no",
		"/api/v1/admin/users?q=a&q=b", "/api/v1/admin/users?q=a&tenant_id=" + tenantID,
		"/api/v1/admin/users?q=" + strings.Repeat("a", 101), "/api/v1/admin/users?q=a%00",
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(http.MethodGet, path))
		if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), `"error_code":"invalid_search"`) {
			t.Fatalf("invalid search %q: %d %s", path, res.Code, res.Body.String())
		}
	}
	malformed := adminRequest(http.MethodGet, "/api/v1/admin/users?q=a")
	malformed.URL.RawQuery = "q=a&limit=%ZZ"
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, malformed)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("malformed encoding accepted: %d %s", res.Code, res.Body.String())
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodPost, "/api/v1/admin/users?q=a"))
	if res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != "GET" {
		t.Fatalf("wrong search method: %d %s", res.Code, res.Body.String())
	}
}

func adminRequest(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer verified-token")
	req.Header.Set("X-Acting-Membership-ID", actingID)
	return req
}

func TestAdminRoutesRequireDependencies(t *testing.T) {
	if _, err := HandlerWithAdmin(nil, nil, testAdmin()); err == nil {
		t.Fatal("missing authenticator accepted")
	}
	if _, err := HandlerWithAdmin(nil, authFunc(verified), nil); err == nil {
		t.Fatal("missing admin service accepted")
	}
	plain := Handler(nil)
	res := httptest.NewRecorder()
	plain.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/admin/users/"+targetUserID))
	if res.Code != http.StatusNotFound {
		t.Fatalf("plain handler exposed admin route: %d", res.Code)
	}
}

func TestAdminRejectsMissingMalformedAndUnverifiedTokens(t *testing.T) {
	handler, err := HandlerWithAdmin(nil, authFunc(verified), testAdmin())
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"", "Basic verified-token", "Bearer", "Bearer verified-token extra", "Bearer wrong"} {
		req := adminRequest(http.MethodGet, "/api/v1/admin/users/"+targetUserID)
		req.Header.Set("Authorization", header)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized || !strings.Contains(res.Body.String(), `"error_code":"unauthorized"`) || strings.Contains(res.Body.String(), "secret verifier detail") {
			t.Fatalf("header %q: %d %s", header, res.Code, res.Body.String())
		}
	}
}

func TestAdminRejectsInvalidVerifiedIdentityAndActingMembership(t *testing.T) {
	handler, err := HandlerWithAdmin(nil, authFunc(func(context.Context, string) (VerifiedIdentity, error) {
		return VerifiedIdentity{TenantID: "untrusted", UserID: actorID}, nil
	}), testAdmin())
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/admin/users/"+targetUserID))
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("invalid identity: %d", res.Code)
	}

	handler, err = HandlerWithAdmin(nil, authFunc(verified), testAdmin())
	if err != nil {
		t.Fatal(err)
	}
	for _, membership := range []string{"", "not-a-uuid"} {
		req := adminRequest(http.MethodGet, "/api/v1/admin/users/"+targetUserID)
		req.Header.Set("X-Acting-Membership-ID", membership)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("membership %q: %d", membership, res.Code)
		}
	}
}

func TestAdminPersonUsesVerifiedActorAndStableResponse(t *testing.T) {
	called := false
	service := testAdmin()
	service.get = func(_ context.Context, id access.TrustedIdentity, target string) (access.Person, error) {
		called = true
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || target != targetUserID {
			t.Fatalf("wrong service input: %+v %s", id, target)
		}
		return access.Person{ID: targetUserID, DisplayName: "目标用户", Memberships: []access.Membership{{ID: targetMemID, OrganizationID: tenantID, OrganizationName: "总部", Departments: []access.Department{{ID: actingID, Name: "信息中心"}}}}}, nil
	}
	handler, err := HandlerWithAdmin(nil, authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := adminRequest(http.MethodGet, "/api/v1/admin/users/"+targetUserID)
	req.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if !called || res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"display_name":"目标用户"`) || !strings.Contains(res.Body.String(), `"organization_name":"总部"`) || strings.Contains(res.Body.String(), `"DisplayName"`) {
		t.Fatalf("unexpected response: %d %s", res.Code, res.Body.String())
	}
}

func TestAdminEndMembershipUsesVerifiedActor(t *testing.T) {
	service := testAdmin()
	service.end = func(_ context.Context, id access.TrustedIdentity, target string) error {
		if id.TenantID != tenantID || id.UserID != actorID || id.ActingMembershipID != actingID || target != targetMemID {
			t.Fatalf("wrong service input: %+v %s", id, target)
		}
		return nil
	}
	handler, err := HandlerWithAdmin(nil, authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodPost, "/api/v1/admin/memberships/"+targetMemID+":end"))
	if res.Code != http.StatusNoContent || res.Body.Len() != 0 {
		t.Fatalf("unexpected response: %d %s", res.Code, res.Body.String())
	}
}

func TestAdminRejectsInvalidTargetsAndBody(t *testing.T) {
	handler, err := HandlerWithAdmin(nil, authFunc(verified), testAdmin())
	if err != nil {
		t.Fatal(err)
	}
	requests := []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, "/api/v1/admin/users/not-a-uuid", "", http.StatusBadRequest},
		{http.MethodPost, "/api/v1/admin/memberships/not-a-uuid:end", "", http.StatusBadRequest},
		{http.MethodPost, "/api/v1/admin/memberships/" + targetMemID, "", http.StatusNotFound},
		{http.MethodPost, "/api/v1/admin/memberships/" + targetMemID + ":end", `{ "role": "group_admin" }`, http.StatusBadRequest},
	}
	for _, tc := range requests {
		req := adminRequest(tc.method, tc.path)
		req.Body = io.NopCloser(strings.NewReader(tc.body))
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != tc.status {
			t.Fatalf("%s %s: got %d want %d", tc.method, tc.path, res.Code, tc.status)
		}
	}
}

func TestAdminRejectsDuplicateIdentityHeadersAndAuthOutage(t *testing.T) {
	handler, err := HandlerWithAdmin(nil, authFunc(verified), testAdmin())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Authorization", "X-Acting-Membership-ID"} {
		req := adminRequest(http.MethodGet, "/api/v1/admin/users/"+targetUserID)
		req.Header.Add(name, req.Header.Get(name))
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if name == "Authorization" && res.Code != http.StatusUnauthorized {
			t.Fatalf("duplicate auth: %d", res.Code)
		}
		if name == "X-Acting-Membership-ID" && res.Code != http.StatusBadRequest {
			t.Fatalf("duplicate membership: %d", res.Code)
		}
	}
	failed, err := HandlerWithAdmin(nil, authFunc(func(context.Context, string) (VerifiedIdentity, error) {
		return VerifiedIdentity{}, errors.Join(ErrAuthUnavailable, errors.New("connection secret"))
	}), testAdmin())
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	failed.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/admin/users/"+targetUserID))
	if res.Code != http.StatusServiceUnavailable || strings.Contains(res.Body.String(), "connection secret") {
		t.Fatalf("auth outage: %d %s", res.Code, res.Body.String())
	}
}

func TestAdminAuthenticatesBeforeMethodAndPathDispatch(t *testing.T) {
	handler, err := HandlerWithAdmin(nil, authFunc(verified), testAdmin())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/admin/users/" + targetUserID, "/api/v1/admin/users//" + targetUserID} {
		req := httptest.NewRequest(http.MethodDelete, path, nil)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized || !strings.Contains(res.Body.String(), `"error_code":"unauthorized"`) || res.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unauthenticated %s: %d %s", path, res.Code, res.Body.String())
		}
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodDelete, "/api/v1/admin/users/"+targetUserID))
	if res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != "GET" || !strings.Contains(res.Body.String(), `"error_code":"method_not_allowed"`) {
		t.Fatalf("authenticated wrong method: %d %s", res.Code, res.Body.String())
	}
}

func TestAdminLogsRejectionsBeforeDomainService(t *testing.T) {
	var logOutput bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logOutput, nil)))
	defer slog.SetDefault(previous)
	handler, err := HandlerWithAdmin(nil, authFunc(verified), testAdmin())
	if err != nil {
		t.Fatal(err)
	}
	requests := []*http.Request{
		adminRequest(http.MethodGet, "/api/v1/admin/users/not-a-uuid"),
		adminRequest(http.MethodPost, "/api/v1/admin/memberships/"+targetMemID+":end"),
		adminRequest(http.MethodGet, "/api/v1/admin/users/"+targetUserID),
	}
	requests[1].Body = io.NopCloser(strings.NewReader("unexpected"))
	requests[2].Header.Del("X-Acting-Membership-ID")
	for _, req := range requests {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("request %s: %d", req.URL.Path, res.Code)
		}
	}
	for _, code := range []string{"invalid_id", "unexpected_body", "invalid_acting_membership"} {
		if !strings.Contains(logOutput.String(), `"error_code":"`+code+`"`) {
			t.Fatalf("missing rejection log for %s: %s", code, logOutput.String())
		}
	}
}

func TestAdminErrorMappingHidesDetails(t *testing.T) {
	cases := []struct {
		serviceError error
		status       int
		code         string
	}{
		{access.ErrInvalidIdentity, http.StatusForbidden, "invalid_identity"},
		{access.ErrNotFound, http.StatusNotFound, "not_found"},
		{access.ErrConflict, http.StatusConflict, "conflict"},
		{errors.Join(access.ErrAuditUnavailable, errors.New("password=secret")), http.StatusServiceUnavailable, "unavailable"},
		{errors.New("password=secret"), http.StatusServiceUnavailable, "unavailable"},
	}
	for _, tc := range cases {
		service := testAdmin()
		service.get = func(context.Context, access.TrustedIdentity, string) (access.Person, error) {
			return access.Person{}, tc.serviceError
		}
		handler, err := HandlerWithAdmin(nil, authFunc(verified), service)
		if err != nil {
			t.Fatal(err)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/admin/users/"+targetUserID))
		if res.Code != tc.status || !strings.Contains(res.Body.String(), `"error_code":"`+tc.code+`"`) || strings.Contains(res.Body.String(), "password=secret") {
			t.Fatalf("error %v: %d %s", tc.serviceError, res.Code, res.Body.String())
		}
	}
}
