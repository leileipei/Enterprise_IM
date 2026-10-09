package policystore

import (
	"context"
	"strings"
)

// Redis can only nominate a durable event. Pending events remain resolvable:
// XADD may succeed before the worker's database publication commit.
func (s Service) ResolveReceiptEvent(ctx context.Context, tenantID, conversationID, eventID string, revision int64) ([]string, error) {
	if s.DB == nil || revision < 1 || !directoryUUIDPattern.MatchString(tenantID) || !directoryUUIDPattern.MatchString(conversationID) || !directoryUUIDPattern.MatchString(eventID) {
		return nil, ErrInvalidReceiptRequest
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	rows, e := tx.Query(ctx, `SELECT t.target_user_id::text FROM receipt_outbox_events e JOIN receipt_outbox_targets t ON t.tenant_id=e.tenant_id AND t.event_id=e.id WHERE e.tenant_id=$1 AND e.conversation_id=$2 AND e.id=$3 AND e.revision=$4 AND e.event_type='receipt_changed' ORDER BY t.target_user_id`, strings.ToLower(tenantID), strings.ToLower(conversationID), strings.ToLower(eventID), revision)
	if e != nil {
		return nil, e
	}
	users := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return nil, e
		}
		users = append(users, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	return users, nil
}

// Removes only short-lived proof material. Established delivery/read evidence
// and device lifecycle rows are deliberately outside this operation.
func (s Service) CleanupExpiredSyncProofs(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 1000 {
		return 0, ErrInvalidReceiptRequest
	}
	if s.DB == nil {
		return 0, ErrForbidden
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback(ctx)
	tag, e := tx.Exec(ctx, `WITH expired AS(SELECT tenant_id,id FROM device_sync_proofs WHERE expires_at<=$1 ORDER BY expires_at,tenant_id,id LIMIT $2 FOR UPDATE SKIP LOCKED) DELETE FROM device_sync_proofs p USING expired e WHERE p.tenant_id=e.tenant_id AND p.id=e.id`, s.now(), limit)
	if e != nil {
		return 0, e
	}
	if e = tx.Commit(ctx); e != nil {
		return 0, e
	}
	return tag.RowsAffected(), nil
}
