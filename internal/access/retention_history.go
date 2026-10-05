package access

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var ErrInvalidRetentionHistoryQuery = errors.New("invalid retention history query")

type RetentionPolicyHistoryPage struct {
	History    []RetentionPolicy
	NextCursor string
}

type retentionHistoryCursor struct {
	TenantID      string `json:"t"`
	BeforeVersion int64  `json:"v"`
}

func parseRetentionHistoryCursor(value, tenantID string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	if len(value) > 1024 {
		return 0, ErrInvalidRetentionHistoryQuery
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) > 768 {
		return 0, ErrInvalidRetentionHistoryQuery
	}
	var c retentionHistoryCursor
	if json.Unmarshal(raw, &c) != nil || c.TenantID != strings.ToLower(tenantID) || c.BeforeVersion < 1 {
		return 0, ErrInvalidRetentionHistoryQuery
	}
	canonical, _ := json.Marshal(c)
	if base64.RawURLEncoding.EncodeToString(canonical) != value {
		return 0, ErrInvalidRetentionHistoryQuery
	}
	return c.BeforeVersion, nil
}

// Reads immutable approvals newest version first, with a tenant-bound keyset cursor.
func (s Service) ListRetentionPolicyHistory(ctx context.Context, id TrustedIdentity, cursor string, limit int) (RetentionPolicyHistoryPage, error) {
	if limit < 1 || limit > 100 {
		return RetentionPolicyHistoryPage{}, ErrInvalidRetentionHistoryQuery
	}
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return RetentionPolicyHistoryPage{}, ErrInvalidIdentity
	}
	before, err := parseRetentionHistoryCursor(cursor, id.TenantID)
	if err != nil {
		return RetentionPolicyHistoryPage{}, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return RetentionPolicyHistoryPage{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		return RetentionPolicyHistoryPage{}, err
	}
	// Preserve the membership-before-tenant order used by writes and EndMembership.
	if err = lockLegalHoldActor(ctx, tx, id); err != nil {
		return RetentionPolicyHistoryPage{}, err
	}
	const action = "retention_policy_history_list"
	checkGrant := func(at time.Time) error {
		g, err := s.resolve(ctx, tx, id, at)
		if errors.Is(err, ErrInvalidIdentity) {
			return deny(ctx, tx, id, action, "tenant", id.TenantID, "invalid_identity", at, ErrInvalidIdentity)
		}
		if err != nil {
			return err
		}
		if !g.all {
			return deny(ctx, tx, id, action, "tenant", id.TenantID, "not_group_admin", at, ErrNotFound)
		}
		return nil
	}
	at := s.currentTime()
	if err = checkGrant(at); err != nil {
		return RetentionPolicyHistoryPage{}, err
	}
	var currentVersion int64
	if err = tx.QueryRow(ctx, `SELECT retention_version FROM tenants WHERE id=$1 FOR SHARE`, id.TenantID).Scan(&currentVersion); err != nil {
		return RetentionPolicyHistoryPage{}, err
	}
	at = s.currentTime()
	if err = checkGrant(at); err != nil {
		return RetentionPolicyHistoryPage{}, err
	}
	rows, err := tx.Query(ctx, `SELECT message_body_retention_days,version,approval_reference,approved_by_user_id::text,approved_at
 FROM tenant_retention_policy_history WHERE tenant_id=$1 AND version<=$2 AND ($3::bigint=0 OR version<$3)
 ORDER BY version DESC LIMIT $4`, id.TenantID, currentVersion, before, limit+1)
	if err != nil {
		return RetentionPolicyHistoryPage{}, err
	}
	history := make([]RetentionPolicy, 0, limit+1)
	for rows.Next() {
		var p RetentionPolicy
		if err = rows.Scan(&p.MessageBodyDays, &p.Version, &p.ApprovalReference, &p.ApprovedByUserID, &p.ApprovedAt); err != nil {
			rows.Close()
			return RetentionPolicyHistoryPage{}, err
		}
		history = append(history, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return RetentionPolicyHistoryPage{}, err
	}
	at = s.currentTime()
	if err = checkGrant(at); err != nil {
		return RetentionPolicyHistoryPage{}, err
	}
	page := RetentionPolicyHistoryPage{History: history}
	if len(history) > limit {
		page.History = history[:limit]
		raw, _ := json.Marshal(retentionHistoryCursor{TenantID: strings.ToLower(id.TenantID), BeforeVersion: page.History[limit-1].Version})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	if err = audit(ctx, tx, id, action, "tenant", id.TenantID, "allow", "listed_approval_history", at); err != nil {
		return RetentionPolicyHistoryPage{}, errors.Join(ErrAuditUnavailable, err)
	}
	// Audit I/O can also outlive a deadline. Roll back its allow event on expiry.
	fresh := s.currentTime()
	grant, err := s.resolve(ctx, tx, id, fresh)
	if err != nil {
		return RetentionPolicyHistoryPage{}, err
	}
	if !grant.all {
		return RetentionPolicyHistoryPage{}, ErrNotFound
	}
	if err = tx.Commit(ctx); err != nil {
		return RetentionPolicyHistoryPage{}, err
	}
	return page, nil
}
