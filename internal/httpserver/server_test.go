package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type pingFunc func(context.Context) error

func (f pingFunc) Ping(ctx context.Context) error { return f(ctx) }

func TestLiveDoesNotDependOnDatabase(t *testing.T) {
	handler := Handler(pingFunc(func(context.Context) error { return errors.New("database down") }))
	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"status":"ok"`) {
		t.Fatalf("unexpected live response: %d %s", res.Code, res.Body.String())
	}
}

func TestReadySucceedsOnlyWhenDatabaseResponds(t *testing.T) {
	handler := Handler(pingFunc(func(context.Context) error { return nil }))
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"status":"ready"`) {
		t.Fatalf("unexpected ready response: %d %s", res.Code, res.Body.String())
	}
}

func TestReadyFailureDoesNotExposeDatabaseDetails(t *testing.T) {
	handler := Handler(pingFunc(func(context.Context) error { return errors.New("password=internal-secret") }))
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable || !strings.Contains(res.Body.String(), `"status":"unavailable"`) || strings.Contains(res.Body.String(), "internal-secret") {
		t.Fatalf("unexpected unavailable response: %d %s", res.Code, res.Body.String())
	}
}

func TestHealthRejectsPost(t *testing.T) {
	handler := Handler(pingFunc(func(context.Context) error { return nil }))
	req := httptest.NewRequest(http.MethodPost, "/health/live", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected status: %d", res.Code)
	}
}
