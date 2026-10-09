package policystore

import (
	"errors"
	"time"
)

var (
	ErrInvalidDeviceRequest          = errors.New("invalid device request")
	ErrInvalidDevice                 = errors.New("invalid device")
	ErrDeviceNotFound                = errors.New("device not found")
	ErrDeviceQuotaExceeded           = errors.New("active device quota exceeded")
	ErrDeviceRegistrationRateLimited = errors.New("device registration rate exceeded")
	ErrDeviceRegistrationExists      = errors.New("device registration exists")
	ErrInvalidClientPolicy           = errors.New("invalid client policy")
)

type DeviceRegistrationExistsError struct{ DeviceID string }

func (e *DeviceRegistrationExistsError) Error() string { return ErrDeviceRegistrationExists.Error() }
func (e *DeviceRegistrationExistsError) Unwrap() error { return ErrDeviceRegistrationExists }

type DeviceOwner struct{ TenantID, UserID string }
type DeviceCredential struct{ ID, Token string }
type DeviceRegistration struct{ RegistrationID, Platform, Name string }
type DeviceGrant struct {
	DeviceID, Token       string
	ExpiresAt, ServerTime time.Time
}
type Device struct {
	ID, RegistrationID, Platform, Name, Status string
	CreatedAt, ExpiresAt                       time.Time
	RevokedAt, LastSeenAt                      *time.Time
}
type DevicePage struct {
	Devices    []Device
	HasMore    bool
	NextCursor string
}
type ClientPolicy struct {
	Version                                                int64
	DeviceLeaseDays, MaxActiveDevices, OfflineCacheSeconds int
	GroupReceiptVisibility                                 string
}
