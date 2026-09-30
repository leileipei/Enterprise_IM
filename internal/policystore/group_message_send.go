package policystore

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
)

type groupSendFailure struct {
	source, target groupMember
	decision       policy.Decision
}

func loadGroupSendMemberships(ctx context.Context, tx pgx.Tx, tenantID string,
	refs []inviteMemberRef) (map[string]policy.Membership, error) {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.membershipID)
	}
	rows, err := tx.Query(ctx, `SELECT m.id::text,m.tenant_id::text,m.organization_id::text,
 o.legal_entity_id::text,u.status,
 CASE WHEN t.status='active' AND o.status='active' AND l.status='active' THEN m.status ELSE 'suspended' END,
 m.effective_from,m.effective_to
 FROM user_organizations m
 JOIN users u ON u.tenant_id=m.tenant_id AND u.id=m.user_id
 JOIN organizations o ON o.tenant_id=m.tenant_id AND o.id=m.organization_id
 JOIN legal_entities l ON l.tenant_id=o.tenant_id AND l.id=o.legal_entity_id
 JOIN tenants t ON t.id=m.tenant_id
 WHERE m.tenant_id=$1 AND m.id=ANY($2::uuid[])
 ORDER BY m.id FOR SHARE OF t,u,m,o,l`, tenantID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := make(map[string]policy.Membership, len(refs))
	for rows.Next() {
		var member policy.Membership
		var end *time.Time
		if err := rows.Scan(&member.ID, &member.TenantID, &member.OrganizationID,
			&member.LegalEntityID, &member.AccountStatus, &member.Status,
			&member.EffectiveFrom, &end); err != nil {
			return nil, err
		}
		if end != nil {
			member.EffectiveTo = *end
		}
		members[member.ID] = member
	}
	return members, rows.Err()
}

func auditGroupSendAllows(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity,
	members []groupMember, actor policy.Membership, at time.Time, version int64,
	rules []policy.Rule) error {
	rows := make([][]any, 0, len(members)-1)
	for _, member := range members {
		if member.userID == id.UserID {
			continue
		}
		decision := policy.Evaluate(policy.Input{Action: policy.ActionSendMessage,
			Actor: actor, Target: member.membership, At: at, ScopeAllowed: true,
			ResourceActive: true, PolicyVersion: version, Rules: rules})
		var policyVersion any
		if decision.PolicyVersion > 0 {
			policyVersion = decision.PolicyVersion
		}
		matched, overridden := decision.MatchedRuleIDs, decision.OverriddenRuleIDs
		if matched == nil {
			matched = []string{}
		}
		if overridden == nil {
			overridden = []string{}
		}
		rows = append(rows, []any{id.TenantID, policyVersion, id.UserID,
			id.ActingMembershipID, member.membership.ID, policy.ActionSendMessage,
			decision.Allowed, decision.Reason, matched, overridden, at})
	}
	if len(rows) == 0 {
		return nil
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"policy_decision_events"}, []string{
		"tenant_id", "policy_version", "actor_user_id", "actor_membership_id",
		"target_membership_id", "action", "allowed", "reason", "matched_rule_ids",
		"overridden_rule_ids", "occurred_at",
	}, pgx.CopyFromRows(rows))
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return nil
}

func evaluateGroupSendPairs(members []groupMember, at time.Time, version int64,
	rules []policy.Rule) *groupSendFailure {
	// When no send rule selects individual memberships, every active member
	// with the same organization and legal entity has the same pair decision.
	// Keep the full pair walk for membership-specific exceptions and denials.
	perMembership := false
	for _, rule := range rules {
		if rule.Action == policy.ActionSendMessage &&
			(rule.SourceMembershipID != "" || rule.TargetMembershipID != "") {
			perMembership = true
			break
		}
	}
	if !perMembership {
		type membershipClass struct{ organizationID, legalEntityID string }
		classes := make(map[membershipClass][]groupMember)
		order := make([]membershipClass, 0)
		for _, member := range members {
			key := membershipClass{member.membership.OrganizationID, member.membership.LegalEntityID}
			if _, exists := classes[key]; !exists {
				order = append(order, key)
			}
			classes[key] = append(classes[key], member)
		}
		for _, sourceClass := range order {
			for _, targetClass := range order {
				source, target := classes[sourceClass][0], classes[targetClass][0]
				if source.userID == target.userID {
					if len(classes[targetClass]) < 2 {
						continue
					}
					target = classes[targetClass][1]
				}
				decision := policy.Evaluate(policy.Input{Action: policy.ActionSendMessage,
					Actor: source.membership, Target: target.membership, At: at,
					ScopeAllowed: true, ResourceActive: true, PolicyVersion: version, Rules: rules})
				if !decision.Allowed {
					return &groupSendFailure{source: source, target: target, decision: decision}
				}
			}
		}
		return nil
	}
	for sourceIndex, source := range members {
		for targetIndex, target := range members {
			if sourceIndex == targetIndex {
				continue
			}
			decision := policy.Evaluate(policy.Input{Action: policy.ActionSendMessage,
				Actor: source.membership, Target: target.membership, At: at,
				ScopeAllowed: true, ResourceActive: true, PolicyVersion: version, Rules: rules})
			if !decision.Allowed {
				return &groupSendFailure{source: source, target: target, decision: decision}
			}
		}
	}
	return nil
}

func blockGroupSend(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity,
	groupID, reason string, version int64, at time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE conversations
 SET status='policy_blocked',last_policy_version=$3,updated_at=$4
 WHERE tenant_id=$1 AND id=$2 AND kind='group'`, id.TenantID, groupID, version, at); err != nil {
		return err
	}
	return finishMessageSend(ctx, tx, id, groupID, "deny", reason, at)
}

func auditGroupSendDenial(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity,
	failure *groupSendFailure, at time.Time) error {
	req := Request{Identity: access.TrustedIdentity{TenantID: id.TenantID,
		UserID: failure.source.userID, ActingMembershipID: failure.source.membership.ID},
		TargetMembershipID: failure.target.membership.ID, Action: policy.ActionSendMessage,
		ScopeAllowed: true, ResourceActive: true}
	if err := auditDecision(ctx, tx, req, failure.decision, at); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return nil
}

func recheckGroupSendTime(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity,
	groupID string, actor policy.Membership, members []groupMember, rules []policy.Rule,
	version int64, decidedAt, fresh time.Time) error {
	if !fresh.After(decidedAt) {
		return nil
	}
	rollbackProvisional := func() error {
		_, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT group_send_provisional")
		return err
	}
	if !memberActiveAt(actor, fresh) {
		if err := rollbackProvisional(); err != nil {
			return err
		}
		if err := finishMessageSend(ctx, tx, id, "", "deny", "invalid_identity", fresh); err != nil {
			return err
		}
		return ErrForbidden
	}
	for _, member := range members {
		if !memberActiveAt(member.membership, fresh) {
			if err := rollbackProvisional(); err != nil {
				return err
			}
			if err := blockGroupSend(ctx, tx, id, groupID, "member_unavailable", version, fresh); err != nil {
				return err
			}
			return ErrGroupPolicyBlocked
		}
	}
	if failure := evaluateGroupSendPairs(members, fresh, version, rules); failure != nil {
		if err := rollbackProvisional(); err != nil {
			return err
		}
		if err := auditGroupSendDenial(ctx, tx, id, failure, fresh); err != nil {
			return err
		}
		if err := blockGroupSend(ctx, tx, id, groupID, string(failure.decision.Reason), version, fresh); err != nil {
			return err
		}
		return ErrGroupPolicyBlocked
	}
	return nil
}

// SendGroupTextMessage serializes with group membership changes on the group
// row. ACK is returned only after message, idempotency key, outbox and audit
// commit together.
func (s Service) SendGroupTextMessage(ctx context.Context, id access.TrustedIdentity,
	groupID, clientMessageID, body string) (MessageACK, error) {
	// Other group operations and administrators acquire actor, group and peer
	// locks in different orders. PostgreSQL aborts one participant of a detected
	// lock cycle; retry the entire atomic send with a fresh authorization check.
	for attempt := 0; attempt < 3; attempt++ {
		ack, err := s.sendGroupTextMessageOnce(ctx, id, groupID, clientMessageID, body)
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "40P01" ||
			attempt == 2 || ctx.Err() != nil {
			return ack, err
		}
	}
	return MessageACK{}, ErrPolicyUnavailable
}

func (s Service) sendGroupTextMessageOnce(ctx context.Context, id access.TrustedIdentity,
	groupID, clientMessageID, body string) (MessageACK, error) {
	if !directoryUUIDPattern.MatchString(groupID) {
		return MessageACK{}, ErrInvalidMessageRequest
	}
	if s.DB == nil || !directoryUUIDPattern.MatchString(id.TenantID) ||
		!directoryUUIDPattern.MatchString(id.UserID) || !directoryUUIDPattern.MatchString(id.ActingMembershipID) {
		return MessageACK{}, ErrForbidden
	}
	id.TenantID, id.UserID, id.ActingMembershipID = strings.ToLower(id.TenantID),
		strings.ToLower(id.UserID), strings.ToLower(id.ActingMembershipID)
	groupID = strings.ToLower(groupID)
	maxRate := s.MessageRatePerSecond
	if maxRate == 0 {
		maxRate = 10
	}
	if maxRate < 0 {
		return MessageACK{}, ErrInvalidMessageRequest
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return MessageACK{}, err
	}
	defer tx.Rollback(ctx)
	at := s.now()
	actor, found, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return MessageACK{}, err
	}
	if !found || !memberActiveAt(actor, at) {
		if err := finishMessageSend(ctx, tx, id, "", "deny", "invalid_identity", at); err != nil {
			return MessageACK{}, err
		}
		return MessageACK{}, ErrForbidden
	}
	if err := validateClientMessageID(clientMessageID, at); err != nil {
		return MessageACK{}, err
	}
	if !utf8.ValidString(body) || strings.TrimSpace(body) == "" ||
		strings.ContainsRune(body, 0) || len(body) > 16*1024 {
		return MessageACK{}, ErrInvalidTextMessage
	}
	digest := sha256.Sum256([]byte(body))
	var groupStatus string
	var lastSeq int64
	err = tx.QueryRow(ctx, `SELECT status,last_seq FROM conversations
 WHERE tenant_id=$1 AND id=$2 AND kind='group' FOR UPDATE`, id.TenantID, groupID).
		Scan(&groupStatus, &lastSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return MessageACK{}, ErrMessageNotAvailable
	}
	if err != nil {
		return MessageACK{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !memberActiveAt(actor, at) {
		if err := finishMessageSend(ctx, tx, id, "", "deny", "invalid_identity", at); err != nil {
			return MessageACK{}, err
		}
		return MessageACK{}, ErrForbidden
	}
	// Verify historical participation before revealing either the group state or
	// an idempotent ACK. Former members may replay their own previously accepted
	// message, while a user who was never a member sees the same unavailable
	// result for active and blocked groups.
	var actorInterval, actorOtherMembership bool
	intervals, err := tx.Query(ctx, `SELECT source_membership_id::text
 FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3 FOR SHARE`,
		id.TenantID, groupID, id.UserID)
	if err != nil {
		return MessageACK{}, err
	}
	for intervals.Next() {
		var membershipID string
		if err := intervals.Scan(&membershipID); err != nil {
			intervals.Close()
			return MessageACK{}, err
		}
		if membershipID == id.ActingMembershipID {
			actorInterval = true
		} else {
			actorOtherMembership = true
		}
	}
	err = intervals.Err()
	intervals.Close()
	if err != nil {
		return MessageACK{}, err
	}
	if !actorInterval {
		if actorOtherMembership {
			if err := finishMessageSend(ctx, tx, id, groupID, "deny", "conversation_context_changed", at); err != nil {
				return MessageACK{}, err
			}
			return MessageACK{}, ErrConversationContextChanged
		}
		if err := finishMessageSend(ctx, tx, id, "", "deny", "group_member_unavailable", at); err != nil {
			return MessageACK{}, err
		}
		return MessageACK{}, ErrMessageNotAvailable
	}
	if ack, exists, same, err := existingMessageACK(ctx, tx, id.TenantID, groupID,
		id.UserID, clientMessageID, digest); err != nil {
		return MessageACK{}, err
	} else if exists {
		return finishExistingMessage(ctx, tx, id, ack, same, at)
	}
	if groupStatus == "policy_blocked" {
		if err := finishMessageSend(ctx, tx, id, groupID, "deny", "group_policy_blocked", at); err != nil {
			return MessageACK{}, err
		}
		return MessageACK{}, ErrGroupPolicyBlocked
	}
	if groupStatus != "active" {
		return MessageACK{}, ErrMessageNotAvailable
	}
	refs, err := loadInviteMembers(ctx, tx, id.TenantID, groupID)
	if err != nil {
		return MessageACK{}, err
	}
	actorInterval = false
	actorOtherMembership = false
	for _, ref := range refs {
		if ref.userID == id.UserID {
			if ref.membershipID == id.ActingMembershipID {
				actorInterval = true
			} else {
				actorOtherMembership = true
			}
		}
	}
	if !actorInterval {
		if actorOtherMembership {
			if err := finishMessageSend(ctx, tx, id, groupID, "deny", "conversation_context_changed", at); err != nil {
				return MessageACK{}, err
			}
			return MessageACK{}, ErrConversationContextChanged
		}
		if err := finishMessageSend(ctx, tx, id, "", "deny", "group_member_unavailable", at); err != nil {
			return MessageACK{}, err
		}
		return MessageACK{}, ErrMessageNotAvailable
	}
	version, err := currentVersion(ctx, tx, id.TenantID)
	if err != nil {
		return MessageACK{}, err
	}
	rules, err := loadRules(ctx, tx, id.TenantID, version)
	if err != nil {
		return MessageACK{}, err
	}
	loaded, err := loadGroupSendMemberships(ctx, tx, id.TenantID, refs)
	if err != nil {
		return MessageACK{}, err
	}
	members := make([]groupMember, 0, len(refs))
	for _, ref := range refs {
		member, memberFound := loaded[ref.membershipID]
		if !memberFound || !memberActiveAt(member, at) {
			if err := blockGroupSend(ctx, tx, id, groupID, "member_unavailable", version, at); err != nil {
				return MessageACK{}, err
			}
			return MessageACK{}, ErrGroupPolicyBlocked
		}
		members = append(members, groupMember{userID: ref.userID, membership: member})
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !memberActiveAt(actor, at) {
		if err := finishMessageSend(ctx, tx, id, "", "deny", "invalid_identity", at); err != nil {
			return MessageACK{}, err
		}
		return MessageACK{}, ErrForbidden
	}
	for _, member := range members {
		if !memberActiveAt(member.membership, at) {
			if err := blockGroupSend(ctx, tx, id, groupID, "member_unavailable", version, at); err != nil {
				return MessageACK{}, err
			}
			return MessageACK{}, ErrGroupPolicyBlocked
		}
	}
	if failure := evaluateGroupSendPairs(members, at, version, rules); failure != nil {
		if err := auditGroupSendDenial(ctx, tx, id, failure, at); err != nil {
			return MessageACK{}, err
		}
		if err := blockGroupSend(ctx, tx, id, groupID, string(failure.decision.Reason), version, at); err != nil {
			return MessageACK{}, err
		}
		return MessageACK{}, ErrGroupPolicyBlocked
	}
	if err := auditGroupSendAllows(ctx, tx, id, members, actor, at, version, rules); err != nil {
		return MessageACK{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
		if !memberActiveAt(actor, at) {
			if err := finishMessageSend(ctx, tx, id, "", "deny", "invalid_identity", at); err != nil {
				return MessageACK{}, err
			}
			return MessageACK{}, ErrForbidden
		}
		for _, member := range members {
			if !memberActiveAt(member.membership, at) {
				if err := blockGroupSend(ctx, tx, id, groupID, "member_unavailable", version, at); err != nil {
					return MessageACK{}, err
				}
				return MessageACK{}, ErrGroupPolicyBlocked
			}
		}
		if failure := evaluateGroupSendPairs(members, at, version, rules); failure != nil {
			if err := auditGroupSendDenial(ctx, tx, id, failure, at); err != nil {
				return MessageACK{}, err
			}
			if err := blockGroupSend(ctx, tx, id, groupID, string(failure.decision.Reason), version, at); err != nil {
				return MessageACK{}, err
			}
			return MessageACK{}, ErrGroupPolicyBlocked
		}
	}
	// Rate reservation and message writes are provisional: a lock wait can cross
	// a policy or membership boundary after the earlier decision.
	if _, err := tx.Exec(ctx, "SAVEPOINT group_send_provisional"); err != nil {
		return MessageACK{}, err
	}
	allowed, err := reserveMessageRate(ctx, tx, id.TenantID, id.UserID, at, maxRate)
	if err != nil {
		return MessageACK{}, err
	}
	if !allowed {
		fresh := s.now()
		if err := recheckGroupSendTime(ctx, tx, id, groupID, actor, members, rules, version, at, fresh); err != nil {
			return MessageACK{}, err
		}
		if fresh.After(at) {
			at = fresh
		}
		if err := finishMessageSend(ctx, tx, id, groupID, "deny", "rate_limited", at); err != nil {
			return MessageACK{}, err
		}
		return MessageACK{}, ErrMessageRateLimited
	}
	ack := MessageACK{ConversationID: groupID, ServerTime: at}
	err = tx.QueryRow(ctx, `UPDATE conversations SET last_seq=last_seq+1,
 last_policy_version=$3,updated_at=$4 WHERE tenant_id=$1 AND id=$2 RETURNING last_seq`,
		id.TenantID, groupID, version, at).Scan(&ack.Seq)
	if err != nil {
		return MessageACK{}, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO messages
 (tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,
  client_msg_id,text_body,content_digest,accepted_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id::text`, id.TenantID, groupID,
		ack.Seq, id.UserID, id.ActingMembershipID, clientMessageID, body, digest[:], at).Scan(&ack.MessageID)
	if err != nil {
		return MessageACK{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO message_idempotency
 (tenant_id,conversation_id,sender_user_id,client_msg_id,message_id,content_digest,accepted_at,expires_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id.TenantID, groupID,
		id.UserID, clientMessageID, ack.MessageID, digest[:], at, at.Add(30*24*time.Hour))
	if err != nil {
		return MessageACK{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events
 (tenant_id,conversation_id,seq,message_id,event_type,next_retry_at,created_at)
 VALUES ($1,$2,$3,$4,'message_created',$5,$5)`, id.TenantID, groupID,
		ack.Seq, ack.MessageID, at)
	if err != nil {
		return MessageACK{}, err
	}
	if err := recheckGroupSendTime(ctx, tx, id, groupID, actor, members, rules, version, at, s.now()); err != nil {
		return MessageACK{}, err
	}
	if err := finishMessageSend(ctx, tx, id, groupID, "allow", "group_send", at); err != nil {
		return MessageACK{}, err
	}
	return ack, nil
}
