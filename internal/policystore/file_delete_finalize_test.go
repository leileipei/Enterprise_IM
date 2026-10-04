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

func finalizeDeleteFixture(t *testing.T) (*pgx.Conn, policystore.Service, files.Metadata, filecleanup.Ticket) {
	t.Helper()
	c, s, m, ticket := deletionCommitFixture(t)
	permit := commitDelete(t, s, ticket, m.ObjectVersionID)
	if e := s.SettleFileDeleteVersion(context.Background(), permit, filecleanup.AbsenceProof{VersionID: permit.VersionID, CheckedAt: downloadDeadline(t, c, 0), Absent: true}); e != nil {
		t.Fatal(e)
	}
	if e := s.RecordFileDeleteInventory(context.Background(), ticket, filecleanup.Inventory{Exhausted: true}); e != nil {
		t.Fatal(e)
	}
	return c, s, m, ticket
}
func TestFileDeleteFinalizeIdempotent(t *testing.T) {
	c, s, m, ticket := finalizeDeleteFixture(t)
	if e := s.FinalizeFileDelete(context.Background(), ticket); e != nil {
		t.Fatal(e)
	}
	if e := s.FinalizeFileDelete(context.Background(), ticket); e != nil {
		t.Fatal("final replay", e)
	}
	var state, name string
	var version, used int64
	var audit, life int
	e := c.QueryRow(context.Background(), `SELECT state,COALESCE(original_filename,''),state_version,(SELECT COALESCE(sum(declared_size_bytes),0) FROM file_objects WHERE state<>'deleted'),(SELECT count(*) FROM file_worker_audit_events WHERE job_id=$2 AND operation='delete_finalize'),(SELECT count(*) FROM file_lifecycle_events WHERE file_id=$1 AND reason_code='object_deleted') FROM file_objects WHERE id=$1`, m.ID, ticket.JobID).Scan(&state, &name, &version, &used, &audit, &life)
	if e != nil || state != "deleted" || name != "" || version != ticket.StateVersion+1 || used != 0 || audit != 1 || life != 1 {
		t.Fatal(state, name, version, used, audit, life, e)
	}
}
func TestFileDeleteFinalizeQuotaAuditAtomic(t *testing.T) {
	for _, where := range []string{"audit", "commit"} {
		t.Run(where, func(t *testing.T) {
			c, s, m, ticket := finalizeDeleteFixture(t)
			if where == "audit" {
				run(t, c, `CREATE FUNCTION reject_delete_finalize() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.operation='delete_finalize' THEN RAISE EXCEPTION 'fixture audit failure';END IF;RETURN NEW;END $$;CREATE TRIGGER reject_delete_finalize BEFORE INSERT ON file_worker_audit_events FOR EACH ROW EXECUTE FUNCTION reject_delete_finalize()`)
			} else {
				run(t, c, `CREATE FUNCTION reject_delete_finalize() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state='deleted' THEN RAISE EXCEPTION 'fixture commit failure';END IF;RETURN NULL;END $$;CREATE CONSTRAINT TRIGGER reject_delete_finalize AFTER UPDATE ON file_objects DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_delete_finalize()`)
			}
			if e := s.FinalizeFileDelete(context.Background(), ticket); e == nil {
				t.Fatal("failed finalization committed")
			}
			var state, name, jobPhase string
			var used int64
			e := c.QueryRow(context.Background(), `SELECT f.state,f.original_filename,j.phase,(SELECT sum(declared_size_bytes) FROM file_objects WHERE state<>'deleted') FROM file_objects f JOIN file_delete_jobs j ON j.file_id=f.id WHERE f.id=$1`, m.ID).Scan(&state, &name, &jobPhase, &used)
			if e != nil || state != "delete_pending" || name == "" || jobPhase == "finished" || used != m.DeclaredSizeBytes {
				t.Fatal("failed final released quota", state, name, jobPhase, used, e)
			}
			table := "file_objects"
			if where == "audit" {
				table = "file_worker_audit_events"
			}
			run(t, c, "DROP TRIGGER reject_delete_finalize ON "+table)
			if e = s.FinalizeFileDelete(context.Background(), ticket); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestFileDeleteFinalizeMessageEvidence(t *testing.T) {
	c, s, cid, m := fileHistoryFixture(t, "direct")
	cleanupPolicy(t, c, 1, true)
	ticket := claimDelete(t, s)
	snapshot := func() string {
		var v string
		e := c.QueryRow(context.Background(), `SELECT concat(msg.seq,':',msg.text_body,':',encode(msg.content_digest,'hex'),':',encode(i.content_digest,'hex'),':',encode(a.sealed_sha256,'hex'),':',encode(f.request_digest,'hex'),':',convo.last_seq) FROM message_attachments a JOIN messages msg ON msg.tenant_id=a.tenant_id AND msg.id=a.message_id JOIN message_idempotency i ON i.tenant_id=msg.tenant_id AND i.message_id=msg.id JOIN file_objects f ON f.id=a.file_id JOIN conversations convo ON convo.id=msg.conversation_id WHERE a.file_id=$1`, m.ID).Scan(&v)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	before := snapshot()
	if e := s.RecordFileDeleteInventory(context.Background(), ticket, filecleanup.Inventory{Exhausted: true}); e != nil {
		t.Fatal(e)
	}
	permit := commitDelete(t, s, ticket, m.ObjectVersionID)
	if e := s.SettleFileDeleteVersion(context.Background(), permit, filecleanup.AbsenceProof{VersionID: permit.VersionID, CheckedAt: downloadDeadline(t, c, 0), Absent: true}); e != nil {
		t.Fatal(e)
	}
	if e := s.RecordFileDeleteInventory(context.Background(), ticket, filecleanup.Inventory{Exhausted: true}); e != nil {
		t.Fatal(e)
	}
	if e := s.FinalizeFileDelete(context.Background(), ticket); e != nil {
		t.Fatal(e)
	}
	if after := snapshot(); after != before {
		t.Fatal("message/source/idempotency/digest changed")
	}
	page, e := s.PullTextMessages(context.Background(), publisher(), cid, 1, 1)
	if e != nil || len(page.Messages) != 1 || page.Messages[0].Attachment == nil || page.Messages[0].Attachment.Available {
		t.Fatal(page, e)
	}
}
func TestFileDeleteFinalizeRequiresFreshInventory(t *testing.T) {
	c, s, m, ticket := deletionCommitFixture(t)
	permit := commitDelete(t, s, ticket, m.ObjectVersionID)
	if e := s.SettleFileDeleteVersion(context.Background(), permit, filecleanup.AbsenceProof{VersionID: permit.VersionID, CheckedAt: downloadDeadline(t, c, 0), Absent: true}); e != nil {
		t.Fatal(e)
	}
	if e := s.FinalizeFileDelete(context.Background(), ticket); !errors.Is(e, filecleanup.ErrIncomplete) {
		t.Fatal("old inventory finalized", e)
	}
}
