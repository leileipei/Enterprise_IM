package retention

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

func (w Worker) processBatch(ctx context.Context, tenantID string, size int) (BatchResult, error) {
	tx, err := w.DB.Begin(ctx)
	if err != nil {
		return BatchResult{}, err
	}
	defer rollback(tx)
	var days int
	err = tx.QueryRow(ctx, `SELECT message_body_retention_days FROM tenants
 WHERE id=$1 AND status='active' FOR SHARE`, tenantID).Scan(&days)
	if errors.Is(err, pgx.ErrNoRows) {
		return BatchResult{}, nil
	}
	if err != nil {
		return BatchResult{}, err
	}
	var candidateTime time.Time
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&candidateTime); err != nil {
		return BatchResult{}, err
	}
	var conversationID string
	// This cutoff only selects a candidate. The final cutoff and hold check are
	// taken in fresh statements after the conversation lock has been acquired.
	err = tx.QueryRow(ctx, `SELECT c.id::text FROM conversations c
 WHERE c.tenant_id=$1
 AND EXISTS (SELECT 1 FROM messages m WHERE m.tenant_id=c.tenant_id
   AND m.conversation_id=c.id AND m.body_cleared_at IS NULL
   AND m.accepted_at<=$2)
 AND NOT EXISTS (SELECT 1 FROM conversation_legal_holds h
   WHERE h.tenant_id=c.tenant_id AND h.conversation_id=c.id AND h.released_at IS NULL)
 ORDER BY c.id LIMIT 1 FOR UPDATE OF c SKIP LOCKED`, tenantID, candidateTime.Add(-time.Duration(days)*24*time.Hour)).Scan(&conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return BatchResult{}, nil
	}
	if err != nil {
		return BatchResult{}, err
	}
	var held bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM conversation_legal_holds
 WHERE tenant_id=$1 AND conversation_id=$2 AND released_at IS NULL)`, tenantID, conversationID).Scan(&held)
	if err != nil {
		return BatchResult{}, err
	}
	if held {
		return BatchResult{}, nil
	}
	at, err := w.batchTime(ctx, tx)
	if err != nil {
		return BatchResult{}, err
	}
	result := BatchResult{TenantID: tenantID, ConversationID: conversationID, RetentionDays: days,
		ClearedAt: at, CutoffAt: at.Add(-time.Duration(days) * 24 * time.Hour)}
	err = tx.QueryRow(ctx, `WITH candidates AS (
 SELECT id FROM messages WHERE tenant_id=$1 AND conversation_id=$2
 AND body_cleared_at IS NULL AND accepted_at<=$3
 ORDER BY accepted_at,seq LIMIT $5 FOR UPDATE
 ), cleared AS (
 UPDATE messages m SET text_body=NULL,body_cleared_at=$4
 FROM candidates c WHERE m.id=c.id AND m.tenant_id=$1 AND m.conversation_id=$2
 AND m.body_cleared_at IS NULL AND m.accepted_at<=$3 RETURNING m.seq
 ) SELECT count(*)::integer,COALESCE(min(seq),0),COALESCE(max(seq),0) FROM cleared`,
		tenantID, conversationID, result.CutoffAt, at, size).Scan(&result.ClearedCount, &result.FirstSeq, &result.LastSeq)
	if err != nil {
		return BatchResult{}, err
	}
	if result.ClearedCount == 0 {
		return BatchResult{}, nil
	}
	err = tx.QueryRow(ctx, `INSERT INTO message_body_clear_batches
 (tenant_id,conversation_id,retention_days,cutoff_at,cleared_at,first_seq,last_seq,cleared_count)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id::text`, tenantID, conversationID, days,
		result.CutoffAt, at, result.FirstSeq, result.LastSeq, result.ClearedCount).Scan(&result.BatchID)
	if err != nil {
		return BatchResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BatchResult{}, err
	}
	return result, nil
}
