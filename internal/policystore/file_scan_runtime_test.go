package policystore_test

import (
	"context"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/filescanner"
	"github.com/leileipei/Enterprise_IM/internal/filetransfer"
	"os"
	"strings"
	"testing"
)

func TestFileScanRealUnprovenRuntime(t *testing.T) {
	if os.Getenv("IM_TEST_SCANNER_MANIFEST") == "" {
		t.Skip("controlled original scanner required")
	}
	c, repo, r := uploadFixture(t)
	objects := realTransferObjects(t)
	ctx := context.Background()
	upload, e := filetransfer.NewService(repo, objects, t.TempDir()+"/upload", uploadOwner)
	if e != nil {
		t.Fatal(e)
	}
	m, e := upload.Upload(ctx, publisher(), r.File.ID, strings.NewReader("x"))
	if e != nil {
		t.Fatal(e)
	}
	scanner, e := filescanner.New(filescanner.Config{QPDFPath: os.Getenv("IM_TEST_QPDF_PATH"), ClamdSocket: os.Getenv("IM_TEST_CLAMD_SOCKET"), RuntimeManifestPath: os.Getenv("IM_TEST_SCANNER_MANIFEST")})
	if e != nil {
		t.Fatal(e)
	}
	worker := filetransfer.ScanWorker{Repo: repo, Objects: objects, Scanner: scanner, SpoolDir: t.TempDir() + "/scan", OwnerID: uploadOwner}
	found, e := worker.RunOnce(ctx)
	if e != nil || !found {
		t.Fatal(found, e)
	}
	scanState(t, c, m.ID, files.StateScanFailed, 3)
	var engine, definition *string
	var sha []byte
	var reason string
	var personal int
	e = c.QueryRow(ctx, `SELECT f.scan_engine,f.scan_definition_version,f.scan_sha256,j.reason_code,(SELECT count(*) FROM audit_events WHERE resource_id=f.id AND action LIKE 'file_scan%') FROM file_objects f JOIN file_scan_jobs j ON j.id=f.scan_job_id WHERE f.id=$1`, m.ID).Scan(&engine, &definition, &sha, &reason, &personal)
	if e != nil || engine != nil || definition != nil || sha != nil || reason != "scanner_unavailable" || personal != 0 || objects.puts.Load() != 1 {
		t.Fatal(engine, definition, sha, reason, personal, e)
	}
}
