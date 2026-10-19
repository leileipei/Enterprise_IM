package policystore

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"time"
)

type fileDownloadDeviceKey struct{}
type fileDownloadDeviceRequest struct{ credential DeviceCredential }

func (fileDownloadDeviceRequest) String() string { return "file download device credential" }

// WithFileDownloadDeviceCredential carries an untrusted native credential.
// Every download authorization transaction binds it to the authenticated owner.
// Requests without device headers retain the existing Web protocol.
func WithFileDownloadDeviceCredential(ctx context.Context, device DeviceCredential) context.Context {
	return context.WithValue(ctx, fileDownloadDeviceKey{}, fileDownloadDeviceRequest{device})
}
func lockFileDownloadDevice(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, at time.Time) (*time.Time, error) {
	request, present := ctx.Value(fileDownloadDeviceKey{}).(fileDownloadDeviceRequest)
	if !present {
		return nil, nil
	}
	// File/conversation evidence locks precede this final short device SHARE lock.
	// Download streaming holds no device lock; subsequent checks reacquire it.
	expiry, err := lockDeviceCredentialTx(ctx, tx, DeviceOwner{TenantID: id.TenantID, UserID: id.UserID}, request.credential, at, false)
	if err != nil {
		return nil, err
	}
	return &expiry, nil
}
