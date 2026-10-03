package retention

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

const DefaultBatchSize = 100
const MaxBatchSize = 1000

var ErrWorkerUnconfigured = errors.New("retention worker requires a database")
var ErrInvalidBatchSize = errors.New("retention batch size must be between 1 and 1000")

type Worker struct {
	DB        access.Beginner
	BatchSize int
	clock     func(context.Context, pgx.Tx) (time.Time, error)
}

// BatchResult describes committed work only. Sequence bounds are the actual
// min/max, not a claim that every sequence between them was cleared.
type BatchResult struct {
	TenantID       string
	ConversationID string
	BatchID        string
	ClearedCount   int
	FirstSeq       int64
	LastSeq        int64
	RetentionDays  int
	CutoffAt       time.Time
	ClearedAt      time.Time
}

func (w Worker) ListActiveTenants(ctx context.Context) ([]string, error) {
	if w.DB == nil {
		return nil, ErrWorkerUnconfigured
	}
	tx, err := w.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	rows, err := tx.Query(ctx, "SELECT id::text FROM tenants WHERE status='active' ORDER BY id")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ids, nil
}

func (w Worker) ProcessTenant(ctx context.Context, tenantID string) (BatchResult, error) {
	if w.DB == nil {
		return BatchResult{}, ErrWorkerUnconfigured
	}
	size := w.BatchSize
	if size == 0 {
		size = DefaultBatchSize
	}
	if size < 1 || size > MaxBatchSize {
		return BatchResult{}, ErrInvalidBatchSize
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return BatchResult{}, err
		}
		result, err := w.processBatch(ctx, tenantID, size)
		if err == nil {
			return result, nil
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "40P01" || attempt == 2 {
			return BatchResult{}, err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return BatchResult{}, ctx.Err()
		case <-timer.C:
		}
	}
	panic("unreachable retention retry")
}

func (w Worker) batchTime(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var at time.Time
	var err error
	if w.clock != nil {
		at, err = w.clock(ctx, tx)
	} else {
		err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&at)
	}
	return at.UTC().Truncate(time.Microsecond), err
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
