package policystore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
)

var (
	ErrInvalidGroupRequest  = errors.New("invalid group creation request")
	ErrGroupNotAvailable    = errors.New("group member or policy not available")
	ErrGroupRequestConflict = errors.New("group creation request conflict")
)

type CreateGroupRequest struct {
	ClientRequestID     string
	Name                string
	MemberMembershipIDs []string
}

type GroupConversation struct {
	ID            string
	LastSeq       int64
	PolicyVersion int64
	MemberCount   int
	Created       bool
}

type groupMember struct {
	userID     string
	membership policy.Membership
}

func normalizeGroupRequest(req CreateGroupRequest, actorMembershipID string) (CreateGroupRequest, [32]byte, error) {
	req.ClientRequestID = strings.ToLower(req.ClientRequestID)
	req.Name = strings.TrimSpace(req.Name)
	if !directoryUUIDPattern.MatchString(req.ClientRequestID) || !utf8.ValidString(req.Name) ||
		req.Name == "" || utf8.RuneCountInString(req.Name) > 120 ||
		len(req.MemberMembershipIDs) < 1 || len(req.MemberMembershipIDs) > 20 {
		return CreateGroupRequest{}, [32]byte{}, ErrInvalidGroupRequest
	}
	for _, r := range req.Name {
		if unicode.IsControl(r) {
			return CreateGroupRequest{}, [32]byte{}, ErrInvalidGroupRequest
		}
	}
	seen := map[string]bool{actorMembershipID: true}
	for i, membershipID := range req.MemberMembershipIDs {
		if !directoryUUIDPattern.MatchString(membershipID) {
			return CreateGroupRequest{}, [32]byte{}, ErrInvalidGroupRequest
		}
		membershipID = strings.ToLower(membershipID)
		if seen[membershipID] {
			return CreateGroupRequest{}, [32]byte{}, ErrInvalidGroupRequest
		}
		seen[membershipID] = true
		req.MemberMembershipIDs[i] = membershipID
	}
	slices.Sort(req.MemberMembershipIDs)
	payload, err := json.Marshal(struct {
		ActingMembershipID  string   `json:"acting_membership_id"`
		Name                string   `json:"name"`
		MemberMembershipIDs []string `json:"member_membership_ids"`
	}{actorMembershipID, req.Name, req.MemberMembershipIDs})
	if err != nil {
		return CreateGroupRequest{}, [32]byte{}, err
	}
	return req, sha256.Sum256(payload), nil
}

func auditGroupCreate(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, groupID,
	outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events
 (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
 VALUES ($1,$2,$3,'group_create','conversation',$4,$5,$6,$7)`,
		id.TenantID, id.UserID, id.ActingMembershipID, nullableID(groupID), outcome, reason, at)
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return nil
}

func finishGroupCreate(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, groupID,
	outcome, reason string, at time.Time) error {
	if err := auditGroupCreate(ctx, tx, id, groupID, outcome, reason, at); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func auditGroupPair(ctx context.Context, tx pgx.Tx, tenantID string, source, target groupMember,
	decision policy.Decision, at time.Time) error {
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
 VALUES ($1,$2,$3,$4,$5,'create_group',$6,$7,$8,$9,$10)`,
		tenantID, version, source.userID, source.membership.ID, target.membership.ID,
		decision.Allowed, decision.Reason, matched, overridden, at)
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return nil
}

func existingGroupCreation(ctx context.Context, tx pgx.Tx, tenantID, userID, requestID string,
	digest [32]byte) (GroupConversation, bool, error) {
	var result GroupConversation
	var stored []byte
	err := tx.QueryRow(ctx, `SELECT c.id::text,c.last_seq,c.last_policy_version,
 (SELECT count(*) FROM conversation_membership_intervals i
  WHERE i.tenant_id=c.tenant_id AND i.conversation_id=c.id AND i.join_seq=1),
 c.group_create_request_digest
 FROM conversations c
 WHERE c.tenant_id=$1 AND c.created_by_user_id=$2 AND c.group_create_request_id=$3
   AND c.kind='group' FOR SHARE OF c`, tenantID, userID, requestID).
		Scan(&result.ID, &result.LastSeq, &result.PolicyVersion, &result.MemberCount, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupConversation{}, false, nil
	}
	if err != nil {
		return GroupConversation{}, false, err
	}
	if len(stored) != len(digest) || !slices.Equal(stored, digest[:]) {
		return GroupConversation{}, true, ErrGroupRequestConflict
	}
	return result, true, nil
}

// CreateGroup creates the conversation and all initial membership intervals in
// one transaction after checking every directed pair against one policy snapshot.
func (s Service) CreateGroup(ctx context.Context, id access.TrustedIdentity, req CreateGroupRequest) (GroupConversation, error) {
	if s.DB == nil || !directoryUUIDPattern.MatchString(id.TenantID) ||
		!directoryUUIDPattern.MatchString(id.UserID) || !directoryUUIDPattern.MatchString(id.ActingMembershipID) {
		return GroupConversation{}, ErrForbidden
	}
	id.TenantID, id.UserID, id.ActingMembershipID = strings.ToLower(id.TenantID),
		strings.ToLower(id.UserID), strings.ToLower(id.ActingMembershipID)
	req.MemberMembershipIDs = slices.Clone(req.MemberMembershipIDs)
	req, digest, err := normalizeGroupRequest(req, id.ActingMembershipID)
	if err != nil {
		return GroupConversation{}, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return GroupConversation{}, err
	}
	defer tx.Rollback(ctx)

	allIDs := append(slices.Clone(req.MemberMembershipIDs), id.ActingMembershipID)
	slices.Sort(allIDs)
	rows, err := tx.Query(ctx, `SELECT id FROM user_organizations
 WHERE tenant_id=$1 AND id=ANY($2::uuid[]) ORDER BY id FOR SHARE NOWAIT`, id.TenantID, allIDs)
	if err != nil {
		return GroupConversation{}, err
	}
	for rows.Next() {
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return GroupConversation{}, err
	}
	var tenantID string
	if err := tx.QueryRow(ctx, "SELECT id::text FROM tenants WHERE id=$1 FOR SHARE", id.TenantID).Scan(&tenantID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GroupConversation{}, ErrForbidden
		}
		return GroupConversation{}, err
	}
	at := s.now()
	actor, found, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return GroupConversation{}, err
	}
	if !found || !memberActiveAt(actor, at) {
		if err := finishGroupCreate(ctx, tx, id, "", "deny", "invalid_identity", at); err != nil {
			return GroupConversation{}, err
		}
		return GroupConversation{}, ErrForbidden
	}
	existing, exists, err := existingGroupCreation(ctx, tx, id.TenantID, id.UserID, req.ClientRequestID, digest)
	if exists || err != nil {
		if errors.Is(err, ErrGroupRequestConflict) {
			if auditErr := finishGroupCreate(ctx, tx, id, "", "deny", "idempotency_conflict", at); auditErr != nil {
				return GroupConversation{}, auditErr
			}
			return GroupConversation{}, err
		}
		if err != nil {
			return GroupConversation{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return GroupConversation{}, err
		}
		return existing, nil
	}

	actorUserID, err := conversationMembershipUser(ctx, tx, id.TenantID, id.ActingMembershipID)
	if err != nil {
		return GroupConversation{}, err
	}
	members := []groupMember{{userID: actorUserID, membership: actor}}
	users := map[string]bool{actorUserID: true}
	for _, memberID := range req.MemberMembershipIDs {
		member, found, err := loadMembership(ctx, tx, id.TenantID, memberID, "")
		if err != nil {
			return GroupConversation{}, err
		}
		if !found || !memberActiveAt(member, at) {
			if err := finishGroupCreate(ctx, tx, id, "", "deny", "member_unavailable", at); err != nil {
				return GroupConversation{}, err
			}
			return GroupConversation{}, ErrGroupNotAvailable
		}
		userID, err := conversationMembershipUser(ctx, tx, id.TenantID, memberID)
		if err != nil {
			return GroupConversation{}, err
		}
		if users[userID] {
			if err := finishGroupCreate(ctx, tx, id, "", "deny", "duplicate_member", at); err != nil {
				return GroupConversation{}, err
			}
			return GroupConversation{}, ErrGroupNotAvailable
		}
		users[userID] = true
		members = append(members, groupMember{userID: userID, membership: member})
	}
	version, err := currentVersion(ctx, tx, id.TenantID)
	if err != nil {
		return GroupConversation{}, err
	}
	rules, err := loadRules(ctx, tx, id.TenantID, version)
	if err != nil {
		return GroupConversation{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT group_create_attempt"); err != nil {
		return GroupConversation{}, err
	}
	var deniedReason policy.Reason
	for sourceIndex, source := range members {
		for targetIndex, target := range members {
			if sourceIndex == targetIndex {
				continue
			}
			decision := policy.Evaluate(policy.Input{Action: policy.ActionCreateGroup,
				Actor: source.membership, Target: target.membership, At: at,
				ScopeAllowed: true, ResourceActive: true, PolicyVersion: version, Rules: rules})
			if err := auditGroupPair(ctx, tx, id.TenantID, source, target, decision, at); err != nil {
				return GroupConversation{}, err
			}
			if !decision.Allowed && deniedReason == "" {
				deniedReason = decision.Reason
			}
		}
	}
	if deniedReason != "" {
		if err := finishGroupCreate(ctx, tx, id, "", "deny", string(deniedReason), at); err != nil {
			return GroupConversation{}, err
		}
		return GroupConversation{}, ErrGroupNotAvailable
	}
	// Row locks prevent data edits, but a membership or timed rule can still
	// expire while the pairwise decision audit is being written.
	if finalAt := s.now(); finalAt.After(at) {
		for _, member := range members {
			if !memberActiveAt(member.membership, finalAt) {
				if err := finishGroupCreate(ctx, tx, id, "", "deny", "member_expired", finalAt); err != nil {
					return GroupConversation{}, err
				}
				return GroupConversation{}, ErrGroupNotAvailable
			}
		}
		for sourceIndex, source := range members {
			for targetIndex, target := range members {
				if sourceIndex == targetIndex {
					continue
				}
				decision := policy.Evaluate(policy.Input{Action: policy.ActionCreateGroup,
					Actor: source.membership, Target: target.membership, At: finalAt,
					ScopeAllowed: true, ResourceActive: true, PolicyVersion: version, Rules: rules})
				if !decision.Allowed {
					if err := auditGroupPair(ctx, tx, id.TenantID, source, target, decision, finalAt); err != nil {
						return GroupConversation{}, err
					}
					if err := finishGroupCreate(ctx, tx, id, "", "deny", string(decision.Reason), finalAt); err != nil {
						return GroupConversation{}, err
					}
					return GroupConversation{}, ErrGroupNotAvailable
				}
			}
		}
		at = finalAt
	}
	result := GroupConversation{PolicyVersion: version, MemberCount: len(members), Created: true}
	err = tx.QueryRow(ctx, `INSERT INTO conversations
 (tenant_id,kind,status,group_name,group_creator_membership_id,group_creator_organization_id,
  group_creator_legal_entity_id,created_by_user_id,last_policy_version,
  group_create_request_id,group_create_request_digest,updated_at)
 VALUES ($1,'group','active',$2,$3,$4,$5,$6,$7,$8,$9,$10)
 ON CONFLICT (tenant_id,created_by_user_id,group_create_request_id)
 WHERE kind='group' AND group_create_request_id IS NOT NULL DO NOTHING
 RETURNING id::text,last_seq`, id.TenantID, req.Name, id.ActingMembershipID,
		actor.OrganizationID, actor.LegalEntityID, id.UserID, version,
		req.ClientRequestID, digest[:], at).Scan(&result.ID, &result.LastSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT group_create_attempt"); err != nil {
			return GroupConversation{}, err
		}
		existing, exists, err := existingGroupCreation(ctx, tx, id.TenantID, id.UserID, req.ClientRequestID, digest)
		if errors.Is(err, ErrGroupRequestConflict) {
			if auditErr := finishGroupCreate(ctx, tx, id, "", "deny", "idempotency_conflict", at); auditErr != nil {
				return GroupConversation{}, auditErr
			}
			return GroupConversation{}, err
		}
		if err != nil {
			return GroupConversation{}, err
		}
		if !exists {
			return GroupConversation{}, errors.New("group creation request conflict without existing row")
		}
		if err := tx.Commit(ctx); err != nil {
			return GroupConversation{}, err
		}
		return existing, nil
	}
	if err != nil {
		return GroupConversation{}, err
	}
	for index, member := range members {
		role := "member"
		if index == 0 {
			role = "owner"
		}
		if _, err := tx.Exec(ctx, `INSERT INTO conversation_membership_intervals
 (tenant_id,conversation_id,user_id,source_membership_id,source_organization_id,
  source_legal_entity_id,role,status,join_seq,joined_policy_version,joined_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,'active',1,$8,$9)`,
			id.TenantID, result.ID, member.userID, member.membership.ID,
			member.membership.OrganizationID, member.membership.LegalEntityID,
			role, version, at); err != nil {
			return GroupConversation{}, err
		}
	}
	if err := finishGroupCreate(ctx, tx, id, result.ID, "allow", "created", at); err != nil {
		return GroupConversation{}, err
	}
	return result, nil
}
