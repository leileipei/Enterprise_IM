package policystore_test

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/filecleanup"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"testing"
)

func TestFileDeleteLateResponseBarrier(t *testing.T) {
	c, s, m, ticket := deletionCommitFixture(t)
	ctx := context.Background()
	permit := commitDelete(t, s, ticket, m.ObjectVersionID)
	peer := filePeer(t, c)
	cleanupPolicy(t, c, 365, false)
	run(t, c, "UPDATE file_delete_jobs SET lease_expires_at=created_at+interval '1 microsecond',updated_at=clock_timestamp() WHERE id=$1", ticket.JobID)
	if e := rawSchemaHold(peer, m.ConversationID); e == nil {
		t.Fatal("hold protected an unresolved remote delete")
	}
	recovered, found, e := (policystore.Service{DB: peer}).ClaimFileDeleteRecovery(ctx, clientB)
	if e != nil || !found || recovered.CommitmentID != permit.CommitmentID {
		t.Fatal(found, e)
	}
	if e = s.SettleFileDeleteVersion(ctx, permit, filecleanup.AbsenceProof{VersionID: permit.VersionID, Absent: true, CheckedAt: downloadDeadline(t, c, 0)}); !errors.Is(e, filecleanup.ErrLeaseLost) {
		t.Fatal("old owner ABA", e)
	}
	if e = rawSchemaHold(c, m.ConversationID); e == nil {
		t.Fatal("takeover cleared hold barrier")
	}
	if e = (policystore.Service{DB: peer}).SettleFileDeleteVersion(ctx, recovered, filecleanup.AbsenceProof{VersionID: permit.VersionID, Absent: true, CheckedAt: downloadDeadline(t, c, 0)}); e != nil {
		t.Fatal(e)
	}
	if e = rawSchemaHold(c, m.ConversationID); e != nil {
		t.Fatal(e)
	}
}
func TestFileDeleteHoldDirectSQLConcurrent(t *testing.T) {
	for _, first := range []string{"hold", "commit"} {
		t.Run(first, func(t *testing.T) {
			c, s, m, ticket := deletionCommitFixture(t)
			peer := filePeer(t, c)
			ctx := context.Background()
			if first == "hold" {
				tx, e := c.Begin(ctx)
				if e != nil {
					t.Fatal(e)
				}
				defer tx.Rollback(ctx)
				if _, e = tx.Exec(ctx, "SELECT id FROM conversations WHERE id=$1 FOR UPDATE", m.ConversationID); e != nil {
					t.Fatal(e)
				}
				if _, e = (policystore.Service{DB: peer}).CommitFileDeleteVersion(ctx, ticket, m.ObjectVersionID); e == nil {
					t.Fatal("commit crossed hold writer")
				}
				if e = tx.Commit(ctx); e != nil {
					t.Fatal(e)
				}
				if e = rawSchemaHold(c, m.ConversationID); e != nil {
					t.Fatal(e)
				}
				if _, e = s.CommitFileDeleteVersion(ctx, ticket, m.ObjectVersionID); !errors.Is(e, filecleanup.ErrBlocked) {
					t.Fatal(e)
				}
			} else {
				commitDelete(t, s, ticket, m.ObjectVersionID)
				if e := rawSchemaHold(peer, m.ConversationID); e == nil {
					t.Fatal("direct SQL bypassed committed barrier")
				}
			}
		})
	}
}
func TestFileDeleteConcurrentQuotaReservation(t *testing.T) {
	c, _, m, ticket := finalizeDeleteFixture(t)
	ctx := context.Background()
	finalDB := filePeer(t, c)
	reserveDB := filePeer(t, c)
	runtimePolicyEdit(t, c)
	run(t, c, "UPDATE tenant_file_upload_policy SET tenant_storage_budget_bytes=$2 WHERE tenant_id=$1", tenantA, files.MaxFileSizeBytes)
	filler := freshFile()
	filler.DeclaredSizeBytes = files.MaxFileSizeBytes - m.DeclaredSizeBytes
	if e := writeFile(c, filler, true); e != nil {
		t.Fatal(e)
	}
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "LOCK TABLE file_worker_audit_events IN ACCESS EXCLUSIVE MODE"); e != nil {
		t.Fatal(e)
	}
	final := make(chan error, 1)
	go func() { final <- (policystore.Service{DB: finalDB}).FinalizeFileDelete(ctx, ticket) }()
	waitFileLock(t, c, finalDB, "RowExclusiveLock", func() { tx.Rollback(ctx) })
	p := reservationParams()
	p.DeclaredSizeBytes = m.DeclaredSizeBytes
	reserved := make(chan error, 1)
	go func() { _, e := (policystore.Service{DB: reserveDB}).ReserveFile(ctx, publisher(), p); reserved <- e }()
	waitFileLock(t, c, reserveDB, "", func() { tx.Rollback(ctx) })
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-final; e != nil {
		t.Fatal(e)
	}
	if e = <-reserved; e != nil {
		t.Fatal("reservation did not see atomic released quota", e)
	}
	var used int64
	if e = c.QueryRow(ctx, "SELECT sum(declared_size_bytes) FROM file_objects WHERE state<>'deleted'").Scan(&used); e != nil || used != files.MaxFileSizeBytes {
		t.Fatal(used, e)
	}
}
func TestFileDeleteFinalizeFault(t *testing.T) { TestFileDeleteFinalizeQuotaAuditAtomic(t) }
func TestFileDeleteConcurrentLateScan(t *testing.T) {
	c := fileDownloadDB(t)
	seedDirectConversation(t, c)
	runtimePolicyEdit(t, c)
	m := storedFile(t, c, "uploaded")
	s := policystore.Service{DB: c}
	ctx := context.Background()
	job, ok, e := s.ClaimFileScan(ctx, uploadOwner)
	if e != nil || !ok {
		t.Fatal(e)
	}
	cleanupPolicy(t, c, 365, true)
	ticket := claimDelete(t, s)
	if ticket.FileID != m.ID {
		t.Fatal(ticket)
	}
	if e = s.CompleteFileScan(ctx, job, cleanScan(job)); !errors.Is(e, files.ErrLeaseLost) {
		t.Fatal("late scanner resurrected pending", e)
	}
	var state string
	if e = c.QueryRow(ctx, "SELECT state FROM file_objects WHERE id=$1", m.ID).Scan(&state); e != nil || state != "delete_pending" {
		t.Fatal(state, e)
	}
}
