package policystore

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var errInvalidMessageIdempotency = errors.New("message idempotency state unavailable")

// One statement sees either the committed active pair or the committed retired
// pair. Missing or inconsistent state is never treated as a new message key.
func existingMessageACK(ctx context.Context, tx pgx.Tx, tenantID, conversationID, senderUserID,
	clientMessageID string, digest [32]byte) (MessageACK, bool, bool, error) {
	var ack MessageACK
	var messageDigest, storedDigest []byte
	var messageRetired, keyRetired *time.Time
	var keyMessageID *string
	err := tx.QueryRow(ctx, `
SELECT m.content_digest,i.content_digest,m.digest_retired_at,i.digest_retired_at,
 i.message_id::text,m.id::text,m.seq,m.accepted_at
FROM messages m LEFT JOIN message_idempotency i
 ON i.tenant_id=m.tenant_id AND i.conversation_id=m.conversation_id
 AND i.sender_user_id=m.sender_user_id AND i.client_msg_id=m.client_msg_id AND i.message_id=m.id
WHERE m.tenant_id=$1 AND m.conversation_id=$2 AND m.sender_user_id=$3 AND m.client_msg_id=$4`,
		tenantID, conversationID, senderUserID, clientMessageID).
		Scan(&messageDigest, &storedDigest, &messageRetired, &keyRetired, &keyMessageID, &ack.MessageID, &ack.Seq, &ack.ServerTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return MessageACK{}, false, false, nil
	}
	if err != nil {
		return MessageACK{}, false, false, err
	}
	if messageRetired != nil || keyRetired != nil {
		return MessageACK{}, true, false, ErrRetryExpired
	}
	if keyMessageID == nil || len(messageDigest) != 32 || len(storedDigest) != 32 || !bytes.Equal(messageDigest, storedDigest) {
		return MessageACK{}, true, false, errInvalidMessageIdempotency
	}
	ack.ConversationID = conversationID
	return ack, true, bytes.Equal(storedDigest, digest[:]), nil
}
