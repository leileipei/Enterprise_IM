package policystore

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"time"
)

type messageDeviceKey struct{}
type messageDeviceAdmissionKey struct{}
type messageDeviceRequest struct{ credential DeviceCredential }

func (messageDeviceRequest) String() string { return "message device credential" }

type messageDeviceAdmission struct{ now func() time.Time }

func (messageDeviceAdmission) String() string { return "message device admission" }

// WithMessageDeviceCredential carries untrusted transport credentials. The
// message transaction binds owner, validates the token and checks the lease.
// No device context preserves the established credentialless Web protocol.
func WithMessageDeviceCredential(ctx context.Context, device DeviceCredential) context.Context {
	return context.WithValue(ctx, messageDeviceKey{}, messageDeviceRequest{credential: device})
}
func (s Service) admitMessageDevice(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity) (context.Context, error) {
	request, present := ctx.Value(messageDeviceKey{}).(messageDeviceRequest)
	if !present {
		return ctx, nil
	}
	// Early rejection is a nonlocking snapshot. Acquire the device lock only
	// after the established identity/policy/conversation/file lock sequence.
	if _, err := deviceCredentialTx(ctx, tx, DeviceOwner{TenantID: id.TenantID, UserID: id.UserID}, request.credential, s.now(), ""); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, messageDeviceAdmissionKey{}, messageDeviceAdmission{now: s.now}), nil
}

// messageDeviceGuard holds the expiry captured while the device row remains
// SHARE locked through commit. Its final check performs no database I/O.
type messageDeviceGuard struct {
	now    func() time.Time
	expiry time.Time
}

func (g messageDeviceGuard) check() error {
	if g.now != nil && !g.now().Before(g.expiry) {
		return ErrInvalidDevice
	}
	return nil
}
func lockMessageDevice(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity) (messageDeviceGuard, error) {
	request, present := ctx.Value(messageDeviceKey{}).(messageDeviceRequest)
	if !present {
		return messageDeviceGuard{}, nil
	}
	admitted, ok := ctx.Value(messageDeviceAdmissionKey{}).(messageDeviceAdmission)
	if !ok || admitted.now == nil {
		return messageDeviceGuard{}, ErrInvalidDevice
	}
	expiry, err := lockDeviceCredentialTx(ctx, tx, DeviceOwner{TenantID: id.TenantID, UserID: id.UserID}, request.credential, admitted.now(), false)
	if err != nil {
		return messageDeviceGuard{}, err
	}
	guard := messageDeviceGuard{now: admitted.now, expiry: expiry}
	// Lock acquisition may itself cross expiry. Business authorization is checked
	// by the caller after this wait, then the lease is checked once more.
	return guard, guard.check()
}
