package policystore_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"testing"
	"time"
)

func scanFixture(t *testing.T) (*pgx.Conn, policystore.Service, files.Metadata) {
	t.Helper()
	c, s, r := uploadFixture(t)
	u := rememberedUpload(t, s, r.File.ID)
	m, e := s.SealFileUpload(context.Background(), publisher(), u)
	if e != nil {
		t.Fatal(e)
	}
	return c, s, m
}
func cleanScan(t files.ScanTicket) files.ScanDecision {
	return files.ScanDecision{State: files.StateReady, Engine: "test-engine", DefinitionVersion: "test-definitions", ReasonCode: "scan_clean", SHA256: uploadMeasured().SHA256}
}
func scanState(t *testing.T, c *pgx.Conn, id string, want files.State, version int64) {
	t.Helper()
	var state files.State
	var v int64
	if e := c.QueryRow(context.Background(), "SELECT state,state_version FROM file_objects WHERE id=$1", id).Scan(&state, &v); e != nil || state != want || v != version {
		t.Fatal(state, v, e)
	}
}
func TestFileScanClaimAtomic(t *testing.T) {
	c, s, m := scanFixture(t)
	ctx := context.Background()
	job, ok, e := s.ClaimFileScan(ctx, uploadOwner)
	if e != nil || !ok || job.Attempt != 1 || job.ClaimVersion != m.StateVersion+1 || job.JobID == "" {
		t.Fatal(job, ok, e)
	}
	scanState(t, c, m.ID, files.StateScanning, 2)
	if _, ok, e = s.ClaimFileScan(ctx, clientB); e != nil || ok {
		t.Fatal("double claim", ok, e)
	}
	if e = s.CompleteFileScan(ctx, job, cleanScan(job)); e != nil {
		t.Fatal(e)
	}
	scanState(t, c, m.ID, files.StateReady, 3)
	var machine, persons, events int
	var engine string
	var hash []byte
	e = c.QueryRow(ctx, `SELECT (SELECT count(*) FROM file_worker_audit_events WHERE file_id=$1),(SELECT count(*) FROM audit_events WHERE resource_id=$1 AND action LIKE 'file_scan%'),(SELECT count(*) FROM file_lifecycle_events WHERE file_id=$1 AND actor_kind='worker'),scan_engine,scan_sha256 FROM file_objects WHERE id=$1`, m.ID).Scan(&machine, &persons, &events, &engine, &hash)
	if e != nil || machine != 2 || persons != 0 || events != 2 || engine != "test-engine" || len(hash) != 32 {
		t.Fatal(machine, persons, events, engine, e)
	}
	if e = s.CompleteFileScan(ctx, job, cleanScan(job)); !errors.Is(e, files.ErrLeaseLost) {
		t.Fatal(e)
	}
	if _, e = s.RenewFileScan(ctx, job); !errors.Is(e, files.ErrLeaseLost) {
		t.Fatal(e)
	}
}
func TestFileScanSHAAndFailedFields(t *testing.T) {
	c, s, m := scanFixture(t)
	ctx := context.Background()
	job, ok, e := s.ClaimFileScan(ctx, uploadOwner)
	if e != nil || !ok {
		t.Fatal(e)
	}
	d := cleanScan(job)
	d.SHA256 = [32]byte{}
	if e = s.CompleteFileScan(ctx, job, d); e == nil {
		t.Fatal("forged hash accepted")
	}
	scanState(t, c, m.ID, files.StateScanning, 2)
	d = files.ScanDecision{State: files.StateScanFailed, ReasonCode: "scanner_limits", Engine: "false-clean"}
	if e = s.CompleteFileScan(ctx, job, d); e == nil {
		t.Fatal("failed result has complete fields")
	}
	d.Engine = ""
	if e = s.CompleteFileScan(ctx, job, d); e != nil {
		t.Fatal(e)
	}
	scanState(t, c, m.ID, files.StateScanFailed, 3)
	var engine, def *string
	var hash []byte
	var delay float64
	if e = c.QueryRow(ctx, `SELECT f.scan_engine,f.scan_definition_version,f.scan_sha256,extract(epoch from j.next_retry_at-j.updated_at)::float8 FROM file_objects f JOIN file_scan_jobs j ON j.id=f.scan_job_id WHERE f.id=$1`, m.ID).Scan(&engine, &def, &hash, &delay); e != nil || engine != nil || def != nil || hash != nil || delay != 10 {
		t.Fatal(engine, def, hash, delay, e)
	}
}
func TestFileScanAuditRollback(t *testing.T) {
	for _, table := range []string{"file_worker_audit_events", "file_lifecycle_events"} {
		t.Run(table, func(t *testing.T) {
			c, s, m := scanFixture(t)
			ctx := context.Background()
			run(t, c, "CREATE FUNCTION refuse_scan_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'unavailable';END $$;CREATE TRIGGER refuse_scan_write BEFORE INSERT ON "+table+" FOR EACH ROW EXECUTE FUNCTION refuse_scan_write()")
			if _, ok, e := s.ClaimFileScan(ctx, uploadOwner); e == nil || ok {
				t.Fatal("claim without evidence")
			}
			scanState(t, c, m.ID, files.StateUploaded, 1)
			var n int
			c.QueryRow(ctx, "SELECT count(*) FROM file_scan_jobs WHERE file_id=$1", m.ID).Scan(&n)
			if n != 0 {
				t.Fatal(n)
			}
			run(t, c, "DROP TRIGGER refuse_scan_write ON "+table)
			job, ok, e := s.ClaimFileScan(ctx, uploadOwner)
			if e != nil || !ok {
				t.Fatal(e)
			}
			run(t, c, "CREATE TRIGGER refuse_scan_write BEFORE INSERT ON "+table+" FOR EACH ROW EXECUTE FUNCTION refuse_scan_write()")
			if e = s.CompleteFileScan(ctx, job, cleanScan(job)); e == nil {
				t.Fatal("completion without evidence")
			}
			scanState(t, c, m.ID, files.StateScanning, 2)
		})
	}
}
func TestFileScanRetrySchedule(t *testing.T) {
	c, s, m := scanFixture(t)
	ctx := context.Background()
	var previous files.ScanTicket
	for attempt := 1; attempt <= 3; attempt++ {
		deadline := time.Now().Add(35 * time.Second)
		var job files.ScanTicket
		var ok bool
		var e error
		for {
			job, ok, e = s.ClaimFileScan(ctx, uploadOwner)
			if e != nil {
				t.Fatal(e)
			}
			if ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("due retry missing")
			}
			time.Sleep(100 * time.Millisecond)
		}
		if job.Attempt != attempt || job.JobID == previous.JobID || job.LeaseToken == previous.LeaseToken {
			t.Fatal(job)
		}
		if previous.JobID != "" {
			if e = s.CompleteFileScan(ctx, previous, cleanScan(previous)); !errors.Is(e, files.ErrLeaseLost) {
				t.Fatal("late result", e)
			}
		}
		if e = s.CompleteFileScan(ctx, job, files.ScanDecision{State: files.StateScanFailed, ReasonCode: "scanner_unavailable"}); e != nil {
			t.Fatal(e)
		}
		var delay *float64
		c.QueryRow(ctx, "SELECT extract(epoch from next_retry_at-updated_at)::float8 FROM file_scan_jobs WHERE id=$1", job.JobID).Scan(&delay)
		if attempt == 1 && (delay == nil || *delay != 10) || attempt == 2 && (delay == nil || *delay != 30) || attempt == 3 && delay != nil {
			t.Fatal(attempt, delay)
		}
		previous = job
	}
	if _, ok, e := s.ClaimFileScan(ctx, uploadOwner); e != nil || ok {
		t.Fatal("fourth attempt", ok, e)
	}
	scanState(t, c, m.ID, files.StateScanFailed, 7)
}

func TestFileScanSizeWithdrawn(t *testing.T) {
	c, s, _ := uploadFixture(t)
	ctx := context.Background()
	p := reservationParams()
	p.UploadRequestID = clientB
	p.DeclaredSizeBytes = 2
	r, e := s.ReserveFile(ctx, publisher(), p)
	if e != nil {
		t.Fatal(e)
	}
	u, e := s.AcquireFileUpload(ctx, publisher(), r.File.ID, uploadOwner)
	if e != nil {
		t.Fatal(e)
	}
	measure := uploadMeasured()
	measure.SizeBytes = 2
	measure.SHA256 = sha256.Sum256([]byte("xx"))
	u, e = s.ReceiveFileUpload(ctx, publisher(), u, measure)
	if e != nil {
		t.Fatal(e)
	}
	u, e = s.BeginFileObjectWrite(ctx, publisher(), u)
	if e != nil {
		t.Fatal(e)
	}
	u, e = s.RememberFileObject(ctx, publisher(), u, "size-version")
	if e != nil {
		t.Fatal(e)
	}
	m, e := s.SealFileUpload(ctx, publisher(), u)
	if e != nil {
		t.Fatal(e)
	}
	job, ok, e := s.ClaimFileScan(ctx, uploadOwner)
	if e != nil || !ok {
		t.Fatal(e)
	}
	run(t, c, "UPDATE tenant_file_upload_policy SET max_size_bytes=1 WHERE tenant_id=$1", tenantA)
	decision := cleanScan(job)
	decision.SHA256 = measure.SHA256
	if e = s.CompleteFileScan(ctx, job, decision); e != nil {
		t.Fatal(e)
	}
	scanState(t, c, m.ID, files.StateRejected, 3)
	var engine, reason string
	c.QueryRow(ctx, "SELECT f.scan_engine,j.reason_code FROM file_objects f JOIN file_scan_jobs j ON j.id=f.scan_job_id WHERE f.id=$1", m.ID).Scan(&engine, &reason)
	if engine != "policy/tenant-file-upload" || reason != "scan_rejected" {
		t.Fatal(engine, reason)
	}
}
func TestFileScanExpiredAuditRollback(t *testing.T) {
	c, s, m := scanFixture(t)
	ctx := context.Background()
	job, ok, e := s.ClaimFileScan(ctx, uploadOwner)
	if e != nil || !ok {
		t.Fatal(e)
	}
	run(t, c, "UPDATE file_scan_jobs SET lease_expires_at=clock_timestamp()+interval '100 milliseconds' WHERE id=$1", job.JobID)
	time.Sleep(200 * time.Millisecond)
	run(t, c, "CREATE FUNCTION refuse_expired_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.operation='scan_expired' THEN RAISE EXCEPTION 'down';END IF;RETURN NEW;END $$;CREATE TRIGGER refuse_expired_audit BEFORE INSERT ON file_worker_audit_events FOR EACH ROW EXECUTE FUNCTION refuse_expired_audit()")
	n, e := s.RecoverExpiredFileScans(ctx, clientB, 100)
	if e == nil || n != 0 {
		t.Fatal(n, e)
	}
	scanState(t, c, m.ID, files.StateScanning, 2)
	run(t, c, "DROP TRIGGER refuse_expired_audit ON file_worker_audit_events")
	n, e = s.RecoverExpiredFileScans(ctx, clientB, 100)
	if e != nil || n != 1 {
		t.Fatal(n, e)
	}
}
