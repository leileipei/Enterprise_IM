package policystore

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ResolveMessageEvent verifies every Redis identifier against the durable
// Outbox and message. Only the historical participants receive a signal.
func (s Service) ResolveMessageEvent(ctx context.Context, tenantID, eventID, conversationID,
	messageID string, seq int64) ([]string, error) {
	if s.DB == nil || seq < 1 || !directoryUUIDPattern.MatchString(tenantID) ||
		!directoryUUIDPattern.MatchString(eventID) || !directoryUUIDPattern.MatchString(conversationID) ||
		!directoryUUIDPattern.MatchString(messageID) {
		return nil, ErrInvalidMessageRequest
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	query := `SELECT m.sender_user_id::text,m.recipient_user_id::text
FROM outbox_events e JOIN messages m
 ON m.tenant_id=e.tenant_id AND m.conversation_id=e.conversation_id
 AND m.id=e.message_id AND m.seq=e.seq
WHERE e.tenant_id=$1 AND e.id=$2 AND e.conversation_id=$3 AND e.message_id=$4
 AND e.seq=$5 AND e.event_type='message_created'`
	var sender string
	var recipient *string
	err = tx.QueryRow(ctx, query, strings.ToLower(tenantID), strings.ToLower(eventID),
		strings.ToLower(conversationID), strings.ToLower(messageID), seq).Scan(&sender, &recipient)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	users := []string{sender}
	if recipient != nil && *recipient != sender {
		users = append(users, *recipient)
	}
	return users, nil
}
