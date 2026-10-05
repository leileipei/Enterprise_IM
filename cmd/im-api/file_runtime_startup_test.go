package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func apiRuntimePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("IM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL required for startup integration")
	}
	ctx := context.Background()
	c, e := pgx.Connect(ctx, dsn)
	if e != nil {
		t.Fatal("fixture database unavailable")
	}
	schema := fmt.Sprintf("p426_start_%d", time.Now().UnixNano())
	if _, e = c.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); c.Close(ctx) })
	c.Exec(ctx, "SET search_path TO "+schema+",public")
	paths, e := filepath.Glob("../../db/migrations/*.up.sql")
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range paths {
		raw, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = c.PgConn().Exec(ctx, string(raw)).ReadAll(); e != nil {
			t.Fatal(e)
		}
	}
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal("invalid fixture DSN")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(pool.Close)
	return pool
}
func apiRuntimeObjects(t *testing.T, blockProbe bool) (fileBusinessConfig, *[]string, *sync.Mutex) {
	t.Helper()
	t.Setenv("IM_FILE_DOWNLOAD_S3_ACCESS_KEY", "startup-reader")
	t.Setenv("IM_FILE_DOWNLOAD_S3_SECRET_KEY", "startup-secret")
	events := []string{}
	mu := &sync.Mutex{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		events = append(events, r.URL.RawQuery)
		mu.Unlock()
		switch {
		case r.URL.Query().Has("versioning"):
			io.WriteString(w, `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`)
		case r.URL.Query().Has("acl"):
			io.WriteString(w, `<AccessControlPolicy><AccessControlList/></AccessControlPolicy>`)
		case r.URL.Query().Has("policy"):
			w.WriteHeader(404)
			io.WriteString(w, `<Error><Code>NoSuchBucketPolicy</Code></Error>`)
		default:
			if blockProbe {
				<-r.Context().Done()
				return
			}
			w.Header().Set("X-Amz-Version-Id", "p426-probe")
			io.WriteString(w, "enterprise-im-file-read-probe-v1\n")
		}
	}))
	t.Cleanup(srv.Close)
	return fileBusinessConfig{Enabled: true, Objects: objectstore.Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "private-files", CredentialSource: "download_environment", PathStyle: true}, SpoolDir: filepath.Join(t.TempDir(), "download"), OwnerID: "10000000-0000-4000-8000-000000000001", ProbeVersionID: "p426-probe"}, &events, mu
}
func TestFileBusinessStartupOrderFull(t *testing.T) {
	pool := apiRuntimePool(t)
	business, events, mu := apiRuntimeObjects(t, false)
	rt, e := startFileRuntime(context.Background(), pool, env(nil), false, objectstore.Config{}, "", business)
	if e != nil {
		t.Fatal("complete startup failed", e)
	}
	if e = rt.CheckHealth(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = rt.Close(); e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	got := append([]string(nil), (*events)...)
	mu.Unlock()
	if len(got) < 4 || got[0] != "versioning=" || got[1] != "acl=" || got[2] != "policy=" || !strings.Contains(got[3], "versionId=p426-probe") {
		t.Fatal("storage startup order differs", got)
	}
	// Upload failure occurs after download claimed its spool. Startup must release
	// that lock before returning, making a new same-owner service possible.
	_, e = startFileRuntime(context.Background(), pool, env(nil), true, objectstore.Config{}, filepath.Join(t.TempDir(), "upload"), business)
	if e == nil {
		t.Fatal("invalid upload storage accepted")
	}
	resumed, e := filedownload.NewService(policystore.Service{DB: pool}, rt.reader, business.SpoolDir, business.OwnerID)
	if e != nil {
		t.Fatal("startup failure retained spool lock", e)
	}
	resumed.Close()
}
func TestFileBusinessStartupBudgetNetwork(t *testing.T) {
	pool := apiRuntimePool(t)
	business, _, _ := apiRuntimeObjects(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	rt, e := startFileRuntime(ctx, pool, env(nil), false, objectstore.Config{}, "", business)
	if e == nil || rt != nil || time.Since(started) > 500*time.Millisecond {
		t.Fatal("startup renewed exhausted budget")
	}
	if _, e = os.Stat(business.SpoolDir); !os.IsNotExist(e) {
		t.Fatal("spool claimed before probe success")
	}
}
func TestFileBusinessStartupOrderSchemaBeforeStorage(t *testing.T) {
	pool := apiRuntimePool(t)
	business, events, mu := apiRuntimeObjects(t, false)
	if _, e := pool.Exec(context.Background(), "ALTER TABLE file_download_sessions DROP COLUMN expected_bytes CASCADE"); e != nil {
		t.Fatal(e)
	}
	rt, e := startFileRuntime(context.Background(), pool, env(nil), false, objectstore.Config{}, "", business)
	if e == nil || rt != nil {
		t.Fatal("weakened schema accepted")
	}
	mu.Lock()
	n := len(*events)
	mu.Unlock()
	if n != 0 {
		t.Fatal("storage accessed before schema validation")
	}
	if _, e = os.Stat(business.SpoolDir); !os.IsNotExist(e) {
		t.Fatal("spool claimed for invalid schema")
	}
}
