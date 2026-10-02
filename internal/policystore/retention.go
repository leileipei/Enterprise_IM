package policystore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// The shared tenant row lock prevents a retention update from committing
// between this read and the final visibility decision in the pull transaction.
func messageBodyRetentionForTenant(ctx context.Context, tx pgx.Tx, tenantID string) (time.Duration, error) {
	var days int
	if err := tx.QueryRow(ctx, `SELECT message_body_retention_days FROM tenants
 WHERE id=$1 FOR SHARE`, tenantID).Scan(&days); err != nil {
		return 0, err
	}
	return time.Duration(days) * 24 * time.Hour, nil
}
