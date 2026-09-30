package policystore

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
)

var (
	ErrInvalidGroupInviteRequest   = errors.New("invalid group invitation request")
	ErrGroupInvitePermissionDenied = errors.New("group invitation requires owner or administrator")
	ErrGroupPolicyBlocked          = errors.New("group policy is blocked")
	ErrGroupInviteConflict         = errors.New("group invitation request conflict")
)

type InviteGroupRequest struct {
	ClientRequestID    string
	TargetMembershipID string
}

type GroupInvitation struct {
	IntervalID    string
	JoinSeq       int64
	PolicyVersion int64
	Created       bool
}

type inviteMemberRef struct {
	userID       string
	membershipID string
}

func auditGroupInvite(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, groupID, outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events
 (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
 VALUES ($1,$2,$3,'group_invite','conversation',$4,$5,$6,$7)`,
		id.TenantID, id.UserID, id.ActingMembershipID, groupID, outcome, reason, at)
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return nil
}

func finishGroupInvite(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, groupID, outcome, reason string, at time.Time) error {
	if err := auditGroupInvite(ctx, tx, id, groupID, outcome, reason, at); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func auditInvitePair(ctx context.Context, tx pgx.Tx, tenantID string, source, target groupMember, decision policy.Decision, at time.Time) error {
	var version any
	if decision.PolicyVersion > 0 {
		version = decision.PolicyVersion
	}
	matched, overridden := decision.MatchedRuleIDs, decision.OverriddenRuleIDs
	if matched == nil {
		matched = []string{}
	}
	if overridden == nil {
		overridden = []string{}
	}
	_, err := tx.Exec(ctx, `INSERT INTO policy_decision_events
 (tenant_id,policy_version,actor_user_id,actor_membership_id,target_membership_id,
  action,allowed,reason,matched_rule_ids,overridden_rule_ids,occurred_at)
 VALUES ($1,$2,$3,$4,$5,'invite_group',$6,$7,$8,$9,$10)`,
		tenantID, version, source.userID, source.membership.ID, target.membership.ID,
		decision.Allowed, decision.Reason, matched, overridden, at)
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return nil
}

func loadInviteMembers(ctx context.Context, tx pgx.Tx, tenantID, groupID string) ([]inviteMemberRef, error) {
	rows, err := tx.Query(ctx, `SELECT user_id::text,source_membership_id::text
 FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND status='active'
 ORDER BY user_id FOR SHARE`, tenantID, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []inviteMemberRef
	for rows.Next() {
		var member inviteMemberRef
		if err := rows.Scan(&member.userID, &member.membershipID); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

func existingGroupInvitation(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, groupID, requestID string, digest [32]byte) (GroupInvitation, bool, error) {
	var result GroupInvitation
	var stored []byte
	err := tx.QueryRow(ctx, `SELECT i.id::text,i.join_seq,i.joined_policy_version,r.request_digest
 FROM group_invitation_requests r
 JOIN conversation_membership_intervals i ON i.id=r.interval_id
 WHERE r.tenant_id=$1 AND r.group_id=$2 AND r.inviter_user_id=$3 AND r.request_id=$4
 FOR SHARE OF r,i`, id.TenantID, groupID, id.UserID, requestID).
		Scan(&result.IntervalID, &result.JoinSeq, &result.PolicyVersion, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupInvitation{}, false, nil
	}
	if err != nil {
		return GroupInvitation{}, false, err
	}
	if !slices.Equal(stored, digest[:]) {
		return GroupInvitation{}, true, ErrGroupInviteConflict
	}
	return result, true, nil
}

func evaluateInvitePairs(ctx context.Context, tx pgx.Tx, tenantID string, existing []groupMember, target groupMember, at time.Time, version int64, rules []policy.Rule, audit bool) (policy.Reason, error) {
	var denied policy.Reason
	for _, member := range existing {
		for _, pair := range [][2]groupMember{{member, target}, {target, member}} {
			decision := policy.Evaluate(policy.Input{Action: policy.ActionInviteGroup,
				Actor: pair[0].membership, Target: pair[1].membership, At: at,
				ScopeAllowed: true, ResourceActive: true, PolicyVersion: version, Rules: rules})
			if audit {
				if err := auditInvitePair(ctx, tx, tenantID, pair[0], pair[1], decision, at); err != nil {
					return "", err
				}
			}
			if !decision.Allowed && denied == "" {
				denied = decision.Reason
			}
		}
	}
	return denied, nil
}

// InviteGroupMember records a single invitation and its policy decisions in
// one transaction. The group row serializes invitations with member exits.
func (s Service) InviteGroupMember(ctx context.Context, id access.TrustedIdentity, groupID string, req InviteGroupRequest) (GroupInvitation, error) {
	if s.DB == nil {
		return GroupInvitation{}, ErrForbidden
	}
	id, groupID, err := normalizeGroupIdentity(id, groupID)
	if err != nil {
		return GroupInvitation{}, err
	}
	if !directoryUUIDPattern.MatchString(req.ClientRequestID) || !directoryUUIDPattern.MatchString(req.TargetMembershipID) {
		return GroupInvitation{}, ErrInvalidGroupInviteRequest
	}
	req.ClientRequestID = strings.ToLower(req.ClientRequestID)
	req.TargetMembershipID = strings.ToLower(req.TargetMembershipID)
	digest := sha256.Sum256([]byte(id.ActingMembershipID + ":" + req.TargetMembershipID))
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return GroupInvitation{}, err
	}
	defer tx.Rollback(ctx)
	actor, err := s.activeGroupActor(ctx, tx, id)
	if err != nil {
		return GroupInvitation{}, err
	}
	var groupStatus string
	var lastSeq int64
	err = tx.QueryRow(ctx, `SELECT status,last_seq FROM conversations
 WHERE tenant_id=$1 AND id=$2 AND kind='group' FOR UPDATE`, id.TenantID, groupID).
		Scan(&groupStatus, &lastSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupInvitation{}, ErrGroupNotAvailable
	}
	if err != nil {
		return GroupInvitation{}, err
	}
	var role string
	err = tx.QueryRow(ctx, `SELECT role FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3 AND status='active' FOR SHARE`,
		id.TenantID, groupID, id.UserID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupInvitation{}, ErrGroupNotAvailable
	}
	if err != nil {
		return GroupInvitation{}, err
	}
	at := s.now()
	if !memberActiveAt(actor, at) {
		return GroupInvitation{}, ErrForbidden
	}
	if role != "owner" && role != "admin" {
		if err := finishGroupInvite(ctx, tx, id, groupID, "deny", "group_permission_denied", at); err != nil {
			return GroupInvitation{}, err
		}
		return GroupInvitation{}, ErrGroupInvitePermissionDenied
	}
	existing, found, err := existingGroupInvitation(ctx, tx, id, groupID, req.ClientRequestID, digest)
	if errors.Is(err, ErrGroupInviteConflict) {
		if auditErr := finishGroupInvite(ctx, tx, id, groupID, "deny", "idempotency_conflict", at); auditErr != nil {
			return GroupInvitation{}, auditErr
		}
		return GroupInvitation{}, err
	}
	if err != nil {
		return GroupInvitation{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return GroupInvitation{}, err
		}
		return existing, nil
	}
	if groupStatus == "policy_blocked" {
		if err := finishGroupInvite(ctx, tx, id, groupID, "deny", "group_policy_blocked", at); err != nil {
			return GroupInvitation{}, err
		}
		return GroupInvitation{}, ErrGroupPolicyBlocked
	}
	if groupStatus != "active" {
		return GroupInvitation{}, ErrGroupNotAvailable
	}
	refs, err := loadInviteMembers(ctx, tx, id.TenantID, groupID)
	if err != nil {
		return GroupInvitation{}, err
	}
	ids := []string{req.TargetMembershipID, id.ActingMembershipID}
	for _, ref := range refs {
		ids = append(ids, ref.membershipID)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	rows, err := tx.Query(ctx, `SELECT id FROM user_organizations
 WHERE tenant_id=$1 AND id=ANY($2::uuid[]) ORDER BY id FOR SHARE NOWAIT`, id.TenantID, ids)
	if err != nil {
		return GroupInvitation{}, err
	}
	for rows.Next() {
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return GroupInvitation{}, err
	}
	targetMembership, found, err := loadMembership(ctx, tx, id.TenantID, req.TargetMembershipID, "")
	if err != nil {
		return GroupInvitation{}, err
	}
	if !found || !memberActiveAt(targetMembership, at) {
		if err := finishGroupInvite(ctx, tx, id, groupID, "deny", "member_unavailable", at); err != nil {
			return GroupInvitation{}, err
		}
		return GroupInvitation{}, ErrGroupNotAvailable
	}
	targetUserID, err := conversationMembershipUser(ctx, tx, id.TenantID, req.TargetMembershipID)
	if err != nil {
		return GroupInvitation{}, err
	}
	for _, ref := range refs {
		if ref.userID == targetUserID {
			if err := finishGroupInvite(ctx, tx, id, groupID, "deny", "already_member", at); err != nil {
				return GroupInvitation{}, err
			}
			return GroupInvitation{}, ErrGroupNotAvailable
		}
	}
	participants := make([]groupMember, 0, len(refs)+1)
	actingIncluded := false
	for _, ref := range refs {
		member, found, err := loadMembership(ctx, tx, id.TenantID, ref.membershipID, ref.userID)
		if err != nil {
			return GroupInvitation{}, err
		}
		if !found {
			return GroupInvitation{}, ErrGroupNotAvailable
		}
		participants = append(participants, groupMember{userID: ref.userID, membership: member})
		if ref.userID == id.UserID && ref.membershipID == id.ActingMembershipID {
			actingIncluded = true
		}
	}
	if !actingIncluded {
		participants = append(participants, groupMember{userID: id.UserID, membership: actor})
	}
	version, err := currentVersion(ctx, tx, id.TenantID)
	if err != nil {
		return GroupInvitation{}, err
	}
	rules, err := loadRules(ctx, tx, id.TenantID, version)
	if err != nil {
		return GroupInvitation{}, err
	}
	target := groupMember{userID: targetUserID, membership: targetMembership}
	denied, err := evaluateInvitePairs(ctx, tx, id.TenantID, participants, target, at, version, rules, true)
	if err != nil {
		return GroupInvitation{}, err
	}
	if denied != "" {
		if err := finishGroupInvite(ctx, tx, id, groupID, "deny", string(denied), at); err != nil {
			return GroupInvitation{}, err
		}
		return GroupInvitation{}, ErrGroupNotAvailable
	}
	if finalAt := s.now(); finalAt.After(at) {
		at = finalAt
		finalDenied, err := evaluateInvitePairs(ctx, tx, id.TenantID, participants, target, at, version, rules, false)
		if err != nil {
			return GroupInvitation{}, err
		}
		if finalDenied != "" {
			if _, err := evaluateInvitePairs(ctx, tx, id.TenantID, participants, target, at, version, rules, true); err != nil {
				return GroupInvitation{}, err
			}
			if err := finishGroupInvite(ctx, tx, id, groupID, "deny", string(finalDenied), at); err != nil {
				return GroupInvitation{}, err
			}
			return GroupInvitation{}, ErrGroupNotAvailable
		}
	}
	result := GroupInvitation{JoinSeq: lastSeq + 1, PolicyVersion: version, Created: true}
	err = tx.QueryRow(ctx, `INSERT INTO conversation_membership_intervals
 (tenant_id,conversation_id,user_id,source_membership_id,source_organization_id,
  source_legal_entity_id,role,status,join_seq,joined_policy_version,joined_at)
 VALUES ($1,$2,$3,$4,$5,$6,'member','active',$7,$8,$9) RETURNING id::text`,
		id.TenantID, groupID, targetUserID, req.TargetMembershipID,
		targetMembership.OrganizationID, targetMembership.LegalEntityID,
		result.JoinSeq, version, at).Scan(&result.IntervalID)
	if err != nil {
		return GroupInvitation{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO group_invitation_requests
 (tenant_id,group_id,inviter_user_id,request_id,acting_membership_id,
  target_membership_id,interval_id,request_digest,created_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id.TenantID, groupID, id.UserID,
		req.ClientRequestID, id.ActingMembershipID, req.TargetMembershipID,
		result.IntervalID, digest[:], at)
	if err != nil {
		return GroupInvitation{}, err
	}
	if _, err := tx.Exec(ctx, "UPDATE conversations SET last_policy_version=$2,updated_at=$3 WHERE id=$1", groupID, version, at); err != nil {
		return GroupInvitation{}, err
	}
	if err := finishGroupInvite(ctx, tx, id, groupID, "allow", "invited", at); err != nil {
		return GroupInvitation{}, err
	}
	return result, nil
}
