package main

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestAppendRuntimeDefaultOff(t *testing.T) {
	for _, v := range []string{"", "false"} {
		enabled, e := importEnabledFromEnv(func(string) string { return v }, false)
		if e != nil || enabled {
			t.Fatal("not default off")
		}
		s, e := startImportService(context.Background(), nil, false)
		if e != nil || s != nil {
			t.Fatal("disabled service touched DB")
		}
	}
	if _, e := importEnabledFromEnv(func(string) string { return "true" }, false); e == nil {
		t.Fatal("import enabled without OIDC")
	}
	if _, e := importEnabledFromEnv(func(string) string { return "invalid" }, true); e == nil {
		t.Fatal("invalid config accepted")
	}
	if _, e := startImportService(context.Background(), nil, true); e == nil {
		t.Fatal("missing database accepted")
	}
	h, e := httpserver.HandlerWithImports(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	for _, method := range []string{"POST", "GET"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/api/admin/import-batches/90000000-0000-4000-8000-000000000004", nil))
		if w.Code != 404 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("closed route", w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/health/live", nil))
	if w.Code != 204 {
		t.Fatal("unrelated route changed")
	}
}

func TestAppendRuntimeMissingMigration(t *testing.T) {
	dsn := os.Getenv("IM_IMPORT_APPLY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated append PostgreSQL fixture required")
	}
	pool, e := pgxpool.New(context.Background(), dsn)
	if e != nil {
		t.Fatal("fixture unavailable")
	}
	defer pool.Close()
	if _, e = startImportService(context.Background(), pool, true); e == nil {
		t.Fatal("missing migration/profile accepted")
	}
}
