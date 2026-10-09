package policystore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

func lockOwnReceiptFactsTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, deviceID, cid string, seqs []int64) error {
	rows, e := tx.Query(ctx, `SELECT seq FROM message_device_receipts WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3 AND device_id=$4 AND seq=ANY($5::bigint[]) ORDER BY seq FOR UPDATE`, id.TenantID, cid, id.UserID, deviceID, seqs)
	if e != nil {
		return e
	}
	found := []int64{}
	for rows.Next() {
		var seq int64
		if e = rows.Scan(&seq); e != nil {
			rows.Close()
			return e
		}
		found = append(found, seq)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if len(found) != len(seqs) {
		return ErrReceiptNotAvailable
	}
	for i, seq := range seqs {
		if found[i] != seq {
			return ErrReceiptNotAvailable
		}
	}
	rows, e = tx.Query(ctx, `SELECT seq FROM message_user_receipts WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3 AND seq=ANY($4::bigint[]) ORDER BY seq FOR UPDATE`, id.TenantID, cid, id.UserID, seqs)
	if e != nil {
		return e
	}
	count := 0
	for rows.Next() {
		count++
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if count != len(seqs) {
		return errors.New("receipt fact pair unavailable")
	}
	return nil
}

func (s Service) ConfirmRead(ctx context.Context, id access.TrustedIdentity, device DeviceCredential, cid string, seqs []int64) (ReceiptState, error) {
	if !validReceiptSeqs(seqs, 500) {
		return ReceiptState{}, ErrInvalidReceiptRequest
	}
	id, device, cid, e := normalizeReceiptRequest(id, device, cid)
	if e != nil || s.DB == nil {
		if e == nil {
			e = ErrForbidden
		}
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
	if _, e = authorizeReceiptMessagesTx(ctx, tx, id, cid, seqs, s.now); e != nil {
		return ReceiptState{}, e
	}
	expiry, e := lockDeviceCredentialTx(ctx, tx, DeviceOwner{id.TenantID, id.UserID}, device, s.now(), false)
	if e != nil {
		return ReceiptState{}, e
	}
	// This scope's scan row can be absent after a valid acting-membership switch.
	// Creating zero progress never fills a delivery gap: authorization uses facts.
	if _, e = tx.Exec(ctx, `INSERT INTO device_delivery_cursors(tenant_id,user_id,device_id,conversation_id,acting_membership_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, id.TenantID, id.UserID, device.ID, cid, id.ActingMembershipID); e != nil {
		return ReceiptState{}, e
	}
	var scanned int64
	if e = tx.QueryRow(ctx, `SELECT scanned_seq FROM device_delivery_cursors WHERE tenant_id=$1 AND user_id=$2 AND device_id=$3 AND conversation_id=$4 AND acting_membership_id=$5 FOR UPDATE`, id.TenantID, id.UserID, device.ID, cid, id.ActingMembershipID).Scan(&scanned); e != nil {
		return ReceiptState{}, e
	}
	if e = lockOwnReceiptFactsTx(ctx, tx, id, device.ID, cid, seqs); e != nil {
		return ReceiptState{}, e
	}
	now := s.now()
	deviceChanges, e := insertedReceiptSeqs(ctx, tx, `UPDATE message_device_receipts SET read_at=$6 WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3 AND device_id=$4 AND seq=ANY($5::bigint[]) AND read_at IS NULL RETURNING seq`, id.TenantID, cid, id.UserID, device.ID, seqs, now)
	if e != nil {
		return ReceiptState{}, e
	}
	userChanges, e := insertedReceiptSeqs(ctx, tx, `UPDATE message_user_receipts SET read_at=$5 WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3 AND seq=ANY($4::bigint[]) AND read_at IS NULL RETURNING seq`, id.TenantID, cid, id.UserID, seqs, now)
	if e != nil {
		return ReceiptState{}, e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO conversation_user_read_cursors(tenant_id,user_id,conversation_id,read_seq,updated_at) VALUES($1,$2,$3,0,$4) ON CONFLICT DO NOTHING`, id.TenantID, id.UserID, cid, now); e != nil {
		return ReceiptState{}, e
	}
	var readSeq int64
	if e = tx.QueryRow(ctx, "SELECT read_seq FROM conversation_user_read_cursors WHERE tenant_id=$1 AND user_id=$2 AND conversation_id=$3 FOR UPDATE", id.TenantID, id.UserID, cid).Scan(&readSeq); e != nil {
		return ReceiptState{}, e
	}
	if _, e = tx.Exec(ctx, "UPDATE conversation_user_read_cursors SET read_seq=$4,updated_at=$5 WHERE tenant_id=$1 AND user_id=$2 AND conversation_id=$3", id.TenantID, id.UserID, cid, max(readSeq, seqs[len(seqs)-1]), now); e != nil {
		return ReceiptState{}, e
	}
	if _, e = recordReceiptUserChangesTx(ctx, tx, id, cid, userChanges); e != nil {
		return ReceiptState{}, e
	}
	if e = auditReceiptTx(ctx, tx, id, cid, "message_read_receipt", now); e != nil {
		return ReceiptState{}, e
	}
	state, e := receiptStateTx(ctx, tx, id, device.ID, cid)
	if e != nil {
		return ReceiptState{}, e
	}
	state.Duplicate = len(deviceChanges) == 0
	if e = s.finalReadCheckTx(ctx, tx, id, device.ID, cid, seqs, expiry); e != nil {
		return ReceiptState{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return ReceiptState{}, e
	}
	return state, nil
}

func (s Service) finalReadCheckTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, deviceID, cid string, seqs []int64, expiry time.Time) error {
	if !s.now().Before(expiry) {
		return ErrInvalidDevice
	}
	if _, e := authorizeReceiptMessagesTx(ctx, tx, id, cid, seqs, s.now); e != nil {
		return e
	}
	if _, e := s.activeGroupActor(ctx, tx, id); e != nil {
		return e
	}
	if !s.now().Before(expiry) {
		return ErrInvalidDevice
	}
	return nil
}
