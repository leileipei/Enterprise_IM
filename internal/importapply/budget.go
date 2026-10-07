package importapply

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"strings"
	"time"
)

type budgetKey struct{}
type sqlKind int

const (
	readSQL sqlKind = iota
	insertSQL
	controlSQL
)

var errSQLBudget = errors.New("IMPORT_SQL_LIMIT")
var errBudget = errors.New("IMPORT_BUDGET_INVALID")

type sqlBudget struct {
	Read, Insert, Control int
	Deadline              time.Time
}

func RequestContext(parent context.Context, start, expiry time.Time) (context.Context, context.CancelFunc, error) {
	if expiry.IsZero() {
		return nil, nil, errBudget
	}
	end := start.Add(30 * time.Second)
	if expiry.Before(end) {
		end = expiry
	}
	if other, ok := parent.Deadline(); ok && other.Before(end) {
		end = other
	}
	ctx, cancel := context.WithDeadline(parent, end)
	ctx = context.WithValue(ctx, budgetKey{}, &sqlBudget{Deadline: end})
	return ctx, cancel, nil
}
func budgetFrom(ctx context.Context) *sqlBudget {
	b, _ := ctx.Value(budgetKey{}).(*sqlBudget)
	return b
}
func (b *sqlBudget) charge(ctx context.Context, kind sqlKind) error {
	if b == nil {
		return errBudget
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	switch kind {
	case readSQL:
		if b.Read >= 256 {
			return errSQLBudget
		}
		b.Read++
	case insertSQL:
		if b.Insert >= 10000 {
			return errSQLBudget
		}
		b.Insert++
	case controlSQL:
		if b.Control >= 32 {
			return errSQLBudget
		}
		b.Control++
	}
	return nil
}
func sqlClass(sql string) sqlKind {
	u := strings.ToUpper(strings.TrimSpace(sql))
	if strings.HasPrefix(u, "SELECT ") || strings.HasPrefix(u, "WITH ") {
		return readSQL
	}
	if strings.HasPrefix(u, "INSERT INTO ") {
		for _, name := range []string{"legal_entities", "organizations", "departments", "users", "user_organizations", "user_departments"} {
			if strings.Contains(sql, ".\""+name+"\" ") {
				return insertSQL
			}
		}
	}
	return controlSQL
}

type meteredTx struct {
	pgx.Tx
	budget *sqlBudget
}

func newMeteredTx(tx pgx.Tx, budget *sqlBudget) *meteredTx { return &meteredTx{tx, budget} }
func (m *meteredTx) sub(ctx context.Context, sql string) (context.Context, context.CancelFunc, error) {
	if e := m.budget.charge(ctx, sqlClass(sql)); e != nil {
		return nil, nil, e
	}
	end := time.Now().Add(5 * time.Second)
	if m.budget.Deadline.Before(end) {
		end = m.budget.Deadline
	}
	c, cancel := context.WithDeadline(ctx, end)
	return c, cancel, nil
}
func (m *meteredTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c, cancel, e := m.sub(ctx, sql)
	if e != nil {
		return pgconn.CommandTag{}, e
	}
	defer cancel()
	return m.Tx.Exec(c, sql, args...)
}

type importRows struct {
	pgx.Rows
	cancel context.CancelFunc
}

func (r *importRows) Close() { r.Rows.Close(); r.cancel() }
func (r *importRows) Next() bool {
	ok := r.Rows.Next()
	if !ok {
		r.Close()
	}
	return ok
}
func (m *meteredTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c, cancel, e := m.sub(ctx, sql)
	if e != nil {
		return nil, e
	}
	rows, e := m.Tx.Query(c, sql, args...)
	if e != nil {
		cancel()
		return nil, e
	}
	return &importRows{rows, cancel}, nil
}

type importRow struct {
	pgx.Row
	cancel context.CancelFunc
	err    error
}

func (r *importRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer r.cancel()
	return r.Row.Scan(dest...)
}
func (m *meteredTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c, cancel, e := m.sub(ctx, sql)
	if e != nil {
		return &importRow{err: e}
	}
	return &importRow{Row: m.Tx.QueryRow(c, sql, args...), cancel: cancel}
}
func (m *meteredTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errBudget
}

type deniedBatch struct{}

func (deniedBatch) Exec() (pgconn.CommandTag, error)                        { return pgconn.CommandTag{}, errBudget }
func (deniedBatch) Query() (pgx.Rows, error)                                { return nil, errBudget }
func (deniedBatch) QueryRow() pgx.Row                                       { return &importRow{err: errBudget} }
func (deniedBatch) Close() error                                            { return errBudget }
func (m *meteredTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return deniedBatch{} }
