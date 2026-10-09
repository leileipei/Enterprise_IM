package policystore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

func syncProofDigest(token string) ([]byte, error) {
	if len(token) != 43 {
		return nil, ErrSyncProofNotFound
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(token)
	if e != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != token {
		return nil, ErrSyncProofNotFound
	}
	d := sha256.Sum256(raw)
	return d[:], nil
}

type receiptProof struct {
	id          string
	after, next int64
	expires     time.Time
	confirmed   *time.Time
}

func receiptProofTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, deviceID, cid string, digest []byte, lock bool) (receiptProof, error) {
	query := `SELECT id::text,after_seq,next_after_seq,expires_at,confirmed_at FROM device_sync_proofs WHERE tenant_id=$1 AND user_id=$2 AND device_id=$3 AND acting_membership_id=$4 AND conversation_id=$5 AND token_digest=$6`
	if lock {
		query += " FOR UPDATE"
	}
	var p receiptProof
	e := tx.QueryRow(ctx, query, id.TenantID, id.UserID, deviceID, id.ActingMembershipID, cid, digest).Scan(&p.id, &p.after, &p.next, &p.expires, &p.confirmed)
	if errors.Is(e, pgx.ErrNoRows) {
		return receiptProof{}, ErrSyncProofNotFound
	}
	return p, e
}
func proofReceiptItemsTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, proofID string) ([]ReceiptAuthorizedMessage, error) {
	rows, e := tx.Query(ctx, "SELECT seq,message_id::text,COALESCE(recipient_interval_id::text,'') FROM device_sync_proof_items WHERE tenant_id=$1 AND proof_id=$2 ORDER BY seq", id.TenantID, proofID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	items := []ReceiptAuthorizedMessage{}
	for rows.Next() {
		var i ReceiptAuthorizedMessage
		if e = rows.Scan(&i.Seq, &i.MessageID, &i.ReaderIntervalID); e != nil {
			return nil, e
		}
		items = append(items, i)
	}
	return items, rows.Err()
}
func validateProofItemsTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, cid string, items []ReceiptAuthorizedMessage, now func() time.Time) error {
	seqs := make([]int64, 0, len(items))
	for _, i := range items {
		seqs = append(seqs, i.Seq)
	}
	valid, e := authorizeReceiptMessagesTx(ctx, tx, id, cid, seqs, now)
	if errors.Is(e, ErrReceiptNotAvailable) || errors.Is(e, ErrMessageNotAvailable) {
		return ErrSyncContextChanged
	}
	if e != nil {
		return e
	}
	if len(valid) != len(items) {
		return ErrSyncContextChanged
	}
	for i, item := range items {
		if valid[i] != item {
			return ErrSyncContextChanged
		}
	}
	return nil
}
func insertedReceiptSeqs(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]int64, error) {
	rows, e := tx.Query(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	seqs := []int64{}
	for rows.Next() {
		var seq int64
		if e = rows.Scan(&seq); e != nil {
			return nil, e
		}
		seqs = append(seqs, seq)
	}
	return seqs, rows.Err()
}

func (s Service) confirmDelivered(ctx context.Context, id access.TrustedIdentity, device DeviceCredential, cid, token string) (ReceiptState, error) {
	id, device, cid, e := normalizeReceiptRequest(id, device, cid)
	if e != nil || s.DB == nil {
		if e == nil {
			e = ErrForbidden
		}
		return ReceiptState{}, e
	}
	digest, e := syncProofDigest(token)
	if e != nil {
		return ReceiptState{}, e
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return ReceiptState{}, e
	}
	defer tx.Rollback(ctx)
	if _, e = s.newHistoryReadContextTx(ctx, tx, id, true); e != nil {
		return ReceiptState{}, e
	}
	if _, e = clientPolicyTx(ctx, tx, id.TenantID, false); e != nil {
		return ReceiptState{}, e
	}
	if _, e = receiptConversationKindTx(ctx, tx, id, cid); e != nil {
		return ReceiptState{}, e
	}
	p, e := receiptProofTx(ctx, tx, id, device.ID, cid, digest, false)
	if e != nil {
		return ReceiptState{}, e
	}
	items, e := proofReceiptItemsTx(ctx, tx, id, p.id)
	if e != nil {
		return ReceiptState{}, e
	}
	if p.confirmed == nil && s.now().Before(p.expires) {
		if e = validateProofItemsTx(ctx, tx, id, cid, items, s.now); e != nil {
			return ReceiptState{}, e
		}
	}
	expiry, e := lockDeviceCredentialTx(ctx, tx, DeviceOwner{id.TenantID, id.UserID}, device, s.now(), false)
	if e != nil {
		return ReceiptState{}, e
	}
	p, e = receiptProofTx(ctx, tx, id, device.ID, cid, digest, true)
	if e != nil {
		return ReceiptState{}, e
	}
	if !s.now().Before(p.expires) {
		return ReceiptState{}, ErrSyncProofExpired
	}
	if p.confirmed != nil {
		if e = auditReceiptTx(ctx, tx, id, cid, "message_delivered", s.now()); e != nil {
			return ReceiptState{}, e
		}
		if _, e = s.activeGroupActor(ctx, tx, id); e != nil {
			return ReceiptState{}, e
		}
		if !s.now().Before(expiry) {
			return ReceiptState{}, ErrInvalidDevice
		}
		if !s.now().Before(p.expires) {
			return ReceiptState{}, ErrSyncProofExpired
		}
		state, e := receiptStateTx(ctx, tx, id, device.ID, cid)
		if e != nil {
			return ReceiptState{}, e
		}
		state.Duplicate = true
		if e = s.finalDeliveredCheckTx(ctx, tx, id, cid, items, expiry, p.expires, false); e != nil {
			return ReceiptState{}, e
		}
		if e = tx.Commit(ctx); e != nil {
			return ReceiptState{}, e
		}
		return state, nil
	}
	if _, e = tx.Exec(ctx, `INSERT INTO device_delivery_cursors(tenant_id,user_id,device_id,conversation_id,acting_membership_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, id.TenantID, id.UserID, device.ID, cid, id.ActingMembershipID); e != nil {
		return ReceiptState{}, e
	}
	var scanned, delivered int64
	if e = tx.QueryRow(ctx, `SELECT scanned_seq,delivered_seq FROM device_delivery_cursors WHERE tenant_id=$1 AND user_id=$2 AND device_id=$3 AND conversation_id=$4 AND acting_membership_id=$5 FOR UPDATE`, id.TenantID, id.UserID, device.ID, cid, id.ActingMembershipID).Scan(&scanned, &delivered); e != nil {
		return ReceiptState{}, e
	}
	if p.after > scanned {
		return ReceiptState{}, ErrSyncGap
	}
	seqs := make([]int64, 0, len(items))
	for _, item := range items {
		seqs = append(seqs, item.Seq)
		if item.Seq > delivered {
			delivered = item.Seq
		}
	}
	now := s.now()
	deviceChanges := []int64{}
	userChanges := []int64{}
	if len(seqs) > 0 {
		deviceChanges, e = insertedReceiptSeqs(ctx, tx, `INSERT INTO message_device_receipts(tenant_id,conversation_id,seq,user_id,device_id,delivered_at) SELECT $1,$2,seq,$3,$4,$6 FROM unnest($5::bigint[]) seq ORDER BY seq ON CONFLICT DO NOTHING RETURNING seq`, id.TenantID, cid, id.UserID, device.ID, seqs, now)
		if e != nil {
			return ReceiptState{}, e
		}
		userChanges, e = insertedReceiptSeqs(ctx, tx, `INSERT INTO message_user_receipts(tenant_id,conversation_id,seq,user_id,delivered_at) SELECT $1,$2,seq,$3,$5 FROM unnest($4::bigint[]) seq ORDER BY seq ON CONFLICT DO NOTHING RETURNING seq`, id.TenantID, cid, id.UserID, seqs, now)
		if e != nil {
			return ReceiptState{}, e
		}
	}
	next := max(scanned, p.next)
	if _, e = tx.Exec(ctx, `UPDATE device_delivery_cursors SET scanned_seq=$6,delivered_seq=$7,updated_at=$8 WHERE tenant_id=$1 AND user_id=$2 AND device_id=$3 AND conversation_id=$4 AND acting_membership_id=$5`, id.TenantID, id.UserID, device.ID, cid, id.ActingMembershipID, next, delivered, now); e != nil {
		return ReceiptState{}, e
	}
	if _, e = recordReceiptUserChangesTx(ctx, tx, id, cid, userChanges); e != nil {
		return ReceiptState{}, e
	}
	if e = auditReceiptTx(ctx, tx, id, cid, "message_delivered", now); e != nil {
		return ReceiptState{}, e
	}
	finalAt := s.now()
	if !finalAt.Before(expiry) {
		return ReceiptState{}, ErrInvalidDevice
	}
	if !finalAt.Before(p.expires) {
		return ReceiptState{}, ErrSyncProofExpired
	}
	if e = validateProofItemsTx(ctx, tx, id, cid, items, s.now); e != nil {
		return ReceiptState{}, e
	}
	if _, e = s.activeGroupActor(ctx, tx, id); e != nil {
		return ReceiptState{}, e
	}
	if _, e = tx.Exec(ctx, "UPDATE device_sync_proofs SET confirmed_at=$3 WHERE tenant_id=$1 AND id=$2", id.TenantID, p.id, finalAt); e != nil {
		return ReceiptState{}, e
	}
	state, e := receiptStateTx(ctx, tx, id, device.ID, cid)
	if e != nil {
		return ReceiptState{}, e
	}
	state.Duplicate = len(deviceChanges) == 0 && next == scanned
	if e = s.finalDeliveredCheckTx(ctx, tx, id, cid, items, expiry, p.expires, true); e != nil {
		return ReceiptState{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return ReceiptState{}, e
	}
	return state, nil
}

func (s Service) finalDeliveredCheckTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, cid string, items []ReceiptAuthorizedMessage, deviceExpiry, proofExpiry time.Time, authorize bool) error {
	at := s.now()
	if !at.Before(deviceExpiry) {
		return ErrInvalidDevice
	}
	if !at.Before(proofExpiry) {
		return ErrSyncProofExpired
	}
	if _, e := s.activeGroupActor(ctx, tx, id); e != nil {
		return e
	}
	if authorize {
		if e := validateProofItemsTx(ctx, tx, id, cid, items, s.now); e != nil {
			return e
		}
	}
	at = s.now()
	if !at.Before(deviceExpiry) {
		return ErrInvalidDevice
	}
	if !at.Before(proofExpiry) {
		return ErrSyncProofExpired
	}
	return nil
}
