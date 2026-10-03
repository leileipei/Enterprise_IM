package access

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type LegalHoldPage struct {
	Holds      []LegalHold
	NextCursor string
}

type legalHoldCursor struct {
	TenantID       string `json:"t"`
	ConversationID string `json:"c"`
	PlacedAt       string `json:"at"`
	ID             string `json:"id"`
}

func parseLegalHoldCursor(value, tenantID, conversationID string) (time.Time, string, error) {
	if value == "" {
		return time.Time{}, "", nil
	}
	if len(value) > 1024 {
		return time.Time{}, "", ErrInvalidLegalHold
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) > 768 {
		return time.Time{}, "", ErrInvalidLegalHold
	}
	var cursor legalHoldCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil ||
		!strings.EqualFold(cursor.TenantID, tenantID) ||
		cursor.ConversationID != conversationID ||
		!legalHoldUUIDPattern.MatchString(cursor.ID) {
		return time.Time{}, "", ErrInvalidLegalHold
	}
	at, err := time.Parse(time.RFC3339Nano, cursor.PlacedAt)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return time.Time{}, "", ErrInvalidLegalHold
	}
	return at, strings.ToLower(cursor.ID), nil
}

func makeLegalHoldCursor(tenantID, conversationID string, hold LegalHold) string {
	raw, _ := json.Marshal(legalHoldCursor{TenantID: strings.ToLower(tenantID),
		ConversationID: conversationID, PlacedAt: hold.PlacedAt.UTC().Format(time.RFC3339Nano), ID: hold.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (s Service) ListLegalHolds(ctx context.Context, id TrustedIdentity,
	conversationID, cursor string, limit int) (LegalHoldPage, error) {
	if !legalHoldUUIDPattern.MatchString(conversationID) || limit < 1 || limit > 500 {
		return LegalHoldPage{}, ErrInvalidLegalHold
	}
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return LegalHoldPage{}, ErrInvalidIdentity
	}
	conversationID = strings.ToLower(conversationID)
	cursorAt, cursorID, err := parseLegalHoldCursor(cursor, id.TenantID, conversationID)
	if err != nil {
		return LegalHoldPage{}, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return LegalHoldPage{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockLegalHoldActor(ctx, tx, id); err != nil {
		return LegalHoldPage{}, err
	}
	at := s.currentTime()
	if err := s.legalHoldGrant(ctx, tx, id, conversationID, "legal_hold_list", at); err != nil {
		return LegalHoldPage{}, err
	}
	var exists string
	err = tx.QueryRow(ctx, `SELECT id::text FROM conversations
 WHERE tenant_id=$1 AND id=$2 FOR SHARE`, id.TenantID, conversationID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return LegalHoldPage{}, deny(ctx, tx, id, "legal_hold_list", "conversation",
			conversationID, "conversation_unavailable", at, ErrNotFound)
	}
	if err != nil {
		return LegalHoldPage{}, err
	}
	if fresh := s.currentTime(); fresh.After(at) {
		at = fresh
		if err := s.legalHoldGrant(ctx, tx, id, conversationID, "legal_hold_list", at); err != nil {
			return LegalHoldPage{}, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT id::text,conversation_id::text,case_reference,
 placed_by_user_id::text,placed_by_membership_id::text,placed_at,
 release_approval_reference,released_by_user_id::text,released_by_membership_id::text,released_at
 FROM conversation_legal_holds
 WHERE tenant_id=$1 AND conversation_id=$2
 AND ($3::timestamptz IS NULL OR (placed_at,id)>($3,$4::uuid))
 ORDER BY placed_at,id LIMIT $5`, id.TenantID, conversationID,
		nullableLegalHoldTime(cursorAt), nullableLegalHoldID(cursorID), limit+1)
	if err != nil {
		return LegalHoldPage{}, err
	}
	holds := make([]LegalHold, 0, limit+1)
	for rows.Next() {
		hold, err := scanLegalHold(rows)
		if err != nil {
			rows.Close()
			return LegalHoldPage{}, err
		}
		holds = append(holds, hold)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return LegalHoldPage{}, err
	}
	page := LegalHoldPage{Holds: holds}
	if len(holds) > limit {
		page.Holds = holds[:limit]
		page.NextCursor = makeLegalHoldCursor(id.TenantID, conversationID, page.Holds[limit-1])
	}
	if err := audit(ctx, tx, id, "legal_hold_list", "conversation",
		conversationID, "allow", "listed", at); err != nil {
		return LegalHoldPage{}, errors.Join(ErrAuditUnavailable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return LegalHoldPage{}, err
	}
	return page, nil
}

func nullableLegalHoldTime(at time.Time) any {
	if at.IsZero() {
		return nil
	}
	return at
}

func nullableLegalHoldID(id string) any {
	if id == "" {
		return nil
	}
	return id
}
