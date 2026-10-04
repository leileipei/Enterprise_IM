package policystore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"strings"
	"unicode/utf8"
)

const (
	MessageTypeText = "text"
	MessageTypeFile = "file"
)

var (
	ErrFileNotBindable        = errors.New("file not bindable")
	ErrFileMessageUnavailable = errors.New("file messages unavailable")
)

type MessageSendRequest struct {
	ClientMessageID string
	MessageType     string
	Text            string
	FileID          string
	Caption         string
}

func validateMessageSendRequest(req MessageSendRequest) (MessageSendRequest, error) {
	if !directoryUUIDPattern.MatchString(req.ClientMessageID) {
		return MessageSendRequest{}, ErrInvalidClientMessageID
	}
	req.ClientMessageID = strings.ToLower(req.ClientMessageID)
	switch req.MessageType {
	case MessageTypeText:
		if req.FileID != "" || req.Caption != "" {
			return MessageSendRequest{}, ErrInvalidMessageRequest
		}
		if !utf8.ValidString(req.Text) || strings.ContainsRune(req.Text, 0) || len(req.Text) > 16384 || strings.TrimSpace(req.Text) == "" {
			return MessageSendRequest{}, ErrInvalidTextMessage
		}
	case MessageTypeFile:
		if req.Text != "" || !directoryUUIDPattern.MatchString(req.FileID) || !utf8.ValidString(req.Caption) || strings.ContainsRune(req.Caption, 0) || len(req.Caption) > 16384 {
			return MessageSendRequest{}, ErrInvalidMessageRequest
		}
		req.FileID = strings.ToLower(req.FileID)
	default:
		return MessageSendRequest{}, ErrInvalidMessageRequest
	}
	return req, nil
}
func fileMessageDigest(id access.TrustedIdentity, conversationID string, req MessageSendRequest, sealedSHA [32]byte) ([32]byte, error) {
	req, err := validateMessageSendRequest(req)
	if err != nil {
		return [32]byte{}, err
	}
	if req.MessageType != MessageTypeFile {
		return [32]byte{}, ErrInvalidMessageRequest
	}
	ids := []string{id.TenantID, conversationID, id.UserID, id.ActingMembershipID}
	for i := range ids {
		if !directoryUUIDPattern.MatchString(ids[i]) {
			return [32]byte{}, ErrInvalidMessageRequest
		}
		ids[i] = strings.ToLower(ids[i])
	}
	canonical, err := json.Marshal([]string{"file-message-v1", MessageTypeFile, ids[0], ids[1], ids[2], ids[3], req.FileID, hex.EncodeToString(sealedSHA[:]), req.Caption})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

// ValidateMessageSendRequest shares content validation with HTTP adapters.
// Presence and NULL checks remain the adapter's responsibility.
func ValidateMessageSendRequest(req MessageSendRequest) (MessageSendRequest, error) {
	return validateMessageSendRequest(req)
}
