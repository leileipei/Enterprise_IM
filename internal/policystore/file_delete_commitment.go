package policystore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/filecleanup"
)

func (s Service) CommitFileDeleteVersion(ctx context.Context, t filecleanup.Ticket, versionID string) (filecleanup.Commitment, error) {
	var zero filecleanup.Commitment
	if filecleanup.ValidateTicket(t) != nil || !uploadVersionValid(versionID) {
		return zero, filecleanup.ErrIncomplete
	}
	c, e := cleanupTransaction(ctx, s, func(tx pgx.Tx) (filecleanup.Commitment, error) {
		j, m, p, at, e := currentDeleteJob(ctx, tx, t)
		if e != nil {
			return zero, e
		}
		if j.Phase != "inventory" || !j.Exhausted || !j.Safe || j.Reason != "" || !p.CleanupEnabled || p.Version != t.PolicyVersion {
			return zero, filecleanup.ErrBlocked
		}
		held, e := deleteHeld(ctx, tx, m)
		if e != nil {
			return zero, e
		}
		if held {
			return zero, filecleanup.ErrBlocked
		}
		reason, e := deleteObligation(ctx, tx, m)
		if e != nil {
			return zero, e
		}
		if reason != "" {
			return zero, filecleanup.ErrBlocked
		}
		due, e := deleteDue(ctx, tx, m, p, at)
		if e != nil {
			return zero, e
		}
		if !due {
			return zero, filecleanup.ErrBlocked
		}
		var unresolved bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM file_delete_versions WHERE tenant_id=$1 AND conversation_id=$2 AND phase IN ('committed','uncertain'))`, t.TenantID, t.ConversationID).Scan(&unresolved); e != nil {
			return zero, e
		}
		if unresolved {
			return zero, filecleanup.ErrBlocked
		}
		var phase string
		e = tx.QueryRow(ctx, `SELECT phase FROM file_delete_versions WHERE tenant_id=$1 AND job_id=$2 AND object_version_id=$3 FOR UPDATE NOWAIT`, t.TenantID, t.JobID, versionID).Scan(&phase)
		if errors.Is(e, pgx.ErrNoRows) || e == nil && phase != "inventoried" {
			return zero, filecleanup.ErrIncomplete
		}
		if e != nil {
			return zero, e
		}
		id, _, e := fileNewIDs(ctx, tx)
		if e != nil {
			return zero, e
		}
		at, e = fileClock(ctx, tx)
		if e != nil {
			return zero, e
		}
		due, e = deleteDue(ctx, tx, m, p, at)
		if e != nil {
			return zero, e
		}
		if !due || !at.Before(t.LeaseExpiresAt) {
			return zero, filecleanup.ErrBlocked
		}
		_, e = tx.Exec(ctx, `UPDATE file_delete_versions SET phase='committed',commitment_id=$4,committed_at=$5,updated_at=$5 WHERE tenant_id=$1 AND job_id=$2 AND object_version_id=$3 AND phase='inventoried'`, t.TenantID, t.JobID, versionID, id, at)
		if e != nil {
			return zero, e
		}
		_, e = tx.Exec(ctx, `UPDATE file_delete_jobs SET phase='deleting',reason_code=NULL,updated_at=$2 WHERE id=$1`, t.JobID, at)
		if e != nil {
			return zero, e
		}
		if e = auditFileWorker(ctx, tx, t.TenantID, t.FileID, t.OwnerID, t.JobID, "delete_commit", "deletion_committed", "allow", at); e != nil {
			return zero, e
		}
		fresh, e := fileClock(ctx, tx)
		if e != nil {
			return zero, e
		}
		if !fresh.Before(t.LeaseExpiresAt) {
			return zero, filecleanup.ErrLeaseLost
		}
		due, e = deleteDue(ctx, tx, m, p, fresh)
		if e != nil {
			return zero, e
		}
		if !due {
			return zero, filecleanup.ErrBlocked
		}
		return filecleanup.Commitment{Ticket: t, CommitmentID: id, VersionID: versionID}, nil
	})
	var pe *pgconn.PgError
	if errors.As(e, &pe) && (pe.Code == "23505" || pe.Code == "23514" && pe.Message == "delete commitment blocked") {
		return zero, errors.Join(filecleanup.ErrBlocked, e)
	}
	return c, e
}

// Recovery never grants a new version. The immutable commitment survives owner
// changes, expired leases and paused retention configuration.
func (s Service) ClaimFileDeleteRecovery(ctx context.Context, ownerID string) (filecleanup.Commitment, bool, error) {
	var zero filecleanup.Commitment
	if !directoryUUIDPattern.MatchString(ownerID) {
		return zero, false, filecleanup.ErrLeaseLost
	}
	ownerID = strings.ToLower(ownerID)
	c, e := cleanupTransaction(ctx, s, func(tx pgx.Tx) (filecleanup.Commitment, error) {
		var job, tenant, convo, file, version string
		e := tx.QueryRow(ctx, `SELECT j.id::text,j.tenant_id::text,j.conversation_id::text,j.file_id::text,v.object_version_id FROM file_delete_versions v JOIN file_delete_jobs j ON j.tenant_id=v.tenant_id AND j.id=v.job_id WHERE v.phase IN ('committed','uncertain') AND (j.owner_id=$1 OR j.lease_expires_at<=clock_timestamp()) AND (j.next_retry_at IS NULL OR j.next_retry_at<=clock_timestamp()) ORDER BY v.committed_at,j.id LIMIT 1`, ownerID).Scan(&job, &tenant, &convo, &file, &version)
		if errors.Is(e, pgx.ErrNoRows) {
			return zero, nil
		}
		if e != nil {
			return zero, e
		}
		m, _, at, e := deleteOrigin(ctx, tx, tenant, convo, file)
		if e != nil {
			return zero, e
		}
		j, e := scanDeleteJob(tx.QueryRow(ctx, "SELECT "+deleteJobColumns+" FROM file_delete_jobs WHERE id=$1 FOR UPDATE NOWAIT", job))
		if e != nil {
			return zero, e
		}
		if m.State != "delete_pending" || m.StateVersion != j.Ticket.StateVersion || j.Phase == "finished" {
			return zero, filecleanup.ErrLeaseLost
		}
		at, e = fileClock(ctx, tx)
		if e != nil {
			return zero, e
		}
		if j.Ticket.OwnerID != ownerID && at.Before(j.Ticket.LeaseExpiresAt) {
			return zero, nil
		}
		var id, phase string
		e = tx.QueryRow(ctx, `SELECT commitment_id::text,phase FROM file_delete_versions WHERE tenant_id=$1 AND job_id=$2 AND object_version_id=$3 FOR UPDATE NOWAIT`, tenant, job, version).Scan(&id, &phase)
		if e != nil {
			return zero, e
		}
		if phase != "committed" && phase != "uncertain" {
			return zero, nil
		}
		_, token, e := fileNewIDs(ctx, tx)
		if e != nil {
			return zero, e
		}
		at, e = fileClock(ctx, tx)
		if e != nil {
			return zero, e
		}
		expiry := at.Add(120 * time.Second)
		retryCount := j.Attempts
		if retryCount < 9223372036854775807 {
			retryCount++
		}
		_, e = tx.Exec(ctx, `UPDATE file_delete_jobs SET owner_id=$2,lease_token=$3,lease_expires_at=$4,updated_at=$5,phase='deleting',reason_code='deletion_uncertain',attempts=CASE WHEN attempts<9223372036854775807 THEN attempts+1 ELSE attempts END,next_retry_at=$6 WHERE id=$1`, job, ownerID, token, expiry, at, deleteRetryAt(at, retryCount))
		if e != nil {
			return zero, e
		}
		_, e = tx.Exec(ctx, `UPDATE file_delete_versions SET phase='uncertain',updated_at=$4 WHERE tenant_id=$1 AND job_id=$2 AND object_version_id=$3`, tenant, job, version, at)
		if e != nil {
			return zero, e
		}
		if e = auditFileWorker(ctx, tx, tenant, file, ownerID, job, "delete_claim", "deletion_uncertain", "allow", at); e != nil {
			return zero, e
		}
		fresh, e := fileClock(ctx, tx)
		if e != nil {
			return zero, e
		}
		if !fresh.Before(expiry) {
			return zero, filecleanup.ErrLeaseLost
		}
		t := j.Ticket
		t.OwnerID = ownerID
		t.LeaseToken = token
		t.LeaseExpiresAt = expiry
		permit := filecleanup.Commitment{Ticket: t, CommitmentID: id, VersionID: version}
		if e = filecleanup.ValidateCommitment(permit); e != nil {
			return zero, e
		}
		return permit, nil
	})
	return c, e == nil && c.CommitmentID != "", e
}
func (s Service) SettleFileDeleteVersion(ctx context.Context, c filecleanup.Commitment, proof filecleanup.AbsenceProof) error {
	if e := filecleanup.ValidateAbsenceProof(c, proof); e != nil {
		return e
	}
	_, e := cleanupTransaction(ctx, s, func(tx pgx.Tx) (bool, error) {
		t := c.Ticket
		_, _, _, at, e := currentDeleteJob(ctx, tx, t)
		if e != nil {
			return false, e
		}
		var phase, id string
		var committed time.Time
		e = tx.QueryRow(ctx, `SELECT phase,COALESCE(commitment_id::text,''),committed_at FROM file_delete_versions WHERE tenant_id=$1 AND job_id=$2 AND object_version_id=$3 FOR UPDATE NOWAIT`, t.TenantID, t.JobID, c.VersionID).Scan(&phase, &id, &committed)
		if errors.Is(e, pgx.ErrNoRows) || e == nil && id != c.CommitmentID {
			return false, filecleanup.ErrLeaseLost
		}
		if e != nil {
			return false, e
		}
		if phase == "absent" {
			return true, nil
		}
		if phase != "committed" && phase != "uncertain" || proof.CheckedAt.Before(committed) || proof.CheckedAt.After(at) {
			return false, filecleanup.ErrIncomplete
		}
		_, e = tx.Exec(ctx, `UPDATE file_delete_versions SET phase='absent',absence_checked_at=$4,updated_at=$5 WHERE tenant_id=$1 AND job_id=$2 AND object_version_id=$3`, t.TenantID, t.JobID, c.VersionID, proof.CheckedAt, at)
		if e != nil {
			return false, e
		}
		if e = auditFileWorker(ctx, tx, t.TenantID, t.FileID, t.OwnerID, t.JobID, "delete_settle", "version_absent", "allow", at); e != nil {
			return false, e
		}
		var remaining bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM file_delete_versions WHERE tenant_id=$1 AND job_id=$2 AND phase<>'absent')`, t.TenantID, t.JobID).Scan(&remaining); e != nil {
			return false, e
		}
		// The last settled obligation invalidates the old exhaustive listing. A fresh
		// full inventory must be persisted before finalization can release quota.
		_, e = tx.Exec(ctx, `UPDATE file_delete_jobs SET phase='inventory',reason_code=CASE WHEN $2 THEN NULL ELSE 'inventory_incomplete' END,inventory_exhausted=CASE WHEN $2 THEN inventory_exhausted ELSE false END,next_key='',next_version='',next_retry_at=NULL,updated_at=$3 WHERE id=$1`, t.JobID, remaining, at)
		if e != nil {
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
