package importcompare

import (
	"context"
	"github.com/jackc/pgx/v5"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"regexp"
)

var appendSchemaPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func ReadAppendSnapshot(ctx context.Context, tx pgx.Tx, schema, tenantID string, input p.Document) (Snapshot, error) {
	if tx == nil || !appendSchemaPattern.MatchString(schema) {
		return Snapshot{}, dbFailure("DATABASE_PROFILE_UNSUPPORTED")
	}
	b := &queryBudget{tx: tx, limit: 256}
	for _, sql := range []string{"SET LOCAL search_path=pg_catalog", "SET LOCAL TimeZone='UTC'", "SET LOCAL lock_timeout='1s'"} {
		if e := b.exec(ctx, sql); e != nil {
			return Snapshot{}, e
		}
	}
	for _, s := range p.Schema() {
		if e := b.exec(ctx, "LOCK TABLE "+qualified(schema, s.Entity)+" IN ACCESS SHARE MODE"); e != nil {
			return Snapshot{}, e
		}
	}
	if e := checkAppendProfile(ctx, b, schema); e != nil {
		return Snapshot{}, e
	}
	return readSnapshot(ctx, b, schema, tenantID, input, true)
}
