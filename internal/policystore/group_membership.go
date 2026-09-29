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

var (
	ErrInvalidGroupMembershipRequest = errors.New("invalid group membership request")
	ErrGroupOwnerTransferRequired    = errors.New("group owner must transfer ownership before leaving")
)

type GroupMembership struct {
	IntervalID  string
	Role        string
	JoinSeq     int64
	GroupStatus string
}

type GroupLeaveResult struct {
	IntervalID string
	Status     string
	LeaveSeq   int64
}

func normalizeGroupIdentity(id access.TrustedIdentity, groupID string) (access.TrustedIdentity, string, error) {
	if !directoryUUIDPattern.MatchString(id.TenantID) || !directoryUUIDPattern.MatchString(id.UserID) ||
		!directoryUUIDPattern.MatchString(id.ActingMembershipID) {
		return access.TrustedIdentity{}, "", ErrForbidden
	}
	if !directoryUUIDPattern.MatchString(groupID) {
		return access.TrustedIdentity{}, "", ErrInvalidGroupMembershipRequest
	}
	id.TenantID, id.UserID, id.ActingMembershipID = strings.ToLower(id.TenantID),
		strings.ToLower(id.UserID), strings.ToLower(id.ActingMembershipID)
	return id, strings.ToLower(groupID), nil
}

func (s Service) activeGroupActor(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity) (policy.Membership, error) {
	member, found, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return policy.Membership{}, err
	}
	if !found || !memberActiveAt(member, s.now()) {
		return policy.Membership{}, ErrForbidden
	}
	return member, nil
}

// GetOwnGroupMembership returns only the caller's current interval. A user
// may act through any of their currently valid organizational memberships.
func (s Service) GetOwnGroupMembership(ctx context.Context, id access.TrustedIdentity, groupID string) (GroupMembership, error) {
	if s.DB == nil {
		return GroupMembership{}, ErrForbidden
	}
	id, groupID, err := normalizeGroupIdentity(id, groupID)
	if err != nil {
		return GroupMembership{}, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return GroupMembership{}, err
	}
	defer tx.Rollback(ctx)
	actor, err := s.activeGroupActor(ctx, tx, id)
	if err != nil {
		return GroupMembership{}, err
	}
	var result GroupMembership
	err = tx.QueryRow(ctx, `SELECT i.id::text,i.role,i.join_seq,c.status
 FROM conversations c JOIN conversation_membership_intervals i
   ON i.tenant_id=c.tenant_id AND i.conversation_id=c.id
 WHERE c.tenant_id=$1 AND c.id=$2 AND c.kind='group' AND c.status IN ('active','policy_blocked')
   AND i.user_id=$3 AND i.status='active'
 FOR SHARE OF c,i`, id.TenantID, groupID, id.UserID).
		Scan(&result.IntervalID, &result.Role, &result.JoinSeq, &result.GroupStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupMembership{}, ErrGroupNotAvailable
	}
	if err != nil {
		return GroupMembership{}, err
	}
	if !memberActiveAt(actor, s.now()) {
		return GroupMembership{}, ErrForbidden
	}
	if err := tx.Commit(ctx); err != nil {
		return GroupMembership{}, err
	}
	return result, nil
}

func auditGroupLeave(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, groupID, outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events
 (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
 VALUES ($1,$2,$3,'group_leave','conversation',$4,$5,$6,$7)`,
		id.TenantID, id.UserID, id.ActingMembershipID, groupID, outcome, reason, at)
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return nil
}

// LeaveGroup closes exactly one membership interval at the group's current
// message sequence. Replaying an already-left interval returns its old result.
func (s Service) LeaveGroup(ctx context.Context, id access.TrustedIdentity, groupID, intervalID string) (GroupLeaveResult, error) {
	if s.DB == nil {
		return GroupLeaveResult{}, ErrForbidden
	}
	id, groupID, err := normalizeGroupIdentity(id, groupID)
	if err != nil {
		return GroupLeaveResult{}, err
	}
	if !directoryUUIDPattern.MatchString(intervalID) {
		return GroupLeaveResult{}, ErrInvalidGroupMembershipRequest
	}
	intervalID = strings.ToLower(intervalID)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return GroupLeaveResult{}, err
	}
	defer tx.Rollback(ctx)
	actor, err := s.activeGroupActor(ctx, tx, id)
	if err != nil {
		return GroupLeaveResult{}, err
	}
	var groupStatus string
	var lastSeq int64
	err = tx.QueryRow(ctx, `SELECT status,last_seq FROM conversations
 WHERE tenant_id=$1 AND id=$2 AND kind='group' FOR UPDATE`, id.TenantID, groupID).
		Scan(&groupStatus, &lastSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupLeaveResult{}, ErrGroupNotAvailable
	}
	if err != nil {
		return GroupLeaveResult{}, err
	}
	var role, status string
	var leaveSeq *int64
	var joinedAt time.Time
	err = tx.QueryRow(ctx, `SELECT role,status,leave_seq,joined_at
 FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3 AND id=$4 FOR UPDATE`,
		id.TenantID, groupID, id.UserID, intervalID).Scan(&role, &status, &leaveSeq, &joinedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupLeaveResult{}, ErrGroupNotAvailable
	}
	if err != nil {
		return GroupLeaveResult{}, err
	}
	at := s.now()
	if !memberActiveAt(actor, at) {
		return GroupLeaveResult{}, ErrForbidden
	}
	if status == "left" && leaveSeq != nil {
		if err := tx.Commit(ctx); err != nil {
			return GroupLeaveResult{}, err
		}
		return GroupLeaveResult{IntervalID: intervalID, Status: "left", LeaveSeq: *leaveSeq}, nil
	}
	if status != "active" || (groupStatus != "active" && groupStatus != "policy_blocked") {
		return GroupLeaveResult{}, ErrGroupNotAvailable
	}
	if role == "owner" {
		if err := auditGroupLeave(ctx, tx, id, groupID, "deny", "owner_transfer_required", at); err != nil {
			return GroupLeaveResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return GroupLeaveResult{}, err
		}
		return GroupLeaveResult{}, ErrGroupOwnerTransferRequired
	}
	if at.Before(joinedAt) {
		at = joinedAt
	}
	if _, err := tx.Exec(ctx, `UPDATE conversation_membership_intervals
 SET status='left',leave_seq=$2,left_at=$3 WHERE id=$1 AND status='active'`, intervalID, lastSeq, at); err != nil {
		return GroupLeaveResult{}, err
	}
	if err := auditGroupLeave(ctx, tx, id, groupID, "allow", "left", at); err != nil {
		return GroupLeaveResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return GroupLeaveResult{}, err
	}
	return GroupLeaveResult{IntervalID: intervalID, Status: "left", LeaveSeq: lastSeq}, nil
}
