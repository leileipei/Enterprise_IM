package policystore

import (
	"context"
	"github.com/jackc/pgx/v5"
	"time"
)

// FileDeleteProofTime timestamps receipt of an exact absence observation using
// the settlement database. This read grants no deletion authority: settlement
// still locks and rechecks the original commitment, lease, and timestamp bounds.
func (s Service) FileDeleteProofTime(ctx context.Context) (time.Time, error) {
	return fileTransaction(ctx, s, func(tx pgx.Tx) (time.Time, error) { return fileClock(ctx, tx) })
}
