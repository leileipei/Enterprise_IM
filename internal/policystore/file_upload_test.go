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

const uploadOwner = "00000000-0000-4000-8000-000000009001"

func uploadFixture(t *testing.T) (*pgx.Conn, policystore.Service, files.Reservation) {
	t.Helper()
	c := db(t)
	seedDirectConversation(t, c)
	runtimePolicyEdit(t, c)
	s := policystore.Service{DB: c}
	r, e := s.ReserveFile(context.Background(), publisher(), reservationParams())
	if e != nil {
		t.Fatal(e)
	}
	return c, s, r
}
func uploadMeasured() files.Measurement {
	return files.Measurement{SizeBytes: 1, SHA256: sha256.Sum256([]byte("x")), DetectedMediaType: "text/plain"}
}
func receivedUpload(t *testing.T, s policystore.Service, id string) files.UploadTicket {
	t.Helper()
	ctx := context.Background()
	u, e := s.AcquireFileUpload(ctx, publisher(), id, uploadOwner)
	if e != nil {
		t.Fatal(e)
	}
	u, e = s.ReceiveFileUpload(ctx, publisher(), u, uploadMeasured())
	if e != nil {
		t.Fatal(e)
	}
	return u
}
func rememberedUpload(t *testing.T, s policystore.Service, id string) files.UploadTicket {
	t.Helper()
	ctx := context.Background()
	u := receivedUpload(t, s, id)
	u, e := s.BeginFileObjectWrite(ctx, publisher(), u)
	if e != nil {
		t.Fatal(e)
	}
	u, e = s.RememberFileObject(ctx, publisher(), u, "fixed-version")
	if e != nil {
		t.Fatal(e)
	}
	return u
}
func TestFileUploadLease(t *testing.T) {
	c, s, r := uploadFixture(t)
	ctx := context.Background()
	u := receivedUpload(t, s, r.File.ID)
	old := u
	u, e := s.RenewFileUpload(ctx, publisher(), u)
	if e != nil || u.LeaseExpiresAt.After(r.File.UploadExpiresAt) || u.Phase != files.UploadReceived {
		t.Fatal(u, e)
	}
	var state string
	var v int64
	if e = c.QueryRow(ctx, "SELECT state,state_version FROM file_objects WHERE id=$1", r.File.ID).Scan(&state, &v); e != nil || state != "allocated" || v != 0 {
		t.Fatal(state, v, e)
	}
	old.LeaseToken = clientB
	if _, e = s.RenewFileUpload(ctx, publisher(), old); !errors.Is(e, files.ErrLeaseLost) {
		t.Fatal(e)
	}
	if e = s.FailFileUpload(ctx, publisher(), u, "receive_failed"); e != nil {
		t.Fatal(e)
	}
	next, e := s.AcquireFileUpload(ctx, publisher(), r.File.ID, uploadOwner)
	if e != nil || next.AttemptID != u.AttemptID || next.LeaseToken == u.LeaseToken || next.Phase != files.UploadReceived {
		t.Fatal(next, e)
	}
	bad := uploadMeasured()
	bad.SHA256 = [32]byte{}
	if _, e = s.ReceiveFileUpload(ctx, publisher(), next, bad); !errors.Is(e, files.ErrUploadConflict) {
		t.Fatal(e)
	}
	if _, e = s.ReceiveFileUpload(ctx, publisher(), next, uploadMeasured()); e != nil {
		t.Fatal(e)
	}
}
func TestFileUploadSealAtomic(t *testing.T) {
	c, s, r := uploadFixture(t)
	ctx := context.Background()
	u := rememberedUpload(t, s, r.File.ID)
	fake := u
	fake.Measurement = &files.Measurement{}
	fake.ObjectVersionID = "forged"
	m, e := s.SealFileUpload(ctx, publisher(), fake)
	if e != nil || m.State != files.StateUploaded || m.ObjectVersionID != "fixed-version" || files.ValidateMetadata(m) != nil {
		t.Fatal(m, e)
	}
	var events, audits int
	if e = c.QueryRow(ctx, "SELECT (SELECT count(*) FROM file_lifecycle_events WHERE file_id=$1),(SELECT count(*) FROM audit_events WHERE action='file_upload_seal' AND resource_id=$1)", m.ID).Scan(&events, &audits); e != nil || events != 2 || audits != 1 {
		t.Fatal(events, audits, e)
	}
	if _, e = s.AcquireFileUpload(ctx, publisher(), m.ID, uploadOwner); !errors.Is(e, files.ErrAlreadyUploaded) {
		t.Fatal(e)
	}
}
func TestFileUploadSealAuditRollback(t *testing.T) {
	c, s, r := uploadFixture(t)
	u := rememberedUpload(t, s, r.File.ID)
	run(t, c, `CREATE FUNCTION refuse_seal_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='file_upload_seal' THEN RAISE EXCEPTION 'failed audit'; END IF;RETURN NEW;END $$;CREATE TRIGGER refuse_seal_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION refuse_seal_audit()`)
	if _, e := s.SealFileUpload(context.Background(), publisher(), u); e == nil {
		t.Fatal("seal with failed audit")
	}
	var state, phase string
	var n int
	if e := c.QueryRow(context.Background(), "SELECT f.state,a.phase,(SELECT count(*) FROM file_lifecycle_events WHERE file_id=f.id) FROM file_objects f JOIN file_upload_attempts a ON a.file_id=f.id WHERE f.id=$1", r.File.ID).Scan(&state, &phase, &n); e != nil || state != "allocated" || phase != "recovered" || n != 1 {
		t.Fatal(state, phase, n, e)
	}
}
func TestFileUploadRecoveryEvidence(t *testing.T) {
	c, s, r := uploadFixture(t)
	ctx := context.Background()
	u := receivedUpload(t, s, r.File.ID)
	u, e := s.BeginFileObjectWrite(ctx, publisher(), u)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.FailFileUpload(ctx, publisher(), u, "object_write_uncertain"); e != nil {
		t.Fatal(e)
	}
	job, found, e := s.ClaimFileUploadRecovery(ctx, uploadOwner)
	if e != nil || !found || job.JobID == "" {
		t.Fatal(job, found, e)
	}
	if e = s.CompleteFileUploadRecovery(ctx, job, files.RecoveryEvidence{Resolved: true, VersionID: "recovered-version", Measurement: uploadMeasured(), ReasonCode: "recovery_resolved"}); e != nil {
		t.Fatal(e)
	}
	var state string
	var v, ev int
	if e = c.QueryRow(ctx, "SELECT state,state_version,(SELECT count(*) FROM file_lifecycle_events WHERE file_id=$1) FROM file_objects WHERE id=$1", r.File.ID).Scan(&state, &v, &ev); e != nil || state != "allocated" || v != 0 || ev != 1 {
		t.Fatal(state, v, ev, e)
	}
	if e = s.CompleteFileUploadRecovery(ctx, job, files.RecoveryEvidence{Resolved: true, VersionID: "evil", Measurement: uploadMeasured()}); !errors.Is(e, files.ErrLeaseLost) {
		t.Fatal("old recovery job", e)
	}
	again, e := s.AcquireFileUpload(ctx, publisher(), r.File.ID, uploadOwner)
	if e != nil || again.Phase != files.UploadRecovered || again.ObjectVersionID != "recovered-version" {
		t.Fatal(again, e)
	}
	again, e = s.ReceiveFileUpload(ctx, publisher(), again, uploadMeasured())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.SealFileUpload(ctx, publisher(), again); e != nil {
		t.Fatal(e)
	}
}
func TestFileUploadRevokedIdentityCanOnlyFail(t *testing.T) {
	c, s, r := uploadFixture(t)
	u := rememberedUpload(t, s, r.File.ID)
	run(t, c, "UPDATE user_organizations SET effective_to=clock_timestamp() WHERE id=$1", adminM)
	if _, e := s.SealFileUpload(context.Background(), publisher(), u); !errors.Is(e, files.ErrInvalidIdentity) {
		t.Fatal(e)
	}
	if e := s.FailFileUpload(context.Background(), publisher(), u, "audit_unavailable"); e != nil {
		t.Fatal(e)
	}
}
func TestFileUploadWaitAuthorization(t *testing.T) {
	c, s, r := uploadFixture(t)
	u := rememberedUpload(t, s, r.File.ID)
	ctx := context.Background()
	peer := filePeer(t, c)
	run(t, c, "UPDATE user_organizations SET effective_to=clock_timestamp()+interval '2 seconds' WHERE id=$1", adminM)
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT tenant_id FROM tenant_file_upload_policy WHERE tenant_id=$1 FOR UPDATE", tenantA); e != nil {
		t.Fatal(e)
	}
	ch := make(chan error, 1)
	go func() { _, e := (policystore.Service{DB: peer}).SealFileUpload(ctx, publisher(), u); ch <- e }()
	waitFileLock(t, c, peer, "", func() { tx.Rollback(ctx) })
	time.Sleep(2300 * time.Millisecond)
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-ch; !errors.Is(e, files.ErrInvalidIdentity) {
		t.Fatal(e)
	}
}
