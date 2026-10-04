package policystore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/filecleanup"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func cleanupPolicy(t *testing.T, c *pgx.Conn, days int64, enabled bool) {
	t.Helper()
	run(t, c, `UPDATE tenant_file_retention_policy SET file_retention_days=$2,cleanup_enabled=$3,version=version+1,approval_reference='CAB-CLEANUP',actor_user_id=$4,acting_membership_id=$5,updated_at=clock_timestamp() WHERE tenant_id=$1`, tenantA, days, enabled, adminA, adminM)
}
func deleteCandidateFixture(t *testing.T, state string) (*pgx.Conn, policystore.Service, files.Metadata) {
	t.Helper()
	c := fileDownloadDB(t)
	seedDirectConversation(t, c)
	m := storedFile(t, c, state)
	cleanupPolicy(t, c, 365, true)
	return c, policystore.Service{DB: c}, m
}
func claimDelete(t *testing.T, s policystore.Service) filecleanup.Ticket {
	t.Helper()
	ticket, found, e := s.ClaimFileDelete(context.Background(), uploadOwner)
	if e != nil || !found || filecleanup.ValidateTicket(ticket) != nil {
		t.Fatal(ticket, found, e)
	}
	return ticket
}
func deleteJobFacts(t *testing.T, c *pgx.Conn, ticket filecleanup.Ticket) (string, bool, bool, string) {
	t.Helper()
	var phase, reason string
	var exhausted, safe bool
	e := c.QueryRow(context.Background(), `SELECT phase,inventory_exhausted,source_safe,COALESCE(reason_code,'') FROM file_delete_jobs WHERE id=$1`, ticket.JobID).Scan(&phase, &exhausted, &safe, &reason)
	if e != nil {
		t.Fatal(e)
	}
	return phase, exhausted, safe, reason
}
func TestFileDeleteCandidatesBound(t *testing.T) {
	c := fileDownloadDB(t)
	m, _ := downloadFixture(t, c)
	s := policystore.Service{DB: c}
	ctx := context.Background()
	cleanupPolicy(t, c, 365, true)
	if _, ok, e := s.ClaimFileDelete(ctx, uploadOwner); e != nil || ok {
		t.Fatal("live bound file selected", ok, e)
	}
	cleanupPolicy(t, c, 1, false)
	if _, ok, e := s.ClaimFileDelete(ctx, uploadOwner); e != nil || ok {
		t.Fatal("paused selected", ok, e)
	}
	cleanupPolicy(t, c, 1, true)
	ticket := claimDelete(t, s)
	var state string
	var version int64
	if e := c.QueryRow(ctx, "SELECT state,state_version FROM file_objects WHERE id=$1", m.ID).Scan(&state, &version); e != nil || state != "delete_pending" || version != m.StateVersion+1 {
		t.Fatal(state, version, e)
	}
	cleanupPolicy(t, c, 365, true)
	if _, e := s.BeginFileDownload(ctx, publisher(), m.ID, uploadOwner, downloadDeadline(t, c, 30*time.Second)); e == nil {
		t.Fatal("retention extension resurrected pending")
	}
	var life, audit int
	e := c.QueryRow(ctx, `SELECT (SELECT count(*) FROM file_lifecycle_events WHERE file_id=$1 AND reason_code='deletion_requested'),(SELECT count(*) FROM file_worker_audit_events WHERE job_id=$2 AND operation='delete_claim')`, m.ID, ticket.JobID).Scan(&life, &audit)
	if e != nil || life != 1 || audit != 1 {
		t.Fatal(life, audit, e)
	}
}
func TestFileDeleteCandidatesOrphan(t *testing.T) {
	for _, state := range []string{"allocated", "uploaded", "scanning", "ready", "rejected", "scan_failed"} {
		t.Run(state, func(t *testing.T) {
			c, s, m := deleteCandidateFixture(t, state)
			ticket := claimDelete(t, s)
			if ticket.FileID != m.ID || ticket.ConversationID != m.ConversationID {
				t.Fatal(ticket)
			}
			var used int64
			if e := c.QueryRow(context.Background(), "SELECT sum(declared_size_bytes) FROM file_objects WHERE state<>'deleted'").Scan(&used); e != nil || used != m.DeclaredSizeBytes {
				t.Fatal("quota released at quarantine", used, e)
			}
		})
	}
	t.Run("unexpired", func(t *testing.T) {
		c := fileDownloadDB(t)
		seedDirectConversation(t, c)
		cleanupPolicy(t, c, 1, true)
		m := freshFile()
		m.CreatedAt = downloadDeadline(t, c, -time.Minute)
		m.UpdatedAt = m.CreatedAt
		m.UploadExpiresAt = m.CreatedAt.Add(time.Hour)
		if e := writeFile(c, m, true); e != nil {
			t.Fatal(e)
		}
		if _, ok, e := (policystore.Service{DB: c}).ClaimFileDelete(context.Background(), uploadOwner); e != nil || ok {
			t.Fatal(ok, e)
		}
	})
}
func TestFileDeleteCandidatesHold(t *testing.T) {
	for _, state := range []string{"allocated", "ready", "rejected", "scan_failed"} {
		t.Run(state, func(t *testing.T) {
			c, s, m := deleteCandidateFixture(t, state)
			if e := rawSchemaHold(c, m.ConversationID); e != nil {
				t.Fatal(e)
			}
			if _, found, e := s.ClaimFileDelete(context.Background(), uploadOwner); e != nil || found {
				t.Fatal("held orphan selected", found, e)
			}
			var got string
			if e := c.QueryRow(context.Background(), "SELECT state FROM file_objects WHERE id=$1", m.ID).Scan(&got); e != nil || got != state {
				t.Fatal(got, e)
			}
		})
	}
}
func TestFileDeleteCandidatesInFlight(t *testing.T) {
	for _, kind := range []string{"upload", "scan", "download"} {
		t.Run(kind, func(t *testing.T) {
			var c *pgx.Conn
			var s policystore.Service
			var m files.Metadata
			if kind == "download" {
				c, s, _, m = fileHistoryFixture(t, "direct")
				_ = beginDownload(t, c, m, publisher())
				cleanupPolicy(t, c, 1, true)
			} else {
				state := "allocated"
				if kind == "scan" {
					state = "scanning"
				}
				c, s, m = deleteCandidateFixture(t, state)
				if kind == "upload" {
					runtimeAttempt(t, c, m)
				} else {
					runtimeScanJob(t, c, m)
				}
			}
			ticket := claimDelete(t, s)
			phase, _, safe, reason := deleteJobFacts(t, c, ticket)
			if phase != "blocked" || safe || reason != "in_flight" && reason != "unknown_upload" {
				t.Fatal(phase, safe, reason)
			}
			var got string
			if e := c.QueryRow(context.Background(), "SELECT state FROM file_objects WHERE id=$1", m.ID).Scan(&got); e != nil || got != "delete_pending" {
				t.Fatal(got, e)
			}
		})
	}
}
func TestFileDeleteCandidatesAuditAtomic(t *testing.T) {
	c, s, m := deleteCandidateFixture(t, "ready")
	run(t, c, `CREATE FUNCTION fail_delete_claim_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.operation='delete_claim' THEN RAISE EXCEPTION 'fixture audit failure';END IF;RETURN NEW;END $$;CREATE TRIGGER fail_delete_claim BEFORE INSERT ON file_worker_audit_events FOR EACH ROW EXECUTE FUNCTION fail_delete_claim_audit()`)
	if _, ok, e := s.ClaimFileDelete(context.Background(), uploadOwner); ok || !errors.Is(e, policystore.ErrAuditUnavailable) {
		t.Fatal(ok, e)
	}
	var state string
	var count int
	if e := c.QueryRow(context.Background(), `SELECT state,(SELECT count(*) FROM file_delete_jobs) FROM file_objects WHERE id=$1`, m.ID).Scan(&state, &count); e != nil || state != "ready" || count != 0 {
		t.Fatal(state, count, e)
	}
}

func TestFileDeleteCandidatesCounterSaturates(t *testing.T) {
	c, s, _ := deleteCandidateFixture(t, "ready")
	ticket := claimDelete(t, s)
	run(t, c, `UPDATE file_delete_jobs SET attempts=9223372036854775807 WHERE id=$1`, ticket.JobID)
	next, found, e := s.ClaimFileDelete(context.Background(), uploadOwner)
	if e != nil || !found || next.JobID != ticket.JobID || next.LeaseToken == ticket.LeaseToken {
		t.Fatal("observation counter stopped reconciliation", next, found, e)
	}
}
