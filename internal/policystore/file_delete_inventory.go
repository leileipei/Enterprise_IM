package policystore

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/filecleanup"
	"github.com/leileipei/Enterprise_IM/internal/files"
)

func inventoryVersionSource(ctx context.Context, tx pgx.Tx, m files.Metadata, v filecleanup.Version) (bool, error) {
	if v.AttemptID == "" {
		return v.VersionID == m.ObjectVersionID && m.ObjectVersionID != "", nil
	}
	var phase, version string
	e := tx.QueryRow(ctx, `SELECT phase,COALESCE(object_version_id,'') FROM file_upload_attempts WHERE tenant_id=$1 AND file_id=$2 AND id=$3 FOR SHARE NOWAIT`, m.TenantID, m.ID, v.AttemptID).Scan(&phase, &version)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	return phase == "sealed" && version == v.VersionID, nil
}
func (s Service) RecordFileDeleteInventory(ctx context.Context, t filecleanup.Ticket, in filecleanup.Inventory) error {
	if e := filecleanup.ValidateInventory(t, in); e != nil {
		return e
	}
	key := "tenants/" + t.TenantID + "/files/" + t.FileID
	if in.NextKey != "" && (!strings.HasPrefix(in.NextKey, key) || len(in.NextKey) > 2048) {
		return filecleanup.ErrIncomplete
	}
	blocked, e := cleanupTransaction(ctx, s, func(tx pgx.Tx) (bool, error) {
		j, m, p, at, e := currentDeleteJob(ctx, tx, t)
		if e != nil {
			return false, e
		}
		if !in.Exhausted && j.NextKey != "" && (in.NextKey < j.NextKey || in.NextKey == j.NextKey && in.NextVersion == j.NextVersion) {
			return false, filecleanup.ErrIncomplete
		}
		held, e := deleteHeld(ctx, tx, m)
		if e != nil {
			return false, e
		}
		if held || !p.CleanupEnabled || p.Version != t.PolicyVersion {
			return false, filecleanup.ErrBlocked
		}
		var unresolved bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM file_delete_versions WHERE job_id=$1 AND phase IN ('committed','uncertain'))`, t.JobID).Scan(&unresolved); e != nil {
			return false, e
		}
		if unresolved {
			return false, filecleanup.ErrBlocked
		}
		reason, e := deleteObligation(ctx, tx, m)
		if e != nil {
			return false, e
		}
		if reason == "" && in.Reason != "" && in.Reason != "inventory_incomplete" {
			reason = in.Reason
		}
		if reason == "" {
			for _, v := range in.Versions {
				safe, e := inventoryVersionSource(ctx, tx, m, v)
				if e != nil {
					return false, e
				}
				if !safe {
					reason = "unknown_version"
					break
				}
				var priorAttempt, phase string
				e = tx.QueryRow(ctx, `SELECT COALESCE(attempt_id::text,''),phase FROM file_delete_versions WHERE tenant_id=$1 AND job_id=$2 AND object_version_id=$3 FOR UPDATE NOWAIT`, t.TenantID, t.JobID, v.VersionID).Scan(&priorAttempt, &phase)
				if e != nil && !errors.Is(e, pgx.ErrNoRows) {
					return false, e
				}
				if e == nil && (priorAttempt != v.AttemptID || phase == "absent") {
					reason = "unknown_version"
					break
				}
			}
		}
		if reason != "" {
			_, e = tx.Exec(ctx, `UPDATE file_delete_jobs SET phase='blocked',reason_code=$2,source_safe=false,inventory_exhausted=false,next_retry_at=$3,updated_at=$4 WHERE id=$1`, t.JobID, reason, deleteRetryAt(at, j.Attempts), at)
			if e != nil {
				return false, e
			}
			if e = auditFileWorker(ctx, tx, t.TenantID, t.FileID, t.OwnerID, t.JobID, "delete_inventory", reason, "error", at); e != nil {
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
		}
		for _, v := range in.Versions {
			var attempt any
			if v.AttemptID != "" {
				attempt = v.AttemptID
			}
			_, e = tx.Exec(ctx, `INSERT INTO file_delete_versions(tenant_id,file_id,conversation_id,job_id,object_version_id,attempt_id,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$7) ON CONFLICT(tenant_id,job_id,object_version_id) DO NOTHING`, t.TenantID, t.FileID, t.ConversationID, t.JobID, v.VersionID, attempt, at)
			if e != nil {
				return false, e
			}
		}
		// A previously sealed version must also be accounted for when it no longer
		// appears in storage; listing absence never silently drops its obligation.
		if in.Exhausted && m.ObjectVersionID != "" {
			_, e = tx.Exec(ctx, `INSERT INTO file_delete_versions(tenant_id,file_id,conversation_id,job_id,object_version_id,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$6) ON CONFLICT(tenant_id,job_id,object_version_id) DO NOTHING`, t.TenantID, t.FileID, t.ConversationID, t.JobID, m.ObjectVersionID, at)
			if e != nil {
				return false, e
			}
		}
		var dbReason any
		reason = "inventory_complete"
		if !in.Exhausted {
			reason = "inventory_incomplete"
			dbReason = reason
		}
		_, e = tx.Exec(ctx, `UPDATE file_delete_jobs SET phase='inventory',inventory_exhausted=$2,source_safe=true,next_key=$3,next_version=$4,reason_code=$5,next_retry_at=NULL,updated_at=$6 WHERE id=$1`, t.JobID, in.Exhausted, in.NextKey, in.NextVersion, dbReason, at)
		if e != nil {
			return false, e
		}
		if e = auditFileWorker(ctx, tx, t.TenantID, t.FileID, t.OwnerID, t.JobID, "delete_inventory", reason, "allow", at); e != nil {
			return false, e
		}
		fresh, e := fileClock(ctx, tx)
		if e != nil {
			return false, e
		}
		if !fresh.Before(t.LeaseExpiresAt) {
			return false, filecleanup.ErrLeaseLost
		}
		return false, nil
	})
	if e != nil {
		return e
	}
	if blocked {
		return filecleanup.ErrBlocked
	}
	return nil
}
func (s Service) GetFileDeleteInventory(ctx context.Context, t filecleanup.Ticket) (filecleanup.Inventory, error) {
	return cleanupTransaction(ctx, s, func(tx pgx.Tx) (filecleanup.Inventory, error) {
		j, _, _, _, e := currentDeleteJob(ctx, tx, t)
		if e != nil {
			return filecleanup.Inventory{}, e
		}
		in := filecleanup.Inventory{Exhausted: j.Exhausted, NextKey: j.NextKey, NextVersion: j.NextVersion, Reason: j.Reason}
		// Settled versions stay in the database as evidence. Fetch only the next
		// bounded set of obligations; finalization checks every persisted row.
		rows, e := tx.Query(ctx, `SELECT object_version_id,COALESCE(attempt_id::text,'') FROM file_delete_versions WHERE tenant_id=$1 AND job_id=$2 AND phase<>'absent' ORDER BY created_at,object_version_id LIMIT 100`, t.TenantID, t.JobID)
		if e != nil {
			return in, e
		}
		for rows.Next() {
			var v filecleanup.Version
			if e = rows.Scan(&v.VersionID, &v.AttemptID); e != nil {
				rows.Close()
				return in, e
			}
			in.Versions = append(in.Versions, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return in, e
		}
		at, e := fileClock(ctx, tx)
		if e != nil {
			return in, e
		}
		if !at.Before(t.LeaseExpiresAt) {
			return in, filecleanup.ErrLeaseLost
		}
		return in, nil
	})
}
