package main

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/filescanner"
	"github.com/leileipei/Enterprise_IM/internal/filetransfer"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

type Config struct {
	Enabled                         bool
	DatabaseURL, WorkerID, SpoolDir string
	ObjectStore                     objectstore.Config
	Scanner                         filescanner.Config
	ScanConcurrency                 int
}

func ConfigFromEnv(lookup func(string) (string, bool)) (Config, error) {
	c := Config{ScanConcurrency: 1}
	get := func(k string) string { v, _ := lookup(k); return v }
	switch get("IM_FILE_WORKER_ENABLED") {
	case "", "false":
		return c, nil
	case "true":
		c.Enabled = true
	default:
		return c, errors.New("invalid file worker enable flag")
	}
	if v := get("IM_FILE_SCAN_CONCURRENCY"); v != "" && v != "1" {
		return c, errors.New("file scan concurrency must be one")
	}
	c.DatabaseURL = get("IM_DATABASE_URL")
	c.WorkerID = get("IM_FILE_WORKER_ID")
	c.SpoolDir = get("IM_FILE_SPOOL_DIR")
	c.ObjectStore = objectstore.Config{Endpoint: get("IM_FILE_S3_ENDPOINT"), Region: get("IM_FILE_S3_REGION"), Bucket: get("IM_FILE_S3_BUCKET"), CredentialSource: get("IM_FILE_S3_CREDENTIAL_SOURCE")}
	if c.ObjectStore.CredentialSource == "" {
		c.ObjectStore.CredentialSource = "environment"
	}
	switch get("IM_FILE_S3_PATH_STYLE") {
	case "", "false":
	case "true":
		c.ObjectStore.PathStyle = true
	default:
		return c, errors.New("invalid S3 path style")
	}
	c.Scanner = filescanner.Config{QPDFPath: get("IM_FILE_QPDF_PATH"), ClamdSocket: get("IM_FILE_CLAMD_SOCKET"), RuntimeManifestPath: get("IM_FILE_SCANNER_MANIFEST")}
	if c.DatabaseURL == "" || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(c.WorkerID) || !filepath.IsAbs(c.SpoolDir) || c.ObjectStore.Endpoint == "" || c.ObjectStore.Region == "" || c.ObjectStore.Bucket == "" || c.ObjectStore.CredentialSource != "environment" || get("IM_FILE_S3_ACCESS_KEY") == "" || get("IM_FILE_S3_SECRET_KEY") == "" || !filepath.IsAbs(c.Scanner.QPDFPath) || !filepath.IsAbs(c.Scanner.ClamdSocket) || !filepath.IsAbs(c.Scanner.RuntimeManifestPath) {
		return c, errors.New("incomplete file worker configuration")
	}
	return c, nil
}
func run(ctx context.Context, c Config) error {
	if !c.Enabled {
		return nil
	}
	check, stop := context.WithTimeout(ctx, 90*time.Second)
	defer stop()
	pool, e := pgxpool.New(check, c.DatabaseURL)
	if e != nil {
		return errors.New("invalid file worker database configuration")
	}
	defer pool.Close()
	if e = pool.Ping(check); e != nil {
		return errors.New("file worker database unavailable")
	}
	var installed bool
	if e = pool.QueryRow(check, `SELECT to_regclass('file_scan_jobs') IS NOT NULL AND to_regclass('file_upload_attempts') IS NOT NULL AND to_regclass('file_worker_audit_events') IS NOT NULL`).Scan(&installed); e != nil || !installed {
		return errors.New("file runtime migration unavailable")
	}
	objects, e := objectstore.NewS3(c.ObjectStore)
	if e != nil {
		return errors.New("file object configuration unavailable")
	}
	if e = objects.ValidateCapabilities(check); e != nil {
		return errors.New("private versioned object storage unavailable")
	}
	scanner, e := filescanner.New(c.Scanner)
	if e != nil {
		return errors.New("scanner configuration unavailable")
	}
	if e = scanner.ValidateRuntime(check); e != nil {
		return errors.New("scanner runtime proof unavailable")
	}
	repo := policystore.Service{DB: pool}
	transfer, e := filetransfer.NewService(repo, objects, c.SpoolDir, c.WorkerID)
	if e != nil {
		return errors.New("file worker spool unavailable")
	}
	defer transfer.Close()
	worker := filetransfer.ScanWorker{Repo: repo, Objects: objects, Scanner: scanner, SpoolDir: c.SpoolDir, OwnerID: c.WorkerID}
	slog.Info("file worker started", "scan_concurrency", 1)
	failures := 0
	for ctx.Err() == nil {
		_, e1 := repo.RecoverExpiredFileScans(ctx, c.WorkerID, 100)
		_, e2 := transfer.RecoverOnce(ctx)
		_, e3 := worker.RunOnce(ctx)
		if ctx.Err() != nil {
			break
		}
		if e1 != nil || e2 != nil || e3 != nil {
			if failures < 6 {
				failures++
			}
			slog.Warn("file worker sweep unavailable")
		} else {
			failures = 0
		}
		delay := time.Second
		if failures > 1 {
			delay = time.Second << uint(failures-1)
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	slog.Info("file worker stopped")
	return nil
}
func main() {
	c, e := ConfigFromEnv(os.LookupEnv)
	if e != nil {
		slog.Error("invalid file worker configuration")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if e = run(ctx, c); e != nil {
		slog.Error("file worker unavailable", "reason", e)
		os.Exit(1)
	}
}
