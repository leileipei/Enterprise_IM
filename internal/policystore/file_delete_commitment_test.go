package policystore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/filecleanup"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func deletionCommitFixture(t *testing.T) (*pgx.Conn, policystore.Service, files.Metadata, filecleanup.Ticket) {
	t.Helper()
	c, s, m := deleteCandidateFixture(t, "ready")
	ticket := claimDelete(t, s)
	if e := s.RecordFileDeleteInventory(context.Background(), ticket, filecleanup.Inventory{Versions: []filecleanup.Version{{VersionID: m.ObjectVersionID}}, Exhausted: true}); e != nil {
		t.Fatal(e)
	}
	return c, s, m, ticket
}
func commitDelete(t *testing.T, s policystore.Service, ticket filecleanup.Ticket, version string) filecleanup.Commitment {
	t.Helper()
	c, e := s.CommitFileDeleteVersion(context.Background(), ticket, version)
	if e != nil || filecleanup.ValidateCommitment(c) != nil {
		t.Fatal(c, e)
	}
	return c
}
func TestFileDeleteCommitmentHoldFirst(t *testing.T) {
	c, s, m, ticket := deletionCommitFixture(t)
	if e := rawSchemaHold(c, m.ConversationID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.CommitFileDeleteVersion(context.Background(), ticket, m.ObjectVersionID); !errors.Is(e, filecleanup.ErrBlocked) {
		t.Fatal(e)
	}
	var n int
	if e := c.QueryRow(context.Background(), "SELECT count(*) FROM file_delete_versions WHERE phase IN ('committed','uncertain')").Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}
func TestFileDeleteCommitmentCommitFirst(t *testing.T) {
	c, s, m, ticket := deletionCommitFixture(t)
	permit := commitDelete(t, s, ticket, m.ObjectVersionID)
	if e := rawSchemaHold(c, m.ConversationID); e == nil {
		t.Fatal("hold crossed unresolved deletion")
	}
	if e := s.SettleFileDeleteVersion(context.Background(), permit, filecleanup.AbsenceProof{VersionID: permit.VersionID, CheckedAt: downloadDeadline(t, c, 0), Absent: false}); !errors.Is(e, filecleanup.ErrIncomplete) {
		t.Fatal("presence settled old deletion", e)
	}
	if e := rawSchemaHold(c, m.ConversationID); e == nil {
		t.Fatal("presence removed barrier")
	}
	proof := filecleanup.AbsenceProof{VersionID: permit.VersionID, CheckedAt: downloadDeadline(t, c, 0), Absent: true}
	if e := s.SettleFileDeleteVersion(context.Background(), permit, proof); e != nil {
		t.Fatal(e)
	}
	if e := s.SettleFileDeleteVersion(context.Background(), permit, proof); e != nil {
		t.Fatal("settlement replay", e)
	}
	if e := rawSchemaHold(c, m.ConversationID); e != nil {
		t.Fatal("settled version retained barrier", e)
	}
	var audits int
	if e := c.QueryRow(context.Background(), "SELECT count(*) FROM file_worker_audit_events WHERE operation='delete_settle'").Scan(&audits); e != nil || audits != 1 {
		t.Fatal(audits, e)
	}
	_, exhausted, _, _ := deleteJobFacts(t, c, ticket)
	if exhausted {
		t.Fatal("last absence skipped fresh inventory")
	}
}
func TestFileDeleteCommitmentPausedRecovery(t *testing.T) {
	c, s, m, ticket := deletionCommitFixture(t)
	original := commitDelete(t, s, ticket, m.ObjectVersionID)
	cleanupPolicy(t, c, 365, false)
	run(t, c, "UPDATE file_delete_jobs SET lease_expires_at=created_at+interval '1 microsecond',updated_at=clock_timestamp() WHERE id=$1", ticket.JobID)
	if e := rawSchemaHold(c, m.ConversationID); e == nil {
		t.Fatal("expiry or pause removed barrier")
	}
	replacementOwner := freshFile().ID
	recovered, found, e := s.ClaimFileDeleteRecovery(context.Background(), replacementOwner)
	if e != nil || !found || recovered.CommitmentID != original.CommitmentID || recovered.VersionID != original.VersionID || recovered.Ticket.OwnerID != replacementOwner || recovered.Ticket.LeaseToken == original.Ticket.LeaseToken {
		t.Fatal(recovered, found, e)
	}
	if e := s.SettleFileDeleteVersion(context.Background(), original, filecleanup.AbsenceProof{VersionID: original.VersionID, CheckedAt: downloadDeadline(t, c, 0), Absent: true}); !errors.Is(e, filecleanup.ErrLeaseLost) {
		t.Fatal("late old owner settled", e)
	}
	if e := rawSchemaHold(c, m.ConversationID); e == nil {
		t.Fatal("takeover removed barrier")
	}
	if e := s.SettleFileDeleteVersion(context.Background(), recovered, filecleanup.AbsenceProof{VersionID: recovered.VersionID, CheckedAt: downloadDeadline(t, c, 0), Absent: true}); e != nil {
		t.Fatal(e)
	}
	if e := rawSchemaHold(c, m.ConversationID); e != nil {
		t.Fatal(e)
	}
}
func TestFileDeleteCommitmentOneVersionPerConversation(t *testing.T) {
	c, s, m, ticket := deletionCommitFixture(t)
	m2 := storedFile(t, c, "ready")
	second := claimDelete(t, s)
	if second.FileID != m2.ID {
		t.Fatal("second candidate", second)
	}
	if e := s.RecordFileDeleteInventory(context.Background(), second, filecleanup.Inventory{Versions: []filecleanup.Version{{VersionID: m2.ObjectVersionID}}, Exhausted: true}); e != nil {
		t.Fatal(e)
	}
	_ = commitDelete(t, s, ticket, m.ObjectVersionID)
	if _, e := s.CommitFileDeleteVersion(context.Background(), second, m2.ObjectVersionID); !errors.Is(e, filecleanup.ErrBlocked) {
		t.Fatal("second unresolved version", e)
	}
}
func TestFileDeleteHoldDirectSQL(t *testing.T) {
	c, s, m, ticket := deletionCommitFixture(t)
	permit := commitDelete(t, s, ticket, m.ObjectVersionID)
	if e := rawSchemaHold(c, m.ConversationID); e == nil {
		t.Fatal("direct SQL bypassed barrier")
	}
	reject(t, c, "UPDATE file_delete_versions SET phase='inventoried',commitment_id=NULL,committed_at=NULL WHERE commitment_id=$1", permit.CommitmentID)
	reject(t, c, "DELETE FROM file_delete_versions WHERE commitment_id=$1", permit.CommitmentID)
}
func TestFileDeleteLateLeaseBarrier(t *testing.T) {
	c, s, m, ticket := deletionCommitFixture(t)
	original := commitDelete(t, s, ticket, m.ObjectVersionID)
	for round := 0; round < 3; round++ {
		run(t, c, "UPDATE file_delete_jobs SET lease_expires_at=created_at+interval '1 microsecond',next_retry_at=NULL,updated_at=clock_timestamp() WHERE id=$1", ticket.JobID)
		recovered, ok, e := s.ClaimFileDeleteRecovery(context.Background(), freshFile().ID)
		if e != nil || !ok || recovered.CommitmentID != original.CommitmentID || recovered.VersionID != original.VersionID {
			t.Fatal(round, recovered, ok, e)
		}
		if e = rawSchemaHold(c, m.ConversationID); e == nil {
			t.Fatal("recovery cleared late-delete barrier", round)
		}
		if e = s.SettleFileDeleteVersion(context.Background(), recovered, filecleanup.AbsenceProof{VersionID: recovered.VersionID, CheckedAt: downloadDeadline(t, c, 0), Absent: false}); !errors.Is(e, filecleanup.ErrIncomplete) {
			t.Fatal(e)
		}
	}
}
func TestFileDeleteCommitmentAuditAtomic(t *testing.T) {
	c, s, m, ticket := deletionCommitFixture(t)
	run(t, c, `CREATE FUNCTION reject_delete_commit_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.operation IN ('delete_commit','delete_settle') THEN RAISE EXCEPTION 'fixture audit failure';END IF;RETURN NEW;END $$;CREATE TRIGGER reject_delete_commit BEFORE INSERT ON file_worker_audit_events FOR EACH ROW EXECUTE FUNCTION reject_delete_commit_audit()`)
	if _, e := s.CommitFileDeleteVersion(context.Background(), ticket, m.ObjectVersionID); !errors.Is(e, policystore.ErrAuditUnavailable) {
		t.Fatal(e)
	}
	var phase string
	if e := c.QueryRow(context.Background(), "SELECT phase FROM file_delete_versions WHERE job_id=$1", ticket.JobID).Scan(&phase); e != nil || phase != "inventoried" {
		t.Fatal(phase, e)
	}
	run(t, c, "DROP TRIGGER reject_delete_commit ON file_worker_audit_events")
	permit := commitDelete(t, s, ticket, m.ObjectVersionID)
	run(t, c, "CREATE TRIGGER reject_delete_commit BEFORE INSERT ON file_worker_audit_events FOR EACH ROW EXECUTE FUNCTION reject_delete_commit_audit()")
	if e := s.SettleFileDeleteVersion(context.Background(), permit, filecleanup.AbsenceProof{VersionID: permit.VersionID, CheckedAt: downloadDeadline(t, c, 0), Absent: true}); !errors.Is(e, policystore.ErrAuditUnavailable) {
		t.Fatal(e)
	}
	if e := rawSchemaHold(c, m.ConversationID); e == nil {
		t.Fatal("failed audit removed barrier")
	}
}
func TestFileDeleteCommitmentFreshRetention(t *testing.T) {
	c := fileDownloadDB(t)
	m, _ := downloadFixture(t, c)
	s := policystore.Service{DB: c}
	cleanupPolicy(t, c, 1, true)
	ticket := claimDelete(t, s)
	if e := s.RecordFileDeleteInventory(context.Background(), ticket, filecleanup.Inventory{Versions: []filecleanup.Version{{VersionID: m.ObjectVersionID}}, Exhausted: true}); e != nil {
		t.Fatal(e)
	}
	cleanupPolicy(t, c, 365, true)
	if _, found, e := s.ClaimFileDelete(context.Background(), uploadOwner); e != nil || found {
		t.Fatal("extended retention still queued", found, e)
	}
	// A previously prepared ticket must also fail its fresh policy/due check.
	if _, e := s.CommitFileDeleteVersion(context.Background(), ticket, m.ObjectVersionID); !errors.Is(e, filecleanup.ErrBlocked) {
		t.Fatal("current extended retention ignored", e)
	}
}

func TestFileDeleteCommitmentBackoffCap(t *testing.T) {
	c, s, m, ticket := deletionCommitFixture(t)
	_ = commitDelete(t, s, ticket, m.ObjectVersionID)
	run(t, c, `UPDATE file_delete_jobs SET attempts=9223372036854775807,lease_expires_at=created_at+interval '1 microsecond',next_retry_at=NULL,updated_at=clock_timestamp() WHERE id=$1`, ticket.JobID)
	recovered, ok, e := s.ClaimFileDeleteRecovery(context.Background(), freshFile().ID)
	if e != nil || !ok {
		t.Fatal(recovered, ok, e)
	}
	var capped bool
	if e = c.QueryRow(context.Background(), `SELECT next_retry_at BETWEEN updated_at+interval '59 seconds' AND updated_at+interval '60 seconds' FROM file_delete_jobs WHERE id=$1`, ticket.JobID).Scan(&capped); e != nil || !capped {
		t.Fatal("observation counter overflow changed backoff", capped, e)
	}
	if e = rawSchemaHold(c, m.ConversationID); e == nil {
		t.Fatal("retry cap removed barrier")
	}
}
