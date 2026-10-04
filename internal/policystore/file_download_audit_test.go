package policystore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestFileDownloadAuthorizationAuditAtomic(t *testing.T) {
	c, s, _, m := fileHistoryFixture(t, "direct")
	ctx := context.Background()
	ticket := beginDownload(t, c, m, publisher())
	run(t, c, `CREATE FUNCTION fail_download_auth_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='file_download_authorize' THEN RAISE EXCEPTION 'injected';END IF;RETURN NEW;END $$;CREATE TRIGGER fail_download_auth_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_download_auth_audit()`)
	if e := s.AuthorizeFileDownload(ctx, publisher(), ticket); !errors.Is(e, policystore.ErrAuditUnavailable) {
		t.Fatal(e)
	}
	var phase string
	var audits int
	if e := c.QueryRow(ctx, "SELECT phase,(SELECT count(*) FROM audit_events WHERE action='file_download_authorize') FROM file_download_sessions WHERE id=$1", ticket.SessionID).Scan(&phase, &audits); e != nil || phase != "preparing" || audits != 0 {
		t.Fatal(phase, audits, e)
	}
	if e := s.CheckFileDownload(ctx, publisher(), ticket); e == nil {
		t.Fatal("unaudited stream")
	}
	if e := s.FinishFileDownload(ctx, ticket, filedownload.Result{Outcome: "interrupted", Reason: "audit_unavailable"}); e != nil {
		t.Fatal(e)
	}
}
func TestFileDownloadAuditRepairGate(t *testing.T) {
	c, s, _, m := fileHistoryFixture(t, "direct")
	ctx := context.Background()
	ticket := beginDownload(t, c, m, publisher())
	if e := s.FinishFileDownload(ctx, ticket, filedownload.Result{Outcome: "interrupted", Reason: "client_disconnected"}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.BeginFileDownload(ctx, publisher(), m.ID, uploadOwner, downloadDeadline(t, c, 45e9)); !errors.Is(e, filedownload.ErrAuditPending) {
		t.Fatal("terminal gap", e)
	}
	_ = beginDownload(t, c, m, groupMemberIdentity())
	run(t, c, `CREATE FUNCTION fail_download_terminal_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.operation='download_terminal' THEN RAISE EXCEPTION 'injected';END IF;RETURN NEW;END $$;CREATE TRIGGER fail_download_terminal_audit BEFORE INSERT ON file_worker_audit_events FOR EACH ROW EXECUTE FUNCTION fail_download_terminal_audit()`)
	if n, e := s.RepairFileDownloadAudit(ctx, freshFile().ID, 20); e == nil || n != 0 {
		t.Fatal("failed audit acknowledged", n, e)
	}
	var ack bool
	if e := c.QueryRow(ctx, "SELECT audit_acked FROM file_download_sessions WHERE id=$1", ticket.SessionID).Scan(&ack); e != nil || ack {
		t.Fatal(ack, e)
	}
	run(t, c, "DROP TRIGGER fail_download_terminal_audit ON file_worker_audit_events")
	owner := freshFile().ID
	if n, e := s.RepairFileDownloadAudit(ctx, owner, 20); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if n, e := s.RepairFileDownloadAudit(ctx, owner, 20); e != nil || n != 0 {
		t.Fatal("duplicate audit", n, e)
	}
	var facts int
	if e := c.QueryRow(ctx, "SELECT count(*) FROM file_worker_audit_events WHERE download_session_id=$1 AND worker_id=$2 AND job_id=$1", ticket.SessionID, owner).Scan(&facts); e != nil || facts != 1 {
		t.Fatal(facts, e)
	}
	_ = beginDownload(t, c, m, publisher())
}
func TestFileDownloadTerminalDBFailureGate(t *testing.T) {
	c, s, _, m := fileHistoryFixture(t, "direct")
	ctx := context.Background()
	ticket := beginDownload(t, c, m, publisher())
	if e := s.AuthorizeFileDownload(ctx, publisher(), ticket); e != nil {
		t.Fatal(e)
	}
	run(t, c, `CREATE FUNCTION fail_download_terminal_fact() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected terminal failure';END $$;CREATE TRIGGER fail_download_terminal_fact BEFORE INSERT ON file_download_terminal_events FOR EACH ROW EXECUTE FUNCTION fail_download_terminal_fact()`)
	if e := s.FinishFileDownload(ctx, ticket, filedownload.Result{Outcome: "completed", Reason: "completed", BytesWritten: 1}); e == nil {
		t.Fatal("failed terminal persisted")
	}
	var phase string
	var n int
	if e := c.QueryRow(ctx, "SELECT phase,(SELECT count(*) FROM file_download_terminal_events WHERE session_id=$1) FROM file_download_sessions WHERE id=$1", ticket.SessionID).Scan(&phase, &n); e != nil || phase != "authorized" || n != 0 {
		t.Fatal(phase, n, e)
	}
	if _, e := s.BeginFileDownload(ctx, publisher(), m.ID, uploadOwner, downloadDeadline(t, c, 45e9)); !errors.Is(e, filedownload.ErrBusy) {
		t.Fatal(e)
	}
}
