package policystore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/filecleanup"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestFileDeleteProofTimeUnavailable(t *testing.T) {
	if _, e := (policystore.Service{}).FileDeleteProofTime(context.Background()); !errors.Is(e, files.ErrDependencyUnavailable) {
		t.Fatal("unconfigured clock accepted", e)
	}
}

// The clock transaction supplies only a timestamp. It cannot bypass the
// existing exact commitment, temporal bounds, or atomic settlement audit.
func TestFileDeleteProofTimeAndClockGuards(t *testing.T) {
	c, s, m, ticket := deletionCommitFixture(t)
	ctx := context.Background()
	permit := commitDelete(t, s, ticket, m.ObjectVersionID)
	var committed, before, after time.Time
	if e := c.QueryRow(ctx, "SELECT committed_at FROM file_delete_versions WHERE commitment_id=$1", permit.CommitmentID).Scan(&committed); e != nil {
		t.Fatal(e)
	}
	if e := c.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&before); e != nil {
		t.Fatal(e)
	}
	proofAt, e := s.FileDeleteProofTime(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&after); e != nil {
		t.Fatal(e)
	}
	if proofAt.Before(before) || proofAt.After(after) {
		t.Fatal("proof time did not come from database interval", before, proofAt, after)
	}
	for _, invalid := range []time.Time{committed.Add(-time.Microsecond), after.Add(time.Hour)} {
		if e = s.SettleFileDeleteVersion(ctx, permit, filecleanup.AbsenceProof{VersionID: permit.VersionID, Absent: true, CheckedAt: invalid}); !errors.Is(e, filecleanup.ErrIncomplete) {
			t.Fatal("out-of-bounds proof accepted", e)
		}
	}
	var phase string
	var audits int
	if e = c.QueryRow(ctx, "SELECT phase FROM file_delete_versions WHERE commitment_id=$1", permit.CommitmentID).Scan(&phase); e != nil || phase != "committed" {
		t.Fatal("invalid proof changed commitment", phase, e)
	}
	if e = c.QueryRow(ctx, "SELECT count(*) FROM file_worker_audit_events WHERE operation='delete_settle'").Scan(&audits); e != nil || audits != 0 {
		t.Fatal("invalid proof changed audit", audits, e)
	}
	proofAt, e = s.FileDeleteProofTime(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SettleFileDeleteVersion(ctx, permit, filecleanup.AbsenceProof{VersionID: permit.VersionID, Absent: true, CheckedAt: proofAt}); e != nil {
		t.Fatal(e)
	}
	if e = c.QueryRow(ctx, "SELECT count(*) FROM file_worker_audit_events WHERE operation='delete_settle'").Scan(&audits); e != nil || audits != 1 {
		t.Fatal("settlement audit missing", audits, e)
	}
}

func TestFileDeleteProofTimeDoesNotGrantLease(t *testing.T) {
	c, s, m, ticket := deletionCommitFixture(t)
	ctx := context.Background()
	permit := commitDelete(t, s, ticket, m.ObjectVersionID)
	proofAt, e := s.FileDeleteProofTime(ctx)
	if e != nil {
		t.Fatal(e)
	}
	run(t, c, "UPDATE file_delete_jobs SET lease_expires_at=created_at+interval '1 microsecond',updated_at=clock_timestamp() WHERE id=$1", ticket.JobID)
	if e = s.SettleFileDeleteVersion(ctx, permit, filecleanup.AbsenceProof{VersionID: permit.VersionID, Absent: true, CheckedAt: proofAt}); !errors.Is(e, filecleanup.ErrLeaseLost) {
		t.Fatal("clock read granted stale owner authority", e)
	}
	if e = rawSchemaHold(c, m.ConversationID); e == nil {
		t.Fatal("expired proof removed unresolved delete barrier")
	}
}
