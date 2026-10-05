package policystore

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ResolveMessageEvent verifies every Redis identifier against the durable
// Outbox and message. Only participants at the message's sequence receive a signal.
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
	query := `SELECT c.kind,m.sender_user_id::text,m.sender_membership_id::text,m.recipient_user_id::text
FROM outbox_events e JOIN messages m
 ON m.tenant_id=e.tenant_id AND m.conversation_id=e.conversation_id
 AND m.id=e.message_id AND m.seq=e.seq
JOIN conversations c ON c.tenant_id=m.tenant_id AND c.id=m.conversation_id
WHERE e.tenant_id=$1 AND e.id=$2 AND e.conversation_id=$3 AND e.message_id=$4
	AND e.seq=$5 AND e.event_type='message_created' FOR SHARE OF c,e,m`
	var kind, sender, senderMembership string
	var recipient *string
	err = tx.QueryRow(ctx, query, strings.ToLower(tenantID), strings.ToLower(eventID),
		strings.ToLower(conversationID), strings.ToLower(messageID), seq).
		Scan(&kind, &sender, &senderMembership, &recipient)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	if kind == "group" {
		rows, err := tx.Query(ctx, `SELECT user_id::text,source_membership_id::text
 FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND join_seq<=$3
   AND (leave_seq IS NULL OR leave_seq>=$3)
 ORDER BY user_id FOR SHARE`, strings.ToLower(tenantID), strings.ToLower(conversationID), seq)
		if err != nil {
			return nil, err
		}
		var users []string
		senderAuthorized := false
		for rows.Next() {
			var userID, membershipID string
			if err := rows.Scan(&userID, &membershipID); err != nil {
				rows.Close()
				return nil, err
			}
			users = append(users, userID)
			if userID == sender && membershipID == senderMembership {
				senderAuthorized = true
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		if !senderAuthorized {
			return nil, nil
		}
		return users, nil
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
