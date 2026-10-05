package access

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidRetentionQuery = errors.New("invalid retention batch query")

type RetentionBatch struct {
	ID, ConversationID, Kind             string
	ProcessedAt                          time.Time
	ProcessedCount                       int
	FirstSeq, LastSeq                    int64
	RetentionDays                        *int
	CutoffAt, MinExpiresAt, MaxExpiresAt *time.Time
}

type RetentionBatchPage struct {
	Batches    []RetentionBatch
	NextCursor string
}

func (s Service) ListRetentionBatches(ctx context.Context, id TrustedIdentity, conversationID, kind, cursor string, limit int) (RetentionBatchPage, error) {
	if !legalHoldUUIDPattern.MatchString(conversationID) || (kind != "body" && kind != "digest") || limit < 1 || limit > 500 {
		return RetentionBatchPage{}, ErrInvalidRetentionQuery
	}
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return RetentionBatchPage{}, ErrInvalidIdentity
	}
	conversationID = strings.ToLower(conversationID)
	cursorAt, cursorID, err := parseRetentionBatchCursor(cursor, id.TenantID, conversationID, kind)
	if err != nil {
		return RetentionBatchPage{}, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return RetentionBatchPage{}, err
	}
	defer tx.Rollback(ctx)
	// A connection's default isolation must not preserve authorization from an old snapshot.
	if _, err = tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		return RetentionBatchPage{}, err
	}
	if err = lockLegalHoldActor(ctx, tx, id); err != nil {
		return RetentionBatchPage{}, err
	}
	const action = "retention_batches_list"
	at := s.currentTime()
	if err = s.legalHoldGrant(ctx, tx, id, conversationID, action, at); err != nil {
		return RetentionBatchPage{}, err
	}
	var found string
	err = tx.QueryRow(ctx, `SELECT id::text FROM conversations WHERE tenant_id=$1 AND id=$2 FOR SHARE`, id.TenantID, conversationID).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return RetentionBatchPage{}, deny(ctx, tx, id, action, "conversation", conversationID, "conversation_unavailable", at, ErrNotFound)
	}
	if err != nil {
		return RetentionBatchPage{}, err
	}
	// Recheck deadlines after any conversation or authorization lock wait.
	at = s.currentTime()
	if err = s.legalHoldGrant(ctx, tx, id, conversationID, action, at); err != nil {
		return RetentionBatchPage{}, err
	}
	query := `SELECT id::text,cleared_at,cleared_count,first_seq,last_seq,
 retention_days,cutoff_at,NULL::timestamptz,NULL::timestamptz
 FROM message_body_clear_batches WHERE tenant_id=$1 AND conversation_id=$2
 AND ($3::timestamptz IS NULL OR (cleared_at,id)<($3,$4::uuid))
 ORDER BY cleared_at DESC,id DESC LIMIT $5`
	if kind == "digest" {
		query = `SELECT id::text,retired_at,retired_count,first_seq,last_seq,
 NULL::integer,NULL::timestamptz,min_expires_at,max_expires_at
 FROM message_digest_retirement_batches WHERE tenant_id=$1 AND conversation_id=$2
 AND ($3::timestamptz IS NULL OR (retired_at,id)<($3,$4::uuid))
 ORDER BY retired_at DESC,id DESC LIMIT $5`
	}
	rows, err := tx.Query(ctx, query, id.TenantID, conversationID, nullableLegalHoldTime(cursorAt), nullableLegalHoldID(cursorID), limit+1)
	if err != nil {
		return RetentionBatchPage{}, err
	}
	batches := make([]RetentionBatch, 0, limit+1)
	for rows.Next() {
		b := RetentionBatch{ConversationID: conversationID, Kind: kind}
		if err = rows.Scan(&b.ID, &b.ProcessedAt, &b.ProcessedCount, &b.FirstSeq, &b.LastSeq, &b.RetentionDays, &b.CutoffAt, &b.MinExpiresAt, &b.MaxExpiresAt); err != nil {
			rows.Close()
			return RetentionBatchPage{}, err
		}
		batches = append(batches, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return RetentionBatchPage{}, err
	}
	// Queries and result transfer can also outlive an authorization deadline.
	at = s.currentTime()
	if err = s.legalHoldGrant(ctx, tx, id, conversationID, action, at); err != nil {
		return RetentionBatchPage{}, err
	}
	page := RetentionBatchPage{Batches: batches}
	if len(batches) > limit {
		page.Batches = batches[:limit]
		page.NextCursor = makeRetentionBatchCursor(id.TenantID, conversationID, kind, page.Batches[limit-1])
	}
	if err = audit(ctx, tx, id, action, "conversation", conversationID, "allow", "listed_"+kind, at); err != nil {
		return RetentionBatchPage{}, errors.Join(ErrAuditUnavailable, err)
	}
	if err = tx.Commit(ctx); err != nil {
		return RetentionBatchPage{}, err
	}
	return page, nil
}
