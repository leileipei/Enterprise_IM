package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type directoryFunc func(context.Context, access.TrustedIdentity, string) (policystore.DirectoryMembership, error)

func (f directoryFunc) GetVisibleMembership(ctx context.Context, id access.TrustedIdentity, target string) (policystore.DirectoryMembership, error) {
	return f(ctx, id, target)
}

func (f directoryFunc) FindVisiblePersonByEmployeeNo(context.Context, access.TrustedIdentity, string) (policystore.DirectoryPerson, error) {
	return policystore.DirectoryPerson{}, policystore.ErrDirectoryNotVisible
}

func (f directoryFunc) SearchVisiblePeople(context.Context, access.TrustedIdentity, string, int) (policystore.DirectorySearchPage, error) {
	return policystore.DirectorySearchPage{}, policystore.ErrInvalidDirectorySearch
}

func (f directoryFunc) ListVisibleOrganizations(context.Context, access.TrustedIdentity) ([]policystore.DirectoryOrganization, error) {
	return nil, policystore.ErrDirectoryNotVisible
}

func (f directoryFunc) ListVisibleOrganizationMembers(context.Context, access.TrustedIdentity, string, string, int) (policystore.DirectoryMemberPage, error) {
	return policystore.DirectoryMemberPage{}, policystore.ErrDirectoryNotVisible
}

type directoryLookupStub struct {
	lookup func(context.Context, access.TrustedIdentity, string) (policystore.DirectoryPerson, error)
}

func (s directoryLookupStub) GetVisibleMembership(context.Context, access.TrustedIdentity, string) (policystore.DirectoryMembership, error) {
	return policystore.DirectoryMembership{}, policystore.ErrDirectoryNotVisible
}

func (s directoryLookupStub) FindVisiblePersonByEmployeeNo(ctx context.Context, id access.TrustedIdentity, number string) (policystore.DirectoryPerson, error) {
	return s.lookup(ctx, id, number)
}

func (s directoryLookupStub) SearchVisiblePeople(context.Context, access.TrustedIdentity, string, int) (policystore.DirectorySearchPage, error) {
	return policystore.DirectorySearchPage{}, policystore.ErrInvalidDirectorySearch
}

func (s directoryLookupStub) ListVisibleOrganizations(context.Context, access.TrustedIdentity) ([]policystore.DirectoryOrganization, error) {
	return nil, policystore.ErrDirectoryNotVisible
}

func (s directoryLookupStub) ListVisibleOrganizationMembers(context.Context, access.TrustedIdentity, string, string, int) (policystore.DirectoryMemberPage, error) {
	return policystore.DirectoryMemberPage{}, policystore.ErrDirectoryNotVisible
}

type directorySearchStub struct {
	search func(context.Context, access.TrustedIdentity, string, int) (policystore.DirectorySearchPage, error)
}

func (s directorySearchStub) GetVisibleMembership(context.Context, access.TrustedIdentity, string) (policystore.DirectoryMembership, error) {
	return policystore.DirectoryMembership{}, policystore.ErrDirectoryNotVisible
}

func (s directorySearchStub) FindVisiblePersonByEmployeeNo(context.Context, access.TrustedIdentity, string) (policystore.DirectoryPerson, error) {
	return policystore.DirectoryPerson{}, policystore.ErrDirectoryNotVisible
}

func (s directorySearchStub) SearchVisiblePeople(ctx context.Context, id access.TrustedIdentity, q string, limit int) (policystore.DirectorySearchPage, error) {
	return s.search(ctx, id, q, limit)
}

func (s directorySearchStub) ListVisibleOrganizations(context.Context, access.TrustedIdentity) ([]policystore.DirectoryOrganization, error) {
	return nil, policystore.ErrDirectoryNotVisible
}

func (s directorySearchStub) ListVisibleOrganizationMembers(context.Context, access.TrustedIdentity, string, string, int) (policystore.DirectoryMemberPage, error) {
	return policystore.DirectoryMemberPage{}, policystore.ErrDirectoryNotVisible
}

type directoryOrganizationsStub struct {
	list func(context.Context, access.TrustedIdentity) ([]policystore.DirectoryOrganization, error)
}

func (s directoryOrganizationsStub) GetVisibleMembership(context.Context, access.TrustedIdentity, string) (policystore.DirectoryMembership, error) {
	return policystore.DirectoryMembership{}, policystore.ErrDirectoryNotVisible
}

func (s directoryOrganizationsStub) FindVisiblePersonByEmployeeNo(context.Context, access.TrustedIdentity, string) (policystore.DirectoryPerson, error) {
	return policystore.DirectoryPerson{}, policystore.ErrDirectoryNotVisible
}

func (s directoryOrganizationsStub) SearchVisiblePeople(context.Context, access.TrustedIdentity, string, int) (policystore.DirectorySearchPage, error) {
	return policystore.DirectorySearchPage{}, policystore.ErrInvalidDirectorySearch
}

func (s directoryOrganizationsStub) ListVisibleOrganizations(ctx context.Context, id access.TrustedIdentity) ([]policystore.DirectoryOrganization, error) {
	return s.list(ctx, id)
}

func (s directoryOrganizationsStub) ListVisibleOrganizationMembers(context.Context, access.TrustedIdentity, string, string, int) (policystore.DirectoryMemberPage, error) {
	return policystore.DirectoryMemberPage{}, policystore.ErrDirectoryNotVisible
}

type directoryOrganizationMembersStub struct {
	directoryFunc
	list func(context.Context, access.TrustedIdentity, string, string, int) (policystore.DirectoryMemberPage, error)
}

func (s directoryOrganizationMembersStub) ListVisibleOrganizationMembers(ctx context.Context, id access.TrustedIdentity, orgID, after string, limit int) (policystore.DirectoryMemberPage, error) {
	return s.list(ctx, id, orgID, after, limit)
}

func TestDirectoryOrganizationMembersRouteUsesVerifiedActorAndVisiblePage(t *testing.T) {
	service := directoryOrganizationMembersStub{list: func(_ context.Context, id access.TrustedIdentity, orgID, after string, limit int) (policystore.DirectoryMemberPage, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) ||
			orgID != tenantID || after != targetMemID || limit != 1 {
			t.Fatalf("untrusted organization member input: %+v %q %q %d", id, orgID, after, limit)
		}
		return policystore.DirectoryMemberPage{People: []policystore.DirectoryPerson{{ID: targetUserID,
			DisplayName: "同事", EmployeeNo: "A002", Memberships: []policystore.DirectoryMembership{{MembershipID: targetMemID,
				OrganizationID: tenantID, OrganizationName: "总部", Departments: []access.Department{}}}}},
			HasMore: true, NextAfter: targetMemID}, nil
	}}
	handler, err := HandlerWithDirectory(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := adminRequest(http.MethodGet, "/api/v1/directory/organizations/"+tenantID+"/members?limit=1&after="+targetMemID)
	req.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"people":[`) ||
		!strings.Contains(res.Body.String(), `"membership_id":"`+targetMemID+`"`) ||
		!strings.Contains(res.Body.String(), `"has_more":true`) ||
		!strings.Contains(res.Body.String(), `"next_after":"`+targetMemID+`"`) {
		t.Fatalf("organization member response: %d %s", res.Code, res.Body.String())
	}
}

func TestDirectoryOrganizationMembersRejectsMalformedRequestsAndMapsErrors(t *testing.T) {
	service := directoryOrganizationMembersStub{list: func(context.Context, access.TrustedIdentity, string, string, int) (policystore.DirectoryMemberPage, error) {
		return policystore.DirectoryMemberPage{People: []policystore.DirectoryPerson{}}, nil
	}}
	handler, err := HandlerWithDirectory(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/v1/directory/organizations/bad/members",
		"/api/v1/directory/organizations/" + tenantID + "/members?limit=0",
		"/api/v1/directory/organizations/" + tenantID + "/members?limit=21",
		"/api/v1/directory/organizations/" + tenantID + "/members?limit=x",
		"/api/v1/directory/organizations/" + tenantID + "/members?after=bad",
		"/api/v1/directory/organizations/" + tenantID + "/members?after=",
		"/api/v1/directory/organizations/" + tenantID + "/members?limit=1&limit=2",
		"/api/v1/directory/organizations/" + tenantID + "/members?include_hidden=true",
		"/api/v1/directory/organizations/" + tenantID + "/members?limit=%ZZ",
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(http.MethodGet, path))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("invalid organization member request %q: %d %s", path, res.Code, res.Body.String())
		}
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, "/api/v1/directory/organizations/" + tenantID + "/members", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/directory/organizations/" + tenantID + "/members/extra", http.StatusNotFound},
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(tc.method, tc.path))
		if res.Code != tc.status {
			t.Fatalf("organization member path %q: %d %s", tc.path, res.Code, res.Body.String())
		}
	}
	for _, failure := range []struct {
		err    error
		status int
		code   string
	}{
		{policystore.ErrDirectoryNotVisible, http.StatusNotFound, "not_found"},
		{policystore.ErrForbidden, http.StatusForbidden, "invalid_identity"},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private detail")), http.StatusServiceUnavailable, "unavailable"},
	} {
		failed, err := HandlerWithDirectory(Handler(nil), authFunc(verified), directoryOrganizationMembersStub{list: func(context.Context, access.TrustedIdentity, string, string, int) (policystore.DirectoryMemberPage, error) {
			return policystore.DirectoryMemberPage{}, failure.err
		}})
		if err != nil {
			t.Fatal(err)
		}
		res := httptest.NewRecorder()
		failed.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/directory/organizations/"+tenantID+"/members"))
		if res.Code != failure.status || !strings.Contains(res.Body.String(), failure.code) || strings.Contains(res.Body.String(), "private detail") {
			t.Fatalf("organization member service error: %d %s", res.Code, res.Body.String())
		}
	}
}

func TestDirectoryOrganizationsRouteUsesVerifiedActorAndReturnsTree(t *testing.T) {
	service := directoryOrganizationsStub{list: func(_ context.Context, id access.TrustedIdentity) ([]policystore.DirectoryOrganization, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) {
			t.Fatalf("untrusted organization actor: %+v", id)
		}
		return []policystore.DirectoryOrganization{{ID: tenantID, Name: "集团", OrgType: "virtual_group"},
			{ID: targetMemID, ParentID: tenantID, Name: "公司", OrgType: "company", HasVisibleMembers: true}}, nil
	}}
	handler, err := HandlerWithDirectory(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := adminRequest(http.MethodGet, "/api/v1/directory/organizations")
	req.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"organizations":[`) ||
		!strings.Contains(res.Body.String(), `"parent_id":null`) ||
		!strings.Contains(res.Body.String(), `"parent_id":"`+tenantID+`"`) ||
		!strings.Contains(res.Body.String(), `"has_visible_members":true`) {
		t.Fatalf("organization tree response: %d %s", res.Code, res.Body.String())
	}
}

func TestDirectoryOrganizationsRouteRejectsQueryAndMapsErrors(t *testing.T) {
	handler, err := HandlerWithDirectory(Handler(nil), authFunc(verified), directoryOrganizationsStub{list: func(context.Context, access.TrustedIdentity) ([]policystore.DirectoryOrganization, error) {
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/directory/organizations?include_hidden=true"))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("query parameters accepted: %d %s", res.Code, res.Body.String())
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodPost, "/api/v1/directory/organizations"))
	if res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != "GET" {
		t.Fatalf("wrong organization method: %d %s", res.Code, res.Body.String())
	}
	for _, failure := range []struct {
		err    error
		status int
	}{
		{policystore.ErrForbidden, http.StatusForbidden},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private detail")), http.StatusServiceUnavailable},
	} {
		failed, err := HandlerWithDirectory(Handler(nil), authFunc(verified), directoryOrganizationsStub{list: func(context.Context, access.TrustedIdentity) ([]policystore.DirectoryOrganization, error) {
			return nil, failure.err
		}})
		if err != nil {
			t.Fatal(err)
		}
		res := httptest.NewRecorder()
		failed.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/directory/organizations"))
		if res.Code != failure.status || strings.Contains(res.Body.String(), "private detail") {
			t.Fatalf("organization error: %d %s", res.Code, res.Body.String())
		}
	}
}

func TestDirectoryNameSearchRouteUsesVerifiedActorAndVisiblePage(t *testing.T) {
	service := directorySearchStub{search: func(_ context.Context, id access.TrustedIdentity, q string, limit int) (policystore.DirectorySearchPage, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || q != "同事" || limit != 1 {
			t.Fatalf("untrusted name search input: %+v %q %d", id, q, limit)
		}
		return policystore.DirectorySearchPage{People: []policystore.DirectoryPerson{{ID: targetUserID,
			DisplayName: "同事", EmployeeNo: "A002", Memberships: []policystore.DirectoryMembership{{MembershipID: targetMemID,
				OrganizationID: tenantID, OrganizationName: "总部", Departments: []access.Department{}}}}}, HasMore: true}, nil
	}}
	handler, err := HandlerWithDirectory(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := adminRequest(http.MethodGet, "/api/v1/directory/users?q=%20%E5%90%8C%E4%BA%8B%20&limit=1")
	req.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"people":[`) ||
		!strings.Contains(res.Body.String(), `"employee_no":"A002"`) ||
		!strings.Contains(res.Body.String(), `"membership_id":"`+targetMemID+`"`) ||
		!strings.Contains(res.Body.String(), `"has_more":true`) {
		t.Fatalf("name search response: %d %s", res.Code, res.Body.String())
	}
}

func TestDirectoryNameSearchRouteRejectsInvalidParametersAndMapsErrors(t *testing.T) {
	service := directorySearchStub{search: func(context.Context, access.TrustedIdentity, string, int) (policystore.DirectorySearchPage, error) {
		return policystore.DirectorySearchPage{}, nil
	}}
	handler, err := HandlerWithDirectory(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/v1/directory/users?q=", "/api/v1/directory/users?q=A",
		"/api/v1/directory/users?q=%20%20", "/api/v1/directory/users?q=%00A",
		"/api/v1/directory/users?q=%FF", "/api/v1/directory/users?q=%ZZ",
		"/api/v1/directory/users?q=" + strings.Repeat("A", 101),
		"/api/v1/directory/users?q=AB&q=CD", "/api/v1/directory/users?q=AB&employee_no=A002",
		"/api/v1/directory/users?q=AB&unknown=true", "/api/v1/directory/users?q=AB&limit=0",
		"/api/v1/directory/users?q=AB&limit=21", "/api/v1/directory/users?q=AB&limit=1&limit=2",
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(http.MethodGet, path))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("invalid name search %q: %d %s", path, res.Code, res.Body.String())
		}
	}
	for _, failure := range []struct {
		err    error
		status int
		code   string
	}{
		{policystore.ErrDirectorySearchTooBroad, http.StatusBadRequest, "refine_search"},
		{policystore.ErrForbidden, http.StatusForbidden, "invalid_identity"},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private detail")), http.StatusServiceUnavailable, "unavailable"},
	} {
		failed, err := HandlerWithDirectory(Handler(nil), authFunc(verified), directorySearchStub{search: func(context.Context, access.TrustedIdentity, string, int) (policystore.DirectorySearchPage, error) {
			return policystore.DirectorySearchPage{}, failure.err
		}})
		if err != nil {
			t.Fatal(err)
		}
		res := httptest.NewRecorder()
		failed.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/directory/users?q=AB"))
		if res.Code != failure.status || !strings.Contains(res.Body.String(), failure.code) || strings.Contains(res.Body.String(), "private detail") {
			t.Fatalf("name search error: %d %s", res.Code, res.Body.String())
		}
	}
}

func TestDirectoryLookupRouteUsesVerifiedActorAndReturnsVisibleAssignments(t *testing.T) {
	service := directoryLookupStub{lookup: func(_ context.Context, id access.TrustedIdentity, number string) (policystore.DirectoryPerson, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || number != "A002" {
			t.Fatalf("untrusted lookup input: %+v %q", id, number)
		}
		return policystore.DirectoryPerson{ID: targetUserID, DisplayName: "同事", EmployeeNo: "A002",
			Memberships: []policystore.DirectoryMembership{{MembershipID: targetMemID, OrganizationID: tenantID,
				OrganizationName: "总部", Title: "工程师", Departments: []access.Department{{ID: actingID, Name: "技术部"}}}}}, nil
	}}
	handler, err := HandlerWithDirectory(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := adminRequest(http.MethodGet, "/api/v1/directory/users?employee_no=%20A002%20")
	req.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"employee_no":"A002"`) ||
		!strings.Contains(res.Body.String(), `"membership_id":"`+targetMemID+`"`) ||
		!strings.Contains(res.Body.String(), `"departments":[`) || strings.Contains(res.Body.String(), "TenantID") {
		t.Fatalf("lookup response: %d %s", res.Code, res.Body.String())
	}
}

func TestDirectoryLookupRouteRejectsMalformedQueriesAndMapsErrors(t *testing.T) {
	handler, err := HandlerWithDirectory(Handler(nil), authFunc(verified), directoryLookupStub{lookup: func(context.Context, access.TrustedIdentity, string) (policystore.DirectoryPerson, error) {
		return policystore.DirectoryPerson{}, policystore.ErrDirectoryNotVisible
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/v1/directory/users", "/api/v1/directory/users?employee_no=%20",
		"/api/v1/directory/users?employee_no=A002&employee_no=A003",
		"/api/v1/directory/users?employee_no=A002&scope_allowed=true",
		"/api/v1/directory/users?employee_no=A%00", "/api/v1/directory/users?employee_no=%FF",
		"/api/v1/directory/users?employee_no=" + strings.Repeat("A", 129),
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(http.MethodGet, path))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("invalid lookup %q: %d %s", path, res.Code, res.Body.String())
		}
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodPost, "/api/v1/directory/users?employee_no=A002"))
	if res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != "GET" {
		t.Fatalf("wrong method: %d %s", res.Code, res.Body.String())
	}
	for _, failure := range []struct {
		err    error
		status int
	}{
		{policystore.ErrDirectoryNotVisible, http.StatusNotFound},
		{policystore.ErrForbidden, http.StatusForbidden},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private detail")), http.StatusServiceUnavailable},
	} {
		failed, err := HandlerWithDirectory(Handler(nil), authFunc(verified), directoryLookupStub{lookup: func(context.Context, access.TrustedIdentity, string) (policystore.DirectoryPerson, error) {
			return policystore.DirectoryPerson{}, failure.err
		}})
		if err != nil {
			t.Fatal(err)
		}
		res := httptest.NewRecorder()
		failed.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/directory/users?employee_no=A002"))
		if res.Code != failure.status || strings.Contains(res.Body.String(), "private detail") {
			t.Fatalf("lookup error: %d %s", res.Code, res.Body.String())
		}
	}
}

func TestDirectoryRouteRequiresDependenciesAndToken(t *testing.T) {
	service := directoryFunc(func(context.Context, access.TrustedIdentity, string) (policystore.DirectoryMembership, error) {
		return policystore.DirectoryMembership{}, nil
	})
	if _, err := HandlerWithDirectory(nil, authFunc(verified), service); err == nil {
		t.Fatal("nil base accepted")
	}
	if _, err := HandlerWithDirectory(Handler(nil), nil, service); err == nil {
		t.Fatal("nil authenticator accepted")
	}
	if _, err := HandlerWithDirectory(Handler(nil), authFunc(verified), nil); err == nil {
		t.Fatal("nil service accepted")
	}
	handler, err := HandlerWithDirectory(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/directory/memberships/"+targetMemID, nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("anonymous directory: %d %s", res.Code, res.Body.String())
	}
}

func TestDirectoryRouteUsesVerifiedActorAndReturnsSelectedProfile(t *testing.T) {
	service := directoryFunc(func(_ context.Context, id access.TrustedIdentity, target string) (policystore.DirectoryMembership, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || target != targetMemID {
			t.Fatalf("wrong identity or target: %+v %s", id, target)
		}
		return policystore.DirectoryMembership{UserID: targetUserID, DisplayName: "同事", EmployeeNo: "A002",
			MembershipID: targetMemID, OrganizationID: tenantID, OrganizationName: "总部", Title: "工程师",
			Departments: []access.Department{{ID: actingID, Name: "技术部"}}}, nil
	})
	handler, err := HandlerWithDirectory(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	req := adminRequest(http.MethodGet, "/api/v1/directory/memberships/"+targetMemID)
	req.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"display_name":"同事"`) ||
		!strings.Contains(res.Body.String(), `"membership_id":"`+targetMemID+`"`) ||
		!strings.Contains(res.Body.String(), `"departments":[`) || strings.Contains(res.Body.String(), "TenantID") {
		t.Fatalf("directory response: %d %s", res.Code, res.Body.String())
	}
}

func TestDirectoryRouteRejectsInvalidPathsAndMapsDeny(t *testing.T) {
	service := directoryFunc(func(_ context.Context, _ access.TrustedIdentity, _ string) (policystore.DirectoryMembership, error) {
		return policystore.DirectoryMembership{}, policystore.ErrDirectoryNotVisible
	})
	handler, err := HandlerWithDirectory(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/api/v1/directory/memberships/not-uuid", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/directory/memberships/" + targetMemID + "?scope_allowed=true", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/directory/memberships/" + targetMemID + "/extra", http.StatusNotFound},
		{http.MethodGet, "/api/v1/directory/unknown", http.StatusNotFound},
		{http.MethodPost, "/api/v1/directory/memberships/" + targetMemID, http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/directory/memberships/" + targetMemID, http.StatusNotFound},
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, adminRequest(tc.method, tc.path))
		if res.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, res.Code, res.Body.String())
		}
	}
	for _, failure := range []struct {
		err    error
		status int
	}{
		{policystore.ErrForbidden, http.StatusForbidden},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private database detail")), http.StatusServiceUnavailable},
	} {
		failed, err := HandlerWithDirectory(Handler(nil), authFunc(verified), directoryFunc(func(context.Context, access.TrustedIdentity, string) (policystore.DirectoryMembership, error) {
			return policystore.DirectoryMembership{}, failure.err
		}))
		if err != nil {
			t.Fatal(err)
		}
		res := httptest.NewRecorder()
		failed.ServeHTTP(res, adminRequest(http.MethodGet, "/api/v1/directory/memberships/"+targetMemID))
		if res.Code != failure.status || strings.Contains(res.Body.String(), "private database detail") {
			t.Fatalf("service error leaked: %d %s", res.Code, res.Body.String())
		}
	}
}
