package policystore

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

func defaultClientPolicy() ClientPolicy {
	return ClientPolicy{DeviceLeaseDays: 30, MaxActiveDevices: 10, OfflineCacheSeconds: 300, GroupReceiptVisibility: "owner_admin"}
}

// The advisory lock covers the default-without-a-row case. All policy users
// acquire it after identity locks; writers use the same key exclusively.
func clientPolicyTx(ctx context.Context, tx pgx.Tx, tenantID string, write bool) (ClientPolicy, error) {
	lock := "pg_advisory_xact_lock_shared"
	rowLock := "FOR SHARE"
	if write {
		lock = "pg_advisory_xact_lock"
		rowLock = "FOR UPDATE"
	}
	if _, err := tx.Exec(ctx, "SELECT "+lock+"(hashtextextended('im_client_policy:'||$1::text,0))", tenantID); err != nil {
		return ClientPolicy{}, err
	}
	var p ClientPolicy
	err := tx.QueryRow(ctx, `SELECT version,device_lease_days,max_active_devices,offline_cache_seconds,group_receipt_visibility
 FROM tenant_client_policies WHERE tenant_id=$1 `+rowLock, tenantID).Scan(&p.Version, &p.DeviceLeaseDays, &p.MaxActiveDevices, &p.OfflineCacheSeconds, &p.GroupReceiptVisibility)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultClientPolicy(), nil
	}
	return p, err
}

func (s Service) GetClientPolicy(ctx context.Context, id access.TrustedIdentity) (ClientPolicy, error) {
	id, err := normalizeDeviceIdentity(id)
	if err != nil || s.DB == nil {
		return ClientPolicy{}, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return ClientPolicy{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = s.activeGroupActor(ctx, tx, id); err != nil {
		return ClientPolicy{}, err
	}
	p, err := clientPolicyTx(ctx, tx, id.TenantID, false)
	if err != nil {
		return ClientPolicy{}, err
	}
	if err = auditDeviceAction(ctx, tx, id, "client_policy_read", id.TenantID, "allow", "effective_policy", s.now()); err != nil {
		return ClientPolicy{}, err
	}
	if _, err = s.activeGroupActor(ctx, tx, id); err != nil {
		return ClientPolicy{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ClientPolicy{}, err
	}
	return p, nil
}

func validClientPolicy(p ClientPolicy) bool {
	return p.DeviceLeaseDays >= 1 && p.DeviceLeaseDays <= 90 && p.MaxActiveDevices >= 1 && p.MaxActiveDevices <= 100 && p.OfflineCacheSeconds >= 0 && p.OfflineCacheSeconds <= 3600 && (p.GroupReceiptVisibility == "owner_admin" || p.GroupReceiptVisibility == "sender_aggregate")
}

func (s Service) SetClientPolicy(ctx context.Context, id access.TrustedIdentity, expectedVersion int64, next ClientPolicy) (ClientPolicy, error) {
	if expectedVersion < 0 || expectedVersion == math.MaxInt64 || !validClientPolicy(next) {
		return ClientPolicy{}, ErrInvalidClientPolicy
	}
	id, err := normalizeDeviceIdentity(id)
	if err != nil || s.DB == nil {
		return ClientPolicy{}, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return ClientPolicy{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = s.activeGroupActor(ctx, tx, id); err != nil {
		return ClientPolicy{}, err
	}
	allowed, err := authorizedPublisher(ctx, tx, id, s.now())
	if err != nil {
		return ClientPolicy{}, err
	}
	if !allowed {
		return ClientPolicy{}, denyDeviceAction(ctx, tx, id, "client_policy_update", id.TenantID, "not_group_admin", s.now(), ErrForbidden)
	}
	current, err := clientPolicyTx(ctx, tx, id.TenantID, true)
	if err != nil {
		return ClientPolicy{}, err
	}
	if current.Version != expectedVersion {
		return ClientPolicy{}, denyDeviceAction(ctx, tx, id, "client_policy_update", id.TenantID, "version_conflict", s.now(), ErrVersionConflict)
	}
	next.Version = current.Version + 1
	if _, err = tx.Exec(ctx, `INSERT INTO tenant_client_policies(tenant_id,version,device_lease_days,max_active_devices,offline_cache_seconds,group_receipt_visibility,updated_at)
 VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(tenant_id) DO UPDATE SET version=EXCLUDED.version,device_lease_days=EXCLUDED.device_lease_days,
 max_active_devices=EXCLUDED.max_active_devices,offline_cache_seconds=EXCLUDED.offline_cache_seconds,group_receipt_visibility=EXCLUDED.group_receipt_visibility,updated_at=EXCLUDED.updated_at`, id.TenantID, next.Version, next.DeviceLeaseDays, next.MaxActiveDevices, next.OfflineCacheSeconds, next.GroupReceiptVisibility, s.now()); err != nil {
		return ClientPolicy{}, err
	}
	if err = auditDeviceAction(ctx, tx, id, "client_policy_update", id.TenantID, "allow", "policy_updated", s.now()); err != nil {
		return ClientPolicy{}, err
	}
	allowed, err = authorizedPublisher(ctx, tx, id, s.now())
	if err != nil {
		return ClientPolicy{}, err
	}
	if !allowed {
		return ClientPolicy{}, ErrForbidden
	}
	if err = tx.Commit(ctx); err != nil {
		return ClientPolicy{}, err
	}
	return next, nil
}

func auditDeviceAction(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, action, resourceID, outcome, reason string, at time.Time) error {
	resourceType := "device"
	if action == "device_list" {
		resourceType = "user"
	}
	if action == "client_policy_read" || action == "client_policy_update" {
		resourceType = "tenant"
	}
	_, err := tx.Exec(ctx, `INSERT INTO audit_events(tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id.TenantID, id.UserID, nullableID(id.ActingMembershipID), action, resourceType, nullableID(resourceID), outcome, reason, at)
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return nil
}

func denyDeviceAction(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, action, resourceID, reason string, at time.Time, result error) error {
	if err := auditDeviceAction(ctx, tx, id, action, resourceID, "deny", reason, at); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return result
}
