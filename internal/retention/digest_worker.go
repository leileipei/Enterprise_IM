package retention

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

type DigestWorker struct {
	DB        access.Beginner
	BatchSize int
	clock     func(context.Context, pgx.Tx) (time.Time, error)
}

// Sequence bounds describe the actual rows, not a contiguous cleared interval.
type DigestBatchResult struct {
	TenantID, ConversationID, BatchID     string
	RetiredCount                          int
	FirstSeq, LastSeq                     int64
	MinExpiresAt, MaxExpiresAt, RetiredAt time.Time
}

func (w DigestWorker) ProcessTenant(ctx context.Context, tenantID string) (DigestBatchResult, error) {
	if w.DB == nil {
		return DigestBatchResult{}, ErrWorkerUnconfigured
	}
	size := w.BatchSize
	if size == 0 {
		size = DefaultBatchSize
	}
	if size < 1 || size > MaxBatchSize {
		return DigestBatchResult{}, ErrInvalidBatchSize
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return DigestBatchResult{}, err
		}
		result, err := w.processDigestBatch(ctx, tenantID, size)
		if err == nil {
			return result, nil
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "40P01" || attempt == 2 {
			return DigestBatchResult{}, err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return DigestBatchResult{}, ctx.Err()
		case <-timer.C:
		}
	}
	panic("unreachable digest retry")
}
func (w DigestWorker) batchTime(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var at time.Time
	var err error
	if w.clock != nil {
		at, err = w.clock(ctx, tx)
	} else {
		err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&at)
	}
	return at.UTC().Truncate(time.Microsecond), err
}
