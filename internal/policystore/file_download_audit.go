package policystore

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/filedownload"
)

// Each repair atomically settles a crash (if needed), writes one machine audit,
// and acknowledges both paired records. No lease expiry clears a gap by itself.
func (s Service) RepairFileDownloadAudit(ctx context.Context, ownerID string, limit int) (int, error) {
	if !directoryUUIDPattern.MatchString(ownerID) || limit < 1 || limit > 20 {
		return 0, filedownload.ErrNotFound
	}
	ownerID = strings.ToLower(ownerID)
	repaired := 0
	for n := 0; n < limit; n++ {
		done, e := downloadTransaction(ctx, s, func(tx pgx.Tx) (bool, error) {
			d, e := scanDownloadSession(tx.QueryRow(ctx, "SELECT "+downloadSessionColumns+` FROM file_download_sessions WHERE NOT audit_acked AND (phase IN ('completed','interrupted','unknown') OR deadline<=clock_timestamp()) ORDER BY updated_at,id LIMIT 1`))
			if errors.Is(e, pgx.ErrNoRows) {
				return false, nil
			}
			if e != nil {
				return false, e
			}
			if e = lockDownloadMachineOrigin(ctx, tx, d.Tenant, d.Conversation, d.File); e != nil {
				return false, e
			}
			d, e = scanDownloadSession(tx.QueryRow(ctx, "SELECT "+downloadSessionColumns+" FROM file_download_sessions WHERE id=$1 FOR UPDATE NOWAIT", d.ID))
			if e != nil {
				return false, e
			}
			if d.Acked {
				return false, nil
			}
			at, e := fileClock(ctx, tx)
			if e != nil {
				return false, e
			}
			if d.Phase == "preparing" || d.Phase == "authorized" {
				if at.Before(d.Deadline) {
					return false, nil
				}
				r := filedownload.Result{Outcome: "unknown", Reason: "process_lost", BytesWritten: d.Written}
				if e = finishDownloadTx(ctx, tx, d, r, at); e != nil {
					return false, e
				}
				d.Phase, d.Reason = r.Outcome, r.Reason
			}
			var auditID int64
			e = tx.QueryRow(ctx, `INSERT INTO file_worker_audit_events(tenant_id,file_id,worker_id,job_id,operation,reason_code,outcome,occurred_at,download_session_id) VALUES($1,$2,$3,$4,'download_terminal',$5,'allow',$6,$4) RETURNING id`, d.Tenant, d.File, ownerID, d.ID, d.Reason, at).Scan(&auditID)
			if e != nil {
				return false, errors.Join(ErrAuditUnavailable, e)
			}
			if _, e = tx.Exec(ctx, "UPDATE file_download_terminal_events SET ack_audit_id=$2 WHERE session_id=$1", d.ID, auditID); e != nil {
				return false, e
			}
			if _, e = tx.Exec(ctx, "UPDATE file_download_sessions SET audit_acked=true WHERE id=$1", d.ID); e != nil {
				return false, e
			}
			return true, nil
		})
		if e != nil {
			return repaired, e
		}
		if !done {
			return repaired, nil
		}
		repaired++
	}
	return repaired, nil
}
