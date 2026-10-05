package access

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/files"
)

type FileUploadPolicyRecord struct {
	Policy                                            files.UploadPolicy
	ApprovalReference, ActorUserID, ActorMembershipID string
	UpdatedAt                                         time.Time
}
type FileUploadPolicyChange struct {
	Policy            files.UploadPolicy
	ExpectedVersion   int64
	ApprovalReference string
}
type FileUploadPolicyHistoryPage struct {
	History    []FileUploadPolicyRecord
	NextCursor string
}

const filePolicyColumns = `enabled,max_size_bytes,allowed_media_types,upload_ttl_seconds,tenant_storage_budget_bytes,version,COALESCE(approval_reference,''),COALESCE(actor_user_id::text,''),COALESCE(acting_membership_id::text,''),updated_at`

func scanFilePolicy(row pgx.Row) (FileUploadPolicyRecord, error) {
	var p FileUploadPolicyRecord
	e := row.Scan(&p.Policy.Enabled, &p.Policy.MaxSizeBytes, &p.Policy.AllowedMediaTypes, &p.Policy.UploadTTLSeconds, &p.Policy.TenantStorageBudgetBytes, &p.Policy.Version, &p.ApprovalReference, &p.ActorUserID, &p.ActorMembershipID, &p.UpdatedAt)
	return p, e
}
func (s Service) filePolicyGrant(ctx context.Context, tx pgx.Tx, id TrustedIdentity, group bool) (time.Time, error) {
	var at time.Time
	if e := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at); e != nil {
		return at, e
	}
	g, e := s.resolve(ctx, tx, id, at)
	if e != nil {
		return at, e
	}
	if group && !g.all {
		return at, ErrNotFound
	}
	return at, nil
}

// Every retry is a new short DB transaction. No object I/O belongs in fn.
func filePolicyTransaction[T any](ctx context.Context, s Service, id TrustedIdentity, group bool, action string, fn func(pgx.Tx, time.Time) (T, error)) (T, error) {
	var zero T
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return zero, ErrInvalidIdentity
	}
	for n := 0; n < 3; n++ {
		value, e := filePolicyTransactionOnce(ctx, s, id, group, action, fn)
		var pe *pgconn.PgError
		if !errors.As(e, &pe) || (pe.Code != "40P01" && pe.Code != "55P03") {
			return value, e
		}
		if n == 2 {
			return zero, errors.Join(files.ErrDependencyUnavailable, e)
		}
	}
	return zero, files.ErrDependencyUnavailable
}
func filePolicyTransactionOnce[T any](ctx context.Context, s Service, id TrustedIdentity, group bool, action string, fn func(pgx.Tx, time.Time) (T, error)) (T, error) {
	var zero T
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return zero, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL READ COMMITTED`); e != nil {
		return zero, e
	}
	if e = lockLegalHoldActor(ctx, tx, id); e != nil {
		return zero, e
	}
	at, e := s.filePolicyGrant(ctx, tx, id, group)
	if errors.Is(e, ErrNotFound) {
		return zero, deny(ctx, tx, id, action, "tenant", id.TenantID, "not_group_admin", at, e)
	}
	if e != nil {
		return zero, e
	}
	value, e := fn(tx, at)
	if errors.Is(e, ErrConflict) || errors.Is(e, files.ErrStorageBudgetExceeded) {
		fresh, authErr := s.filePolicyGrant(ctx, tx, id, group)
		if authErr != nil {
			return zero, authErr
		}
		reason := "version_conflict"
		if errors.Is(e, files.ErrStorageBudgetExceeded) {
			reason = "storage_budget_exceeded"
		}
		return zero, deny(ctx, tx, id, action, "tenant", id.TenantID, reason, fresh, e)
	}
	if e != nil {
		return zero, e
	}
	at, e = s.filePolicyGrant(ctx, tx, id, group)
	if e != nil {
		return zero, e
	}
	reason := "file_upload_policy"
	switch action {
	case "file_retention_policy_read", "file_retention_policy_update", "file_retention_policy_history_list":
		reason = "file_retention_policy"
	}
	if e = audit(ctx, tx, id, action, "tenant", id.TenantID, "allow", reason, at); e != nil {
		return zero, errors.Join(ErrAuditUnavailable, e)
	}
	if _, e = s.filePolicyGrant(ctx, tx, id, group); e != nil {
		return zero, e
	}
	if e = tx.Commit(ctx); e != nil {
		return zero, e
	}
	return value, nil
}
func (s Service) GetFileUploadPolicy(ctx context.Context, id TrustedIdentity) (FileUploadPolicyRecord, error) {
	return filePolicyTransaction(ctx, s, id, true, "file_upload_policy_read", func(tx pgx.Tx, _ time.Time) (FileUploadPolicyRecord, error) {
		return scanFilePolicy(tx.QueryRow(ctx, `SELECT `+filePolicyColumns+` FROM tenant_file_upload_policy WHERE tenant_id=$1 FOR SHARE`, id.TenantID))
	})
}
func (s Service) GetEffectiveFileUploadPolicy(ctx context.Context, id TrustedIdentity) (files.UploadPolicy, error) {
	return filePolicyTransaction(ctx, s, id, false, "file_upload_policy_effective_read", func(tx pgx.Tx, _ time.Time) (files.UploadPolicy, error) {
		p, e := scanFilePolicy(tx.QueryRow(ctx, `SELECT `+filePolicyColumns+` FROM tenant_file_upload_policy WHERE tenant_id=$1 FOR SHARE`, id.TenantID))
		return p.Policy, e
	})
}
func (s Service) SetFileUploadPolicy(ctx context.Context, id TrustedIdentity, c FileUploadPolicyChange) (FileUploadPolicyRecord, error) {
	p, e := files.NormalizeUploadPolicy(c.Policy)
	if e != nil || c.ExpectedVersion < 0 || len(c.ApprovalReference) < 1 || len(c.ApprovalReference) > 128 || !utf8.ValidString(c.ApprovalReference) || strings.TrimSpace(c.ApprovalReference) != c.ApprovalReference || strings.IndexFunc(c.ApprovalReference, unicode.IsControl) >= 0 {
		return FileUploadPolicyRecord{}, files.ErrInvalidUploadPolicy
	}
	return filePolicyTransaction(ctx, s, id, true, "file_upload_policy_update", func(tx pgx.Tx, _ time.Time) (FileUploadPolicyRecord, error) {
		old, e := scanFilePolicy(tx.QueryRow(ctx, `SELECT `+filePolicyColumns+` FROM tenant_file_upload_policy WHERE tenant_id=$1 FOR UPDATE`, id.TenantID))
		if e != nil {
			return FileUploadPolicyRecord{}, e
		}
		at, e := s.filePolicyGrant(ctx, tx, id, true)
		if e != nil {
			return FileUploadPolicyRecord{}, e
		}
		if old.Policy.Version != c.ExpectedVersion || old.Policy.Version == math.MaxInt64 {
			return FileUploadPolicyRecord{}, ErrConflict
		}
		var used int64
		if e = tx.QueryRow(ctx, `SELECT COALESCE(sum(declared_size_bytes),0) FROM file_objects WHERE tenant_id=$1 AND state<>'deleted'`, id.TenantID).Scan(&used); e != nil {
			return FileUploadPolicyRecord{}, e
		}
		if p.TenantStorageBudgetBytes < used {
			return FileUploadPolicyRecord{}, files.ErrStorageBudgetExceeded
		}
		at, e = s.filePolicyGrant(ctx, tx, id, true)
		if e != nil {
			return FileUploadPolicyRecord{}, e
		}
		p.Version = old.Policy.Version + 1
		value := FileUploadPolicyRecord{Policy: p, ApprovalReference: c.ApprovalReference, ActorUserID: id.UserID, ActorMembershipID: id.ActingMembershipID, UpdatedAt: at}
		_, e = tx.Exec(ctx, `UPDATE tenant_file_upload_policy SET enabled=$2,max_size_bytes=$3,allowed_media_types=$4,upload_ttl_seconds=$5,tenant_storage_budget_bytes=$6,version=$7,approval_reference=$8,actor_user_id=$9,acting_membership_id=$10,updated_at=$11 WHERE tenant_id=$1`, id.TenantID, p.Enabled, p.MaxSizeBytes, p.AllowedMediaTypes, p.UploadTTLSeconds, p.TenantStorageBudgetBytes, p.Version, c.ApprovalReference, id.UserID, id.ActingMembershipID, at)
		if e != nil {
			return FileUploadPolicyRecord{}, e
		}
		_, e = tx.Exec(ctx, `INSERT INTO tenant_file_upload_policy_history(tenant_id,enabled,max_size_bytes,allowed_media_types,upload_ttl_seconds,tenant_storage_budget_bytes,version,approval_reference,actor_user_id,acting_membership_id,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, id.TenantID, p.Enabled, p.MaxSizeBytes, p.AllowedMediaTypes, p.UploadTTLSeconds, p.TenantStorageBudgetBytes, p.Version, c.ApprovalReference, id.UserID, id.ActingMembershipID, at)
		return value, e
	})
}
func (s Service) ListFileUploadPolicyHistory(ctx context.Context, id TrustedIdentity, cursor string, limit int) (FileUploadPolicyHistoryPage, error) {
	before, e := parseRetentionHistoryCursor(cursor, id.TenantID)
	if e != nil || limit < 1 || limit > 100 {
		return FileUploadPolicyHistoryPage{}, files.ErrInvalidUploadPolicy
	}
	return filePolicyTransaction(ctx, s, id, true, "file_upload_policy_history_list", func(tx pgx.Tx, _ time.Time) (FileUploadPolicyHistoryPage, error) {
		current, e := scanFilePolicy(tx.QueryRow(ctx, `SELECT `+filePolicyColumns+` FROM tenant_file_upload_policy WHERE tenant_id=$1 FOR SHARE`, id.TenantID))
		if e != nil {
			return FileUploadPolicyHistoryPage{}, e
		}
		rows, e := tx.Query(ctx, `SELECT `+filePolicyColumns+` FROM tenant_file_upload_policy_history WHERE tenant_id=$1 AND version<=$2 AND ($3::bigint=0 OR version<$3) ORDER BY version DESC LIMIT $4`, id.TenantID, current.Policy.Version, before, limit+1)
		if e != nil {
			return FileUploadPolicyHistoryPage{}, e
		}
		history := make([]FileUploadPolicyRecord, 0, limit+1)
		for rows.Next() {
			p, e := scanFilePolicy(rows)
			if e != nil {
				rows.Close()
				return FileUploadPolicyHistoryPage{}, e
			}
			history = append(history, p)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return FileUploadPolicyHistoryPage{}, e
		}
		page := FileUploadPolicyHistoryPage{History: history}
		if len(history) > limit {
			page.History = history[:limit]
			page.NextCursor = encodeFilePolicyCursor(id.TenantID, page.History[limit-1].Policy.Version)
		}
		return page, nil
	})
}

func encodeFilePolicyCursor(tenant string, before int64) string {
	b, _ := json.Marshal(retentionHistoryCursor{TenantID: tenant, BeforeVersion: before})
	return base64.RawURLEncoding.EncodeToString(b)
}
