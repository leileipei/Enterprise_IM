package access

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

type retentionBatchCursor struct {
	TenantID       string `json:"t"`
	ConversationID string `json:"c"`
	Kind           string `json:"k"`
	ProcessedAt    string `json:"at"`
	ID             string `json:"id"`
}

func parseRetentionBatchCursor(value, tenantID, conversationID, kind string) (time.Time, string, error) {
	if value == "" {
		return time.Time{}, "", nil
	}
	if len(value) > 1024 {
		return time.Time{}, "", ErrInvalidRetentionQuery
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) > 768 {
		return time.Time{}, "", ErrInvalidRetentionQuery
	}
	var c retentionBatchCursor
	if json.Unmarshal(raw, &c) != nil || c.TenantID != strings.ToLower(tenantID) || c.ConversationID != conversationID || c.Kind != kind || !legalHoldUUIDPattern.MatchString(c.ID) {
		return time.Time{}, "", ErrInvalidRetentionQuery
	}
	// Exact canonical encoding rejects unknown/duplicate fields and noncanonical base64.
	canonical, _ := json.Marshal(c)
	if base64.RawURLEncoding.EncodeToString(canonical) != value {
		return time.Time{}, "", ErrInvalidRetentionQuery
	}
	at, err := time.Parse(time.RFC3339Nano, c.ProcessedAt)
	if err != nil {
		return time.Time{}, "", ErrInvalidRetentionQuery
	}
	return at, strings.ToLower(c.ID), nil
}

func makeRetentionBatchCursor(tenantID, conversationID, kind string, b RetentionBatch) string {
	raw, _ := json.Marshal(retentionBatchCursor{TenantID: strings.ToLower(tenantID), ConversationID: conversationID, Kind: kind, ProcessedAt: b.ProcessedAt.UTC().Format(time.RFC3339Nano), ID: b.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}
