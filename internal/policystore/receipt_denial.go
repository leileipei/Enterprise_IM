package policystore

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"time"
)

// Public adapters audit only a closed business-denial set. Inner operations have
// returned and rolled back every tentative fact, cursor, revision and allow audit
// before this separate, bounded audit transaction can begin.
func (s Service) receiptOperationError(ctx context.Context, id access.TrustedIdentity, action string, operation error) error {
	if operation == nil || s.DB == nil {
		return operation
	}
	reason := ""
	for _, entry := range []struct {
		err    error
		reason string
	}{{ErrReceiptNotAvailable, "receipt_unavailable"}, {ErrMessageNotAvailable, "receipt_unavailable"}, {ErrSyncProofNotFound, "sync_proof_unavailable"}, {ErrSyncProofExpired, "sync_proof_expired"}, {ErrSyncGap, "sync_gap"}, {ErrSyncContextChanged, "sync_context_changed"}, {ErrSyncProofQuota, "sync_proof_quota"}, {ErrInvalidDevice, "invalid_device"}, {ErrForbidden, "invalid_identity"}} {
		if errors.Is(operation, entry.err) {
			reason = entry.reason
			break
		}
	}
	if reason == "" {
		return operation
	}
	normalized, err := normalizeDeviceIdentity(id)
	if err != nil {
		return operation
	}
	auditCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tx, err := s.DB.Begin(auditCtx)
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	defer tx.Rollback(auditCtx)
	// Unknown/foreign resource identifiers are not copied to denial records. The
	// verified actor, closed action/reason and outcome provide the security receipt.
	if err = auditMessageRead(auditCtx, tx, normalized, "", "deny", reason, s.now(), action); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	if err = tx.Commit(auditCtx); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return operation
}
func (s Service) SyncDeviceMessages(ctx context.Context, id access.TrustedIdentity, device DeviceCredential, cid string, after int64, limit int) (DeviceSyncPage, error) {
	value, err := s.syncDeviceMessages(ctx, id, device, cid, after, limit)
	return value, s.receiptOperationError(ctx, id, "device_sync", err)
}
func (s Service) ConfirmDelivered(ctx context.Context, id access.TrustedIdentity, device DeviceCredential, cid, token string) (ReceiptState, error) {
	value, err := s.confirmDelivered(ctx, id, device, cid, token)
	return value, s.receiptOperationError(ctx, id, "message_delivered", err)
}
func (s Service) ConfirmRead(ctx context.Context, id access.TrustedIdentity, device DeviceCredential, cid string, seqs []int64) (ReceiptState, error) {
	value, err := s.confirmRead(ctx, id, device, cid, seqs)
	return value, s.receiptOperationError(ctx, id, "message_read_receipt", err)
}
func (s Service) QueryReceipts(ctx context.Context, id access.TrustedIdentity, device DeviceCredential, cid string, seqs []int64) ([]ReceiptSummary, error) {
	value, err := s.queryReceipts(ctx, id, device, cid, seqs)
	return value, s.receiptOperationError(ctx, id, "message_receipt_query", err)
}
func (s Service) ListReceiptUsers(ctx context.Context, id access.TrustedIdentity, device DeviceCredential, cid string, seq int64, cursor string, limit int) (ReceiptUserPage, error) {
	value, err := s.listReceiptUsers(ctx, id, device, cid, seq, cursor, limit)
	return value, s.receiptOperationError(ctx, id, "message_receipt_users", err)
}
func (s Service) GetUnread(ctx context.Context, id access.TrustedIdentity, device DeviceCredential, cid string) (UnreadState, error) {
	value, err := s.getUnread(ctx, id, device, cid)
	return value, s.receiptOperationError(ctx, id, "message_unread_query", err)
}
