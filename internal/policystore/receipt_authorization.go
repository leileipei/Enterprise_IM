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
	ErrInvalidReceiptRequest = errors.New("invalid receipt request")
	ErrSyncProofNotFound     = errors.New("sync proof not available")
	ErrSyncProofExpired      = errors.New("sync proof expired")
	ErrSyncProofQuota        = errors.New("sync proof quota exceeded")
	ErrSyncGap               = errors.New("sync page gap")
	ErrSyncContextChanged    = errors.New("sync context changed")
	ErrReceiptNotAvailable   = errors.New("receipt message not available")
)

type ReceiptAuthorizedMessage struct {
	Seq                         int64
	MessageID, ReaderIntervalID string
}

func normalizeReceiptRequest(id access.TrustedIdentity, device DeviceCredential, cid string) (access.TrustedIdentity, DeviceCredential, string, error) {
	normalized, e := normalizeDeviceIdentity(id)
	if e != nil {
		return access.TrustedIdentity{}, DeviceCredential{}, "", e
	}
	if !directoryUUIDPattern.MatchString(cid) {
		return access.TrustedIdentity{}, DeviceCredential{}, "", ErrInvalidReceiptRequest
	}
	if _, e = deviceDigest(device); e != nil {
		return access.TrustedIdentity{}, DeviceCredential{}, "", e
	}
	device.ID = strings.ToLower(device.ID)
	return normalized, device, strings.ToLower(cid), nil
}
func validReceiptSeqs(seqs []int64, maximum int) bool {
	if len(seqs) < 1 || len(seqs) > maximum {
		return false
	}
	previous := int64(0)
	for _, seq := range seqs {
		if seq <= previous {
			return false
		}
		previous = seq
	}
	return true
}
func receiptConversationKindTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, cid string) (string, error) {
	var kind string
	e := tx.QueryRow(ctx, "SELECT kind FROM conversations WHERE tenant_id=$1 AND id=$2 FOR SHARE", id.TenantID, cid).Scan(&kind)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", ErrReceiptNotAvailable
	}
	if e != nil {
		return "", e
	}
	if kind != "direct" && kind != "group" {
		return "", ErrReceiptNotAvailable
	}
	return kind, nil
}
func receiptHistoryBatchTx(ctx context.Context, tx pgx.Tx, scope historyReadContext, cid, kind string, after int64, limit int, seqs []int64) (historyReadBatch, error) {
	if kind == "direct" {
		return readDirectHistorySelectionTx(ctx, tx, scope, cid, after, limit, historyReadAll, seqs)
	}
	return readGroupHistorySelectionTx(ctx, tx, scope, cid, after, limit, historyReadAll, seqs)
}

// Fetch matching peer facts for future as well as active rules before reading
// the final clock. Activation during that database read must not evade filtering.
func receiptFinalPageTx(ctx context.Context, tx pgx.Tx, scope historyReadContext, batch historyReadBatch, kind string, now func() time.Time) (MessagePage, error) {
	var matched []policy.Rule
	if kind == "group" && len(batch.Page.Messages) > 0 && hasGroupHistoryHardDeny(scope.Rules) {
		var e error
		matched, e = groupHistoryMatchingDenials(ctx, tx, scope.Identity, batch.Page.ConversationID, scope.Actor, batch.ReaderIntervals, scope.Rules)
		if e != nil {
			return MessagePage{}, e
		}
	}
	return filterReceiptPage(scope, batch, kind, matched, now())
}
func filterReceiptPage(scope historyReadContext, batch historyReadBatch, kind string, matched []policy.Rule, at time.Time) (MessagePage, error) {
	if !memberActiveAt(scope.Actor, at) {
		return MessagePage{}, ErrForbidden
	}
	page := filterDirectHistoryBatch(scope, batch, at)
	if kind == "group" && groupHistoryDenialsActive(matched, at) {
		for i, m := range page.Messages {
			page.Messages[i] = PulledMessage{Seq: m.Seq, Redacted: true}
		}
	}
	return page, nil
}
func receiptPageItemsTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, page MessagePage, kind string) ([]ReceiptAuthorizedMessage, error) {
	type interval struct {
		id    string
		join  int64
		leave *int64
	}
	var intervals []interval
	if kind == "group" {
		rows, e := tx.Query(ctx, "SELECT id::text,join_seq,leave_seq FROM conversation_membership_intervals WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3 ORDER BY join_seq FOR SHARE", id.TenantID, page.ConversationID, id.UserID)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var i interval
			if e = rows.Scan(&i.id, &i.join, &i.leave); e != nil {
				rows.Close()
				return nil, e
			}
			intervals = append(intervals, i)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
	}
	items := make([]ReceiptAuthorizedMessage, 0, len(page.Messages))
	for _, m := range page.Messages {
		if m.Redacted || m.SenderUserID == id.UserID {
			continue
		}
		item := ReceiptAuthorizedMessage{Seq: m.Seq, MessageID: m.MessageID}
		if kind == "group" {
			for _, i := range intervals {
				if i.join <= m.Seq && (i.leave == nil || m.Seq <= *i.leave) {
					item.ReaderIntervalID = i.id
					break
				}
			}
			if item.ReaderIntervalID == "" {
				return nil, ErrReceiptNotAvailable
			}
		}
		items = append(items, item)
	}
	return items, nil
}

// Reuses the same final history filtering as sync. No current-membership
// shortcut and no receipt high-water mark can authorize a missing message.
func authorizeReceiptMessagesTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, cid string, seqs []int64, now func() time.Time) ([]ReceiptAuthorizedMessage, error) {
	if len(seqs) == 0 {
		return []ReceiptAuthorizedMessage{}, nil
	}
	if !validReceiptSeqs(seqs, 500) {
		return nil, ErrInvalidReceiptRequest
	}
	s := Service{Now: now}
	scope, e := s.newHistoryReadContextTx(ctx, tx, id, true)
	if e != nil {
		return nil, e
	}
	kind, e := receiptConversationKindTx(ctx, tx, id, cid)
	if e != nil {
		return nil, e
	}
	batch, e := receiptHistoryBatchTx(ctx, tx, scope, cid, kind, 0, len(seqs), seqs)
	if e != nil {
		return nil, e
	}
	page, e := receiptFinalPageTx(ctx, tx, scope, batch, kind, now)
	if e != nil {
		return nil, e
	}
	items, e := receiptPageItemsTx(ctx, tx, id, page, kind)
	if e != nil {
		return nil, e
	}
	if len(items) != len(seqs) {
		return nil, ErrReceiptNotAvailable
	}
	for i, item := range items {
		if item.Seq != seqs[i] {
			return nil, ErrReceiptNotAvailable
		}
	}
	return items, nil
}

func receiptStateTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, deviceID, cid string) (ReceiptState, error) {
	var state ReceiptState
	e := tx.QueryRow(ctx, `SELECT
 COALESCE((SELECT scanned_seq FROM device_delivery_cursors WHERE tenant_id=$1 AND user_id=$2 AND device_id=$3 AND conversation_id=$4 AND acting_membership_id=$5),0),
 COALESCE((SELECT delivered_seq FROM device_delivery_cursors WHERE tenant_id=$1 AND user_id=$2 AND device_id=$3 AND conversation_id=$4 AND acting_membership_id=$5),0),
 COALESCE((SELECT read_seq FROM conversation_user_read_cursors WHERE tenant_id=$1 AND user_id=$2 AND conversation_id=$4),0),
 COALESCE((SELECT revision FROM conversation_receipt_versions WHERE tenant_id=$1 AND conversation_id=$4),0)`, id.TenantID, id.UserID, deviceID, cid, id.ActingMembershipID).Scan(&state.ScannedSeq, &state.DeliveredSeq, &state.ReadSeq, &state.ReceiptRevision)
	return state, e
}
func auditReceiptTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, cid, action string, at time.Time) error {
	if e := auditMessageRead(ctx, tx, id, cid, "allow", "authorized_receipt", at, action); e != nil {
		return errors.Join(ErrAuditUnavailable, e)
	}
	return nil
}
