package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestAPIShutdownActiveDownload(t *testing.T) {
	entered := make(chan context.Context, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- r.Context(); <-r.Context().Done() }))
	defer server.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r, e := server.Client().Get(server.URL)
		if e == nil {
			r.Body.Close()
		}
	}()
	ctx := <-entered
	calls := 0
	e := shutdownAPIWithBudgets(server.Config, []func() error{func() error {
		calls++
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
			return errors.New("request remained active")
		}
	}}, 20*time.Millisecond, time.Second)
	if e == nil || calls != 1 {
		t.Fatal("grace timeout not reported or cleanup missing", e, calls)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connection retained")
	}
}
func TestAPIShutdownCleanupTimeout(t *testing.T) {
	blocker := make(chan struct{})
	finished := make(chan struct{})
	started := time.Now()
	e := shutdownAPIWithBudgets(nil, []func() error{func() error { defer close(finished); <-blocker; return nil }}, time.Second, 20*time.Millisecond)
	if e == nil || time.Since(started) > 200*time.Millisecond {
		t.Fatal("cleanup not bounded")
	}
	close(blocker)
	<-finished
	order := []int{}
	e = shutdownAPI(nil, []func() error{func() error { order = append(order, 1); return nil }, func() error { order = append(order, 2); return errors.New("private cleanup detail") }})
	if e == nil || e.Error() == "private cleanup detail" || len(order) != 2 || order[0] != 2 || order[1] != 1 {
		t.Fatal("cleanup order or fixed errors incorrect", order)
	}
	if e = shutdownAPI(nil, nil); e != nil {
		t.Fatal(e)
	}
}
func TestAPIStartupFailureClosesResources(t *testing.T) {
	pool := apiRuntimePool(t)
	business, _, _ := apiRuntimeObjects(t, false)
	settings := map[string]string{"IM_DATABASE_URL": pool.Config().ConnString(), "IM_OIDC_ENABLED": "true", "IM_OIDC_ISSUER": "https://issuer.example.test", "IM_OIDC_AUDIENCE": "api", "IM_OIDC_JWKS_URL": "https://issuer.example.test/keys", "IM_OIDC_ALLOWED_CLIENT_IDS": "web", "IM_FILE_BUSINESS_ENABLED": "true", "IM_FILE_S3_ENDPOINT": business.Objects.Endpoint, "IM_FILE_S3_REGION": business.Objects.Region, "IM_FILE_S3_BUCKET": business.Objects.Bucket, "IM_FILE_S3_PATH_STYLE": "true", "IM_FILE_DOWNLOAD_S3_ACCESS_KEY": "startup-reader", "IM_FILE_DOWNLOAD_S3_SECRET_KEY": "startup-secret", "IM_FILE_DOWNLOAD_SPOOL_DIR": business.SpoolDir, "IM_FILE_DOWNLOAD_OWNER_ID": business.OwnerID, "IM_FILE_READ_PROBE_VERSION_ID": business.ProbeVersionID, "IM_WEB_ENABLED": "true"}
	if e := runAPI(context.Background(), env(settings), slog.New(slog.NewTextHandler(io.Discard, nil))); e == nil {
		t.Fatal("invalid web assembly accepted")
	}
	reader, e := objectstore.NewS3ReadOnly(business.Objects)
	if e != nil {
		t.Fatal(e)
	}
	resumed, e := filedownload.NewService(policystore.Service{DB: pool}, reader, business.SpoolDir, business.OwnerID)
	if e != nil {
		t.Fatal("later startup failure retained download lock", e)
	}
	resumed.Close()
}
