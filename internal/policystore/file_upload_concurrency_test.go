package policystore_test

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"testing"
	"time"
)

func TestFileUploadConcurrentOwner(t *testing.T) {
	c, s, r := uploadFixture(t)
	peer := filePeer(t, c)
	ctx := context.Background()
	ch := make(chan error, 2)
	go func() { _, e := s.AcquireFileUpload(ctx, publisher(), r.File.ID, uploadOwner); ch <- e }()
	go func() {
		_, e := (policystore.Service{DB: peer}).AcquireFileUpload(ctx, publisher(), r.File.ID, clientB)
		ch <- e
	}()
	a, b := <-ch, <-ch
	if !(a == nil && errors.Is(b, files.ErrUploadBusy) || b == nil && errors.Is(a, files.ErrUploadBusy)) {
		t.Fatal(a, b)
	}
}
func TestFileUploadDisabledAfterObjectEvidence(t *testing.T) {
	c, s, r := uploadFixture(t)
	u := rememberedUpload(t, s, r.File.ID)
	run(t, c, "UPDATE tenant_file_upload_policy SET enabled=false WHERE tenant_id=$1", tenantA)
	if _, e := s.SealFileUpload(context.Background(), publisher(), u); !errors.Is(e, files.ErrUploadDisabled) {
		t.Fatal(e)
	}
}

func TestFileUploadConfigChangedDuringWait(t *testing.T) {
	c, s, r := uploadFixture(t)
	u := rememberedUpload(t, s, r.File.ID)
	ctx := context.Background()
	peer := filePeer(t, c)
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
	if _, e = tx.Exec(ctx, "UPDATE tenant_file_upload_policy SET enabled=false WHERE tenant_id=$1", tenantA); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-ch; !errors.Is(e, files.ErrUploadDisabled) {
		t.Fatal(e)
	}
}
func TestFileUploadLeaseExpiresDuringAudit(t *testing.T) {
	c, s, r := uploadFixture(t)
	u := rememberedUpload(t, s, r.File.ID)
	ctx := context.Background()
	run(t, c, "UPDATE file_upload_attempts SET lease_expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1", u.AttemptID)
	peer := filePeer(t, c)
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "LOCK TABLE audit_events IN ACCESS EXCLUSIVE MODE"); e != nil {
		t.Fatal(e)
	}
	ch := make(chan error, 1)
	go func() { _, e := (policystore.Service{DB: peer}).SealFileUpload(ctx, publisher(), u); ch <- e }()
	waitFileLock(t, c, peer, "RowExclusiveLock", func() { tx.Rollback(ctx) })
	time.Sleep(2300 * time.Millisecond)
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-ch; !errors.Is(e, files.ErrLeaseLost) {
		t.Fatal(e)
	}
	var n int
	if e = c.QueryRow(ctx, "SELECT count(*) FROM file_lifecycle_events WHERE file_id=$1", r.File.ID).Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
}
func TestFileUploadRecoveryRetriesAndMismatch(t *testing.T) {
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
	for i := 1; i <= 3; i++ {
		job, found, e := s.ClaimFileUploadRecovery(ctx, uploadOwner)
		if e != nil || !found || job.LookupAttempt != i {
			t.Fatal(job, found, e)
		}
		bad := uploadMeasured()
		bad.SHA256 = [32]byte{}
		if e = s.CompleteFileUploadRecovery(ctx, job, files.RecoveryEvidence{Resolved: true, VersionID: "mismatch", Measurement: bad}); !errors.Is(e, files.ErrUploadConflict) {
			t.Fatal(e)
		}
		if e = s.CompleteFileUploadRecovery(ctx, job, files.RecoveryEvidence{ReasonCode: "recovery_mismatch"}); e != nil {
			t.Fatal(e)
		}
		var delay *float64
		if e = c.QueryRow(ctx, "SELECT EXTRACT(epoch FROM next_lookup_at-updated_at)::float8 FROM file_upload_attempts WHERE id=$1", u.AttemptID).Scan(&delay); e != nil {
			t.Fatal(e)
		}
		if i < 3 {
			want := float64(10)
			if i == 2 {
				want = 30
			}
			if delay == nil || *delay != want {
				t.Fatal(delay, want)
			}
			run(t, c, "UPDATE file_upload_attempts SET next_lookup_at=clock_timestamp() WHERE id=$1", u.AttemptID)
		} else if delay != nil {
			t.Fatal("unbounded retry", delay)
		}
	}
	if _, found, e := s.ClaimFileUploadRecovery(ctx, uploadOwner); e != nil || found {
		t.Fatal(found, e)
	}
	var n int
	if e = c.QueryRow(ctx, "SELECT count(*) FROM file_lifecycle_events WHERE file_id=$1", r.File.ID).Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
}
func TestFileUploadSealEventRollback(t *testing.T) {
	c, s, r := uploadFixture(t)
	u := rememberedUpload(t, s, r.File.ID)
	run(t, c, `CREATE FUNCTION refuse_seal_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.reason_code='upload_sealed' THEN RAISE EXCEPTION 'event unavailable';END IF;RETURN NEW;END $$;CREATE TRIGGER refuse_seal_event BEFORE INSERT ON file_lifecycle_events FOR EACH ROW EXECUTE FUNCTION refuse_seal_event()`)
	if _, e := s.SealFileUpload(context.Background(), publisher(), u); e == nil {
		t.Fatal("event failure sealed")
	}
	var state string
	if e := c.QueryRow(context.Background(), "SELECT state FROM file_objects WHERE id=$1", r.File.ID).Scan(&state); e != nil || state != "allocated" {
		t.Fatal(state, e)
	}
}

func TestFileUploadAllowExpiresDuringWait(t *testing.T) {
	c, s, r := uploadFixture(t)
	u := rememberedUpload(t, s, r.File.ID)
	ctx := context.Background()
	run(t, c, "UPDATE conversations SET direct_high_membership_id=$2 WHERE id=$1", directA, targetM)
	run(t, c, "INSERT INTO policy_versions(tenant_id,version,status,published_by_user_id,reason) VALUES($1,1,'draft',$2,'temporary allow')", tenantA, adminA)
	run(t, c, `INSERT INTO policy_rules(tenant_id,version,rule_id,effect,action,source_organization_id,target_organization_id,bidirectional,requested_by_user_id,approved_by_user_id,reason,effective_from,effective_to) VALUES($1,1,'allow','allow','send_message',$2,$3,true,$4,$4,'temporary','2020-01-01',clock_timestamp()+interval '2 seconds')`, tenantA, orgA, orgA2, adminA)
	run(t, c, "UPDATE policy_versions SET status='published',published_at=clock_timestamp() WHERE tenant_id=$1", tenantA)
	run(t, c, "INSERT INTO policy_current(tenant_id,current_version) VALUES($1,1)", tenantA)
	peer := filePeer(t, c)
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
	if e = <-ch; !errors.Is(e, files.ErrFileNotFound) {
		t.Fatal(e)
	}
}
func TestFileUploadGroupBlockedDuringWait(t *testing.T) {
	c, s, _ := uploadFixture(t)
	ctx := context.Background()
	g, e := s.CreateGroup(ctx, publisher(), createGroupRequest(targetM2))
	if e != nil {
		t.Fatal(e)
	}
	p := reservationParams()
	p.ConversationID = g.ID
	p.UploadRequestID = clientB
	r, e := s.ReserveFile(ctx, publisher(), p)
	if e != nil {
		t.Fatal(e)
	}
	u := rememberedUpload(t, s, r.File.ID)
	peer := filePeer(t, c)
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT id FROM conversations WHERE id=$1 FOR UPDATE", g.ID); e != nil {
		t.Fatal(e)
	}
	ch := make(chan error, 1)
	go func() { _, e := (policystore.Service{DB: peer}).SealFileUpload(ctx, publisher(), u); ch <- e }()
	waitFileLock(t, c, peer, "", func() { tx.Rollback(ctx) })
	if _, e = tx.Exec(ctx, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", g.ID); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-ch; !errors.Is(e, files.ErrFileNotFound) {
		t.Fatal(e)
	}
}
func TestFileUploadExpiryAndStateAreNotOracles(t *testing.T) {
	c, s, _ := uploadFixture(t)
	ctx := context.Background()
	expired := storedFile(t, c, "allocated")
	if _, e := s.AcquireFileUpload(ctx, publisher(), expired.ID, uploadOwner); !errors.Is(e, files.ErrUploadExpired) {
		t.Fatal(e)
	}
	if _, e := s.AcquireFileUpload(ctx, groupMemberIdentity(), expired.ID, uploadOwner); !errors.Is(e, files.ErrFileNotFound) {
		t.Fatal(e)
	}
	uploaded := storedFile(t, c, "uploaded")
	if _, e := s.AcquireFileUpload(ctx, groupMemberIdentity(), uploaded.ID, uploadOwner); !errors.Is(e, files.ErrFileNotFound) {
		t.Fatal(e)
	}
}
func TestFileUploadExpiredReceivingGetsNewToken(t *testing.T) {
	c, s, r := uploadFixture(t)
	ctx := context.Background()
	u, e := s.AcquireFileUpload(ctx, publisher(), r.File.ID, uploadOwner)
	if e != nil {
		t.Fatal(e)
	}
	run(t, c, "UPDATE file_upload_attempts SET lease_expires_at=clock_timestamp() WHERE id=$1", u.AttemptID)
	next, e := s.AcquireFileUpload(ctx, publisher(), r.File.ID, clientB)
	if e != nil || next.AttemptID == u.AttemptID || next.LeaseToken == u.LeaseToken || !next.File.UploadExpiresAt.Equal(u.File.UploadExpiresAt) {
		t.Fatal(next, e)
	}
	if _, e = s.RenewFileUpload(ctx, publisher(), u); !errors.Is(e, files.ErrLeaseLost) {
		t.Fatal(e)
	}
}
