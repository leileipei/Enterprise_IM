package policystore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/filecleanup"
	"github.com/leileipei/Enterprise_IM/internal/files"
)

type deleteJob struct {
	Ticket                              filecleanup.Ticket
	Phase, Reason, NextKey, NextVersion string
	Exhausted, Safe                     bool
	Attempts                            int64
}

const deleteJobColumns = `id::text,tenant_id::text,file_id::text,conversation_id::text,owner_id::text,lease_token::text,policy_version,expected_state_version,lease_expires_at,phase,COALESCE(reason_code,''),next_key,next_version,inventory_exhausted,source_safe,attempts`

func scanDeleteJob(row pgx.Row) (deleteJob, error) {
	var j deleteJob
	t := &j.Ticket
	e := row.Scan(&t.JobID, &t.TenantID, &t.FileID, &t.ConversationID, &t.OwnerID, &t.LeaseToken, &t.PolicyVersion, &t.StateVersion, &t.LeaseExpiresAt, &j.Phase, &j.Reason, &j.NextKey, &j.NextVersion, &j.Exhausted, &j.Safe, &j.Attempts)
	return j, e
}
func cleanupTransaction[T any](ctx context.Context, s Service, fn func(pgx.Tx) (T, error)) (T, error) {
	v, e := fileTransaction(ctx, s, fn)
	var pe *pgconn.PgError
	if errors.As(e, &pe) && (pe.Code == "40P01" || pe.Code == "55P03") {
		return v, errors.Join(filecleanup.ErrLeaseLost, e)
	}
	return v, e
}

// Workers use a machine origin, never a forged human membership. Every writer
// follows tenant -> conversation -> retention policy -> file -> job/version.
func deleteOrigin(ctx context.Context, tx pgx.Tx, tenant, conversation, file string) (files.Metadata, files.RetentionPolicy, time.Time, error) {
	var m files.Metadata
	var p files.RetentionPolicy
	var id string
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"SELECT id::text FROM tenants WHERE id=$1 FOR SHARE NOWAIT", []any{tenant}},
		{"SELECT id::text FROM conversations WHERE tenant_id=$1 AND id=$2 FOR UPDATE NOWAIT", []any{tenant, conversation}},
	} {
		if e := tx.QueryRow(ctx, q.sql, q.args...).Scan(&id); e != nil {
			return m, p, time.Time{}, e
		}
	}
	if e := tx.QueryRow(ctx, `SELECT file_retention_days,cleanup_enabled,version FROM tenant_file_retention_policy WHERE tenant_id=$1 FOR SHARE NOWAIT`, tenant).Scan(&p.Days, &p.CleanupEnabled, &p.Version); e != nil {
		return m, p, time.Time{}, e
	}
	m, e := scanFile(tx.QueryRow(ctx, "SELECT "+fileSelect+" FROM file_objects WHERE tenant_id=$1 AND conversation_id=$2 AND id=$3 FOR UPDATE NOWAIT", tenant, conversation, file))
	if e != nil {
		return m, p, time.Time{}, e
	}
	if files.ValidateMetadata(m) != nil {
		return m, p, time.Time{}, files.ErrDependencyUnavailable
	}
	at, e := fileClock(ctx, tx)
	return m, p, at, e
}
func deleteHeld(ctx context.Context, tx pgx.Tx, m files.Metadata) (bool, error) {
	var held bool
	e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_legal_holds WHERE tenant_id=$1 AND conversation_id=$2 AND released_at IS NULL)`, m.TenantID, m.ConversationID).Scan(&held)
	return held, e
}
func deleteDue(ctx context.Context, tx pgx.Tx, m files.Metadata, p files.RetentionPolicy, at time.Time) (bool, error) {
	var accepted time.Time
	e := tx.QueryRow(ctx, `SELECT msg.accepted_at FROM message_attachments a JOIN messages msg ON msg.tenant_id=a.tenant_id AND msg.conversation_id=a.conversation_id AND msg.id=a.message_id WHERE a.tenant_id=$1 AND a.file_id=$2`, m.TenantID, m.ID).Scan(&accepted)
	if errors.Is(e, pgx.ErrNoRows) {
		return !at.Before(m.UploadExpiresAt), nil
	}
	if e != nil {
		return false, e
	}
	expires, e := files.FileExpiresAt(accepted, p.Days)
	return e == nil && !at.Before(expires), e
}
func deleteObligation(ctx context.Context, tx pgx.Tx, m files.Metadata) (string, error) {
	var uploads, inflight bool
	e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM file_upload_attempts WHERE tenant_id=$1 AND file_id=$2 AND phase NOT IN ('sealed','receive_failed')),EXISTS(SELECT 1 FROM file_scan_jobs WHERE tenant_id=$1 AND file_id=$2 AND status='running') OR EXISTS(SELECT 1 FROM file_download_sessions WHERE tenant_id=$1 AND file_id=$2 AND NOT audit_acked)`, m.TenantID, m.ID).Scan(&uploads, &inflight)
	if e != nil {
		return "", e
	}
	if uploads {
		return "unknown_upload", nil
	}
	if inflight {
		return "in_flight", nil
	}
	return "", nil
}
func deleteRetryAt(at time.Time, n int64) time.Time {
	delay := time.Second
	for i := int64(1); i < n && delay < 60*time.Second; i++ {
		delay *= 2
	}
	if delay > 60*time.Second {
		delay = 60 * time.Second
	}
	return at.Add(delay)
}
func (s Service) ClaimFileDelete(ctx context.Context, ownerID string) (filecleanup.Ticket, bool, error) {
	var zero filecleanup.Ticket
	if !directoryUUIDPattern.MatchString(ownerID) {
		return zero, false, filecleanup.ErrLeaseLost
	}
	ownerID = strings.ToLower(ownerID)
	ticket, e := cleanupTransaction(ctx, s, func(tx pgx.Tx) (filecleanup.Ticket, error) {
		// A bounded selection is rechecked after all locks. No storage I/O occurs here.
		rows, e := tx.Query(ctx, `SELECT f.tenant_id::text,f.conversation_id::text,f.id::text FROM file_objects f JOIN tenant_file_retention_policy p ON p.tenant_id=f.tenant_id LEFT JOIN file_delete_jobs j ON j.tenant_id=f.tenant_id AND j.file_id=f.id WHERE p.cleanup_enabled AND f.state<>'deleted' AND NOT EXISTS(SELECT 1 FROM conversation_legal_holds h WHERE h.tenant_id=f.tenant_id AND h.conversation_id=f.conversation_id AND h.released_at IS NULL) AND NOT EXISTS(SELECT 1 FROM file_delete_versions v WHERE v.tenant_id=f.tenant_id AND v.conversation_id=f.conversation_id AND v.phase IN ('committed','uncertain')) AND (j.id IS NULL OR (j.phase<>'finished' AND (j.owner_id=$1 OR j.lease_expires_at<=clock_timestamp()) AND (j.next_retry_at IS NULL OR j.next_retry_at<=clock_timestamp()))) AND (f.state='delete_pending' OR EXISTS(SELECT 1 FROM message_attachments a JOIN messages msg ON msg.tenant_id=a.tenant_id AND msg.id=a.message_id WHERE a.tenant_id=f.tenant_id AND a.file_id=f.id AND msg.accepted_at+p.file_retention_days*interval '24 hours'<=clock_timestamp()) OR (NOT EXISTS(SELECT 1 FROM message_attachments a WHERE a.tenant_id=f.tenant_id AND a.file_id=f.id) AND f.upload_expires_at<=clock_timestamp())) ORDER BY f.updated_at,f.id LIMIT 20`, ownerID)
		if e != nil {
			return zero, e
		}
		type candidate struct{ tenant, convo, file string }
		var candidates []candidate
		for rows.Next() {
			var c candidate
			if e = rows.Scan(&c.tenant, &c.convo, &c.file); e != nil {
				rows.Close()
				return zero, e
			}
			candidates = append(candidates, c)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return zero, e
		}
		for _, c := range candidates {
			m, p, at, e := deleteOrigin(ctx, tx, c.tenant, c.convo, c.file)
			if e != nil {
				return zero, e
			}
			held, e := deleteHeld(ctx, tx, m)
			if e != nil {
				return zero, e
			}
			if held || !p.CleanupEnabled || m.State == files.StateDeleted {
				continue
			}
			if m.State != files.StateDeletePending {
				due, e := deleteDue(ctx, tx, m, p, at)
				if e != nil {
					return zero, e
				}
				if !due {
					continue
				}
			}
			j, e := scanDeleteJob(tx.QueryRow(ctx, "SELECT "+deleteJobColumns+" FROM file_delete_jobs WHERE tenant_id=$1 AND file_id=$2 FOR UPDATE NOWAIT", m.TenantID, m.ID))
			exists := e == nil
			if e != nil && !errors.Is(e, pgx.ErrNoRows) {
				return zero, e
			}
			if exists && (j.Phase == "finished" || j.Ticket.OwnerID != ownerID && at.Before(j.Ticket.LeaseExpiresAt)) {
				continue
			}
			var unresolved bool
			if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM file_delete_versions WHERE tenant_id=$1 AND conversation_id=$2 AND phase IN ('committed','uncertain'))`, m.TenantID, m.ConversationID).Scan(&unresolved); e != nil {
				return zero, e
			}
			if unresolved {
				continue
			}
			job, token, e := fileNewIDs(ctx, tx)
			if e != nil {
				return zero, e
			}
			if exists {
				job = j.Ticket.JobID
			}
			at, e = fileClock(ctx, tx)
			if e != nil {
				return zero, e
			}
			if !exists && m.State != files.StateDeletePending {
				from := m.State
				m, e = scanFile(tx.QueryRow(ctx, `UPDATE file_objects SET state='delete_pending',state_version=state_version+1,deletion_requested_at=$3,updated_at=$3 WHERE tenant_id=$1 AND id=$2 RETURNING `+fileSelect, m.TenantID, m.ID, at))
				if e != nil {
					return zero, e
				}
				if e = scanEvent(ctx, tx, m, from, job, at); e != nil {
					return zero, e
				}
			}
			expiry := at.Add(120 * time.Second)
			if !exists {
				_, e = tx.Exec(ctx, `INSERT INTO file_delete_jobs(id,tenant_id,file_id,conversation_id,policy_version,expected_state_version,owner_id,lease_token,lease_expires_at,created_at,updated_at,attempts) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10,1)`, job, m.TenantID, m.ID, m.ConversationID, p.Version, m.StateVersion, ownerID, token, expiry, at)
			} else {
				if m.State != files.StateDeletePending || m.StateVersion != j.Ticket.StateVersion {
					return zero, filecleanup.ErrLeaseLost
				}
				_, e = tx.Exec(ctx, `UPDATE file_delete_jobs SET owner_id=$2,lease_token=$3,lease_expires_at=$4,updated_at=$5,policy_version=$6,attempts=CASE WHEN attempts<9223372036854775807 THEN attempts+1 ELSE attempts END,next_retry_at=NULL WHERE id=$1`, job, ownerID, token, expiry, at, p.Version)
			}
			if e != nil {
				return zero, e
			}
			retryCount := j.Attempts
			if retryCount < 9223372036854775807 {
				retryCount++
			}
			reason, e := deleteObligation(ctx, tx, m)
			if e != nil {
				return zero, e
			}

			// Resolved database obligations require a new inventory; storage
			// ambiguity and delete markers remain quarantined.
			if reason == "" && exists && j.Phase == "blocked" && (j.Reason == "in_flight" || j.Reason == "unknown_upload") {
				_, e = tx.Exec(ctx, `UPDATE file_delete_jobs SET phase='pending',reason_code=NULL,source_safe=false,inventory_exhausted=false,next_key='',next_version='' WHERE id=$1`, job)
				if e != nil {
					return zero, e
				}
			}
			if reason != "" {
				_, e = tx.Exec(ctx, `UPDATE file_delete_jobs SET phase='blocked',reason_code=$2,source_safe=false,inventory_exhausted=false,next_retry_at=$3 WHERE id=$1`, job, reason, deleteRetryAt(at, retryCount))
				if e != nil {
					return zero, e
				}
			}
			if e = auditFileWorker(ctx, tx, m.TenantID, m.ID, ownerID, job, "delete_claim", "deletion_requested", "allow", at); e != nil {
				return zero, e
			}
			fresh, e := fileClock(ctx, tx)
			if e != nil {
				return zero, e
			}
			if !fresh.Before(expiry) {
				return zero, filecleanup.ErrLeaseLost
			}
			t := filecleanup.Ticket{JobID: job, TenantID: m.TenantID, FileID: m.ID, ConversationID: m.ConversationID, OwnerID: ownerID, LeaseToken: token, PolicyVersion: p.Version, StateVersion: m.StateVersion, LeaseExpiresAt: expiry}
			if e = filecleanup.ValidateTicket(t); e != nil {
				return zero, e
			}
			return t, nil
		}
		return zero, nil
	})
	return ticket, e == nil && ticket.JobID != "", e
}
func currentDeleteJob(ctx context.Context, tx pgx.Tx, t filecleanup.Ticket) (deleteJob, files.Metadata, files.RetentionPolicy, time.Time, error) {
	var j deleteJob
	var m files.Metadata
	var p files.RetentionPolicy
	if filecleanup.ValidateTicket(t) != nil {
		return j, m, p, time.Time{}, filecleanup.ErrLeaseLost
	}
	m, p, at, e := deleteOrigin(ctx, tx, t.TenantID, t.ConversationID, t.FileID)
	if e != nil {
		return j, m, p, at, e
	}
	j, e = scanDeleteJob(tx.QueryRow(ctx, "SELECT "+deleteJobColumns+" FROM file_delete_jobs WHERE id=$1 FOR UPDATE NOWAIT", t.JobID))
	if errors.Is(e, pgx.ErrNoRows) {
		return j, m, p, at, filecleanup.ErrLeaseLost
	}
	if e != nil {
		return j, m, p, at, e
	}
	at, e = fileClock(ctx, tx)
	if e != nil {
		return j, m, p, at, e
	}
	if j.Ticket != t || !at.Before(t.LeaseExpiresAt) || j.Phase == "finished" || m.State != files.StateDeletePending || m.StateVersion != t.StateVersion {
		return j, m, p, at, filecleanup.ErrLeaseLost
	}
	return j, m, p, at, nil
}
