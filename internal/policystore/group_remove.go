package policystore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

var ErrGroupRemovePermissionDenied = errors.New("group member removal not permitted")

type GroupRemoveResult struct {
	IntervalID string
	Status     string
	LeaveSeq   int64
}

func auditGroupRemove(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, groupID, outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events
 (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
 VALUES ($1,$2,$3,'group_remove','conversation',$4,$5,$6,$7)`,
		id.TenantID, id.UserID, id.ActingMembershipID, groupID, outcome, reason, at)
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return nil
}

func finishGroupRemove(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, groupID, outcome, reason string, at time.Time) error {
	if err := auditGroupRemove(ctx, tx, id, groupID, outcome, reason, at); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RemoveGroupMember closes only the requested interval. A replay of a removed
// interval returns its stored end sequence after current role authorization.
func (s Service) RemoveGroupMember(ctx context.Context, id access.TrustedIdentity, groupID, intervalID string) (GroupRemoveResult, error) {
	if s.DB == nil {
		return GroupRemoveResult{}, ErrForbidden
	}
	id, groupID, err := normalizeGroupIdentity(id, groupID)
	if err != nil {
		return GroupRemoveResult{}, err
	}
	if !directoryUUIDPattern.MatchString(intervalID) {
		return GroupRemoveResult{}, ErrInvalidGroupMembershipRequest
	}
	intervalID = strings.ToLower(intervalID)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return GroupRemoveResult{}, err
	}
	defer tx.Rollback(ctx)
	actor, err := s.activeGroupActor(ctx, tx, id)
	if err != nil {
		return GroupRemoveResult{}, err
	}
	var groupStatus string
	var lastSeq int64
	err = tx.QueryRow(ctx, `SELECT status,last_seq FROM conversations
 WHERE tenant_id=$1 AND id=$2 AND kind='group' FOR UPDATE`, id.TenantID, groupID).
		Scan(&groupStatus, &lastSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupRemoveResult{}, ErrGroupNotAvailable
	}
	if err != nil {
		return GroupRemoveResult{}, err
	}
	var actorRole string
	err = tx.QueryRow(ctx, `SELECT role FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3 AND status='active' FOR SHARE`,
		id.TenantID, groupID, id.UserID).Scan(&actorRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupRemoveResult{}, ErrGroupNotAvailable
	}
	if err != nil {
		return GroupRemoveResult{}, err
	}
	at := s.now()
	if !memberActiveAt(actor, at) {
		return GroupRemoveResult{}, ErrForbidden
	}
	if actorRole != "owner" && actorRole != "admin" {
		if err := finishGroupRemove(ctx, tx, id, groupID, "deny", "group_permission_denied", at); err != nil {
			return GroupRemoveResult{}, err
		}
		return GroupRemoveResult{}, ErrGroupRemovePermissionDenied
	}
	var targetUserID, targetRole, targetStatus string
	var leaveSeq *int64
	var joinedAt time.Time
	err = tx.QueryRow(ctx, `SELECT user_id::text,role,status,leave_seq,joined_at
 FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND id=$3 FOR UPDATE`,
		id.TenantID, groupID, intervalID).Scan(&targetUserID, &targetRole, &targetStatus, &leaveSeq, &joinedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupRemoveResult{}, ErrGroupNotAvailable
	}
	if err != nil {
		return GroupRemoveResult{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !memberActiveAt(actor, at) {
		return GroupRemoveResult{}, ErrForbidden
	}
	if targetRole == "owner" {
		if err := finishGroupRemove(ctx, tx, id, groupID, "deny", "owner_transfer_required", at); err != nil {
			return GroupRemoveResult{}, err
		}
		return GroupRemoveResult{}, ErrGroupOwnerTransferRequired
	}
	if targetUserID == id.UserID || (actorRole == "admin" && targetRole == "admin") {
		if err := finishGroupRemove(ctx, tx, id, groupID, "deny", "group_permission_denied", at); err != nil {
			return GroupRemoveResult{}, err
		}
		return GroupRemoveResult{}, ErrGroupRemovePermissionDenied
	}
	if targetStatus == "removed" && leaveSeq != nil {
		if err := tx.Commit(ctx); err != nil {
			return GroupRemoveResult{}, err
		}
		return GroupRemoveResult{IntervalID: intervalID, Status: "removed", LeaveSeq: *leaveSeq}, nil
	}
	if targetStatus != "active" || (groupStatus != "active" && groupStatus != "policy_blocked") {
		return GroupRemoveResult{}, ErrGroupNotAvailable
	}
	if at.Before(joinedAt) {
		at = joinedAt
	}
	if _, err := tx.Exec(ctx, `UPDATE conversation_membership_intervals
 SET status='removed',leave_seq=$2,left_at=$3 WHERE id=$1 AND status='active'`, intervalID, lastSeq, at); err != nil {
		return GroupRemoveResult{}, err
	}
	if err := finishGroupRemove(ctx, tx, id, groupID, "allow", "removed", at); err != nil {
		return GroupRemoveResult{}, err
	}
	return GroupRemoveResult{IntervalID: intervalID, Status: "removed", LeaveSeq: lastSeq}, nil
}
