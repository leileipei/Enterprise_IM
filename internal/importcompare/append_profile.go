package importcompare

import (
	"context"
	"github.com/jackc/pgx/v5"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
)

func CheckAppendProfile(ctx context.Context, tx pgx.Tx, schema string) error {
	if tx == nil || !appendSchemaPattern.MatchString(schema) {
		return dbFailure("DATABASE_PROFILE_UNSUPPORTED")
	}
	return checkAppendProfile(ctx, &queryBudget{tx: tx, limit: 256}, schema)
}
func checkAppendProfile(ctx context.Context, b *queryBudget, schema string) error {
	rows, e := b.query(ctx, `SELECT current_setting('transaction_read_only'),current_setting('transaction_isolation'),current_setting('server_encoding'),rolsuper,rolbypassrls FROM pg_catalog.pg_roles WHERE rolname=current_user`)
	if e != nil {
		return e
	}
	var ro, iso, encoding string
	var super, bypass bool
	if !rows.Next() {
		rows.Close()
		return dbFailure("DATABASE_ACCESS_UNSUPPORTED")
	}
	e = rows.Scan(&ro, &iso, &encoding, &super, &bypass)
	rows.Close()
	if e != nil {
		return e
	}
	if ro != "off" || iso != "serializable" || super || bypass {
		return dbFailure("DATABASE_ACCESS_UNSUPPORTED")
	}
	if encoding != "UTF8" {
		return dbFailure("DATABASE_PROFILE_UNSUPPORTED")
	}
	tables := []string{}
	for _, s := range p.Schema() {
		tables = append(tables, string(s.Entity))
	}
	tables = append(tables, "audit_events", "import_batches")
	rows, e = b.query(ctx, `SELECT c.relname,c.relkind::text,c.relpersistence::text,c.relrowsecurity,c.relforcerowsecurity,pg_catalog.has_table_privilege(c.oid,'SELECT'),pg_catalog.has_any_column_privilege(c.oid,'UPDATE'),pg_catalog.has_table_privilege(c.oid,'INSERT') FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=ANY($2::text[])`, schema, tables)
	if e != nil {
		return e
	}
	count := 0
	for rows.Next() {
		var name, kind, persistence string
		var rls, force, read, update, insert bool
		if e = rows.Scan(&name, &kind, &persistence, &rls, &force, &read, &update, &insert); e != nil {
			rows.Close()
			return e
		}
		count++
		meta := name == "audit_events" || name == "import_batches"
		if kind != "r" || persistence != "p" || rls || force || name != "audit_events" && !read || !meta && !update || meta && !insert {
			rows.Close()
			return dbFailure("DATABASE_ACCESS_UNSUPPORTED")
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if count != 11 {
		return dbFailure("DATABASE_PROFILE_UNSUPPORTED")
	}
	insertTables, insertColumns := []string{}, []string{}
	for _, s := range p.Schema() {
		if s.Entity == "tenants" || s.Entity == "external_identities" || s.Entity == "admin_grants" {
			continue
		}
		for _, f := range s.Fields {
			insertTables = append(insertTables, qualified(schema, s.Entity))
			insertColumns = append(insertColumns, string(f.Name))
		}
	}
	rows, e = b.query(ctx, `SELECT pg_catalog.has_column_privilege(v.tbl::regclass,v.col,'INSERT') FROM unnest($1::text[],$2::text[]) AS v(tbl,col)`, insertTables, insertColumns)
	if e != nil {
		return e
	}
	n := 0
	for rows.Next() {
		allowed := false
		if e = rows.Scan(&allowed); e != nil {
			rows.Close()
			return e
		}
		n++
		if !allowed {
			rows.Close()
			return dbFailure("DATABASE_ACCESS_UNSUPPORTED")
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if n != len(insertColumns) {
		return dbFailure("DATABASE_PROFILE_UNSUPPORTED")
	}
	if e = checkStructure(ctx, b, schema); e != nil {
		return e
	}
	rows, e = b.query(ctx, `SELECT pg_catalog.has_sequence_privilege(pg_catalog.pg_get_serial_sequence($1,'id'),'USAGE')`, pgx.Identifier{schema, "audit_events"}.Sanitize())
	if e != nil {
		return e
	}
	ok := false
	if rows.Next() {
		e = rows.Scan(&ok)
	}
	rows.Close()
	if e != nil {
		return e
	}
	if !ok {
		return dbFailure("DATABASE_ACCESS_UNSUPPORTED")
	}
	rows, e = b.query(ctx, `SELECT c.relname,a.attname,t.typname,a.attnotnull FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace JOIN pg_catalog.pg_attribute a ON a.attrelid=c.oid JOIN pg_catalog.pg_type t ON t.oid=a.atttypid JOIN pg_catalog.pg_namespace tn ON tn.oid=t.typnamespace WHERE n.nspname=$1 AND c.relname IN ('import_batches','audit_events') AND a.attnum>0 AND NOT a.attisdropped AND tn.nspname='pg_catalog'`, schema)
	if e != nil {
		return e
	}
	wanted := map[string]string{"tenant_id": "uuid", "request_id": "uuid", "actor_user_id": "uuid", "acting_membership_id": "uuid", "protocol_version": "text", "input_sha256": "bytea", "state": "text", "reason": "text", "receipt": "jsonb", "completed_at": "timestamptz"}
	seen := map[string]bool{}
	for rows.Next() {
		var table, column, typ string
		var notnull bool
		if e = rows.Scan(&table, &column, &typ, &notnull); e != nil {
			rows.Close()
			return e
		}
		if table == "import_batches" {
			if want, exists := wanted[column]; exists {
				if typ != want || !notnull {
					rows.Close()
					return dbFailure("DATABASE_PROFILE_UNSUPPORTED")
				}
				seen[column] = true
			}
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if len(seen) != len(wanted) {
		return dbFailure("DATABASE_PROFILE_UNSUPPORTED")
	}
	rows, e = b.query(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_constraint k JOIN pg_catalog.pg_class c ON c.oid=k.conrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname='import_batches' AND k.contype='p' AND k.convalidated AND ARRAY(SELECT a.attname::text FROM unnest(k.conkey) WITH ORDINALITY u(num,ord) JOIN pg_catalog.pg_attribute a ON a.attrelid=c.oid AND a.attnum=u.num ORDER BY u.ord)=ARRAY['tenant_id','request_id']),EXISTS(SELECT 1 FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname='import_batches' AND t.tgname='import_batches_immutable' AND t.tgenabled IN ('O','A') AND NOT t.tgisinternal)`, schema)
	if e != nil {
		return e
	}
	pk, guard := false, false
	if rows.Next() {
		e = rows.Scan(&pk, &guard)
	}
	rows.Close()
	if e != nil {
		return e
	}
	if !pk || !guard {
		return dbFailure("DATABASE_PROFILE_UNSUPPORTED")
	}
	return nil
}
