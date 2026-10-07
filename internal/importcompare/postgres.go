package importcompare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"strings"
	"time"
)

type PGReader struct{ Config Config }

func mappedError(ctx context.Context, err error) error {
	if e := p.ContextFailure(ctx); e != nil {
		return e
	}
	var failure p.Failure
	if errors.As(err, &failure) {
		return failure
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "42501", "42809", "0A000":
			return dbFailure("DATABASE_ACCESS_UNSUPPORTED")
		case "42P01", "42703":
			return dbFailure("DATABASE_PROFILE_UNSUPPORTED")
		case "57014":
			return dbFailure("TIMEOUT")
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return dbFailure("TIMEOUT")
	}
	return dbFailure("DATABASE_READ_FAILED")
}
func (r PGReader) Read(parent context.Context, tenantID string, input p.Document) (result Snapshot, err error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	fail := func(e error) (Snapshot, error) { return Snapshot{}, mappedError(ctx, e) }
	cfg, e := driverConfig(r.Config)
	if e != nil {
		return Snapshot{}, e
	}
	connection, cancelConnect := context.WithTimeout(ctx, cfg.ConnectTimeout)
	conn, e := pgx.ConnectConfig(connection, cfg)
	cancelConnect()
	if e != nil {
		if ce := p.ContextFailure(ctx); ce != nil {
			return Snapshot{}, ce
		}
		return Snapshot{}, dbFailure("DATABASE_CONNECT_FAILED")
	}
	var tx pgx.Tx
	var b *queryBudget
	closed := false
	rolledBack := false
	defer func() {
		if !closed {
			clean, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if tx != nil && !rolledBack {
				if b != nil && b.count < maxSQL {
					b.count++
					_ = tx.Rollback(clean)
				}
			}
			_ = conn.Close(clean)
			select {
			case <-conn.PgConn().CleanupDone():
			case <-clean.Done():
				_ = conn.PgConn().Conn().Close()
			}

		}
	}()
	b = &queryBudget{}
	if e = b.step(ctx); e != nil {
		return fail(e)
	}
	start, stop := context.WithTimeout(ctx, 5*time.Second)
	tx, e = conn.BeginTx(start, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	stop()
	if e != nil {
		return fail(e)
	}
	b.tx = tx
	for _, sql := range []string{"SET LOCAL statement_timeout = '5s'", "SET LOCAL search_path = pg_catalog", "SET LOCAL TimeZone = 'UTC'"} {
		if e = b.exec(ctx, sql); e != nil {
			return fail(e)
		}
	}
	if e = checkModes(ctx, b); e != nil {
		return fail(e)
	}
	for _, s := range p.Schema() {
		if e = b.exec(ctx, "LOCK TABLE "+qualified(r.Config.Schema, s.Entity)+" IN ACCESS SHARE MODE"); e != nil {
			return fail(e)
		}
	}
	if e = checkProfile(ctx, b, r.Config.Schema); e != nil {
		return fail(e)
	}
	snapshot, e := readSnapshot(ctx, b, r.Config.Schema, tenantID, input, false)
	if e != nil {
		return fail(e)
	}
	if e = b.rollback(ctx); e != nil {
		return fail(e)
	}
	rolledBack = true
	if e = conn.Close(ctx); e != nil {
		return fail(e)
	}
	closed = true
	snapshot.SQLCount = b.count
	if e = p.ContextFailure(ctx); e != nil {
		return fail(e)
	}
	return snapshot, nil
}
func readGlobalKeys(ctx context.Context, b *queryBudget, schema, tenant string, input p.Document, out map[RowRef]bool) error {
	for _, s := range p.Schema() {
		keys := pk(s.Entity)
		rows := input.Tables[s.Entity]
		for start := 0; start < len(rows); start += 1000 {
			end := start + 1000
			if end > len(rows) {
				end = len(rows)
			}
			batch := rows[start:end]
			values, columns, predicates := []string{}, []string{"ord"}, []string{}
			for _, key := range keys {
				quoted := pgx.Identifier{string(key)}.Sanitize()
				columns = append(columns, quoted)
				predicates = append(predicates, "t."+quoted+"=w."+quoted)
			}
			args := []any{}
			for _, r := range batch {
				tuple := []string{fmt.Sprintf("$%d::integer", len(args)+1)}
				args = append(args, r.Ordinal)
				for _, f := range keys {
					kind := "uuid"
					if s.Entity == "external_identities" {
						kind = "text"
					}
					tuple = append(tuple, fmt.Sprintf("$%d::%s", len(args)+1, kind))
					args = append(args, r.Values[f].Text)
				}
				values = append(values, "("+strings.Join(tuple, ",")+")")
			}
			args = append(args, tenant)
			tenantColumn := "tenant_id"
			if s.Entity == "tenants" {
				tenantColumn = "id"
			}
			predicates = append(predicates, fmt.Sprintf("t.%s<>$%d::uuid", pgx.Identifier{tenantColumn}.Sanitize(), len(args)))
			sql := "WITH wanted(" + strings.Join(columns, ",") + ") AS (VALUES " + strings.Join(values, ",") + ") SELECT w.ord,EXISTS(SELECT 1 FROM " + qualified(schema, s.Entity) + " t WHERE " + strings.Join(predicates, " AND ") + ") FROM wanted w ORDER BY w.ord"
			result, e := b.query(ctx, sql, args...)
			if e != nil {
				return e
			}
			expected := map[int]bool{}
			for _, r := range batch {
				expected[r.Ordinal] = true
			}
			n := 0
			for result.Next() {
				var ordinal int
				var occupied bool
				if e = result.Scan(&ordinal, &occupied); e != nil {
					result.Close()
					return e
				}
				if !expected[ordinal] {
					result.Close()
					return dbFailure("DATABASE_READ_FAILED")
				}
				delete(expected, ordinal)
				n++
				out[RowRef{s.Entity, ordinal}] = occupied
			}
			e = result.Err()
			result.Close()
			if e != nil {
				return e
			}
			if n != len(batch) || len(expected) != 0 {
				return dbFailure("DATABASE_READ_FAILED")
			}
		}
	}
	return nil
}

func readSnapshot(ctx context.Context, b *queryBudget, schema, tenantID string, input p.Document, lockRows bool) (Snapshot, error) {
	fail := func(e error) (Snapshot, error) { return Snapshot{}, mappedError(ctx, e) }
	var e error
	snapshot := Snapshot{Data: p.Document{Tables: map[p.Entity][]p.Record{}}, GlobalKeys: map[RowRef]bool{}}
	rowCount, byteCount := 0, 0
	for _, s := range p.Schema() {
		table := s.Entity
		snapshot.Data.Tables[table] = []p.Record{}
		projection := []string{}
		cellTooLong := []string{}
		for _, f := range s.Fields {
			column := pgx.Identifier{string(f.Name)}.Sanitize()
			cell := column + "::text"
			if f.Kind == "time" {
				cell = "CASE WHEN " + column + " IS NULL THEN NULL WHEN pg_catalog.isfinite(" + column + ") AND " + column + " >= TIMESTAMPTZ '0001-01-01 00:00:00+00' AND " + column + " < TIMESTAMPTZ '10000-01-01 00:00:00+00' THEN pg_catalog.to_char(" + column + " AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') ELSE 'unsupported' END"
			}
			length := "pg_catalog.octet_length(" + cell + ")>4096"
			cellTooLong = append(cellTooLong, "coalesce("+length+",false)")
			projection = append(projection, "CASE WHEN "+length+" THEN NULL ELSE "+cell+" END")
		}
		projection = append([]string{strings.Join(cellTooLong, " OR ")}, projection...)
		selector := "tenant_id"
		if table == "tenants" {
			selector = "id"
		}
		order := []string{}
		for _, f := range pk(table) {
			order = append(order, pgx.Identifier{string(f)}.Sanitize())
		}
		sql := "SELECT " + strings.Join(projection, ",") + " FROM " + qualified(schema, table) + " WHERE " + pgx.Identifier{selector}.Sanitize() + "=$1::uuid ORDER BY " + strings.Join(order, ",") + " LIMIT $2::integer"
		if lockRows {
			sql += " FOR SHARE"
		}
		rows, e := b.query(ctx, sql, tenantID, maxStoredRows-rowCount+1)
		if e != nil {
			return fail(e)
		}
		for rows.Next() {
			rowCount++
			if rowCount > maxStoredRows {
				rows.Close()
				return fail(dbFailure("DATABASE_LIMIT"))
			}
			tooLong := false
			cells := make([]*string, len(s.Fields))
			scan := []any{&tooLong}
			for n := range cells {
				scan = append(scan, &cells[n])
			}
			if e = rows.Scan(scan...); e != nil {
				rows.Close()
				return fail(e)
			}
			if tooLong {
				rows.Close()
				return fail(dbFailure("DATABASE_DATA_UNSUPPORTED"))
			}
			values := map[p.Field]json.RawMessage{}
			for n, f := range s.Fields {
				cell := cells[n]
				if cell == nil {
					values[f.Name] = json.RawMessage("null")
					continue
				}
				byteCount += len(*cell)
				if byteCount > maxStoredBytes {
					rows.Close()
					return fail(dbFailure("DATABASE_LIMIT"))
				}
				if f.Kind == "bool" {
					values[f.Name] = json.RawMessage(*cell)
				} else {
					values[f.Name], _ = json.Marshal(*cell)
				}
			}
			record, issues, e := p.NormalizeRecord(ctx, table, len(snapshot.Data.Tables[table])+1, values)
			if e != nil {
				rows.Close()
				return fail(e)
			}
			if len(issues) > 0 {
				rows.Close()
				return fail(dbFailure("DATABASE_DATA_UNSUPPORTED"))
			}
			snapshot.Data.Tables[table] = append(snapshot.Data.Tables[table], record)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return fail(e)
		}
		if table == "tenants" {
			snapshot.TenantFound = len(snapshot.Data.Tables[table]) == 1
			if !snapshot.TenantFound {
				break
			}
		}
	}
	if snapshot.TenantFound {
		if e = readGlobalKeys(ctx, b, schema, tenantID, input, snapshot.GlobalKeys); e != nil {
			return fail(e)
		}
	}
	snapshot.SQLCount = b.count
	return snapshot, nil
}
