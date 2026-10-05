package policystore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"slices"
	"strings"
	"time"
)

func scanEvent(ctx context.Context, tx pgx.Tx, m files.Metadata, from files.State, job string, at time.Time) error {
	reason, e := files.TransitionReason(from, m.State)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO file_lifecycle_events(tenant_id,file_id,state_version,from_state,to_state,reason_code,occurred_at,actor_kind,worker_job_id) VALUES($1,$2,$3,$4,$5,$6,$7,'worker',$8)`, m.TenantID, m.ID, m.StateVersion, from, m.State, reason, at, job)
	return e
}
func scanRetryAt(at time.Time, attempt int) any {
	if attempt >= 3 {
		return nil
	}
	delay := 10 * time.Second
	if attempt == 2 {
		delay = 30 * time.Second
	}
	return at.Add(delay)
}
func (s Service) ClaimFileScan(ctx context.Context, ownerID string) (files.ScanTicket, bool, error) {
	var zero files.ScanTicket
	if !directoryUUIDPattern.MatchString(ownerID) {
		return zero, false, files.ErrInvalidMetadata
	}
	ownerID = strings.ToLower(ownerID)
	out, e := fileTransaction(ctx, s, func(tx pgx.Tx) (files.ScanTicket, error) {
		at, e := fileClock(ctx, tx)
		if e != nil {
			return zero, e
		}
		var tenant, id string
		e = tx.QueryRow(ctx, `SELECT f.tenant_id::text,f.id::text FROM file_objects f WHERE f.state='uploaded' OR (f.state='scan_failed' AND EXISTS(SELECT 1 FROM file_scan_jobs j WHERE j.tenant_id=f.tenant_id AND j.file_id=f.id AND j.id=f.scan_job_id AND j.status IN ('completed','expired') AND j.attempt<3 AND j.next_retry_at<=$1)) ORDER BY f.updated_at,f.id LIMIT 1 FOR UPDATE OF f SKIP LOCKED`, at).Scan(&tenant, &id)
		if errors.Is(e, pgx.ErrNoRows) {
			return zero, nil
		}
		if e != nil {
			return zero, e
		}
		m, e := scanFile(tx.QueryRow(ctx, "SELECT "+fileSelect+" FROM file_objects WHERE tenant_id=$1 AND id=$2", tenant, id))
		if e != nil {
			return zero, e
		}
		if files.ValidateMetadata(m) != nil {
			return zero, files.ErrDependencyUnavailable
		}
		var prior int
		if e = tx.QueryRow(ctx, "SELECT COALESCE(MAX(attempt),0) FROM file_scan_jobs WHERE tenant_id=$1 AND file_id=$2", tenant, id).Scan(&prior); e != nil {
			return zero, e
		}
		if prior >= 3 {
			return zero, nil
		}
		job, token, e := fileNewIDs(ctx, tx)
		if e != nil {
			return zero, e
		}
		at, e = fileClock(ctx, tx)
		if e != nil {
			return zero, e
		}
		from := m.State
		expiry := at.Add(120 * time.Second)
		m, e = scanFile(tx.QueryRow(ctx, `UPDATE file_objects SET state='scanning',state_version=state_version+1,scan_job_id=$3,updated_at=$4 WHERE tenant_id=$1 AND id=$2 RETURNING `+fileSelect, tenant, id, job, at))
		if e != nil {
			return zero, e
		}
		_, e = tx.Exec(ctx, `INSERT INTO file_scan_jobs(id,tenant_id,file_id,claim_version,sealed_sha256,attempt,status,lease_token,owner_id,lease_expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,'running',$7,$8,$9,$10,$10)`, job, tenant, id, m.StateVersion, m.SHA256, prior+1, token, ownerID, expiry, at)
		if e != nil {
			return zero, e
		}
		if e = scanEvent(ctx, tx, m, from, job, at); e != nil {
			return zero, e
		}
		reason, _ := files.TransitionReason(from, m.State)
		if e = auditFileWorker(ctx, tx, tenant, id, ownerID, job, "scan_claim", string(reason), "allow", at); e != nil {
			return zero, e
		}
		fresh, e := fileClock(ctx, tx)
		if e != nil {
			return zero, e
		}
		if !fresh.Before(expiry) {
			return zero, files.ErrLeaseLost
		}
		return files.ScanTicket{File: m, JobID: job, LeaseToken: token, OwnerID: ownerID, LeaseExpiresAt: expiry, ClaimVersion: m.StateVersion, Attempt: prior + 1}, nil
	})
	return out, out.JobID != "" && e == nil, e
}
func currentScan(ctx context.Context, tx pgx.Tx, t files.ScanTicket) (files.ScanTicket, time.Time, error) {
	var zero files.ScanTicket
	for _, id := range []string{t.File.TenantID, t.File.ID, t.JobID, t.LeaseToken, t.OwnerID} {
		if !directoryUUIDPattern.MatchString(id) {
			return zero, time.Time{}, files.ErrLeaseLost
		}
	}
	m, e := scanFile(tx.QueryRow(ctx, "SELECT "+fileSelect+" FROM file_objects WHERE tenant_id=$1 AND id=$2 FOR UPDATE", t.File.TenantID, t.File.ID))
	if errors.Is(e, pgx.ErrNoRows) {
		return zero, time.Time{}, files.ErrLeaseLost
	}
	if e != nil {
		return zero, time.Time{}, e
	}
	u := files.ScanTicket{File: m}
	var status string
	var hash []byte
	e = tx.QueryRow(ctx, `SELECT id::text,lease_token::text,owner_id::text,lease_expires_at,claim_version,attempt,status,sealed_sha256 FROM file_scan_jobs WHERE tenant_id=$1 AND file_id=$2 AND id=$3 FOR UPDATE`, m.TenantID, m.ID, t.JobID).Scan(&u.JobID, &u.LeaseToken, &u.OwnerID, &u.LeaseExpiresAt, &u.ClaimVersion, &u.Attempt, &status, &hash)
	if errors.Is(e, pgx.ErrNoRows) {
		return zero, time.Time{}, files.ErrLeaseLost
	}
	if e != nil {
		return zero, time.Time{}, e
	}
	at, e := fileClock(ctx, tx)
	if e != nil {
		return zero, at, e
	}
	if status != "running" || m.State != files.StateScanning || m.ScanJobID != u.JobID || m.StateVersion != u.ClaimVersion || u.ClaimVersion != t.ClaimVersion || u.OwnerID != t.OwnerID || u.LeaseToken != t.LeaseToken || !bytes.Equal(hash, m.SHA256) || !at.Before(u.LeaseExpiresAt) {
		return zero, at, files.ErrLeaseLost
	}
	return u, at, nil
}
func (s Service) RenewFileScan(ctx context.Context, t files.ScanTicket) (files.ScanTicket, error) {
	return fileTransaction(ctx, s, func(tx pgx.Tx) (files.ScanTicket, error) {
		u, at, e := currentScan(ctx, tx, t)
		if e != nil {
			return u, e
		}
		u.LeaseExpiresAt = at.Add(120 * time.Second)
		_, e = tx.Exec(ctx, "UPDATE file_scan_jobs SET lease_expires_at=$3,updated_at=$4 WHERE tenant_id=$1 AND id=$2", u.File.TenantID, u.JobID, u.LeaseExpiresAt, at)
		if e != nil {
			return u, e
		}
		fresh, e := fileClock(ctx, tx)
		if e != nil {
			return u, e
		}
		if !fresh.Before(u.LeaseExpiresAt) {
			return u, files.ErrLeaseLost
		}
		return u, nil
	})
}
func validScanDecision(d files.ScanDecision, hash []byte) error {
	switch d.State {
	case files.StateScanFailed:
		if d.Engine != "" || d.DefinitionVersion != "" || d.SHA256 != [32]byte{} || !slices.Contains([]string{"scan_error", "object_read_failed", "object_mismatch", "scanner_unavailable", "scanner_protocol_error", "scanner_limits", "definitions_stale", "structure_invalid"}, d.ReasonCode) {
			return files.ErrInvalidMetadata
		}
	case files.StateReady, files.StateRejected:
		if !uploadVersionValid(d.Engine) || !uploadVersionValid(d.DefinitionVersion) || len(d.Engine) > 128 || len(d.DefinitionVersion) > 128 || !bytes.Equal(d.SHA256[:], hash) {
			return files.ErrInvalidMetadata
		}
		if d.State == files.StateReady && d.ReasonCode != "scan_clean" || d.State == files.StateRejected && !slices.Contains([]string{"scan_rejected", "structure_invalid", "encrypted_file", "type_not_allowed"}, d.ReasonCode) {
			return files.ErrInvalidMetadata
		}
	default:
		return files.ErrInvalidMetadata
	}
	return nil
}
func (s Service) CompleteFileScan(ctx context.Context, t files.ScanTicket, d files.ScanDecision) error {
	if !directoryUUIDPattern.MatchString(t.File.TenantID) {
		return files.ErrLeaseLost
	}
	_, e := fileTransaction(ctx, s, func(tx pgx.Tx) (bool, error) {
		// Configuration precedes file locks, matching uploader/admin lock order.
		var policy files.UploadPolicy
		e := tx.QueryRow(ctx, `SELECT enabled,max_size_bytes,allowed_media_types,version FROM tenant_file_upload_policy WHERE tenant_id=$1 FOR SHARE`, t.File.TenantID).Scan(&policy.Enabled, &policy.MaxSizeBytes, &policy.AllowedMediaTypes, &policy.Version)
		if errors.Is(e, pgx.ErrNoRows) {
			return false, files.ErrLeaseLost
		}
		if e != nil {
			return false, e
		}
		u, at, e := currentScan(ctx, tx, t)
		if e != nil {
			return false, e
		}
		if e = validScanDecision(d, u.File.SHA256); e != nil {
			return false, e
		}
		decision := d
		if decision.State == files.StateReady {
			reason := ""
			if !policy.Enabled {
				reason = "upload_disabled"
			} else if !slices.Contains(policy.AllowedMediaTypes, u.File.DetectedMediaType) || !slices.Contains(policy.AllowedMediaTypes, u.File.DeclaredMediaType) {
				reason = "type_not_allowed"
			} else if u.File.DeclaredSizeBytes > policy.MaxSizeBytes {
				reason = "scan_rejected"
			}
			if reason != "" {
				decision.State = files.StateRejected
				decision.Engine = "policy/tenant-file-upload"
				decision.DefinitionVersion = fmt.Sprintf("version-%d/rules-v1", policy.Version)
				decision.ReasonCode = reason
			}
		}
		var engine, definition, scanned, hash any
		var next any
		outcome := "allow"
		if decision.State == files.StateScanFailed {
			next = scanRetryAt(at, u.Attempt)
			outcome = "error"
		} else {
			engine = decision.Engine
			definition = decision.DefinitionVersion
			scanned = at
			hash = u.File.SHA256
			if decision.State == files.StateRejected {
				outcome = "deny"
			}
		}
		m, e := scanFile(tx.QueryRow(ctx, `UPDATE file_objects SET state=$3,state_version=state_version+1,updated_at=$4,scan_engine=$5,scan_definition_version=$6,scanned_at=$7,scan_sha256=$8 WHERE tenant_id=$1 AND id=$2 RETURNING `+fileSelect, u.File.TenantID, u.File.ID, decision.State, at, engine, definition, scanned, hash))
		if e != nil {
			return false, e
		}
		_, e = tx.Exec(ctx, `UPDATE file_scan_jobs SET status='completed',result_state=$3,reason_code=$4,next_retry_at=$5,updated_at=$6 WHERE tenant_id=$1 AND id=$2`, m.TenantID, u.JobID, decision.State, decision.ReasonCode, next, at)
		if e != nil {
			return false, e
		}
		if e = scanEvent(ctx, tx, m, files.StateScanning, u.JobID, at); e != nil {
			return false, e
		}
		if e = auditFileWorker(ctx, tx, m.TenantID, m.ID, u.OwnerID, u.JobID, "scan_complete", decision.ReasonCode, outcome, at); e != nil {
			return false, e
		}
		fresh, e := fileClock(ctx, tx)
		if e != nil {
			return false, e
		}
		if !fresh.Before(u.LeaseExpiresAt) {
			return false, files.ErrLeaseLost
		}
		return true, nil
	})
	return e
}
func (s Service) RecoverExpiredFileScans(ctx context.Context, ownerID string, limit int) (int, error) {
	if !directoryUUIDPattern.MatchString(ownerID) || limit < 1 || limit > 100 {
		return 0, files.ErrInvalidMetadata
	}
	ownerID = strings.ToLower(ownerID)
	return fileTransaction(ctx, s, func(tx pgx.Tx) (int, error) {
		at, e := fileClock(ctx, tx)
		if e != nil {
			return 0, e
		}
		rows, e := tx.Query(ctx, `SELECT f.tenant_id::text,f.id::text,j.id::text,j.attempt FROM file_objects f JOIN file_scan_jobs j ON j.tenant_id=f.tenant_id AND j.file_id=f.id AND j.id=f.scan_job_id AND j.claim_version=f.state_version AND j.sealed_sha256=f.sha256 WHERE f.state='scanning' AND j.status='running' AND j.lease_expires_at<=$1 ORDER BY j.lease_expires_at,j.id LIMIT $2 FOR UPDATE OF f,j SKIP LOCKED`, at, limit)
		if e != nil {
			return 0, e
		}
		type item struct {
			tenant, file, job string
			attempt           int
		}
		var found []item
		for rows.Next() {
			var v item
			if e = rows.Scan(&v.tenant, &v.file, &v.job, &v.attempt); e != nil {
				rows.Close()
				return 0, e
			}
			found = append(found, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return 0, e
		}
		for _, v := range found {
			at, e = fileClock(ctx, tx)
			if e != nil {
				return 0, e
			}
			m, e := scanFile(tx.QueryRow(ctx, `UPDATE file_objects SET state='scan_failed',state_version=state_version+1,updated_at=$3 WHERE tenant_id=$1 AND id=$2 RETURNING `+fileSelect, v.tenant, v.file, at))
			if e != nil {
				return 0, e
			}
			_, e = tx.Exec(ctx, `UPDATE file_scan_jobs SET status='expired',result_state='scan_failed',reason_code='lease_lost',next_retry_at=$3,updated_at=$4 WHERE tenant_id=$1 AND id=$2`, v.tenant, v.job, scanRetryAt(at, v.attempt), at)
			if e != nil {
				return 0, e
			}
			if e = scanEvent(ctx, tx, m, files.StateScanning, v.job, at); e != nil {
				return 0, e
			}
			if e = auditFileWorker(ctx, tx, v.tenant, v.file, ownerID, v.job, "scan_expired", "lease_lost", "error", at); e != nil {
				return 0, e
			}
		}
		return len(found), nil
	})
}
