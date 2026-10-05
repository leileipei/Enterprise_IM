package policystore

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"time"
)

// Test bridge exposes no production authorization endpoint.
func FileDownloadAuthorizationForTest(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, fileID string) (files.Metadata, string, int64, time.Time, error) {
	return authorizeFileDownloadTx(ctx, tx, id, fileID)
}

// Captures only value facts; callers can close their transaction before evaluation.
func FileVisibilityForTest(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, fileID string) (func(time.Time) (files.Metadata, string, int64, error), error) {
	f, err := loadFileVisibilityFactsTx(ctx, tx, id, fileID)
	if err != nil {
		return nil, err
	}
	return func(at time.Time) (files.Metadata, string, int64, error) { return evaluateFileVisibility(f, at) }, nil
}
