package policystore

import (
	"context"
	"errors"
	"time"
)

var ErrReceiptRateLimited = errors.New("receipt write rate exceeded")

// Operational accounting is separate from message and registration quotas.
// A committed count is shared by API processes, including rejected HTTP writes.
func (s Service) CheckReceiptWriteRate(ctx context.Context, owner DeviceOwner) error {
	owner, err := normalizeDeviceOwner(owner)
	if err != nil || s.DB == nil {
		return ErrForbidden
	}
	rate := s.ReceiptWriteRatePerSecond
	if rate == 0 {
		rate = 10
	}
	if rate < 1 || rate > 10000 {
		return ErrInvalidDeviceRequest
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockDeviceOwnerTx(ctx, tx, owner, false); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO receipt_write_windows(tenant_id,user_id,window_start,write_count) VALUES($1,$2,$3,0) ON CONFLICT(tenant_id,user_id) DO NOTHING`, owner.TenantID, owner.UserID, s.now()); err != nil {
		return err
	}
	var started time.Time
	var count int
	if err = tx.QueryRow(ctx, "SELECT window_start,write_count FROM receipt_write_windows WHERE tenant_id=$1 AND user_id=$2 FOR UPDATE", owner.TenantID, owner.UserID).Scan(&started, &count); err != nil {
		return err
	}
	now := s.now()
	if !now.Before(started.Add(time.Second)) {
		started = now
		count = 0
	}
	if count >= rate {
		return ErrReceiptRateLimited
	}
	if _, err = tx.Exec(ctx, "UPDATE receipt_write_windows SET window_start=$3,write_count=$4 WHERE tenant_id=$1 AND user_id=$2", owner.TenantID, owner.UserID, started, count+1); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
