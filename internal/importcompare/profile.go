package importcompare

import (
	"context"
	"github.com/jackc/pgx/v5"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"slices"
	"strings"
)

func dbFailure(code p.Code) error { return p.Failure{Code: code} }
func checkModes(ctx context.Context, b *queryBudget) error {
	rows, err := b.query(ctx, `SELECT current_setting('transaction_read_only'), current_setting('transaction_isolation'), current_setting('server_encoding'), rolsuper,rolbypassrls FROM pg_catalog.pg_roles WHERE rolname=current_user`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var readOnly, isolation, encoding string
	var super, bypass bool
	if !rows.Next() {
		return dbFailure("DATABASE_ACCESS_UNSUPPORTED")
	}
	if err = rows.Scan(&readOnly, &isolation, &encoding, &super, &bypass); err != nil {
		return err
	}
	if readOnly != "on" || isolation != "repeatable read" || super || bypass {
		return dbFailure("DATABASE_ACCESS_UNSUPPORTED")
	}
	if encoding != "UTF8" {
		return dbFailure("DATABASE_PROFILE_UNSUPPORTED")
	}
	if rows.Next() {
		return dbFailure("DATABASE_READ_FAILED")
	}
	return rows.Err()
}
func checkProfile(ctx context.Context, b *queryBudget, schema string) error {
	tables := []string{}
	for _, s := range p.Schema() {
		tables = append(tables, string(s.Entity))
	}
	rows, err := b.query(ctx, `SELECT c.relname,c.relkind::text,c.relpersistence::text,c.relrowsecurity,c.relforcerowsecurity,pg_catalog.has_table_privilege(c.oid,'SELECT'),(pg_catalog.has_table_privilege(c.oid,'INSERT,UPDATE,DELETE,TRUNCATE') OR pg_catalog.has_any_column_privilege(c.oid,'INSERT,UPDATE')) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=ANY($2::text[])`, schema, tables)
	if err != nil {
		return err
	}
	n := 0
	for rows.Next() {
		var name, kind, persistence string
		var rls, force, read, write bool
		if err = rows.Scan(&name, &kind, &persistence, &rls, &force, &read, &write); err != nil {
			rows.Close()
			return err
		}
		n++
		if kind != "r" || persistence != "p" || rls || force || !read || write {
			rows.Close()
			return dbFailure("DATABASE_ACCESS_UNSUPPORTED")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if n != 9 {
		return dbFailure("DATABASE_PROFILE_UNSUPPORTED")
	}
	return checkStructure(ctx, b, schema)
}
func checkStructure(ctx context.Context, b *queryBudget, schema string) error {
	tables := []string{}
	for _, s := range p.Schema() {
		tables = append(tables, string(s.Entity))
	}
	rows, err := b.query(ctx, `SELECT c.relname,a.attname,t.typname,nt.nspname,a.attnotnull,coalesce(col.collisdeterministic,true) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace JOIN pg_catalog.pg_attribute a ON a.attrelid=c.oid JOIN pg_catalog.pg_type t ON t.oid=a.atttypid JOIN pg_catalog.pg_namespace nt ON nt.oid=t.typnamespace LEFT JOIN pg_catalog.pg_collation col ON col.oid=a.attcollation WHERE n.nspname=$1 AND c.relname=ANY($2::text[]) AND a.attnum>0 AND NOT a.attisdropped`, schema, tables)
	if err != nil {
		return err
	}
	type column struct {
		typ, namespace         string
		notnull, deterministic bool
	}
	cols := map[string]map[string]column{}
	for rows.Next() {
		var table, name string
		var c column
		if err = rows.Scan(&table, &name, &c.typ, &c.namespace, &c.notnull, &c.deterministic); err != nil {
			rows.Close()
			return err
		}
		if cols[table] == nil {
			cols[table] = map[string]column{}
		}
		cols[table][name] = c
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	types := map[string]string{"text": "text", "uuid": "uuid", "time": "timestamptz", "bool": "bool"}
	for _, s := range p.Schema() {
		for _, f := range s.Fields {
			c, ok := cols[string(s.Entity)][string(f.Name)]
			if !ok || c.typ != types[f.Kind] || c.namespace != "pg_catalog" || c.notnull == f.Nullable {
				return dbFailure("DATABASE_PROFILE_UNSUPPORTED")
			}
			unique := slices.Contains(pk(s.Entity), f.Name) || slices.Contains(businessKeys[s.Entity], f.Name)
			if f.Kind == "text" && unique && !c.deterministic {
				return dbFailure("DATABASE_PROFILE_UNSUPPORTED")
			}
		}
	}
	rows, err = b.query(ctx, `SELECT c.relname,con.contype::text,ARRAY(SELECT coalesce(a.attname,'') FROM unnest(con.conkey) WITH ORDINALITY k(num,ord) LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid=c.oid AND a.attnum=k.num ORDER BY k.ord),coalesce(dest.relname,''),coalesce(dn.nspname,''),ARRAY(SELECT coalesce(a.attname,'') FROM unnest(con.confkey) WITH ORDINALITY k(num,ord) LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid=dest.oid AND a.attnum=k.num ORDER BY k.ord),coalesce(pg_catalog.pg_get_expr(idx.indexprs,c.oid),''),coalesce(pg_catalog.pg_get_expr(idx.indpred,c.oid),''),ARRAY(SELECT ns.nspname||'.'||op.oprname FROM unnest(con.conexclop) WITH ORDINALITY k(id,ord) JOIN pg_catalog.pg_operator op ON op.oid=k.id JOIN pg_catalog.pg_namespace ns ON ns.oid=op.oprnamespace ORDER BY k.ord),con.convalidated,coalesce(idx.indisvalid,true),coalesce(idx.indisready,true),con.confupdtype::text,con.confdeltype::text,coalesce(am.amname,'') FROM pg_catalog.pg_constraint con JOIN pg_catalog.pg_class c ON c.oid=con.conrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace LEFT JOIN pg_catalog.pg_class dest ON dest.oid=con.confrelid LEFT JOIN pg_catalog.pg_namespace dn ON dn.oid=dest.relnamespace LEFT JOIN pg_catalog.pg_index idx ON idx.indexrelid=con.conindid LEFT JOIN pg_catalog.pg_class ic ON ic.oid=con.conindid LEFT JOIN pg_catalog.pg_am am ON am.oid=ic.relam WHERE n.nspname=$1 AND c.relname=ANY($2::text[]) AND con.contype IN ('p','u','f','x')`, schema, tables)
	if err != nil {
		return err
	}
	available := map[p.Entity][]constraintContract{}
	for rows.Next() {
		var table, destSchema, update, delete, method string
		var c constraintContract
		var validated, valid, ready bool
		if err = rows.Scan(&table, &c.kind, &c.fields, &c.target, &destSchema, &c.targetFields, &c.expression, &c.predicate, &c.operators, &validated, &valid, &ready, &update, &delete, &method); err != nil {
			rows.Close()
			return err
		}
		if !validated || !valid || !ready || c.kind == "f" && (destSchema != schema || update != "a" || delete != "a") || c.kind == "x" && method != "gist" {
			continue
		}
		c.expression = strings.TrimPrefix(c.expression, "pg_catalog.")
		available[p.Entity(table)] = append(available[p.Entity(table)], c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for entity, contracts := range profileContracts() {
		for _, want := range contracts {
			matched := false
			for _, got := range available[entity] {
				if got.kind == want.kind && slices.Equal(got.fields, want.fields) && got.target == want.target && slices.Equal(got.targetFields, want.targetFields) && got.expression == want.expression && got.predicate == want.predicate && slices.Equal(got.operators, want.operators) {
					matched = true
					break
				}
			}
			if !matched {
				return dbFailure("DATABASE_PROFILE_UNSUPPORTED")
			}
		}
	}
	return nil
}
func qualified(schema string, e p.Entity) string { return pgx.Identifier{schema, string(e)}.Sanitize() }
