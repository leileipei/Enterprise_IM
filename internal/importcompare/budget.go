package importcompare

import (
	"context"
	"github.com/jackc/pgx/v5"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"time"
)

const maxSQL = 128
const maxStoredRows = 20000
const maxStoredBytes = 64 * 1024 * 1024

type queryBudget struct {
	tx    pgx.Tx
	count int
	limit int
}

func (b *queryBudget) step(ctx context.Context) error {
	if err := p.ContextFailure(ctx); err != nil {
		return err
	}
	limit := b.limit
	if limit == 0 {
		limit = maxSQL
	}
	if b.count >= limit {
		return p.Failure{Code: "DATABASE_LIMIT"}
	}
	b.count++
	return nil
}

type boundedRows struct {
	pgx.Rows
	cancel context.CancelFunc
}

func (r *boundedRows) Close() { r.Rows.Close(); r.cancel() }
func (r *boundedRows) Next() bool {
	ok := r.Rows.Next()
	if !ok {
		r.Close()
	}
	return ok
}
func (b *queryBudget) query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := b.step(ctx); err != nil {
		return nil, err
	}
	sub, cancel := context.WithTimeout(ctx, 5*time.Second)
	rows, err := b.tx.Query(sub, sql, args...)
	if err != nil {
		cancel()
		return nil, err
	}
	return &boundedRows{rows, cancel}, nil
}
func (b *queryBudget) exec(ctx context.Context, sql string, args ...any) error {
	if err := b.step(ctx); err != nil {
		return err
	}
	sub, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := b.tx.Exec(sub, sql, args...)
	return err
}
func (b *queryBudget) rollback(ctx context.Context) error {
	if err := b.step(ctx); err != nil {
		return err
	}
	sub, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return b.tx.Rollback(sub)
}
