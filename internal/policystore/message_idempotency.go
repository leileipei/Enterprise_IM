package policystore

import (
	"bytes"
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

var errInvalidMessageIdempotency = errors.New("message idempotency state unavailable")

// The legacy signature is retained. A file record can never be an identical
// text request even when its content digest happens to match the caller's hash.
func existingMessageACK(ctx context.Context, tx pgx.Tx, tenantID, conversationID, senderUserID, clientMessageID string, digest [32]byte) (MessageACK, bool, bool, error) {
	p, exists, err := readMessageReplayProof(ctx, tx, tenantID, conversationID, senderUserID, clientMessageID)
	if err != nil || !exists {
		return MessageACK{}, exists, false, err
	}
	if err = p.validate(); err != nil {
		return MessageACK{}, true, false, err
	}
	return p.ack, true, p.kind == MessageTypeText && bytes.Equal(p.keyDigest, digest[:]), nil
}
