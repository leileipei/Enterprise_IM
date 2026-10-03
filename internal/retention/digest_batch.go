package retention

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var errDigestUpdateMismatch = errors.New("digest retirement update set mismatch")

func (w DigestWorker) processDigestBatch(ctx context.Context, tenantID string, size int) (DigestBatchResult, error) {
	tx, err := w.DB.Begin(ctx)
	if err != nil {
		return DigestBatchResult{}, err
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		return DigestBatchResult{}, err
	}
	var activeID string
	err = tx.QueryRow(ctx, "SELECT id::text FROM tenants WHERE id=$1 AND status='active' FOR SHARE", tenantID).Scan(&activeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DigestBatchResult{}, nil
	}
	if err != nil {
		return DigestBatchResult{}, err
	}
	var candidateAt time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&candidateAt); err != nil {
		return DigestBatchResult{}, err
	}
	var cid string
	err = tx.QueryRow(ctx, `SELECT c.id::text FROM conversations c WHERE c.tenant_id=$1
 AND EXISTS(SELECT 1 FROM message_idempotency i JOIN messages m ON m.tenant_id=i.tenant_id AND m.id=i.message_id
 WHERE i.tenant_id=c.tenant_id AND i.conversation_id=c.id AND i.digest_retired_at IS NULL
 AND m.digest_retired_at IS NULL AND m.text_body IS NULL AND m.body_cleared_at<=$2 AND i.expires_at<=$2)
 AND NOT EXISTS(SELECT 1 FROM conversation_legal_holds h WHERE h.tenant_id=c.tenant_id AND h.conversation_id=c.id AND h.released_at IS NULL)
 ORDER BY c.id LIMIT 1 FOR UPDATE OF c SKIP LOCKED`, tenantID, candidateAt).Scan(&cid)
	if errors.Is(err, pgx.ErrNoRows) {
		return DigestBatchResult{}, nil
	}
	if err != nil {
		return DigestBatchResult{}, err
	}
	var held bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_legal_holds WHERE tenant_id=$1 AND conversation_id=$2 AND released_at IS NULL)`, tenantID, cid).Scan(&held); err != nil {
		return DigestBatchResult{}, err
	}
	if held {
		return DigestBatchResult{}, nil
	}
	at, err := w.batchTime(ctx, tx)
	if err != nil {
		return DigestBatchResult{}, err
	}
	rows, err := tx.Query(ctx, `SELECT m.id::text FROM message_idempotency i JOIN messages m ON m.tenant_id=i.tenant_id AND m.id=i.message_id
 WHERE i.tenant_id=$1 AND i.conversation_id=$2 AND i.digest_retired_at IS NULL AND i.expires_at<=$3
 AND m.digest_retired_at IS NULL AND m.text_body IS NULL AND m.body_cleared_at<=$3
 ORDER BY i.expires_at,m.seq LIMIT $4`, tenantID, cid, at, size)
	if err != nil {
		return DigestBatchResult{}, err
	}
	candidates, err := digestIDs(rows)
	if err != nil {
		return DigestBatchResult{}, err
	}
	ids := make([]string, 0, len(candidates))
	result := DigestBatchResult{TenantID: tenantID, ConversationID: cid, RetiredAt: at}
	for _, id := range candidates {
		var seq int64
		var sender, client string
		err = tx.QueryRow(ctx, `SELECT seq,sender_user_id::text,client_msg_id::text FROM messages WHERE tenant_id=$1 AND conversation_id=$2 AND id=$3
 AND digest_retired_at IS NULL AND text_body IS NULL AND body_cleared_at<=$4 FOR UPDATE`, tenantID, cid, id, at).Scan(&seq, &sender, &client)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return DigestBatchResult{}, err
		}
		var expiry time.Time
		var retired *time.Time
		err = tx.QueryRow(ctx, `SELECT expires_at,digest_retired_at FROM message_idempotency WHERE tenant_id=$1 AND conversation_id=$2
 AND sender_user_id=$3 AND client_msg_id=$4 AND message_id=$5 FOR UPDATE`, tenantID, cid, sender, client, id).Scan(&expiry, &retired)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return DigestBatchResult{}, err
		}
		if retired != nil || expiry.After(at) {
			continue
		}
		ids = append(ids, id)
		if result.RetiredCount == 0 || seq < result.FirstSeq {
			result.FirstSeq = seq
		}
		if seq > result.LastSeq {
			result.LastSeq = seq
		}
		if result.RetiredCount == 0 || expiry.Before(result.MinExpiresAt) {
			result.MinExpiresAt = expiry
		}
		if result.RetiredCount == 0 || expiry.After(result.MaxExpiresAt) {
			result.MaxExpiresAt = expiry
		}
		result.RetiredCount++
	}
	if len(ids) == 0 {
		return DigestBatchResult{}, nil
	}
	rows, err = tx.Query(ctx, `UPDATE messages SET content_digest=NULL,digest_retired_at=$3 WHERE tenant_id=$1 AND id=ANY($2::uuid[])
 AND digest_retired_at IS NULL AND text_body IS NULL AND body_cleared_at<=$3 RETURNING id::text`, tenantID, ids, at)
	if err != nil {
		return DigestBatchResult{}, err
	}
	updated, err := digestIDs(rows)
	if err != nil {
		return DigestBatchResult{}, err
	}
	if !sameDigestIDs(ids, updated) {
		return DigestBatchResult{}, errDigestUpdateMismatch
	}
	rows, err = tx.Query(ctx, `UPDATE message_idempotency SET content_digest=NULL,digest_retired_at=$4 WHERE tenant_id=$1 AND conversation_id=$2
 AND message_id=ANY($3::uuid[]) AND digest_retired_at IS NULL AND expires_at<=$4 RETURNING message_id::text`, tenantID, cid, ids, at)
	if err != nil {
		return DigestBatchResult{}, err
	}
	updated, err = digestIDs(rows)
	if err != nil {
		return DigestBatchResult{}, err
	}
	if !sameDigestIDs(ids, updated) {
		return DigestBatchResult{}, errDigestUpdateMismatch
	}
	err = tx.QueryRow(ctx, `INSERT INTO message_digest_retirement_batches(tenant_id,conversation_id,retired_at,retired_count,first_seq,last_seq,min_expires_at,max_expires_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id::text`, tenantID, cid, at, result.RetiredCount, result.FirstSeq, result.LastSeq, result.MinExpiresAt, result.MaxExpiresAt).Scan(&result.BatchID)
	if err != nil {
		return DigestBatchResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return DigestBatchResult{}, err
	}
	return result, nil
}
func digestIDs(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func sameDigestIDs(expected, actual []string) bool {
	if len(expected) != len(actual) {
		return false
	}
	set := make(map[string]bool, len(expected))
	for _, id := range expected {
		set[id] = true
	}
	for _, id := range actual {
		if !set[id] {
			return false
		}
		delete(set, id)
	}
	return len(set) == 0
}
