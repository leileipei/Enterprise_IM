package policystore

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
)

var ErrInvalidConversationListRequest = errors.New("invalid conversation list request")

type ListedConversation struct {
	ID                   string
	LastSeq              int64
	UpdatedAt            time.Time
	PeerVisible          bool
	PeerDisplayName      string
	PeerOrganizationName string
}

type ConversationListPage struct {
	Conversations []ListedConversation
	HasMore       bool
	NextCursor    string
}

func encodeConversationCursor(at time.Time, id string) string {
	value := at.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeConversationCursor(cursor string) (time.Time, string, error) {
	if cursor == "" {
		return time.Time{}, "", nil
	}
	if len(cursor) > 256 {
		return time.Time{}, "", ErrInvalidConversationListRequest
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", ErrInvalidConversationListRequest
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 2 || !directoryUUIDPattern.MatchString(parts[1]) || parts[1] != strings.ToLower(parts[1]) {
		return time.Time{}, "", ErrInvalidConversationListRequest
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil || at.IsZero() || encodeConversationCursor(at, parts[1]) != cursor {
		return time.Time{}, "", ErrInvalidConversationListRequest
	}
	return at, parts[1], nil
}

func auditConversationList(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `
INSERT INTO audit_events (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
VALUES ($1,$2,$3,'conversation_list','conversation',NULL,$4,$5,$6)`,
		id.TenantID, id.UserID, id.ActingMembershipID, outcome, reason, at)
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return nil
}

// ListDirectConversations returns a bounded page for the caller's currently
// selected membership. Peer profile fields are separately policy filtered.
func (s Service) ListDirectConversations(ctx context.Context, id access.TrustedIdentity, cursor string, limit int) (ConversationListPage, error) {
	if limit < 1 || limit > 50 {
		return ConversationListPage{}, ErrInvalidConversationListRequest
	}
	before, beforeID, err := decodeConversationCursor(cursor)
	if err != nil {
		return ConversationListPage{}, err
	}
	if s.DB == nil || !directoryUUIDPattern.MatchString(id.TenantID) ||
		!directoryUUIDPattern.MatchString(id.UserID) || !directoryUUIDPattern.MatchString(id.ActingMembershipID) {
		return ConversationListPage{}, ErrForbidden
	}
	id.TenantID = strings.ToLower(id.TenantID)
	id.UserID = strings.ToLower(id.UserID)
	id.ActingMembershipID = strings.ToLower(id.ActingMembershipID)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return ConversationListPage{}, err
	}
	defer tx.Rollback(ctx)
	at := s.now()
	actor, found, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return ConversationListPage{}, err
	}
	if !found || !memberActiveAt(actor, at) {
		if err := auditConversationList(ctx, tx, id, "deny", "invalid_identity", at); err != nil {
			return ConversationListPage{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ConversationListPage{}, err
		}
		return ConversationListPage{}, ErrForbidden
	}
	// A published policy cannot replace the selected snapshot midway through
	// profile filtering. No row is normal before the first policy publication.
	var version int64
	err = tx.QueryRow(ctx, `SELECT current_version FROM policy_current WHERE tenant_id=$1 FOR SHARE`, id.TenantID).Scan(&version)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ConversationListPage{}, err
	}
	version, err = currentVersion(ctx, tx, id.TenantID)
	if err != nil {
		return ConversationListPage{}, err
	}
	rules, err := loadRules(ctx, tx, id.TenantID, version)
	if err != nil {
		return ConversationListPage{}, err
	}
	query := `
SELECT c.id::text,c.last_seq,c.updated_at,
 CASE WHEN c.direct_user_low_id=$2 THEN c.direct_high_membership_id::text ELSE c.direct_low_membership_id::text END
FROM conversations c
WHERE c.tenant_id=$1 AND c.kind='direct' AND c.status='active'
 AND ((c.direct_user_low_id=$2 AND c.direct_low_membership_id=$3)
   OR (c.direct_user_high_id=$2 AND c.direct_high_membership_id=$3))`
	args := []any{id.TenantID, id.UserID, id.ActingMembershipID}
	if cursor != "" {
		query += ` AND (c.updated_at,c.id) < ($4::timestamptz,$5::uuid)`
		args = append(args, before, beforeID)
	}
	query += ` ORDER BY c.updated_at DESC,c.id DESC LIMIT $` + strconv.Itoa(len(args)+1) + ` FOR SHARE OF c`
	args = append(args, limit+1)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return ConversationListPage{}, err
	}
	type candidate struct {
		item   ListedConversation
		peerID string
	}
	candidates := make([]candidate, 0, limit+1)
	for rows.Next() {
		var entry candidate
		if err := rows.Scan(&entry.item.ID, &entry.item.LastSeq, &entry.item.UpdatedAt, &entry.peerID); err != nil {
			rows.Close()
			return ConversationListPage{}, err
		}
		candidates = append(candidates, entry)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return ConversationListPage{}, err
	}
	page := ConversationListPage{Conversations: make([]ListedConversation, 0, min(len(candidates), limit))}
	page.HasMore = len(candidates) > limit
	if page.HasMore {
		candidates = candidates[:limit]
	}
	peerSnapshots := make([]policy.Membership, len(candidates))
	for index, entry := range candidates {
		peer, peerFound, err := loadMembership(ctx, tx, id.TenantID, entry.peerID, "")
		if err != nil {
			return ConversationListPage{}, err
		}
		if fresh := s.now(); fresh.After(at) {
			at = fresh
		}
		decision := policy.Decision{PolicyVersion: version, Reason: policy.ReasonInvalidContext}
		if peerFound {
			decision = policy.Evaluate(policy.Input{Action: policy.ActionDirectoryView,
				Actor: actor, Target: peer, At: at, ScopeAllowed: true, ResourceActive: true,
				PolicyVersion: version, Rules: rules})
		}
		if err := auditDecision(ctx, tx, Request{Identity: id, TargetMembershipID: entry.peerID,
			Action: policy.ActionDirectoryView, ScopeAllowed: true, ResourceActive: true}, decision, at); err != nil {
			return ConversationListPage{}, errors.Join(ErrAuditUnavailable, err)
		}
		if decision.Allowed {
			var name, organization string
			err := tx.QueryRow(ctx, `
SELECT u.display_name,o.name FROM user_organizations m
JOIN users u ON u.tenant_id=m.tenant_id AND u.id=m.user_id
JOIN organizations o ON o.tenant_id=m.tenant_id AND o.id=m.organization_id
WHERE m.tenant_id=$1 AND m.id=$2`, id.TenantID, entry.peerID).Scan(&name, &organization)
			if err != nil {
				return ConversationListPage{}, err
			}
			entry.item.PeerVisible = true
			entry.item.PeerDisplayName = name
			entry.item.PeerOrganizationName = organization
			peerSnapshots[index] = peer
		}
		page.Conversations = append(page.Conversations, entry.item)
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !memberActiveAt(actor, at) {
		if err := auditConversationList(ctx, tx, id, "deny", "invalid_identity", at); err != nil {
			return ConversationListPage{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ConversationListPage{}, err
		}
		return ConversationListPage{}, ErrForbidden
	}
	for index := range page.Conversations {
		if !page.Conversations[index].PeerVisible {
			continue
		}
		decision := policy.Evaluate(policy.Input{Action: policy.ActionDirectoryView,
			Actor: actor, Target: peerSnapshots[index], At: at, ScopeAllowed: true,
			ResourceActive: true, PolicyVersion: version, Rules: rules})
		if decision.Allowed {
			continue
		}
		if err := auditDecision(ctx, tx, Request{Identity: id, TargetMembershipID: candidates[index].peerID,
			Action: policy.ActionDirectoryView, ScopeAllowed: true, ResourceActive: true}, decision, at); err != nil {
			return ConversationListPage{}, errors.Join(ErrAuditUnavailable, err)
		}
		page.Conversations[index].PeerVisible = false
		page.Conversations[index].PeerDisplayName = ""
		page.Conversations[index].PeerOrganizationName = ""
	}
	if page.HasMore {
		last := page.Conversations[len(page.Conversations)-1]
		page.NextCursor = encodeConversationCursor(last.UpdatedAt, last.ID)
	}
	if err := auditConversationList(ctx, tx, id, "allow", "page", at); err != nil {
		return ConversationListPage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ConversationListPage{}, err
	}
	return page, nil
}
