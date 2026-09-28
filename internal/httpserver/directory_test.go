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
