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
	id                 string
	seq                int64
	senderID           string
	senderMembershipID string
	text               string
	at                 time.Time
}

// PullGroupTextMessages preserves the group's sequence timeline. A reader sees
// the body only when both reader and sender occupied a valid interval at seq.
func (s Service) PullGroupTextMessages(ctx context.Context, id access.TrustedIdentity,
	groupID string, afterSeq int64, limit int) (MessagePage, error) {
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
	actor, found, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return MessagePage{}, err
	}
	if !found {
		return MessagePage{}, ErrForbidden
	}
	at := s.now()
	if !memberActiveAt(actor, at) {
		if err := finishMessagePull(ctx, tx, id, "", "deny", "invalid_identity", at); err != nil {
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
		if err := finishMessagePull(ctx, tx, id, "", "deny", "group_history_unavailable", at); err != nil {
			return MessagePage{}, err
		}
		return MessagePage{}, ErrMessageNotAvailable
	}
	if err != nil {
		return MessagePage{}, err
	}
	version, err := currentVersion(ctx, tx, id.TenantID)
	if err != nil {
		return MessagePage{}, err
	}
	rules, err := loadRules(ctx, tx, id.TenantID, version)
	if err != nil {
		return MessagePage{}, err
	}
	page := MessagePage{ConversationID: groupID,
		Messages: make([]PulledMessage, 0, min(limit, 100)), NextAfterSeq: afterSeq}
	rows, err := tx.Query(ctx, `SELECT id::text,seq,sender_user_id::text,sender_membership_id::text,text_body,accepted_at
 FROM messages WHERE tenant_id=$1 AND conversation_id=$2 AND seq>$3
 ORDER BY seq LIMIT $4`, id.TenantID, groupID, afterSeq, limit+1)
	if err != nil {
		return MessagePage{}, err
	}
	var messages []groupHistoryMessage
	senderIDs := map[string]bool{id.UserID: true}
	for rows.Next() {
		var message groupHistoryMessage
		if err := rows.Scan(&message.id, &message.seq, &message.senderID,
			&message.senderMembershipID, &message.text, &message.at); err != nil {
			rows.Close()
			return MessagePage{}, err
		}
		if len(messages) == limit {
			page.HasMore = true
			break
		}
		messages = append(messages, message)
		senderIDs[message.senderID] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return MessagePage{}, err
	}
	intervals := make(map[string][]groupHistoryInterval, len(senderIDs))
	if len(messages) > 0 {
		users := make([]string, 0, len(senderIDs))
		for userID := range senderIDs {
			users = append(users, userID)
		}
		rows, err := tx.Query(ctx, `SELECT user_id::text,source_membership_id::text,
 source_organization_id::text,join_seq,leave_seq
 FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=ANY($3::uuid[])
   AND join_seq<=$4 AND (leave_seq IS NULL OR leave_seq>=$5)
 ORDER BY user_id,join_seq FOR SHARE`, id.TenantID, groupID, users,
			messages[len(messages)-1].seq, messages[0].seq)
		if err != nil {
			return MessagePage{}, err
		}
		for rows.Next() {
			var interval groupHistoryInterval
			if err := rows.Scan(&interval.userID, &interval.membershipID,
				&interval.organizationID, &interval.joinSeq, &interval.leaveSeq); err != nil {
				rows.Close()
				return MessagePage{}, err
			}
			intervals[interval.userID] = append(intervals[interval.userID], interval)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return MessagePage{}, err
		}
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !memberActiveAt(actor, at) {
		if err := finishMessagePull(ctx, tx, id, "", "deny", "invalid_identity", at); err != nil {
			return MessagePage{}, err
		}
		return MessagePage{}, ErrForbidden
	}
	groupHardDenied := false
	if len(messages) > 0 && hasGroupHistoryHardDeny(rules) {
		var readerIntervals []groupHistoryInterval
		rows, err := tx.Query(ctx, `SELECT user_id::text,source_membership_id::text,
 source_organization_id::text,join_seq,leave_seq
 FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3
 ORDER BY join_seq FOR SHARE`, id.TenantID, groupID, id.UserID)
		if err != nil {
			return MessagePage{}, err
		}
		for rows.Next() {
			var interval groupHistoryInterval
			if err := rows.Scan(&interval.userID, &interval.membershipID,
				&interval.organizationID, &interval.joinSeq, &interval.leaveSeq); err != nil {
				rows.Close()
				return MessagePage{}, err
			}
			readerIntervals = append(readerIntervals, interval)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return MessagePage{}, err
		}
		if fresh := s.now(); fresh.After(at) {
			at = fresh
		}
		if !memberActiveAt(actor, at) {
			if err := finishMessagePull(ctx, tx, id, "", "deny", "invalid_identity", at); err != nil {
				return MessagePage{}, err
			}
			return MessagePage{}, ErrForbidden
		}
		groupHardDenied, err = groupHistoryHardDenied(ctx, tx, id, groupID, actor,
			readerIntervals, rules, at)
		if err != nil {
			return MessagePage{}, err
		}
	}
	for _, message := range messages {
		item := PulledMessage{Seq: message.seq, Redacted: true}
		reader, readerFound := groupIntervalAt(intervals[id.UserID], message.seq)
		sender, senderFound := groupIntervalAt(intervals[message.senderID], message.seq)
		if !groupHardDenied && readerFound && senderFound &&
			sender.membershipID == message.senderMembershipID &&
			at.Before(message.at.Add(retention)) && !policy.HistoryHardDeny(actor,
			reader.policyMembership(id.TenantID), sender.policyMembership(id.TenantID), at, rules) {
			item = PulledMessage{MessageID: message.id, Seq: message.seq,
				SenderUserID: message.senderID, Text: message.text, ServerTime: message.at}
		}
		page.Messages = append(page.Messages, item)
		page.NextAfterSeq = message.seq
	}
	if err := finishMessagePull(ctx, tx, id, groupID, "allow", "group_history_page", at); err != nil {
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

func groupHistoryHardDenied(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity,
	groupID string, actor policy.Membership, readerIntervals []groupHistoryInterval,
	rules []policy.Rule, at time.Time) (bool, error) {
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
	for _, rule := range rules {
		if rule.Effect != policy.EffectHardDeny || rule.Action != policy.ActionSendMessage ||
			at.Before(rule.EffectiveFrom) || (!rule.EffectiveTo.IsZero() && !at.Before(rule.EffectiveTo)) {
			continue
		}
		if readerMatches(rule.SourceOrganizationID, rule.SourceMembershipID) {
			found, err := peerExists(rule.TargetOrganizationID, rule.TargetMembershipID)
			if err != nil || found {
				return found, err
			}
		}
		if readerMatches(rule.TargetOrganizationID, rule.TargetMembershipID) {
			found, err := peerExists(rule.SourceOrganizationID, rule.SourceMembershipID)
			if err != nil || found {
				return found, err
			}
		}
	}
	return false, nil
}
