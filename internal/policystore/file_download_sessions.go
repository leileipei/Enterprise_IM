package policystore

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"github.com/leileipei/Enterprise_IM/internal/files"
)

var _ filedownload.Repository = Service{}

type downloadSession struct {
	ID, Tenant, File, Conversation, Message, User, Membership, Owner, Token, Phase string
	Expected, Written                                                              int64
	Deadline, Lease                                                                time.Time
	Reason                                                                         string
	Acked                                                                          bool
}

const downloadSessionColumns = `id::text,tenant_id::text,file_id::text,conversation_id::text,message_id::text,requester_user_id::text,source_membership_id::text,owner_id::text,lease_token::text,phase,expected_bytes,bytes_written,deadline,lease_expires_at,COALESCE(reason_code,''),audit_acked`

func scanDownloadSession(row pgx.Row) (downloadSession, error) {
	var d downloadSession
	e := row.Scan(&d.ID, &d.Tenant, &d.File, &d.Conversation, &d.Message, &d.User, &d.Membership, &d.Owner, &d.Token, &d.Phase, &d.Expected, &d.Written, &d.Deadline, &d.Lease, &d.Reason, &d.Acked)
	return d, e
}
func downloadTransaction[T any](ctx context.Context, s Service, fn func(pgx.Tx) (T, error)) (T, error) {
	v, e := fileTransaction(ctx, s, fn)
	var pe *pgconn.PgError
	if errors.As(e, &pe) && (pe.Code == "40P01" || pe.Code == "55P03" || pe.Code == "23505") {
		return v, errors.Join(filedownload.ErrBusy, e)
	}
	if errors.Is(e, files.ErrDependencyUnavailable) {
		return v, errors.Join(filedownload.ErrUnavailable, e)
	}
	return v, e
}
func downloadSessionMatches(d downloadSession, t filedownload.Ticket) bool {
	return d.ID == t.SessionID && d.Tenant == t.Identity.TenantID && d.User == t.Identity.UserID && d.Membership == t.Identity.ActingMembershipID && d.File == t.File.ID && d.Conversation == t.File.ConversationID && d.Message == t.MessageID && d.Owner == t.OwnerID && d.Token == t.LeaseToken && d.Expected == *t.File.ActualSizeBytes && d.Deadline.Equal(t.Deadline) && d.Lease.Equal(t.LeaseExpiresAt)
}
func downloadSessionAt(ctx context.Context, tx pgx.Tx, t filedownload.Ticket, lock string) (downloadSession, error) {
	d, e := scanDownloadSession(tx.QueryRow(ctx, "SELECT "+downloadSessionColumns+" FROM file_download_sessions WHERE id=$1 "+lock, t.SessionID))
	if errors.Is(e, pgx.ErrNoRows) || e == nil && !downloadSessionMatches(d, t) {
		return d, filedownload.ErrNotFound
	}
	return d, e
}
func downloadLive(d downloadSession, at time.Time) bool {
	return !d.Acked && (d.Phase == "preparing" || d.Phase == "authorized") && at.Before(d.Deadline) && at.Before(d.Lease)
}
func sameDownloadSeal(a, b files.Metadata) bool {
	return a.ID == b.ID && a.ConversationID == b.ConversationID && a.StateVersion == b.StateVersion && a.ObjectVersionID == b.ObjectVersionID && bytes.Equal(a.SHA256, b.SHA256) && a.ActualSizeBytes != nil && b.ActualSizeBytes != nil && *a.ActualSizeBytes == *b.ActualSizeBytes
}
func (s Service) BeginFileDownload(ctx context.Context, id access.TrustedIdentity, fileID, ownerID string, deadline time.Time) (filedownload.Ticket, error) {
	var zero filedownload.Ticket
	id, e := fileIdentity(id)
	if e != nil {
		return zero, filedownload.ErrInvalidIdentity
	}
	if !directoryUUIDPattern.MatchString(ownerID) || deadline.IsZero() || deadline.Year() < 1 || deadline.Year() > 9999 {
		return zero, filedownload.ErrNotFound
	}
	ownerID = strings.ToLower(ownerID)
	return downloadTransaction(ctx, s, func(tx pgx.Tx) (filedownload.Ticket, error) {
		// Serializes the cross-node per-user admission before any conversation lock.
		var user string
		e := tx.QueryRow(ctx, "SELECT id::text FROM users WHERE tenant_id=$1 AND id=$2 FOR UPDATE NOWAIT", id.TenantID, id.UserID).Scan(&user)
		if errors.Is(e, pgx.ErrNoRows) {
			return zero, filedownload.ErrInvalidIdentity
		}
		if e != nil {
			return zero, e
		}
		m, mid, seq, at, e := authorizeFileDownloadTx(ctx, tx, id, fileID)
		if e != nil {
			return zero, e
		}
		if !at.Before(deadline) {
			return zero, filedownload.ErrBusy
		}
		if max := at.Add(60 * time.Second); deadline.After(max) {
			deadline = max
		}
		var phase string
		e = tx.QueryRow(ctx, "SELECT phase FROM file_download_sessions WHERE tenant_id=$1 AND requester_user_id=$2 AND file_id=$3 AND NOT audit_acked", id.TenantID, id.UserID, m.ID).Scan(&phase)
		if e == nil {
			if phase == "completed" || phase == "interrupted" || phase == "unknown" {
				return zero, filedownload.ErrAuditPending
			}
			return zero, filedownload.ErrBusy
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return zero, e
		}
		var active int
		if e = tx.QueryRow(ctx, "SELECT count(*) FROM file_download_sessions WHERE tenant_id=$1 AND requester_user_id=$2 AND phase IN ('preparing','authorized')", id.TenantID, id.UserID).Scan(&active); e != nil {
			return zero, e
		}
		if active >= 2 {
			return zero, filedownload.ErrLimit
		}
		session, token, e := fileNewIDs(ctx, tx)
		if e != nil {
			return zero, e
		}
		at, e = fileClock(ctx, tx)
		if e != nil {
			return zero, e
		}
		if !at.Before(deadline) {
			return zero, filedownload.ErrBusy
		}
		ticket := filedownload.Ticket{SessionID: session, OwnerID: ownerID, LeaseToken: token, Identity: id, File: m, MessageID: mid, MessageSeq: seq, Deadline: deadline.UTC(), LeaseExpiresAt: deadline.UTC()}
		if e = filedownload.ValidateTicket(ticket); e != nil {
			return zero, e
		}
		_, e = tx.Exec(ctx, `INSERT INTO file_download_sessions(id,tenant_id,file_id,conversation_id,message_id,requester_user_id,source_membership_id,owner_id,lease_token,phase,expected_bytes,created_at,updated_at,deadline,lease_expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'preparing',$10,$11,$11,$12,$12)`, session, id.TenantID, m.ID, m.ConversationID, mid, id.UserID, id.ActingMembershipID, ownerID, token, *m.ActualSizeBytes, at, deadline)
		if e != nil {
			return zero, e
		}
		_, _, _, fresh, e := authorizeFileDownloadTx(ctx, tx, id, m.ID)
		if e != nil {
			return zero, e
		}
		if !fresh.Before(deadline) {
			return zero, filedownload.ErrBusy
		}
		return ticket, nil
	})
}
func downloadCaller(id access.TrustedIdentity, t filedownload.Ticket) error {
	normalized, e := fileIdentity(id)
	if e != nil {
		return filedownload.ErrInvalidIdentity
	}
	if filedownload.ValidateTicket(t) != nil || normalized != t.Identity {
		return filedownload.ErrNotFound
	}
	return nil
}
func (s Service) AuthorizeFileDownload(ctx context.Context, id access.TrustedIdentity, t filedownload.Ticket) error {
	if e := downloadCaller(id, t); e != nil {
		return e
	}
	_, e := downloadTransaction(ctx, s, func(tx pgx.Tx) (bool, error) {
		m, mid, seq, at, e := authorizeFileDownloadTx(ctx, tx, t.Identity, t.File.ID)
		if e != nil {
			return false, e
		}
		if !sameDownloadSeal(m, t.File) || mid != t.MessageID || seq != t.MessageSeq {
			return false, filedownload.ErrNotFound
		}
		d, e := downloadSessionAt(ctx, tx, t, "FOR UPDATE")
		if e != nil {
			return false, e
		}
		if !downloadLive(d, at) {
			return false, filedownload.ErrBusy
		}
		if d.Phase == "authorized" {
			return true, nil
		}
		var auditID int64
		e = tx.QueryRow(ctx, `INSERT INTO audit_events(tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at) VALUES($1,$2,$3,'file_download_authorize','file',$4,'allow','download_authorized',$5) RETURNING id`, t.Identity.TenantID, t.Identity.UserID, t.Identity.ActingMembershipID, t.File.ID, at).Scan(&auditID)
		if e != nil {
			return false, errors.Join(ErrAuditUnavailable, e)
		}
		_, _, _, at, e = authorizeFileDownloadTx(ctx, tx, t.Identity, t.File.ID)
		if e != nil {
			return false, e
		}
		if !downloadLive(d, at) {
			return false, filedownload.ErrBusy
		}
		_, e = tx.Exec(ctx, "UPDATE file_download_sessions SET phase='authorized',authorized_audit_id=$2,updated_at=$3 WHERE id=$1", t.SessionID, auditID, at)
		return e == nil, e
	})
	return e
}
func (s Service) CheckFileDownload(ctx context.Context, id access.TrustedIdentity, t filedownload.Ticket) error {
	if e := downloadCaller(id, t); e != nil {
		return e
	}
	_, e := downloadTransaction(ctx, s, func(tx pgx.Tx) (bool, error) {
		m, mid, seq, _, e := authorizeFileDownloadTx(ctx, tx, t.Identity, t.File.ID)
		if e != nil {
			return false, e
		}
		if !sameDownloadSeal(m, t.File) || mid != t.MessageID || seq != t.MessageSeq {
			return false, filedownload.ErrNotFound
		}
		d, e := downloadSessionAt(ctx, tx, t, "FOR SHARE")
		if e != nil {
			return false, e
		}
		_, _, _, _, e = authorizeFileDownloadTx(ctx, tx, t.Identity, t.File.ID)
		if e != nil {
			return false, e
		}
		at, e := fileClock(ctx, tx)
		if e != nil {
			return false, e
		}
		if d.Phase != "authorized" || !downloadLive(d, at) {
			return false, filedownload.ErrBusy
		}
		return true, nil
	})
	return e
}

// Machine settlement retains the original ticket's identity even after its
// appointment is revoked. The fact records observed output, not a fresh grant.
func (s Service) FinishFileDownload(ctx context.Context, t filedownload.Ticket, r filedownload.Result) error {
	if e := filedownload.ValidateTicket(t); e != nil {
		return filedownload.ErrNotFound
	}
	if e := filedownload.ValidateResult(*t.File.ActualSizeBytes, r); e != nil {
		return e
	}
	_, e := downloadTransaction(ctx, s, func(tx pgx.Tx) (bool, error) {
		if e := lockDownloadMachineOrigin(ctx, tx, t.Identity.TenantID, t.File.ConversationID, t.File.ID); e != nil {
			return false, e
		}
		d, e := downloadSessionAt(ctx, tx, t, "FOR UPDATE")
		if e != nil {
			return false, e
		}
		if d.Phase == "completed" || d.Phase == "interrupted" || d.Phase == "unknown" {
			if d.Phase == r.Outcome && d.Reason == r.Reason && d.Written == r.BytesWritten {
				return true, nil
			}
			return false, filedownload.ErrBusy
		}
		at, e := fileClock(ctx, tx)
		if e != nil {
			return false, e
		}
		if r.Outcome == "completed" && (d.Phase != "authorized" || !downloadLive(d, at)) {
			return false, filedownload.ErrBusy
		}
		if e = finishDownloadTx(ctx, tx, d, r, at); e != nil {
			return false, e
		}
		return true, nil
	})
	return e
}
func lockDownloadMachineOrigin(ctx context.Context, tx pgx.Tx, tenant, conversation, file string) error {
	var value string
	if e := tx.QueryRow(ctx, "SELECT id::text FROM conversations WHERE tenant_id=$1 AND id=$2 FOR UPDATE NOWAIT", tenant, conversation).Scan(&value); e != nil {
		return e
	}
	return tx.QueryRow(ctx, "SELECT id::text FROM file_objects WHERE tenant_id=$1 AND id=$2 FOR SHARE NOWAIT", tenant, file).Scan(&value)
}
func finishDownloadTx(ctx context.Context, tx pgx.Tx, d downloadSession, r filedownload.Result, at time.Time) error {
	if _, e := tx.Exec(ctx, "UPDATE file_download_sessions SET phase=$2,reason_code=$3,bytes_written=$4,updated_at=$5 WHERE id=$1", d.ID, r.Outcome, r.Reason, r.BytesWritten, at); e != nil {
		return e
	}
	_, e := tx.Exec(ctx, `INSERT INTO file_download_terminal_events(session_id,tenant_id,file_id,outcome,reason_code,bytes_written,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, d.ID, d.Tenant, d.File, r.Outcome, r.Reason, r.BytesWritten, at)
	return e
}
