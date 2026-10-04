package policystore_test

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/filetransfer"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

type countingFileObjects struct {
	objectstore.Store
	puts         atomic.Int32
	loseResponse bool
}

func (s *countingFileObjects) PutVersion(ctx context.Context, l objectstore.Location, a string, m files.Measurement, r io.ReadSeeker) (objectstore.VersionRef, error) {
	s.puts.Add(1)
	ref, e := s.Store.PutVersion(ctx, l, a, m, r)
	if e == nil && s.loseResponse {
		return objectstore.VersionRef{}, files.ErrDependencyUnavailable
	}
	return ref, e
}
func realTransferObjects(t *testing.T) *countingFileObjects {
	t.Helper()
	if os.Getenv("IM_TEST_S3_ENDPOINT") == "" {
		t.Skip("requires dedicated S3; run-s3 requires environment")
	}
	s, e := objectstore.NewS3(objectstore.Config{Endpoint: os.Getenv("IM_TEST_S3_ENDPOINT"), Region: "us-east-1", Bucket: os.Getenv("IM_TEST_S3_BUCKET"), CredentialSource: "environment", PathStyle: true})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ValidateCapabilities(context.Background()); e != nil {
		t.Fatal(e)
	}
	return &countingFileObjects{Store: s}
}
func TestFileTransferRealUploadRecovery(t *testing.T) {
	c, repo, r := uploadFixture(t)
	objects := realTransferObjects(t)
	objects.loseResponse = true
	svc, e := filetransfer.NewService(repo, objects, t.TempDir()+"/private", uploadOwner)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	if _, e = svc.Upload(ctx, publisher(), r.File.ID, strings.NewReader("x")); !errors.Is(e, files.ErrDependencyUnavailable) {
		t.Fatal(e)
	}
	var state string
	if e = c.QueryRow(ctx, "SELECT state FROM file_objects WHERE id=$1", r.File.ID).Scan(&state); e != nil || state != "allocated" {
		t.Fatal(state, e)
	}
	if found, e := svc.RecoverOnce(ctx); e != nil || !found {
		t.Fatal(found, e)
	}
	objects.loseResponse = false
	m, e := svc.Upload(ctx, publisher(), r.File.ID, strings.NewReader("x"))
	if e != nil || m.State != files.StateUploaded || objects.puts.Load() != 1 {
		t.Fatal(m.State, e, objects.puts.Load())
	}
	var user, worker int
	if e = c.QueryRow(ctx, "SELECT count(*) FILTER(WHERE actor_kind='user'),count(*) FILTER(WHERE actor_kind='worker') FROM file_lifecycle_events WHERE file_id=$1 AND to_state='uploaded'", m.ID).Scan(&user, &worker); e != nil || user != 1 || worker != 0 {
		t.Fatal(user, worker, e)
	}
}
func TestFileTransferRealSealAuditFailureNoReput(t *testing.T) {
	c, repo, r := uploadFixture(t)
	objects := realTransferObjects(t)
	svc, e := filetransfer.NewService(repo, objects, t.TempDir()+"/private", uploadOwner)
	if e != nil {
		t.Fatal(e)
	}
	run(t, c, `CREATE FUNCTION reject_transfer_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='file_upload_seal' THEN RAISE EXCEPTION 'audit failure';END IF;RETURN NEW;END $$;CREATE TRIGGER reject_transfer_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_transfer_audit()`)
	ctx := context.Background()
	if _, e = svc.Upload(ctx, publisher(), r.File.ID, strings.NewReader("x")); e == nil {
		t.Fatal("audit failed seal accepted")
	}
	var state string
	if e = c.QueryRow(ctx, "SELECT state FROM file_objects WHERE id=$1", r.File.ID).Scan(&state); e != nil || state != "allocated" {
		t.Fatal(state, e)
	}
	run(t, c, "DROP TRIGGER reject_transfer_audit ON audit_events")
	m, e := svc.Upload(ctx, publisher(), r.File.ID, strings.NewReader("x"))
	if e != nil || m.State != files.StateUploaded || objects.puts.Load() != 1 {
		t.Fatal(m.State, e, objects.puts.Load())
	}
}
func TestFileTransferRealMaximum(t *testing.T) {
	_, repo, _ := uploadFixture(t)
	objects := realTransferObjects(t)
	p := reservationParams()
	p.UploadRequestID = clientB
	p.DeclaredSizeBytes = files.MaxFileSizeBytes
	r, e := repo.ReserveFile(context.Background(), publisher(), p)
	if e != nil {
		t.Fatal(e)
	}
	svc, e := filetransfer.NewService(repo, objects, t.TempDir()+"/private", uploadOwner)
	if e != nil {
		t.Fatal(e)
	}
	m, e := svc.Upload(context.Background(), publisher(), r.File.ID, io.LimitReader(repeatByte{}, files.MaxFileSizeBytes))
	if e != nil || m.State != files.StateUploaded || m.ActualSizeBytes == nil || *m.ActualSizeBytes != files.MaxFileSizeBytes {
		t.Fatal(m.State, e)
	}
}

type repeatByte struct{}

func (repeatByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
