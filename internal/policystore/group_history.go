package policystore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
)

type groupHistoryInterval struct {
	userID         string
	membershipID   string
	organizationID string
	joinSeq        int64
	leaveSeq       *int64
}

func groupIntervalAt(intervals []groupHistoryInterval, seq int64) (groupHistoryInterval, bool) {
	for _, interval := range intervals {
		if interval.joinSeq <= seq && (interval.leaveSeq == nil || seq <= *interval.leaveSeq) {
			return interval, true
		}
	}
	return groupHistoryInterval{}, false
}

func (i groupHistoryInterval) policyMembership(tenantID string) policy.Membership {
	return policy.Membership{ID: i.membershipID, TenantID: tenantID, OrganizationID: i.organizationID}
}

type groupHistoryMessage struct {
	messageType        string
	attachment         *MessageAttachment
	id                 string
	seq                int64
	senderID           string
	senderMembershipID string
	text               *string
	clearedAt          *time.Time
	at                 time.Time
}

// PullGroupTextMessages preserves the group's sequence timeline. A reader sees
// the body only when both reader and sender occupied a valid interval at seq.
func (s Service) PullGroupTextMessages(ctx context.Context, id access.TrustedIdentity,
	groupID string, afterSeq int64, limit int) (MessagePage, error) {
	return s.readGroupTextMessages(ctx, id, groupID, afterSeq, limit, "message_pull")
}

func (s Service) readGroupTextMessages(ctx context.Context, id access.TrustedIdentity, groupID string, afterSeq int64, limit int, action string) (MessagePage, error) {
	if !directoryUUIDPattern.MatchString(groupID) || afterSeq < 0 || limit < 1 || limit > 500 {
		return MessagePage{}, ErrInvalidMessageRequest
	}
	if s.DB == nil || !directoryUUIDPattern.MatchString(id.TenantID) ||
		!directoryUUIDPattern.MatchString(id.UserID) || !directoryUUIDPattern.MatchString(id.ActingMembershipID) {
		return MessagePage{}, ErrForbidden
	}
	id.TenantID, id.UserID, id.ActingMembershipID = strings.ToLower(id.TenantID),
		strings.ToLower(id.UserID), strings.ToLower(id.ActingMembershipID)
	groupID = strings.ToLower(groupID)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return MessagePage{}, err
	}
	defer tx.Rollback(ctx)
	if action == "message_search" {
		if _, err = tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
			return MessagePage{}, err
		}
	}
	finish := func(conversationID, outcome, reason string, at time.Time) error {
		return finishMessageRead(ctx, tx, id, conversationID, outcome, reason, at, action)
	}
	actor, found, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return MessagePage{}, err
	}
	if !found {
		return MessagePage{}, ErrForbidden
	}
	at := s.now()
	if !memberActiveAt(actor, at) {
		if err := finish("", "deny", "invalid_identity", at); err != nil {
			return MessagePage{}, err
		}
		return MessagePage{}, ErrForbidden
	}
	var lockedGroupID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM conversations
 WHERE tenant_id=$1 AND id=$2 AND kind='group' FOR SHARE`, id.TenantID, groupID).Scan(&lockedGroupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return MessagePage{}, ErrMessageNotAvailable
	}
	if err != nil {
		return MessagePage{}, err
	}
	retention, err := messageBodyRetentionForTenant(ctx, tx, id.TenantID)
	if err != nil {
		return MessagePage{}, err
	}
	var readerIntervalID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3 LIMIT 1 FOR SHARE`,
		id.TenantID, groupID, id.UserID).Scan(&readerIntervalID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := finish("", "deny", "group_history_unavailable", at); err != nil {
			return MessagePage{}, err
		}
		return MessagePage{}, ErrMessageNotAvailable
	}
	if err != nil {
		return MessagePage{}, err
	}
	if action == "message_search" {
		var pointer int64
		err = tx.QueryRow(ctx, "SELECT current_version FROM policy_current WHERE tenant_id=$1 FOR SHARE", id.TenantID).Scan(&pointer)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return MessagePage{}, err
		}
	}
	version, err := currentVersion(ctx, tx, id.TenantID)
	if err != nil {
		return MessagePage{}, err
	}
	rules, err := loadRules(ctx, tx, id.TenantID, version)
	if err != nil {
		return MessagePage{}, err
	}
	scope := historyReadContext{Identity: id, Actor: actor, Rules: rules, Retention: retention}
	read := readGroupHistoryBatchTx
	if action == "message_search" {
		read = readGroupTextSearchBatchTx
	}
	batch, err := read(ctx, tx, scope, groupID, afterSeq, limit)
	if err != nil {
		return MessagePage{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !memberActiveAt(actor, at) {
		if err := finish("", "deny", "invalid_identity", at); err != nil {
			return MessagePage{}, err
		}
		return MessagePage{}, ErrForbidden
	}
	page, err := filterGroupHistoryBatchTx(ctx, tx, scope, batch, at)
	if err != nil {
		return MessagePage{}, err
	}
	reason := "group_history_page"
	if action == "message_search" {
		reason = "group_search_page"
	}
	if err := finish(groupID, "allow", reason, at); err != nil {
		return MessagePage{}, err
	}
	return page, nil
}

func hasGroupHistoryHardDeny(rules []policy.Rule) bool {
	for _, rule := range rules {
		if rule.Effect == policy.EffectHardDeny && rule.Action == policy.ActionSendMessage {
			return true
		}
	}
	return false
}

func groupHistoryRuleSideMatches(member policy.Membership, organizationID, membershipID string) bool {
	return (organizationID == "" || organizationID == member.OrganizationID) &&
		(membershipID == "" || membershipID == member.ID)
}

func groupHistoryMatchingDenials(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity,
	groupID string, actor policy.Membership, readerIntervals []groupHistoryInterval,
	rules []policy.Rule) ([]policy.Rule, error) {
	readerMatches := func(organizationID, membershipID string) bool {
		if groupHistoryRuleSideMatches(actor, organizationID, membershipID) {
			return true
		}
		for _, interval := range readerIntervals {
			if groupHistoryRuleSideMatches(interval.policyMembership(id.TenantID), organizationID, membershipID) {
				return true
			}
		}
		return false
	}
	peerExists := func(organizationID, membershipID string) (bool, error) {
		var found bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND user_id<>$3
   AND ($4::uuid IS NULL OR source_organization_id=$4)
   AND ($5::uuid IS NULL OR source_membership_id=$5))`,
			id.TenantID, groupID, id.UserID, nullableID(organizationID), nullableID(membershipID)).Scan(&found)
		return found, err
	}
	var matched []policy.Rule
	for _, rule := range rules {
		if rule.Effect != policy.EffectHardDeny || rule.Action != policy.ActionSendMessage {
			continue
		}
		if readerMatches(rule.SourceOrganizationID, rule.SourceMembershipID) {
			found, err := peerExists(rule.TargetOrganizationID, rule.TargetMembershipID)
			if err != nil {
				return nil, err
			}
			if found {
				matched = append(matched, rule)
				continue
			}
		}
		if readerMatches(rule.TargetOrganizationID, rule.TargetMembershipID) {
			found, err := peerExists(rule.SourceOrganizationID, rule.SourceMembershipID)
			if err != nil {
				return nil, err
			}
			if found {
				matched = append(matched, rule)
			}
		}
	}
	return matched, nil
}

// Matching facts are loaded before the final clock. Future rules must also be
// included so activation during the last peer query cannot evade that check.
func groupHistoryDenialsActive(rules []policy.Rule, at time.Time) bool {
	for _, r := range rules {
		if !at.Before(r.EffectiveFrom) && (r.EffectiveTo.IsZero() || at.Before(r.EffectiveTo)) {
			return true
		}
	}
	return false
}
func groupHistoryHardDenied(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, groupID string, actor policy.Membership, readerIntervals []groupHistoryInterval, rules []policy.Rule, at time.Time) (bool, error) {
	var active []policy.Rule
	for _, r := range rules {
		if !at.Before(r.EffectiveFrom) && (r.EffectiveTo.IsZero() || at.Before(r.EffectiveTo)) {
			active = append(active, r)
		}
	}
	matched, e := groupHistoryMatchingDenials(ctx, tx, id, groupID, actor, readerIntervals, active)
	return len(matched) > 0, e
}
