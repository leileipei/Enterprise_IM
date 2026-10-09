package policystore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

func (s Service) syncDeviceMessages(ctx context.Context, id access.TrustedIdentity, device DeviceCredential, cid string, after int64, limit int) (DeviceSyncPage, error) {
	if after < 0 || limit < 1 || limit > 500 {
		return DeviceSyncPage{}, ErrInvalidReceiptRequest
	}
	id, device, cid, e := normalizeReceiptRequest(id, device, cid)
	if e != nil || s.DB == nil {
		if e == nil {
			e = ErrForbidden
		}
		return DeviceSyncPage{}, e
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return DeviceSyncPage{}, e
	}
	defer tx.Rollback(ctx)
	scope, e := s.newHistoryReadContextTx(ctx, tx, id, true)
	if e != nil {
		return DeviceSyncPage{}, e
	}
	config, e := clientPolicyTx(ctx, tx, id.TenantID, false)
	if e != nil {
		return DeviceSyncPage{}, e
	}
	kind, e := receiptConversationKindTx(ctx, tx, id, cid)
	if e != nil {
		return DeviceSyncPage{}, e
	}
	batch, e := receiptHistoryBatchTx(ctx, tx, scope, cid, kind, after, limit, nil)
	if e != nil {
		return DeviceSyncPage{}, e
	}
	expiry, e := lockDeviceCredentialTx(ctx, tx, DeviceOwner{id.TenantID, id.UserID}, device, s.now(), true)
	if e != nil {
		return DeviceSyncPage{}, e
	}
	if e = auditReceiptTx(ctx, tx, id, cid, "device_sync", s.now()); e != nil {
		return DeviceSyncPage{}, e
	}
	now := s.now()
	if !now.Before(expiry) {
		return DeviceSyncPage{}, ErrInvalidDevice
	}
	page, e := receiptFinalPageTx(ctx, tx, scope, batch, kind, s.now)
	if e != nil {
		return DeviceSyncPage{}, e
	}
	now = s.now()
	if !now.Before(expiry) {
		return DeviceSyncPage{}, ErrInvalidDevice
	}
	result := DeviceSyncPage{Page: page, ServerTime: now, ContentLeases: map[int64]ContentLease{}}
	state, e := receiptStateTx(ctx, tx, id, device.ID, cid)
	if e != nil {
		return DeviceSyncPage{}, e
	}
	result.ReceiptRevision = state.ReceiptRevision
	if len(page.Messages) > 0 {
		var count int
		e = tx.QueryRow(ctx, "SELECT count(*) FROM device_sync_proofs WHERE tenant_id=$1 AND user_id=$2 AND device_id=$3 AND confirmed_at IS NULL AND expires_at>$4", id.TenantID, id.UserID, device.ID, now).Scan(&count)
		if e != nil {
			return DeviceSyncPage{}, e
		}
		if count >= 20 {
			return DeviceSyncPage{}, ErrSyncProofQuota
		}
		raw := make([]byte, 32)
		if _, e = rand.Read(raw); e != nil {
			return DeviceSyncPage{}, e
		}
		digest := sha256.Sum256(raw)
		result.SyncProof = base64.RawURLEncoding.EncodeToString(raw)
		result.ProofExpiresAt = now.Add(300 * time.Second)
		var proofID string
		e = tx.QueryRow(ctx, `INSERT INTO device_sync_proofs(tenant_id,user_id,device_id,acting_membership_id,conversation_id,token_digest,after_seq,next_after_seq,issued_at,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id::text`, id.TenantID, id.UserID, device.ID, id.ActingMembershipID, cid, digest[:], after, page.NextAfterSeq, now, result.ProofExpiresAt).Scan(&proofID)
		if e != nil {
			return DeviceSyncPage{}, e
		}
		items, e := receiptPageItemsTx(ctx, tx, id, page, kind)
		if e != nil {
			return DeviceSyncPage{}, e
		}
		rows := make([][]any, 0, len(items))
		for _, item := range items {
			rows = append(rows, []any{id.TenantID, proofID, cid, id.UserID, device.ID, id.ActingMembershipID, item.Seq, item.MessageID, nullableID(item.ReaderIntervalID)})
		}
		if len(rows) > 0 {
			if _, e = tx.CopyFrom(ctx, pgx.Identifier{"device_sync_proof_items"}, []string{"tenant_id", "proof_id", "conversation_id", "user_id", "device_id", "acting_membership_id", "seq", "message_id", "recipient_interval_id"}, pgx.CopyFromRows(rows)); e != nil {
				return DeviceSyncPage{}, e
			}
		}
	}
	finalAt := s.now()
	if !finalAt.Before(expiry) {
		return DeviceSyncPage{}, ErrInvalidDevice
	}
	final, e := receiptFinalPageTx(ctx, tx, scope, batch, kind, s.now)
	if e != nil {
		return DeviceSyncPage{}, e
	}
	for i, m := range final.Messages {
		if m.Redacted != page.Messages[i].Redacted {
			return DeviceSyncPage{}, ErrSyncContextChanged
		}
	}
	finalAt = s.now()
	if !finalAt.Before(expiry) {
		return DeviceSyncPage{}, ErrInvalidDevice
	}
	if result.SyncProof != "" && !finalAt.Before(result.ProofExpiresAt) {
		return DeviceSyncPage{}, ErrSyncProofExpired
	}
	for _, m := range page.Messages {
		if m.Redacted {
			continue
		}
		contentExpiry := m.ServerTime.Add(scope.Retention)
		cacheUntil := finalAt.Add(time.Duration(config.OfflineCacheSeconds) * time.Second)
		if expiry.Before(cacheUntil) {
			cacheUntil = expiry
		}
		if contentExpiry.Before(cacheUntil) {
			cacheUntil = contentExpiry
		}
		lease := ContentLease{ContentExpiresAt: contentExpiry, CacheUntil: cacheUntil}
		if config.OfflineCacheSeconds == 0 {
			lease.OnlineUntil = finalAt.Add(5 * time.Second)
			if expiry.Before(lease.OnlineUntil) {
				lease.OnlineUntil = expiry
			}
			if contentExpiry.Before(lease.OnlineUntil) {
				lease.OnlineUntil = contentExpiry
			}
		}
		result.ContentLeases[m.Seq] = lease
	}
	result.ServerTime = finalAt
	if e = tx.Commit(ctx); e != nil {
		return DeviceSyncPage{}, e
	}
	return result, nil
}
