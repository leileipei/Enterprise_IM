package policystore

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"slices"
	"strings"
	"time"
)

func auditFileWorker(ctx context.Context, tx pgx.Tx, tenant, file, worker, job, operation, reason, outcome string, at time.Time) error {
	_, e := tx.Exec(ctx, `INSERT INTO file_worker_audit_events(tenant_id,file_id,worker_id,job_id,operation,reason_code,outcome,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, tenant, file, worker, job, operation, reason, outcome, at)
	if e != nil {
		return errors.Join(ErrAuditUnavailable, e)
	}
	return nil
}
func (s Service) ClaimFileUploadRecovery(ctx context.Context, ownerID string) (files.RecoveryTicket, bool, error) {
	var zero files.RecoveryTicket
	if !directoryUUIDPattern.MatchString(ownerID) {
		return zero, false, files.ErrInvalidMetadata
	}
	ownerID = strings.ToLower(ownerID)
	out, e := fileTransaction(ctx, s, func(tx pgx.Tx) (files.RecoveryTicket, error) {
		at, e := fileClock(ctx, tx)
		if e != nil {
			return zero, e
		}
		var tenant, file, attempt string
		e = tx.QueryRow(ctx, `SELECT a.tenant_id::text,a.file_id::text,a.id::text FROM file_upload_attempts a JOIN file_objects f ON f.tenant_id=a.tenant_id AND f.id=a.file_id WHERE f.state='allocated' AND a.phase IN ('storing','recovery_pending') AND a.lease_expires_at<=$1 AND a.lookup_attempts<3 AND (a.next_lookup_at IS NULL OR a.next_lookup_at<=$1) AND (a.recovery_lease_expires_at IS NULL OR a.recovery_lease_expires_at<=$1) ORDER BY a.updated_at,a.id LIMIT 1 FOR UPDATE OF f,a SKIP LOCKED`, at).Scan(&tenant, &file, &attempt)
		if errors.Is(e, pgx.ErrNoRows) {
			return zero, nil
		}
		if e != nil {
			return zero, e
		}
		m, e := scanFile(tx.QueryRow(ctx, "SELECT "+fileSelect+" FROM file_objects WHERE tenant_id=$1 AND id=$2", tenant, file))
		if e != nil {
			return zero, e
		}
		at, e = fileClock(ctx, tx)
		if e != nil {
			return zero, e
		}
		job, token, e := fileNewIDs(ctx, tx)
		if e != nil {
			return zero, e
		}
		lease := at.Add(120 * time.Second)
		var lookups int
		e = tx.QueryRow(ctx, `UPDATE file_upload_attempts SET phase='recovery_pending',recovery_job_id=$3,recovery_lease_token=$4,recovery_owner_id=$5,recovery_lease_expires_at=$6,lookup_attempts=lookup_attempts+1,updated_at=$7,last_reason_code='recovery_started' WHERE tenant_id=$1 AND id=$2 RETURNING lookup_attempts`, tenant, attempt, job, token, ownerID, lease, at).Scan(&lookups)
		if e != nil {
			return zero, e
		}
		u, e := scanAttempt(tx.QueryRow(ctx, "SELECT "+attemptSelect+" FROM file_upload_attempts WHERE tenant_id=$1 AND id=$2", tenant, attempt), m)
		if e != nil {
			return zero, e
		}
		if e = auditFileWorker(ctx, tx, tenant, file, ownerID, job, "upload_recovery_claim", "recovery_started", "allow", at); e != nil {
			return zero, e
		}
		fresh, e := fileClock(ctx, tx)
		if e != nil {
			return zero, e
		}
		if !fresh.Before(lease) {
			return zero, files.ErrLeaseLost
		}
		return files.RecoveryTicket{Upload: u, JobID: job, LeaseToken: token, OwnerID: ownerID, LeaseExpiresAt: lease, LookupAttempt: lookups}, nil
	})
	return out, out.JobID != "" && e == nil, e
}
func (s Service) CompleteFileUploadRecovery(ctx context.Context, t files.RecoveryTicket, evidence files.RecoveryEvidence) error {
	for _, id := range []string{t.Upload.File.TenantID, t.Upload.File.ID, t.Upload.AttemptID, t.JobID, t.LeaseToken, t.OwnerID} {
		if !directoryUUIDPattern.MatchString(id) {
			return files.ErrLeaseLost
		}
	}
	_, e := fileTransaction(ctx, s, func(tx pgx.Tx) (bool, error) {
		tenant, file := t.Upload.File.TenantID, t.Upload.File.ID
		m, e := scanFile(tx.QueryRow(ctx, "SELECT "+fileSelect+" FROM file_objects WHERE tenant_id=$1 AND id=$2 FOR UPDATE", tenant, file))
		if e != nil {
			return false, e
		}
		if m.State != files.StateAllocated {
			return false, files.ErrLeaseLost
		}
		u, e := scanAttempt(tx.QueryRow(ctx, "SELECT "+attemptSelect+" FROM file_upload_attempts WHERE tenant_id=$1 AND file_id=$2 AND id=$3 FOR UPDATE", tenant, file, t.Upload.AttemptID), m)
		if e != nil {
			return false, files.ErrLeaseLost
		}
		var job, token, owner *string
		var expiry *time.Time
		var lookups int
		e = tx.QueryRow(ctx, "SELECT recovery_job_id::text,recovery_lease_token::text,recovery_owner_id::text,recovery_lease_expires_at,lookup_attempts FROM file_upload_attempts WHERE tenant_id=$1 AND id=$2", tenant, u.AttemptID).Scan(&job, &token, &owner, &expiry, &lookups)
		if e != nil {
			return false, e
		}
		at, e := fileClock(ctx, tx)
		if e != nil {
			return false, e
		}
		if job == nil || token == nil || owner == nil || expiry == nil || *job != t.JobID || *token != t.LeaseToken || *owner != t.OwnerID || !at.Before(*expiry) || u.Phase != files.UploadRecoveryPending {
			return false, files.ErrLeaseLost
		}
		phase, reason, outcome := files.UploadRecoveryPending, evidence.ReasonCode, "error"
		var version any
		var next any
		if evidence.Resolved {
			if !uploadVersionValid(evidence.VersionID) || u.Measurement == nil || *u.Measurement != evidence.Measurement {
				return false, files.ErrUploadConflict
			}
			if u.ObjectVersionID != "" && u.ObjectVersionID != evidence.VersionID {
				return false, files.ErrUploadConflict
			}
			phase = files.UploadRecovered
			reason = "recovery_resolved"
			outcome = "allow"
			version = evidence.VersionID
		} else {
			if !slices.Contains([]string{"recovery_pending", "recovery_ambiguous", "recovery_mismatch", "object_read_failed", "audit_unavailable"}, reason) {
				return false, files.ErrInvalidMetadata
			}
			if lookups < 3 {
				delay := 10 * time.Second
				if lookups == 2 {
					delay = 30 * time.Second
				}
				next = at.Add(delay)
			}
			if u.ObjectVersionID != "" {
				version = u.ObjectVersionID
			}
		}
		_, e = tx.Exec(ctx, `UPDATE file_upload_attempts SET phase=$3,object_version_id=$4,last_reason_code=$5,next_lookup_at=$6,recovery_job_id=NULL,recovery_lease_token=NULL,recovery_owner_id=NULL,recovery_lease_expires_at=NULL,updated_at=$7 WHERE tenant_id=$1 AND id=$2`, tenant, u.AttemptID, phase, version, reason, next, at)
		if e != nil {
			return false, e
		}
		if e = auditFileWorker(ctx, tx, tenant, file, t.OwnerID, t.JobID, "upload_recovery_complete", reason, outcome, at); e != nil {
			return false, e
		}
		fresh, e := fileClock(ctx, tx)
		if e != nil {
			return false, e
		}
		if !fresh.Before(*expiry) {
			return false, files.ErrLeaseLost
		}
		return true, nil
	})
	return e
}
