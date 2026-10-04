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
