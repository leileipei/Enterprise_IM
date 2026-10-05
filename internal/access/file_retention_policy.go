package access

import (
	"context"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/files"
)

type FileRetentionPolicyRecord struct {
	Policy                                            files.RetentionPolicy
	ApprovalReference, ActorUserID, ActorMembershipID string
	UpdatedAt                                         time.Time
}
type FileRetentionPolicyChange struct {
	Policy            files.RetentionPolicy
	ExpectedVersion   int64
	ApprovalReference string
}
type FileRetentionPolicyHistoryPage struct {
	History    []FileRetentionPolicyRecord
	NextCursor string
}

const fileRetentionPolicyColumns = `file_retention_days,cleanup_enabled,version,COALESCE(approval_reference,''),COALESCE(actor_user_id::text,''),COALESCE(acting_membership_id::text,''),updated_at`

func scanFileRetentionPolicy(row pgx.Row) (FileRetentionPolicyRecord, error) {
	var p FileRetentionPolicyRecord
	e := row.Scan(&p.Policy.Days, &p.Policy.CleanupEnabled, &p.Policy.Version, &p.ApprovalReference, &p.ActorUserID, &p.ActorMembershipID, &p.UpdatedAt)
	return p, e
}
func parseFileRetentionCursor(cursor, tenant string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	if !strings.HasPrefix(cursor, "file-retention-v1.") {
		return 0, files.ErrInvalidRetentionPolicy
	}
	return parseRetentionHistoryCursor(strings.TrimPrefix(cursor, "file-retention-v1."), tenant)
}
func (s Service) GetFileRetentionPolicy(ctx context.Context, id TrustedIdentity) (FileRetentionPolicyRecord, error) {
	return filePolicyTransaction(ctx, s, id, true, "file_retention_policy_read", func(tx pgx.Tx, _ time.Time) (FileRetentionPolicyRecord, error) {
		return scanFileRetentionPolicy(tx.QueryRow(ctx, `SELECT `+fileRetentionPolicyColumns+` FROM tenant_file_retention_policy WHERE tenant_id=$1 FOR SHARE`, id.TenantID))
	})
}
func (s Service) SetFileRetentionPolicy(ctx context.Context, id TrustedIdentity, c FileRetentionPolicyChange) (FileRetentionPolicyRecord, error) {
	p, e := files.NormalizeRetentionPolicy(c.Policy)
	if e != nil || c.ExpectedVersion < 0 || len(c.ApprovalReference) < 1 || len(c.ApprovalReference) > 128 || !utf8.ValidString(c.ApprovalReference) || strings.TrimSpace(c.ApprovalReference) != c.ApprovalReference || strings.IndexFunc(c.ApprovalReference, unicode.IsControl) >= 0 {
		return FileRetentionPolicyRecord{}, files.ErrInvalidRetentionPolicy
	}
	return filePolicyTransaction(ctx, s, id, true, "file_retention_policy_update", func(tx pgx.Tx, _ time.Time) (FileRetentionPolicyRecord, error) {
		old, e := scanFileRetentionPolicy(tx.QueryRow(ctx, `SELECT `+fileRetentionPolicyColumns+` FROM tenant_file_retention_policy WHERE tenant_id=$1 FOR UPDATE`, id.TenantID))
		if e != nil {
			return FileRetentionPolicyRecord{}, e
		}
		at, e := s.filePolicyGrant(ctx, tx, id, true)
		if e != nil {
			return FileRetentionPolicyRecord{}, e
		}
		if old.Policy.Version != c.ExpectedVersion || old.Policy.Version == math.MaxInt64 {
			return FileRetentionPolicyRecord{}, ErrConflict
		}
		at, e = s.filePolicyGrant(ctx, tx, id, true)
		if e != nil {
			return FileRetentionPolicyRecord{}, e
		}
		p.Version = old.Policy.Version + 1
		value := FileRetentionPolicyRecord{Policy: p, ApprovalReference: c.ApprovalReference, ActorUserID: id.UserID, ActorMembershipID: id.ActingMembershipID, UpdatedAt: at}
		_, e = tx.Exec(ctx, `UPDATE tenant_file_retention_policy SET file_retention_days=$2,cleanup_enabled=$3,version=$4,approval_reference=$5,actor_user_id=$6,acting_membership_id=$7,updated_at=$8 WHERE tenant_id=$1`, id.TenantID, p.Days, p.CleanupEnabled, p.Version, c.ApprovalReference, id.UserID, id.ActingMembershipID, at)
		if e != nil {
			return FileRetentionPolicyRecord{}, e
		}
		_, e = tx.Exec(ctx, `INSERT INTO tenant_file_retention_policy_history(tenant_id,file_retention_days,cleanup_enabled,version,approval_reference,actor_user_id,acting_membership_id,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id.TenantID, p.Days, p.CleanupEnabled, p.Version, c.ApprovalReference, id.UserID, id.ActingMembershipID, at)
		return value, e
	})
}
func (s Service) ListFileRetentionPolicyHistory(ctx context.Context, id TrustedIdentity, cursor string, limit int) (FileRetentionPolicyHistoryPage, error) {
	before, e := parseFileRetentionCursor(cursor, id.TenantID)
	if e != nil || limit < 1 || limit > 100 {
		return FileRetentionPolicyHistoryPage{}, files.ErrInvalidRetentionPolicy
	}
	return filePolicyTransaction(ctx, s, id, true, "file_retention_policy_history_list", func(tx pgx.Tx, _ time.Time) (FileRetentionPolicyHistoryPage, error) {
		current, e := scanFileRetentionPolicy(tx.QueryRow(ctx, `SELECT `+fileRetentionPolicyColumns+` FROM tenant_file_retention_policy WHERE tenant_id=$1 FOR SHARE`, id.TenantID))
		if e != nil {
			return FileRetentionPolicyHistoryPage{}, e
		}
		rows, e := tx.Query(ctx, `SELECT `+fileRetentionPolicyColumns+` FROM tenant_file_retention_policy_history WHERE tenant_id=$1 AND version<=$2 AND ($3::bigint=0 OR version<$3) ORDER BY version DESC LIMIT $4`, id.TenantID, current.Policy.Version, before, limit+1)
		if e != nil {
			return FileRetentionPolicyHistoryPage{}, e
		}
		history := make([]FileRetentionPolicyRecord, 0, limit+1)
		for rows.Next() {
			p, e := scanFileRetentionPolicy(rows)
			if e != nil {
				rows.Close()
				return FileRetentionPolicyHistoryPage{}, e
			}
			history = append(history, p)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return FileRetentionPolicyHistoryPage{}, e
		}
		page := FileRetentionPolicyHistoryPage{History: history}
		if len(history) > limit {
			page.History = history[:limit]
			page.NextCursor = "file-retention-v1." + encodeFilePolicyCursor(id.TenantID, page.History[limit-1].Policy.Version)
		}
		return page, nil
	})
}
