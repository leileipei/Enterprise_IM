package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type selfContextFunc func(context.Context, string, string) (policystore.SelfContext, error)

func (f selfContextFunc) GetSelfContext(ctx context.Context, tenantID, userID string) (policystore.SelfContext, error) {
	return f(ctx, tenantID, userID)
}

func selfRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer verified-token")
	return r
}

func TestSelfContextUsesOnlyBearerIdentity(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	service := selfContextFunc(func(_ context.Context, tenant, user string) (policystore.SelfContext, error) {
		if tenant != tenantID || user != actorID {
			t.Fatalf("untrusted identity: %q %q", tenant, user)
		}
		return policystore.SelfContext{TenantID: tenant, UserID: user, DisplayName: "本人", GlobalEmployeeNo: "E001", Memberships: []policystore.SelfMembership{{ID: actingID, IsPrimary: true}}}, nil
	})
	handler, err := HandlerWithSelfContext(base, authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, selfRequest(http.MethodGet, "/api/v1/me"))
	if res.Code != http.StatusOK || res.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(res.Body.String(), `"tenant_id":"`+tenantID+`"`) ||
		!strings.Contains(res.Body.String(), `"memberships":[{"id":"`+actingID+`"`) {
		t.Fatalf("self response: %d %s", res.Code, res.Body.String())
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, selfRequest(http.MethodGet, "/api/v1/elsewhere"))
	if res.Code != http.StatusTeapot {
		t.Fatalf("base route lost: %d", res.Code)
	}
}

func TestSelfContextRejectsUntrustedInputsAndMethods(t *testing.T) {
	called := false
	handler, err := HandlerWithSelfContext(http.NotFoundHandler(), authFunc(verified), selfContextFunc(func(context.Context, string, string) (policystore.SelfContext, error) {
		called = true
		return policystore.SelfContext{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	requests := []*http.Request{
		selfRequest(http.MethodGet, "/api/v1/me?tenant_id="+tenantID),
		selfRequest(http.MethodGet, "/api/v1/me?"),
		selfRequest(http.MethodGet, "/api/v1/me"),
		selfRequest(http.MethodGet, "/api/v1/me"),
		selfRequest(http.MethodGet, "/api/v1/me"),
	}
	requests[2].Header.Set("X-Acting-Membership-ID", actingID)
	requests[3].Header["X-Acting-Membership-Id"] = []string{}
	requests[4].Body = ioNopCloser("x")
	for _, request := range requests {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, request)
		if res.Code != http.StatusBadRequest || called {
			t.Fatalf("untrusted request accepted: %d %s", res.Code, res.Body.String())
		}
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, selfRequest(http.MethodPost, "/api/v1/me"))
	if res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != "GET" || called {
		t.Fatalf("method accepted: %d %s", res.Code, res.Body.String())
	}
}

func TestSelfContextAuthenticationAndServiceFailures(t *testing.T) {
	serviceError := error(nil)
	service := selfContextFunc(func(context.Context, string, string) (policystore.SelfContext, error) {
		return policystore.SelfContext{}, serviceError
	})
	handler, err := HandlerWithSelfContext(http.NotFoundHandler(), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		request *http.Request
		status  int
	}{
		{httptest.NewRequest(http.MethodGet, "/api/v1/me", nil), http.StatusUnauthorized},
		{selfRequest(http.MethodGet, "/api/v1/me"), http.StatusUnauthorized},
	} {
		if test.request.Header.Get("Authorization") != "" {
			test.request.Header.Set("Authorization", "Bearer invalid-token")
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, test.request)
		if res.Code != test.status || res.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("auth failure: %d %s", res.Code, res.Body.String())
		}
	}
	serviceError = policystore.ErrForbidden
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, selfRequest(http.MethodGet, "/api/v1/me"))
	if res.Code != http.StatusForbidden {
		t.Fatalf("frozen identity: %d %s", res.Code, res.Body.String())
	}
	serviceError = errors.New("internal database detail")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, selfRequest(http.MethodGet, "/api/v1/me"))
	if res.Code != http.StatusServiceUnavailable || strings.Contains(res.Body.String(), "internal database detail") {
		t.Fatalf("database failure: %d %s", res.Code, res.Body.String())
	}
	missingDB, err := HandlerWithSelfContext(http.NotFoundHandler(), authFunc(verified), policystore.Service{})
	if err != nil {
		t.Fatal(err)
	}
	res = httptest.NewRecorder()
	missingDB.ServeHTTP(res, selfRequest(http.MethodGet, "/api/v1/me"))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing database: %d %s", res.Code, res.Body.String())
	}
	unavailable, err := HandlerWithSelfContext(http.NotFoundHandler(), authFunc(func(context.Context, string) (VerifiedIdentity, error) {
		return VerifiedIdentity{}, ErrAuthUnavailable
	}), service)
	if err != nil {
		t.Fatal(err)
	}
	res = httptest.NewRecorder()
	unavailable.ServeHTTP(res, selfRequest(http.MethodGet, "/api/v1/me"))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("auth outage: %d %s", res.Code, res.Body.String())
	}
}

func ioNopCloser(value string) io.ReadCloser { return io.NopCloser(strings.NewReader(value)) }
