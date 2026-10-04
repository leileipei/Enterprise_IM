package retention

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

func retireAttachmentFingerprints(ctx context.Context, tx pgx.Tx, tenantID string, messageIDs []string, at time.Time) ([]string, error) {
	rows, err := tx.Query(ctx, `UPDATE message_attachments SET sealed_sha256=NULL,fingerprint_retired_at=$3
 WHERE tenant_id=$1 AND message_id=ANY($2::uuid[]) AND fingerprint_retired_at IS NULL
 RETURNING message_id::text`, tenantID, messageIDs, at)
	if err != nil {
		return nil, err
	}
	return digestIDs(rows)
}
