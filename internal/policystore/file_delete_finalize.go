package policystore

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/filecleanup"
	"github.com/leileipei/Enterprise_IM/internal/files"
)

// Finalization serializes quota release with reservations and keeps the original
// message, attachment and idempotency evidence. No storage call occurs in a tx.
func (s Service) FinalizeFileDelete(ctx context.Context, t filecleanup.Ticket) error {
	if filecleanup.ValidateTicket(t) != nil {
		return filecleanup.ErrLeaseLost
	}
	_, e := cleanupTransaction(ctx, s, func(tx pgx.Tx) (bool, error) {
		var id string
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`SELECT id::text FROM tenants WHERE id=$1 FOR SHARE NOWAIT`, []any{t.TenantID}},
			{`SELECT id::text FROM conversations WHERE tenant_id=$1 AND id=$2 FOR UPDATE NOWAIT`, []any{t.TenantID, t.ConversationID}},
			{`SELECT tenant_id::text FROM tenant_file_retention_policy WHERE tenant_id=$1 FOR SHARE NOWAIT`, []any{t.TenantID}},
			{`SELECT tenant_id::text FROM tenant_file_upload_policy WHERE tenant_id=$1 FOR UPDATE NOWAIT`, []any{t.TenantID}},
		} {
			if e := tx.QueryRow(ctx, q.sql, q.args...).Scan(&id); e != nil {
				return false, e
			}
		}
		m, p, at, e := deleteOrigin(ctx, tx, t.TenantID, t.ConversationID, t.FileID)
		if e != nil {
			return false, e
		}
		j, e := scanDeleteJob(tx.QueryRow(ctx, "SELECT "+deleteJobColumns+" FROM file_delete_jobs WHERE id=$1 FOR UPDATE NOWAIT", t.JobID))
		if e != nil {
			return false, e
		}
		if j.Ticket != t {
			return false, filecleanup.ErrLeaseLost
		}
		if j.Phase == "finished" && m.State == files.StateDeleted && m.StateVersion == t.StateVersion+1 {
			return true, nil
		}
		if !at.Before(t.LeaseExpiresAt) || m.State != files.StateDeletePending || m.StateVersion != t.StateVersion {
			return false, filecleanup.ErrLeaseLost
		}
		if j.Phase != "inventory" || !j.Exhausted || !j.Safe || j.NextKey != "" || j.NextVersion != "" || j.Reason != "" {
			return false, filecleanup.ErrIncomplete
		}
		if !p.CleanupEnabled || p.Version != t.PolicyVersion {
			return false, filecleanup.ErrBlocked
		}
		held, e := deleteHeld(ctx, tx, m)
		if e != nil {
			return false, e
		}
		if held {
			return false, filecleanup.ErrBlocked
		}
		reason, e := deleteObligation(ctx, tx, m)
		if e != nil {
			return false, e
		}
		if reason != "" {
			return false, filecleanup.ErrBlocked
		}
		var remaining bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM file_delete_versions WHERE tenant_id=$1 AND job_id=$2 AND phase<>'absent')`, t.TenantID, t.JobID).Scan(&remaining); e != nil {
			return false, e
		}
		if remaining {
			return false, filecleanup.ErrIncomplete
		}
		at, e = fileClock(ctx, tx)
		if e != nil {
			return false, e
		}
		due, e := deleteDue(ctx, tx, m, p, at)
		if e != nil {
			return false, e
		}
		if !due {
			return false, filecleanup.ErrBlocked
		}
		if !at.Before(t.LeaseExpiresAt) {
			return false, filecleanup.ErrLeaseLost
		}
		m, e = scanFile(tx.QueryRow(ctx, `UPDATE file_objects SET state='deleted',state_version=state_version+1,deleted_at=$3,updated_at=$3,original_filename=NULL,declared_media_type=NULL,object_key=NULL,object_version_id=NULL,detected_media_type=NULL,sha256=NULL,scan_job_id=NULL,scan_engine=NULL,scan_definition_version=NULL,scanned_at=NULL,scan_sha256=NULL WHERE tenant_id=$1 AND id=$2 RETURNING `+fileSelect, t.TenantID, t.FileID, at))
		if e != nil {
			return false, e
		}
		if e = scanEvent(ctx, tx, m, files.StateDeletePending, t.JobID, at); e != nil {
			return false, e
		}
		if e = auditFileWorker(ctx, tx, t.TenantID, t.FileID, t.OwnerID, t.JobID, "delete_finalize", "object_deleted", "allow", at); e != nil {
			return false, e
		}
		if _, e = tx.Exec(ctx, `UPDATE file_delete_jobs SET phase='finished',reason_code=NULL,updated_at=$2 WHERE id=$1`, t.JobID, at); e != nil {
			return false, e
		}
		fresh, e := fileClock(ctx, tx)
		if e != nil {
			return false, e
		}
		if !fresh.Before(t.LeaseExpiresAt) {
			return false, filecleanup.ErrLeaseLost
		}
		return true, nil
	})
	return e
}
