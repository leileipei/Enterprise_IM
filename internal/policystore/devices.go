package policystore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

func normalizeDeviceOwner(owner DeviceOwner) (DeviceOwner, error) {
	if !directoryUUIDPattern.MatchString(owner.TenantID) || !directoryUUIDPattern.MatchString(owner.UserID) {
		return DeviceOwner{}, ErrForbidden
	}
	owner.TenantID = strings.ToLower(owner.TenantID)
	owner.UserID = strings.ToLower(owner.UserID)
	return owner, nil
}
func normalizeDeviceIdentity(id access.TrustedIdentity) (access.TrustedIdentity, error) {
	owner, err := normalizeDeviceOwner(DeviceOwner{id.TenantID, id.UserID})
	if err != nil || !directoryUUIDPattern.MatchString(id.ActingMembershipID) {
		return access.TrustedIdentity{}, ErrForbidden
	}
	return access.TrustedIdentity{TenantID: owner.TenantID, UserID: owner.UserID, ActingMembershipID: strings.ToLower(id.ActingMembershipID)}, nil
}
func validDeviceRegistration(req DeviceRegistration) bool {
	if !directoryUUIDPattern.MatchString(req.RegistrationID) || req.RegistrationID[14] != '7' || !strings.ContainsRune("89abAB", rune(req.RegistrationID[19])) {
		return false
	}
	switch req.Platform {
	case "web", "windows", "macos", "android", "ios":
	default:
		return false
	}
	return utf8.ValidString(req.Name) && strings.TrimSpace(req.Name) == req.Name && utf8.RuneCountInString(req.Name) >= 1 && utf8.RuneCountInString(req.Name) <= 80 && strings.IndexFunc(req.Name, unicode.IsControl) < 0
}

// Registration upgrades the owner row only after its acting membership is
// locked. The per-owner advisory lock serializes that upgrade, avoiding two
// concurrent readers upgrading the same row while counting devices.
func lockDeviceOwnerTx(ctx context.Context, tx pgx.Tx, owner DeviceOwner, write bool) error {
	mode := "FOR SHARE"
	if write {
		mode = "FOR UPDATE"
	}
	var status string
	err := tx.QueryRow(ctx, "SELECT status FROM users WHERE tenant_id=$1 AND id=$2 "+mode, owner.TenantID, owner.UserID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "active") {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, "SELECT status FROM tenants WHERE id=$1 FOR SHARE", owner.TenantID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "active") {
		return ErrForbidden
	}
	return err
}

func (s Service) RegisterDevice(ctx context.Context, id access.TrustedIdentity, req DeviceRegistration) (DeviceGrant, error) {
	if !validDeviceRegistration(req) {
		return DeviceGrant{}, ErrInvalidDeviceRequest
	}
	id, err := normalizeDeviceIdentity(id)
	if err != nil || s.DB == nil {
		return DeviceGrant{}, ErrForbidden
	}
	rate := s.DeviceRegistrationsPerHour
	if rate == 0 {
		rate = 10
	}
	if rate < 1 || rate > 1000 {
		return DeviceGrant{}, ErrInvalidDeviceRequest
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return DeviceGrant{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('im_device_register:'||$1::text||':'||$2::text,0))", id.TenantID, id.UserID); err != nil {
		return DeviceGrant{}, err
	}
	var memberID string
	err = tx.QueryRow(ctx, "SELECT id::text FROM user_organizations WHERE tenant_id=$1 AND user_id=$2 AND id=$3 FOR SHARE", id.TenantID, id.UserID, id.ActingMembershipID).Scan(&memberID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceGrant{}, ErrForbidden
	}
	if err != nil {
		return DeviceGrant{}, err
	}
	if err = lockDeviceOwnerTx(ctx, tx, DeviceOwner{id.TenantID, id.UserID}, true); err != nil {
		return DeviceGrant{}, err
	}
	if _, err = s.activeGroupActor(ctx, tx, id); err != nil {
		return DeviceGrant{}, err
	}
	p, err := clientPolicyTx(ctx, tx, id.TenantID, false)
	if err != nil {
		return DeviceGrant{}, err
	}
	var existingID string
	err = tx.QueryRow(ctx, "SELECT id::text FROM user_devices WHERE tenant_id=$1 AND user_id=$2 AND registration_id=$3", id.TenantID, id.UserID, strings.ToLower(req.RegistrationID)).Scan(&existingID)
	if err == nil {
		return DeviceGrant{}, denyDeviceAction(ctx, tx, id, "device_register", existingID, "registration_exists", s.now(), &DeviceRegistrationExistsError{DeviceID: existingID})
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return DeviceGrant{}, err
	}
	now := s.now()
	var count int
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM user_devices WHERE tenant_id=$1 AND user_id=$2 AND revoked_at IS NULL AND expires_at>$3", id.TenantID, id.UserID, now).Scan(&count); err != nil {
		return DeviceGrant{}, err
	}
	if count >= p.MaxActiveDevices {
		return DeviceGrant{}, denyDeviceAction(ctx, tx, id, "device_register", "", "device_quota", now, ErrDeviceQuotaExceeded)
	}
	var window time.Time
	var used int
	err = tx.QueryRow(ctx, "SELECT window_start,created_count FROM device_registration_windows WHERE tenant_id=$1 AND user_id=$2 FOR UPDATE", id.TenantID, id.UserID).Scan(&window, &used)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return DeviceGrant{}, err
	}
	if errors.Is(err, pgx.ErrNoRows) || !now.Before(window.Add(time.Hour)) {
		window = now
		used = 0
	}
	if used >= rate {
		return DeviceGrant{}, denyDeviceAction(ctx, tx, id, "device_register", "", "registration_rate", now, ErrDeviceRegistrationRateLimited)
	}
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return DeviceGrant{}, err
	}
	digest := sha256.Sum256(raw)
	grant := DeviceGrant{Token: base64.RawURLEncoding.EncodeToString(raw), ServerTime: now, ExpiresAt: now.Add(time.Duration(p.DeviceLeaseDays) * 24 * time.Hour)}
	err = tx.QueryRow(ctx, `INSERT INTO user_devices(id,tenant_id,user_id,registration_id,platform,name,credential_digest,created_at,expires_at)
 VALUES(gen_random_uuid(),$1,$2,$3,$4,$5,$6,$7,$8) RETURNING id::text`, id.TenantID, id.UserID, strings.ToLower(req.RegistrationID), req.Platform, req.Name, digest[:], now, grant.ExpiresAt).Scan(&grant.DeviceID)
	if err != nil {
		return DeviceGrant{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO device_registration_windows(tenant_id,user_id,window_start,created_count) VALUES($1,$2,$3,$4)
 ON CONFLICT(tenant_id,user_id) DO UPDATE SET window_start=EXCLUDED.window_start,created_count=EXCLUDED.created_count`, id.TenantID, id.UserID, window, used+1); err != nil {
		return DeviceGrant{}, err
	}
	if err = auditDeviceAction(ctx, tx, id, "device_register", grant.DeviceID, "allow", "device_registered", now); err != nil {
		return DeviceGrant{}, err
	}
	if _, err = s.activeGroupActor(ctx, tx, id); err != nil {
		return DeviceGrant{}, err
	}
	if !s.now().Before(grant.ExpiresAt) {
		return DeviceGrant{}, ErrInvalidDevice
	}
	if err = tx.Commit(ctx); err != nil {
		return DeviceGrant{}, err
	}
	return grant, nil
}

// Device credentials are never copied into logs or errors. Strict round-trip
// encoding also rejects padding and noncanonical trailing bits.
func deviceDigest(credential DeviceCredential) ([]byte, error) {
	if !directoryUUIDPattern.MatchString(credential.ID) || len(credential.Token) != 43 {
		return nil, ErrInvalidDevice
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(credential.Token)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != credential.Token {
		return nil, ErrInvalidDevice
	}
	digest := sha256.Sum256(raw)
	return digest[:], nil
}
func lockDeviceCredentialTx(ctx context.Context, tx pgx.Tx, owner DeviceOwner, credential DeviceCredential, at time.Time, write bool) (time.Time, error) {
	digest, err := deviceDigest(credential)
	if err != nil {
		return time.Time{}, err
	}
	lock := "FOR SHARE"
	if write {
		lock = "FOR UPDATE"
	}
	var stored []byte
	var expiry time.Time
	var revoked *time.Time
	err = tx.QueryRow(ctx, `SELECT credential_digest,expires_at,revoked_at FROM user_devices WHERE tenant_id=$1 AND user_id=$2 AND id=$3 `+lock, owner.TenantID, owner.UserID, strings.ToLower(credential.ID)).Scan(&stored, &expiry, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrInvalidDevice
	}
	if err != nil {
		return time.Time{}, err
	}
	if subtle.ConstantTimeCompare(stored, digest) != 1 || revoked != nil || !at.Before(expiry) {
		return time.Time{}, ErrInvalidDevice
	}
	return expiry, nil
}

func (s Service) RenewDevice(ctx context.Context, id access.TrustedIdentity, credential DeviceCredential) (DeviceGrant, error) {
	if _, err := deviceDigest(credential); err != nil {
		return DeviceGrant{}, err
	}
	id, err := normalizeDeviceIdentity(id)
	if err != nil || s.DB == nil {
		return DeviceGrant{}, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return DeviceGrant{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = s.activeGroupActor(ctx, tx, id); err != nil {
		return DeviceGrant{}, err
	}
	p, err := clientPolicyTx(ctx, tx, id.TenantID, false)
	if err != nil {
		return DeviceGrant{}, err
	}
	now := s.now()
	oldExpiry, err := lockDeviceCredentialTx(ctx, tx, DeviceOwner{id.TenantID, id.UserID}, credential, now, true)
	if err != nil {
		return DeviceGrant{}, err
	}
	now = s.now()
	if !now.Before(oldExpiry) {
		return DeviceGrant{}, ErrInvalidDevice
	}
	expiry := now.Add(time.Duration(p.DeviceLeaseDays) * 24 * time.Hour)
	if oldExpiry.After(expiry) {
		expiry = oldExpiry
	}
	if _, err = tx.Exec(ctx, "UPDATE user_devices SET expires_at=$4,last_seen_at=$5 WHERE tenant_id=$1 AND user_id=$2 AND id=$3", id.TenantID, id.UserID, strings.ToLower(credential.ID), expiry, now); err != nil {
		return DeviceGrant{}, err
	}
	if err = auditDeviceAction(ctx, tx, id, "device_renew", strings.ToLower(credential.ID), "allow", "device_renewed", now); err != nil {
		return DeviceGrant{}, err
	}
	if _, err = s.activeGroupActor(ctx, tx, id); err != nil {
		return DeviceGrant{}, err
	}
	// Validate against the original lease after any audit wait. An expired
	// credential cannot renew itself by changing the row before this check.
	if !s.now().Before(oldExpiry) {
		return DeviceGrant{}, ErrInvalidDevice
	}
	if err = tx.Commit(ctx); err != nil {
		return DeviceGrant{}, err
	}
	return DeviceGrant{DeviceID: strings.ToLower(credential.ID), ExpiresAt: expiry, ServerTime: now}, nil
}

func (s Service) ListDevices(ctx context.Context, owner DeviceOwner, cursor string, limit int) (DevicePage, error) {
	if limit < 1 || limit > 100 {
		return DevicePage{}, ErrInvalidDeviceRequest
	}
	before, beforeID, err := decodeConversationCursor(cursor)
	if err != nil {
		return DevicePage{}, ErrInvalidDeviceRequest
	}
	owner, err = normalizeDeviceOwner(owner)
	if err != nil || s.DB == nil {
		return DevicePage{}, ErrForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return DevicePage{}, err
	}
	defer tx.Rollback(ctx)
	if err = lockDeviceOwnerTx(ctx, tx, owner, false); err != nil {
		return DevicePage{}, err
	}
	query := `SELECT id::text,registration_id::text,platform,name,created_at,expires_at,revoked_at,last_seen_at FROM user_devices WHERE tenant_id=$1 AND user_id=$2`
	args := []any{owner.TenantID, owner.UserID}
	if cursor != "" {
		query += " AND (created_at,id)<($3,$4::uuid)"
		args = append(args, before, beforeID)
	}
	args = append(args, limit+1)
	query += " ORDER BY created_at DESC,id DESC LIMIT $" + strconv.Itoa(len(args))
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return DevicePage{}, err
	}
	page := DevicePage{Devices: make([]Device, 0, limit)}
	for rows.Next() {
		var d Device
		if err = rows.Scan(&d.ID, &d.RegistrationID, &d.Platform, &d.Name, &d.CreatedAt, &d.ExpiresAt, &d.RevokedAt, &d.LastSeenAt); err != nil {
			rows.Close()
			return DevicePage{}, err
		}
		page.Devices = append(page.Devices, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return DevicePage{}, err
	}
	if len(page.Devices) > limit {
		page.HasMore = true
		page.Devices = page.Devices[:limit]
		last := page.Devices[limit-1]
		page.NextCursor = encodeConversationCursor(last.CreatedAt, last.ID)
	}
	id := access.TrustedIdentity{TenantID: owner.TenantID, UserID: owner.UserID}
	if err = auditDeviceAction(ctx, tx, id, "device_list", owner.UserID, "allow", "owner_active", s.now()); err != nil {
		return DevicePage{}, err
	}
	now := s.now()
	for i := range page.Devices {
		d := &page.Devices[i]
		d.Status = "active"
		if d.RevokedAt != nil {
			d.Status = "revoked"
		} else if !now.Before(d.ExpiresAt) {
			d.Status = "expired"
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return DevicePage{}, err
	}
	return page, nil
}

func (s Service) RevokeDevice(ctx context.Context, owner DeviceOwner, deviceID string) error {
	owner, err := normalizeDeviceOwner(owner)
	if err != nil || s.DB == nil {
		return ErrForbidden
	}
	if !directoryUUIDPattern.MatchString(deviceID) {
		return ErrInvalidDeviceRequest
	}
	deviceID = strings.ToLower(deviceID)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockDeviceOwnerTx(ctx, tx, owner, false); err != nil {
		return err
	}
	id := access.TrustedIdentity{TenantID: owner.TenantID, UserID: owner.UserID}
	var revoked *time.Time
	err = tx.QueryRow(ctx, "SELECT revoked_at FROM user_devices WHERE tenant_id=$1 AND user_id=$2 AND id=$3 FOR UPDATE", owner.TenantID, owner.UserID, deviceID).Scan(&revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return denyDeviceAction(ctx, tx, id, "device_revoke", deviceID, "device_not_found", s.now(), ErrDeviceNotFound)
	}
	if err != nil {
		return err
	}
	now := s.now()
	if revoked == nil {
		if _, err = tx.Exec(ctx, "UPDATE user_devices SET revoked_at=$4 WHERE tenant_id=$1 AND user_id=$2 AND id=$3", owner.TenantID, owner.UserID, deviceID, now); err != nil {
			return err
		}
	}
	if err = auditDeviceAction(ctx, tx, id, "device_revoke", deviceID, "allow", "device_revoked", now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
