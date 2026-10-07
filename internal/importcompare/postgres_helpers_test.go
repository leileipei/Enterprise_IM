package importcompare

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type compareFixture struct {
	owner                *pgx.Conn
	schema, role, tenant string
	input                p.Document
	reader               PGReader
	dsn                  string
}

func newCompareFixture(t *testing.T) *compareFixture {
	t.Helper()
	dsn := os.Getenv("IM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires owned PostgreSQL comparison fixture")
	}
	ctx := context.Background()
	owner, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	n := fmt.Sprintf("im_compare_%d", time.Now().UnixNano())
	f := &compareFixture{owner: owner, schema: n, role: n + "_reader", input: singleDocument(t), dsn: dsn}
	f.tenant = f.input.Tables["tenants"][0].Values["id"].Text
	quote := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	f.exec(t, "CREATE SCHEMA "+quote(f.schema))
	f.exec(t, "SET search_path TO "+quote(f.schema)+", public")
	paths, _ := filepath.Glob("../../db/migrations/*.up.sql")
	if len(paths) != 21 {
		t.Fatal("fixture migration count")
	}
	for _, path := range paths {
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = owner.PgConn().Exec(ctx, string(b)).ReadAll(); e != nil {
			t.Fatal(e)
		}
	}
	for _, s := range p.Schema() {
		for _, row := range f.input.Tables[s.Entity] {
			f.insert(t, s, row)
		}
	}
	f.exec(t, "CREATE ROLE "+quote(f.role)+" LOGIN")
	f.exec(t, "GRANT USAGE ON SCHEMA "+quote(f.schema)+" TO "+quote(f.role))
	f.exec(t, "GRANT SELECT ON ALL TABLES IN SCHEMA "+quote(f.schema)+" TO "+quote(f.role))
	settings, e := ParseDSN(dsn)
	if e != nil {
		t.Fatal(e)
	}
	settings["user"] = f.role
	f.reader = PGReader{Config: Config{connection: settings, Schema: f.schema}}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = owner.Exec(cleanup, "ROLLBACK")
		if _, e := owner.Exec(cleanup, "DROP SCHEMA "+quote(f.schema)+" CASCADE"); e != nil {
			t.Error("fixture schema cleanup", e)
		}
		if _, e := owner.Exec(cleanup, "DROP OWNED BY "+quote(f.role)); e != nil {
			t.Error("fixture role ownership cleanup", e)
		}
		if _, e := owner.Exec(cleanup, "DROP ROLE "+quote(f.role)); e != nil {
			t.Error("fixture role cleanup", e)
		}
		owner.Close(cleanup)
	})
	return f
}
func (f *compareFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.owner.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
func (f *compareFixture) insert(t *testing.T, s p.TableSchema, r p.Record) {
	t.Helper()
	cols, params := []string{}, []string{}
	args := []any{}
	for i, field := range s.Fields {
		cols = append(cols, pgx.Identifier{string(field.Name)}.Sanitize())
		params = append(params, fmt.Sprintf("$%d", i+1))
		v := r.Values[field.Name]
		var value any = v.Text
		if v.IsNull {
			value = nil
		} else if field.Kind == "time" {
			value = v.Time
		} else if field.Kind == "bool" {
			value = v.Bool
		}
		args = append(args, value)
	}
	f.exec(t, "INSERT INTO "+pgx.Identifier{f.schema, string(s.Entity)}.Sanitize()+" ("+strings.Join(cols, ",")+") VALUES ("+strings.Join(params, ",")+")", args...)
}
func (f *compareFixture) digest(t *testing.T) string {
	t.Helper()
	out := ""
	for _, s := range p.Schema() {
		var hash string
		if e := f.owner.QueryRow(context.Background(), "SELECT coalesce(md5(string_agg(t::text,'' ORDER BY t::text)),md5('')) FROM "+pgx.Identifier{f.schema, string(s.Entity)}.Sanitize()+" t").Scan(&hash); e != nil {
			t.Fatal(e)
		}
		out += hash
	}
	return out
}
