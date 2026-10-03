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

// These private batches retain authorization evidence until the page's final check.
// Helpers never begin, audit, or commit a transaction.
type historicalPair struct{ reader, peer policy.Membership }
type historyReadContext struct {
	Identity  access.TrustedIdentity
	Actor     policy.Membership
	Rules     []policy.Rule
	Retention time.Duration
}
type historyReadBatch struct {
	Page            MessagePage
	Direct          []historicalPair
	ReaderIntervals []groupHistoryInterval
}

func (s Service) newHistoryReadContextTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, lockPolicy bool) (historyReadContext, error) {
	actor, found, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return historyReadContext{}, err
	}
	if !found || !memberActiveAt(actor, s.now()) {
		return historyReadContext{}, ErrForbidden
	}
	retention, err := messageBodyRetentionForTenant(ctx, tx, id.TenantID)
	if err != nil {
		return historyReadContext{}, err
	}
	if lockPolicy {
		var version int64
		err = tx.QueryRow(ctx, `SELECT current_version FROM policy_current WHERE tenant_id=$1 FOR SHARE`, id.TenantID).Scan(&version)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return historyReadContext{}, err
		}
	}
	version, err := currentVersion(ctx, tx, id.TenantID)
	if err != nil {
		return historyReadContext{}, err
	}
	rules, err := loadRules(ctx, tx, id.TenantID, version)
	if err != nil {
		return historyReadContext{}, err
	}
	return historyReadContext{Identity: id, Actor: actor, Rules: rules, Retention: retention}, nil
}

func filterDirectHistoryBatch(scope historyReadContext, batch historyReadBatch, at time.Time) MessagePage {
	page := batch.Page
	page.Messages = append([]PulledMessage{}, batch.Page.Messages...)
	for i, m := range page.Messages {
		if m.Redacted {
			continue
		}
		if i >= len(batch.Direct) || !at.Before(m.ServerTime.Add(scope.Retention)) ||
			policy.HistoryHardDeny(scope.Actor, batch.Direct[i].reader, batch.Direct[i].peer, at, scope.Rules) {
			page.Messages[i] = PulledMessage{Seq: m.Seq, Redacted: true}
		}
	}
	return page
}

func filterGroupHistoryBatchTx(ctx context.Context, tx pgx.Tx, scope historyReadContext, batch historyReadBatch, at time.Time) (MessagePage, error) {
	page := filterDirectHistoryBatch(scope, batch, at)
	if len(page.Messages) > 0 && hasGroupHistoryHardDeny(scope.Rules) {
		denied, err := groupHistoryHardDenied(ctx, tx, scope.Identity, page.ConversationID, scope.Actor, batch.ReaderIntervals, scope.Rules, at)
		if err != nil {
			return MessagePage{}, err
		}
		if denied {
			for i, m := range page.Messages {
				page.Messages[i] = PulledMessage{Seq: m.Seq, Redacted: true}
			}
		}
	}
	return page, nil
}

func readDirectHistoryBatchTx(ctx context.Context, tx pgx.Tx, scope historyReadContext, conversationID string, afterSeq int64, limit int) (historyReadBatch, error) {
	id := scope.Identity
	if limit < 1 || limit > 500 || afterSeq < 0 || !directoryUUIDPattern.MatchString(conversationID) {
		return historyReadBatch{}, ErrInvalidMessageRequest
	}
	var lowUser, highUser string
	err := tx.QueryRow(ctx, `SELECT direct_user_low_id::text,direct_user_high_id::text FROM conversations WHERE tenant_id=$1 AND id=$2 AND kind='direct' FOR SHARE`, id.TenantID, conversationID).Scan(&lowUser, &highUser)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && id.UserID != lowUser && id.UserID != highUser) {
		return historyReadBatch{}, ErrMessageNotAvailable
	}
	if err != nil {
		return historyReadBatch{}, err
	}
	page := MessagePage{ConversationID: conversationID,
		Messages: make([]PulledMessage, 0, min(limit, 100)), NextAfterSeq: afterSeq}
	histories := make([]historicalPair, 0, min(limit, 100))
	rows, err := tx.Query(ctx, `
SELECT m.id::text,m.seq,m.sender_user_id::text,m.sender_membership_id::text,
 COALESCE(m.recipient_user_id::text,''),COALESCE(m.recipient_membership_id::text,''),
 m.text_body,m.accepted_at,m.body_cleared_at,
 COALESCE(m.sender_organization_id::text,''),COALESCE(m.recipient_organization_id::text,'')
FROM messages m
WHERE m.tenant_id=$1 AND m.conversation_id=$2 AND m.seq>$3
ORDER BY m.seq LIMIT $4`, id.TenantID, conversationID, afterSeq, limit+1)
	if err != nil {
		return historyReadBatch{}, err
	}
	for rows.Next() {
		var messageID, senderUser, senderMember, recipientUser, recipientMember string
		var senderOrg, recipientOrg string
		var body *string
		var clearedAt *time.Time
		var seq int64
		var acceptedAt time.Time
		if err := rows.Scan(&messageID, &seq, &senderUser, &senderMember,
			&recipientUser, &recipientMember, &body, &acceptedAt, &clearedAt, &senderOrg, &recipientOrg); err != nil {
			rows.Close()
			return historyReadBatch{}, err
		}
		if len(page.Messages) == limit {
			page.HasMore = true
			break
		}
		item := PulledMessage{Seq: seq, Redacted: true}
		history := historicalPair{}
		validPair := (senderUser == lowUser && recipientUser == highUser) ||
			(senderUser == highUser && recipientUser == lowUser)
		if body != nil && clearedAt == nil && validPair && senderMember != "" && recipientMember != "" && senderOrg != "" && recipientOrg != "" {
			sender := policy.Membership{ID: senderMember, TenantID: id.TenantID, OrganizationID: senderOrg}
			recipient := policy.Membership{ID: recipientMember, TenantID: id.TenantID, OrganizationID: recipientOrg}
			historicalReader, peer := sender, recipient
			if strings.EqualFold(id.UserID, recipientUser) {
				historicalReader, peer = recipient, sender
			}
			item = PulledMessage{MessageID: messageID, Seq: seq, SenderUserID: senderUser,
				Text: *body, ServerTime: acceptedAt, Redacted: false}
			history = historicalPair{reader: historicalReader, peer: peer}
		}
		page.Messages = append(page.Messages, item)
		histories = append(histories, history)
		page.NextAfterSeq = seq
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return historyReadBatch{}, err
	}
	return historyReadBatch{Page: page, Direct: histories}, nil
}

func readGroupHistoryBatchTx(ctx context.Context, tx pgx.Tx, scope historyReadContext, groupID string, afterSeq int64, limit int) (historyReadBatch, error) {
	id := scope.Identity
	if limit < 1 || limit > 500 || afterSeq < 0 || !directoryUUIDPattern.MatchString(groupID) {
		return historyReadBatch{}, ErrInvalidMessageRequest
	}
	var locked string
	err := tx.QueryRow(ctx, `SELECT id::text FROM conversations WHERE tenant_id=$1 AND id=$2 AND kind='group' FOR SHARE`, id.TenantID, groupID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return historyReadBatch{}, ErrMessageNotAvailable
	}
	if err != nil {
		return historyReadBatch{}, err
	}
	err = tx.QueryRow(ctx, `SELECT id::text FROM conversation_membership_intervals WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3 LIMIT 1 FOR SHARE`, id.TenantID, groupID, id.UserID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return historyReadBatch{}, ErrMessageNotAvailable
	}
	if err != nil {
		return historyReadBatch{}, err
	}
	page := MessagePage{ConversationID: groupID,
		Messages: make([]PulledMessage, 0, min(limit, 100)), NextAfterSeq: afterSeq}
	rows, err := tx.Query(ctx, `SELECT id::text,seq,sender_user_id::text,sender_membership_id::text,text_body,accepted_at,body_cleared_at
 FROM messages WHERE tenant_id=$1 AND conversation_id=$2 AND seq>$3
 ORDER BY seq LIMIT $4`, id.TenantID, groupID, afterSeq, limit+1)
	if err != nil {
		return historyReadBatch{}, err
	}
	var messages []groupHistoryMessage
	senderIDs := map[string]bool{id.UserID: true}
	for rows.Next() {
		var message groupHistoryMessage
		if err := rows.Scan(&message.id, &message.seq, &message.senderID,
			&message.senderMembershipID, &message.text, &message.at, &message.clearedAt); err != nil {
			rows.Close()
			return historyReadBatch{}, err
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
		return historyReadBatch{}, err
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
			return historyReadBatch{}, err
		}
		for rows.Next() {
			var interval groupHistoryInterval
			if err := rows.Scan(&interval.userID, &interval.membershipID,
				&interval.organizationID, &interval.joinSeq, &interval.leaveSeq); err != nil {
				rows.Close()
				return historyReadBatch{}, err
			}
			intervals[interval.userID] = append(intervals[interval.userID], interval)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return historyReadBatch{}, err
		}
	}

	var readerIntervals []groupHistoryInterval
	rows, err = tx.Query(ctx, `SELECT user_id::text,source_membership_id::text,
 source_organization_id::text,join_seq,leave_seq
 FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3
 ORDER BY join_seq FOR SHARE`, id.TenantID, groupID, id.UserID)
	if err != nil {
		return historyReadBatch{}, err
	}
	for rows.Next() {
		var interval groupHistoryInterval
		if err := rows.Scan(&interval.userID, &interval.membershipID,
			&interval.organizationID, &interval.joinSeq, &interval.leaveSeq); err != nil {
			rows.Close()
			return historyReadBatch{}, err
		}
		readerIntervals = append(readerIntervals, interval)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return historyReadBatch{}, err
	}

	histories := make([]historicalPair, 0, len(messages))
	for _, message := range messages {
		item := PulledMessage{Seq: message.seq, Redacted: true}
		reader, readerFound := groupIntervalAt(intervals[id.UserID], message.seq)
		sender, senderFound := groupIntervalAt(intervals[message.senderID], message.seq)
		if message.text != nil && message.clearedAt == nil && readerFound && senderFound &&
			sender.membershipID == message.senderMembershipID {
			item = PulledMessage{MessageID: message.id, Seq: message.seq,
				SenderUserID: message.senderID, Text: *message.text, ServerTime: message.at}
		}
		histories = append(histories, historicalPair{reader: reader.policyMembership(id.TenantID), peer: sender.policyMembership(id.TenantID)})
		page.Messages = append(page.Messages, item)
		page.NextAfterSeq = message.seq
	}
	return historyReadBatch{Page: page, Direct: histories, ReaderIntervals: readerIntervals}, nil
}
