package policystore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"strings"
	"time"
)

type messageReplayProof struct {
	ack                        MessageACK
	messageDigest, keyDigest   []byte
	messageRetired, keyRetired *time.Time
	keyMessageID               *string
	kind                       string
	fileID                     *string
	fingerprint                []byte
	fingerprintRetired         *time.Time
	senderMembershipID         string
}

// A single statement observes a whole committed proof, including concurrent
// triple retirement. File object metadata and the cleared caption are irrelevant.
func readMessageReplayProof(ctx context.Context, tx pgx.Tx, tenantID, conversationID, senderUserID, clientMessageID string) (messageReplayProof, bool, error) {
	var p messageReplayProof
	err := tx.QueryRow(ctx, `
SELECT m.content_digest,i.content_digest,m.digest_retired_at,i.digest_retired_at,
 i.message_id::text,m.id::text,m.seq,m.accepted_at,m.message_type,
 a.file_id::text,a.sealed_sha256,a.fingerprint_retired_at,m.sender_membership_id::text
FROM messages m LEFT JOIN message_idempotency i
 ON i.tenant_id=m.tenant_id AND i.conversation_id=m.conversation_id
 AND i.sender_user_id=m.sender_user_id AND i.client_msg_id=m.client_msg_id AND i.message_id=m.id
LEFT JOIN message_attachments a ON a.tenant_id=m.tenant_id AND a.message_id=m.id
WHERE m.tenant_id=$1 AND m.conversation_id=$2 AND m.sender_user_id=$3 AND m.client_msg_id=$4`,
		tenantID, conversationID, senderUserID, clientMessageID).Scan(&p.messageDigest, &p.keyDigest, &p.messageRetired, &p.keyRetired, &p.keyMessageID, &p.ack.MessageID, &p.ack.Seq, &p.ack.ServerTime, &p.kind, &p.fileID, &p.fingerprint, &p.fingerprintRetired, &p.senderMembershipID)
	if errors.Is(err, pgx.ErrNoRows) {
		return messageReplayProof{}, false, nil
	}
	if err != nil {
		return messageReplayProof{}, false, err
	}
	p.ack.ConversationID = conversationID
	return p, true, nil
}
func (p messageReplayProof) validate() error {
	if p.keyMessageID == nil || *p.keyMessageID != p.ack.MessageID || p.senderMembershipID == "" {
		return errInvalidMessageIdempotency
	}
	switch p.kind {
	case MessageTypeText:
		if p.fileID != nil || p.fingerprint != nil || p.fingerprintRetired != nil {
			return errInvalidMessageIdempotency
		}
	case MessageTypeFile:
		if p.fileID == nil || !directoryUUIDPattern.MatchString(*p.fileID) {
			return errInvalidMessageIdempotency
		}
	default:
		return errInvalidMessageIdempotency
	}
	retired := p.messageRetired != nil || p.keyRetired != nil || p.fingerprintRetired != nil
	if retired {
		if p.messageRetired == nil || p.keyRetired == nil || !p.messageRetired.Equal(*p.keyRetired) || p.messageDigest != nil || p.keyDigest != nil {
			return errInvalidMessageIdempotency
		}
		if p.kind == MessageTypeFile && (p.fingerprintRetired == nil || !p.messageRetired.Equal(*p.fingerprintRetired) || p.fingerprint != nil) {
			return errInvalidMessageIdempotency
		}
		return ErrRetryExpired
	}
	if len(p.messageDigest) != 32 || len(p.keyDigest) != 32 || !bytes.Equal(p.messageDigest, p.keyDigest) || (p.kind == MessageTypeFile && len(p.fingerprint) != 32) {
		return errInvalidMessageIdempotency
	}
	return nil
}
func existingTypedMessageACK(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, conversationID string, req MessageSendRequest) (MessageACK, bool, bool, error) {
	req, err := validateMessageSendRequest(req)
	if err != nil {
		return MessageACK{}, false, false, err
	}
	p, exists, err := readMessageReplayProof(ctx, tx, strings.ToLower(id.TenantID), strings.ToLower(conversationID), strings.ToLower(id.UserID), req.ClientMessageID)
	if err != nil || !exists {
		return MessageACK{}, exists, false, err
	}
	if err = p.validate(); err != nil {
		return MessageACK{}, true, false, err
	}
	if p.kind != req.MessageType {
		return p.ack, true, false, nil
	}
	if req.MessageType == MessageTypeText {
		digest := sha256.Sum256([]byte(req.Text))
		return p.ack, true, bytes.Equal(p.keyDigest, digest[:]), nil
	}
	if !strings.EqualFold(p.senderMembershipID, id.ActingMembershipID) || !strings.EqualFold(*p.fileID, req.FileID) {
		return p.ack, true, false, nil
	}
	var sealed [32]byte
	copy(sealed[:], p.fingerprint)
	digest, err := fileMessageDigest(id, conversationID, req, sealed)
	if err != nil {
		return MessageACK{}, true, false, err
	}
	return p.ack, true, bytes.Equal(p.keyDigest, digest[:]), nil
}
