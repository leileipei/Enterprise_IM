package policystore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

var (
	ErrInvalidGroupOwnerTransferRequest   = errors.New("invalid group owner transfer request")
	ErrGroupOwnerTransferPermissionDenied = errors.New("group owner transfer requires current owner")
	ErrGroupOwnerTransferConflict         = errors.New("group owner transfer request conflict")
)

type GroupOwnerTransferRequest struct {
	ClientRequestID  string
	SourceIntervalID string
	TargetIntervalID string
}

type GroupOwnerTransfer struct {
	SourceIntervalID string
	TargetIntervalID string
	Created          bool
}

func auditGroupOwnerTransfer(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity,
	groupID, outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events
 (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
 VALUES ($1,$2,$3,'group_owner_transfer','conversation',$4,$5,$6,$7)`,
		id.TenantID, id.UserID, id.ActingMembershipID, groupID, outcome, reason, at)
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return nil
}

func finishGroupOwnerTransfer(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity,
	groupID, outcome, reason string, at time.Time) error {
	if err := auditGroupOwnerTransfer(ctx, tx, id, groupID, outcome, reason, at); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TransferGroupOwner changes only current roles. The request ledger makes a
// replay safe even after ownership is transferred again.
func (s Service) TransferGroupOwner(ctx context.Context, id access.TrustedIdentity, groupID string,
	req GroupOwnerTransferRequest) (GroupOwnerTransfer, error) {
	if s.DB == nil {
		return GroupOwnerTransfer{}, ErrForbidden
	}
	id, groupID, err := normalizeGroupIdentity(id, groupID)
	if err != nil {
		return GroupOwnerTransfer{}, err
	}
	if !directoryUUIDPattern.MatchString(req.ClientRequestID) ||
		!directoryUUIDPattern.MatchString(req.SourceIntervalID) ||
		!directoryUUIDPattern.MatchString(req.TargetIntervalID) {
		return GroupOwnerTransfer{}, ErrInvalidGroupOwnerTransferRequest
	}
	req.ClientRequestID = strings.ToLower(req.ClientRequestID)
	req.SourceIntervalID = strings.ToLower(req.SourceIntervalID)
	req.TargetIntervalID = strings.ToLower(req.TargetIntervalID)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return GroupOwnerTransfer{}, err
	}
	defer tx.Rollback(ctx)
	actor, found, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return GroupOwnerTransfer{}, err
	}
	if !found {
		return GroupOwnerTransfer{}, ErrForbidden
	}
	at := s.now()
	if !memberActiveAt(actor, at) {
		if err := finishGroupOwnerTransfer(ctx, tx, id, groupID, "deny", "invalid_identity", at); err != nil {
			return GroupOwnerTransfer{}, err
		}
		return GroupOwnerTransfer{}, ErrForbidden
	}
	var groupStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM conversations
 WHERE tenant_id=$1 AND id=$2 AND kind='group' FOR UPDATE`, id.TenantID, groupID).Scan(&groupStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupOwnerTransfer{}, ErrGroupNotAvailable
	}
	if err != nil {
		return GroupOwnerTransfer{}, err
	}
	at = s.now()
	if !memberActiveAt(actor, at) {
		if err := finishGroupOwnerTransfer(ctx, tx, id, groupID, "deny", "invalid_identity", at); err != nil {
			return GroupOwnerTransfer{}, err
		}
		return GroupOwnerTransfer{}, ErrForbidden
	}
	var storedActing, storedSource, storedTarget string
	err = tx.QueryRow(ctx, `SELECT acting_membership_id::text,source_interval_id::text,target_interval_id::text
 FROM group_owner_transfer_requests
 WHERE tenant_id=$1 AND group_id=$2 AND actor_user_id=$3 AND request_id=$4 FOR SHARE`,
		id.TenantID, groupID, id.UserID, req.ClientRequestID).Scan(&storedActing, &storedSource, &storedTarget)
	if err == nil {
		if storedActing != id.ActingMembershipID || storedSource != req.SourceIntervalID || storedTarget != req.TargetIntervalID {
			if err := finishGroupOwnerTransfer(ctx, tx, id, groupID, "deny", "idempotency_conflict", at); err != nil {
				return GroupOwnerTransfer{}, err
			}
			return GroupOwnerTransfer{}, ErrGroupOwnerTransferConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return GroupOwnerTransfer{}, err
		}
		return GroupOwnerTransfer{SourceIntervalID: storedSource, TargetIntervalID: storedTarget}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return GroupOwnerTransfer{}, err
	}
	if req.SourceIntervalID == req.TargetIntervalID {
		return GroupOwnerTransfer{}, ErrInvalidGroupOwnerTransferRequest
	}
	if groupStatus != "active" && groupStatus != "policy_blocked" {
		return GroupOwnerTransfer{}, ErrGroupNotAvailable
	}
	var sourceRole, sourceStatus string
	err = tx.QueryRow(ctx, `SELECT role,status FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND id=$3 AND user_id=$4 FOR UPDATE`,
		id.TenantID, groupID, req.SourceIntervalID, id.UserID).Scan(&sourceRole, &sourceStatus)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && sourceStatus != "active") {
		return GroupOwnerTransfer{}, ErrGroupNotAvailable
	}
	if err != nil {
		return GroupOwnerTransfer{}, err
	}
	if sourceRole != "owner" {
		if err := finishGroupOwnerTransfer(ctx, tx, id, groupID, "deny", "group_permission_denied", at); err != nil {
			return GroupOwnerTransfer{}, err
		}
		return GroupOwnerTransfer{}, ErrGroupOwnerTransferPermissionDenied
	}
	var targetUserID, targetMembershipID, targetStatus string
	err = tx.QueryRow(ctx, `SELECT user_id::text,source_membership_id::text,status
 FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND id=$3 FOR UPDATE`,
		id.TenantID, groupID, req.TargetIntervalID).Scan(&targetUserID, &targetMembershipID, &targetStatus)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (targetStatus != "active" || targetUserID == id.UserID)) {
		return GroupOwnerTransfer{}, ErrGroupNotAvailable
	}
	if err != nil {
		return GroupOwnerTransfer{}, err
	}
	// A target leaving the group locks its organizational membership before the
	// group row. NOWAIT avoids waiting here while holding the group lock.
	var lockedID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM user_organizations
 WHERE tenant_id=$1 AND id=$2 FOR SHARE NOWAIT`, id.TenantID, targetMembershipID).Scan(&lockedID)
	if err != nil {
		return GroupOwnerTransfer{}, err
	}
	target, found, err := loadMembership(ctx, tx, id.TenantID, targetMembershipID, targetUserID)
	if err != nil {
		return GroupOwnerTransfer{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !memberActiveAt(actor, at) {
		if err := finishGroupOwnerTransfer(ctx, tx, id, groupID, "deny", "invalid_identity", at); err != nil {
			return GroupOwnerTransfer{}, err
		}
		return GroupOwnerTransfer{}, ErrForbidden
	}
	if !found || !memberActiveAt(target, at) {
		return GroupOwnerTransfer{}, ErrGroupNotAvailable
	}
	if _, err := tx.Exec(ctx, `UPDATE conversation_membership_intervals SET role='member'
 WHERE tenant_id=$1 AND conversation_id=$2 AND id=$3 AND status='active' AND role='owner'`,
		id.TenantID, groupID, req.SourceIntervalID); err != nil {
		return GroupOwnerTransfer{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE conversation_membership_intervals SET role='owner'
 WHERE tenant_id=$1 AND conversation_id=$2 AND id=$3 AND status='active'`,
		id.TenantID, groupID, req.TargetIntervalID); err != nil {
		return GroupOwnerTransfer{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO group_owner_transfer_requests
 (tenant_id,group_id,actor_user_id,request_id,acting_membership_id,source_interval_id,target_interval_id,created_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id.TenantID, groupID, id.UserID, req.ClientRequestID,
		id.ActingMembershipID, req.SourceIntervalID, req.TargetIntervalID, at)
	if err != nil {
		return GroupOwnerTransfer{}, err
	}
	if err := finishGroupOwnerTransfer(ctx, tx, id, groupID, "allow", "transferred", at); err != nil {
		return GroupOwnerTransfer{}, err
	}
	return GroupOwnerTransfer{SourceIntervalID: req.SourceIntervalID, TargetIntervalID: req.TargetIntervalID, Created: true}, nil
}
