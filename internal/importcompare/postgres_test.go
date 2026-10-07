package importcompare

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"testing"
	"time"
)

func TestComparePGSnapshotReadOnly(t *testing.T) {
	f := newCompareFixture(t)
	before := f.digest(t)
	s, err := f.reader.Read(context.Background(), f.tenant, f.input)
	if err != nil || !s.TenantFound || s.SQLCount > 128 {
		t.Fatal("bounded complete snapshot", err)
	}
	cl, is, err := Compare(context.Background(), f.input, s)
	if err != nil || *cl["total"].Identical != 66 || is.Total() != 0 {
		t.Fatal("read snapshot values", err)
	}
	if before != f.digest(t) {
		t.Fatal("business contents changed")
	}
	cfg, err := driverConfig(f.reader.Config)
	if err != nil {
		t.Fatal(err)
	}
	read, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close(context.Background())
	for _, table := range p.Schema() {
		name := pgx.Identifier{f.schema, string(table.Entity)}.Sanitize()
		for _, statement := range []string{"INSERT INTO " + name + " DEFAULT VALUES", "UPDATE " + name + " SET id=id", "DELETE FROM " + name, "TRUNCATE " + name} {
			if table.Entity == "external_identities" && statement == "UPDATE "+name+" SET id=id" {
				statement = "UPDATE " + name + " SET issuer=issuer"
			}
			_, e := read.Exec(context.Background(), statement)
			var pe *pgconn.PgError
			if !errors.As(e, &pe) || pe.Code != "42501" {
				t.Fatal("mutation was not rejected by credentials", e)
			}
		}
	}
	missing, err := f.reader.Read(context.Background(), newID(99999), f.input)
	if err != nil || missing.TenantFound {
		t.Fatal("missing target", err)
	}
}
func TestComparePGIsolation(t *testing.T) {
	f := newCompareFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	lock, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	_, err = lock.Exec(ctx, "LOCK TABLE "+pgx.Identifier{f.schema, "admin_grants"}.Sanitize()+" IN ACCESS EXCLUSIVE MODE")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		s Snapshot
		e error
	}
	done := make(chan result, 1)
	go func() { s, e := f.reader.Read(ctx, f.tenant, f.input); done <- result{s, e} }()
	writer, e := pgx.Connect(ctx, f.dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer writer.Close(ctx)
	for {
		var n int
		err = writer.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE usename=$1 AND wait_event_type='Lock'", f.role).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		select {
		case r := <-done:
			t.Fatal("reader did not wait for structure lock", r.e)
		case <-ctx.Done():
			t.Fatal("lock wait absent")
		case <-time.After(10 * time.Millisecond):
		}
	}
	_, e = writer.Exec(ctx, "UPDATE "+pgx.Identifier{f.schema, "users"}.Sanitize()+" SET display_name='after_snapshot' WHERE id=$1", f.input.Tables["users"][0].Values["id"].Text)
	if e != nil {
		t.Fatal(e)
	}
	if e = lock.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	r := <-done
	if r.e != nil {
		t.Fatal(r.e)
	}
	cl, _, e := Compare(ctx, f.input, r.s)
	if e != nil || *cl["total"].Identical != 66 {
		t.Fatal("mixed database snapshot", e)
	}
}
func TestComparePGProfile(t *testing.T) {
	f := newCompareFixture(t)
	cases := []struct{ name, sql, code string }{
		{"missing_select", "REVOKE SELECT ON " + pgx.Identifier{f.schema, "users"}.Sanitize() + " FROM " + pgx.Identifier{f.role}.Sanitize(), "DATABASE_ACCESS_UNSUPPORTED"},
		{"write_permission", "GRANT UPDATE ON " + pgx.Identifier{f.schema, "users"}.Sanitize() + " TO " + pgx.Identifier{f.role}.Sanitize(), "DATABASE_ACCESS_UNSUPPORTED"},
		{"superuser", "ALTER ROLE " + pgx.Identifier{f.role}.Sanitize() + " SUPERUSER", "DATABASE_ACCESS_UNSUPPORTED"},
		{"bypass", "ALTER ROLE " + pgx.Identifier{f.role}.Sanitize() + " BYPASSRLS", "DATABASE_ACCESS_UNSUPPORTED"},
		{"rls", "ALTER TABLE " + pgx.Identifier{f.schema, "users"}.Sanitize() + " ENABLE ROW LEVEL SECURITY", "DATABASE_ACCESS_UNSUPPORTED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.exec(t, tc.sql)
			_, err := f.reader.Read(context.Background(), f.tenant, f.input)
			if err == nil || err.Error() != tc.code {
				t.Fatal("unsupported profile accepted", err)
			}
			switch tc.name {
			case "missing_select":
				f.exec(t, "GRANT SELECT ON "+pgx.Identifier{f.schema, "users"}.Sanitize()+" TO "+pgx.Identifier{f.role}.Sanitize())
			case "write_permission":
				f.exec(t, "REVOKE UPDATE ON "+pgx.Identifier{f.schema, "users"}.Sanitize()+" FROM "+pgx.Identifier{f.role}.Sanitize())
			case "superuser":
				f.exec(t, "ALTER ROLE "+pgx.Identifier{f.role}.Sanitize()+" NOSUPERUSER")
			case "bypass":
				f.exec(t, "ALTER ROLE "+pgx.Identifier{f.role}.Sanitize()+" NOBYPASSRLS")
			case "rls":
				f.exec(t, "ALTER TABLE "+pgx.Identifier{f.schema, "users"}.Sanitize()+" DISABLE ROW LEVEL SECURITY")
			}
		})
	}
}
func TestComparePGLimits(t *testing.T) {
	f := newCompareFixture(t)
	f.exec(t, "UPDATE "+pgx.Identifier{f.schema, "users"}.Sanitize()+" SET display_name=repeat('x',4097)")
	_, err := f.reader.Read(context.Background(), f.tenant, f.input)
	if err == nil || err.Error() != "DATABASE_DATA_UNSUPPORTED" {
		t.Fatal("oversize cell accepted", err)
	}
}

func TestComparePGColumnWritePermission(t *testing.T) {
	f := newCompareFixture(t)
	f.exec(t, "GRANT UPDATE(display_name) ON "+pgx.Identifier{f.schema, "users"}.Sanitize()+" TO "+pgx.Identifier{f.role}.Sanitize())
	_, err := f.reader.Read(context.Background(), f.tenant, f.input)
	if err == nil || err.Error() != "DATABASE_ACCESS_UNSUPPORTED" {
		t.Fatal("column UPDATE credential was accepted", err)
	}
}

func TestComparePGStructuralProfile(t *testing.T) {
	for _, kind := range []string{"missing_column", "wrong_type", "missing_unique", "missing_exclusion", "view", "foreign", "nondeterministic", "non_utf8"} {
		t.Run(kind, func(t *testing.T) {
			f := newCompareFixture(t)
			name := pgx.Identifier{f.schema, "users"}.Sanitize()
			code := "DATABASE_PROFILE_UNSUPPORTED"
			switch kind {
			case "missing_column":
				f.exec(t, "ALTER TABLE "+name+" RENAME COLUMN display_name TO hidden_name")
			case "wrong_type":
				f.exec(t, "ALTER TABLE "+name+" ALTER COLUMN display_name TYPE varchar(200)")
			case "missing_unique":
				f.exec(t, "ALTER TABLE "+name+" DROP CONSTRAINT users_tenant_id_global_employee_no_key")
			case "missing_exclusion":
				var name string
				if err := f.owner.QueryRow(context.Background(), "SELECT conname FROM pg_catalog.pg_constraint WHERE conrelid=$1::regclass AND contype='x' ORDER BY conname LIMIT 1", pgx.Identifier{f.schema, "user_organizations"}.Sanitize()).Scan(&name); err != nil {
					t.Fatal(err)
				}
				f.exec(t, "ALTER TABLE "+pgx.Identifier{f.schema, "user_organizations"}.Sanitize()+" DROP CONSTRAINT "+pgx.Identifier{name}.Sanitize())
			case "view":
				f.exec(t, "ALTER TABLE "+name+" RENAME TO users_old")
				f.exec(t, "CREATE VIEW "+name+" AS SELECT * FROM "+pgx.Identifier{f.schema, "users_old"}.Sanitize())
				f.exec(t, "GRANT SELECT ON "+name+" TO "+pgx.Identifier{f.role}.Sanitize())
				code = "DATABASE_ACCESS_UNSUPPORTED"
			case "foreign":
				f.exec(t, "CREATE EXTENSION IF NOT EXISTS file_fdw WITH SCHEMA public")
				f.exec(t, "ALTER TABLE "+name+" RENAME TO users_old")
				server := f.schema + "_files"
				f.exec(t, "CREATE SERVER "+pgx.Identifier{server}.Sanitize()+" FOREIGN DATA WRAPPER file_fdw")
				t.Cleanup(func() { f.exec(t, "DROP SERVER IF EXISTS "+pgx.Identifier{server}.Sanitize()+" CASCADE") })
				f.exec(t, "CREATE FOREIGN TABLE "+name+" (id uuid,tenant_id uuid,global_employee_no text,display_name text,status text) SERVER "+pgx.Identifier{server}.Sanitize()+" OPTIONS (filename '/dev/null')")
				f.exec(t, "GRANT SELECT ON "+name+" TO "+pgx.Identifier{f.role}.Sanitize())
				code = "DATABASE_ACCESS_UNSUPPORTED"
			case "nondeterministic":
				coll := pgx.Identifier{f.schema, "nondet"}.Sanitize()
				f.exec(t, "CREATE COLLATION "+coll+" (provider=icu,locale='und',deterministic=false)")
				f.exec(t, "ALTER TABLE "+name+" ALTER COLUMN global_employee_no TYPE text COLLATE "+coll)
			case "non_utf8":
				database := f.schema + "_ascii"
				f.exec(t, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH ENCODING='SQL_ASCII' TEMPLATE=template0 LC_COLLATE='C' LC_CTYPE='C'")
				t.Cleanup(func() { f.exec(t, "DROP DATABASE "+pgx.Identifier{database}.Sanitize()) })
				settings := connectionSettings{}
				for k, v := range f.reader.Config.connection {
					settings[k] = v
				}
				settings["dbname"] = database
				f.reader.Config.connection = settings
			}
			_, err := f.reader.Read(context.Background(), f.tenant, f.input)
			if err == nil || err.Error() != code {
				t.Fatal("unsupported structural profile accepted", err)
			}
		})
	}
}
func TestComparePGResourceEdges(t *testing.T) {
	f := newCompareFixture(t)
	ctx := context.Background()
	users := pgx.Identifier{f.schema, "users"}.Sanitize()
	f.exec(t, "INSERT INTO "+users+" (id,tenant_id,global_employee_no,display_name) SELECT ('88888888-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid,$1::uuid,'limit-'||n,'' FROM generate_series(1,19934) n", f.tenant)
	s, err := f.reader.Read(ctx, f.tenant, f.input)
	if err != nil || len(s.Data.Tables["users"]) != 19945 {
		t.Fatal("20000 row boundary", err)
	}
	f.exec(t, "INSERT INTO "+users+" (id,tenant_id,global_employee_no,display_name) VALUES ($1::uuid,$2::uuid,'over-row','')", newID(70001), f.tenant)
	_, err = f.reader.Read(ctx, f.tenant, f.input)
	if err == nil || err.Error() != "DATABASE_LIMIT" {
		t.Fatal("20001 row limit", err)
	}
	// Hand-counted: each full user is 36+36+4096+4096+6 = 8270 UTF8 bytes.
	schema := p.Schema()
	for n := len(schema) - 1; n > 0; n-- {
		f.exec(t, "DELETE FROM "+pgx.Identifier{f.schema, string(schema[n].Entity)}.Sanitize())
	}
	f.exec(t, "INSERT INTO "+users+" (id,tenant_id,global_employee_no,display_name) SELECT ('99999999-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid,$1::uuid,lpad(n::text,8,'0')||repeat('x',4088),repeat('x',4096) FROM generate_series(1,8115) n", f.tenant)
	tenantBytes := 0
	for _, v := range f.input.Tables["tenants"][0].Values {
		tenantBytes += len(v.Text)
	}
	excess := 8115*8270 + tenantBytes - 64*1024*1024
	if excess < 1 || excess > 4096 {
		t.Fatal("byte fixture")
	}
	f.exec(t, "UPDATE "+users+" SET display_name=repeat('x',$1::int) WHERE global_employee_no LIKE '00008115%'", 4096-excess)
	s, err = f.reader.Read(ctx, f.tenant, f.input)
	if err != nil || len(s.Data.Tables["users"]) != 8115 {
		t.Fatal("64 MiB boundary", err)
	}
	f.exec(t, "UPDATE "+users+" SET display_name=display_name||'x' WHERE global_employee_no LIKE '00008115%'")
	_, err = f.reader.Read(ctx, f.tenant, f.input)
	if err == nil || err.Error() != "DATABASE_LIMIT" {
		t.Fatal("64 MiB plus one byte", err)
	}
}
func TestComparePGUnsupportedTimes(t *testing.T) {
	for _, value := range []string{"-infinity", "infinity", "0001-01-01 00:00:00 BC"} {
		t.Run(value, func(t *testing.T) {
			f := newCompareFixture(t)
			f.exec(t, "DELETE FROM "+pgx.Identifier{f.schema, "user_departments"}.Sanitize())
			f.exec(t, "DELETE FROM "+pgx.Identifier{f.schema, "admin_grants"}.Sanitize())
			member := pgx.Identifier{f.schema, "user_organizations"}.Sanitize()
			id := f.input.Tables["user_organizations"][0].Values["id"].Text
			f.exec(t, "DELETE FROM "+member+" WHERE id<>$1::uuid", id)
			f.exec(t, "UPDATE "+member+" SET effective_to=NULL,effective_from=$1::timestamptz WHERE id=$2::uuid", value, id)
			_, err := f.reader.Read(context.Background(), f.tenant, f.input)
			if err == nil || err.Error() != "DATABASE_DATA_UNSUPPORTED" {
				t.Fatal("unsupported time", err)
			}
		})
	}
}
func TestCompareUnitQueryBudget(t *testing.T) {
	b := queryBudget{}
	for n := 0; n < 128; n++ {
		if err := b.step(context.Background()); err != nil {
			t.Fatal("128th query rejected")
		}
	}
	if err := b.step(context.Background()); err == nil || err.Error() != "DATABASE_LIMIT" || b.count != 128 {
		t.Fatal("129th query allowed")
	}
}

func TestComparePGGlobalOccupancy(t *testing.T) {
	f := newCompareFixture(t)
	other := cloneRecord(f.input.Tables["tenants"][0])
	text(&other, "id", newID(91000))
	text(&other, "code", "other-synthetic-tenant")
	f.insert(t, p.Schema()[0], other)
	u := cloneRecord(f.input.Tables["users"][0])
	text(&u, "id", newID(91001))
	text(&u, "tenant_id", other.Values["id"].Text)
	text(&u, "global_employee_no", "outside-input")
	f.insert(t, p.Schema()[4], u)
	identity := cloneRecord(f.input.Tables["external_identities"][0])
	text(&identity, "issuer", "outside-synthetic-issuer")
	text(&identity, "subject", "outside-synthetic-subject")
	text(&identity, "tenant_id", other.Values["id"].Text)
	text(&identity, "user_id", u.Values["id"].Text)
	f.insert(t, p.Schema()[7], identity)
	in := cloneDoc(f.input)
	text(&u, "tenant_id", f.tenant)
	u.Ordinal = 12
	in.Tables["users"] = append(in.Tables["users"], u)
	text(&identity, "tenant_id", f.tenant)
	text(&identity, "user_id", f.input.Tables["users"][0].Values["id"].Text)
	identity.Ordinal = 11
	in.Tables["external_identities"] = append(in.Tables["external_identities"], identity)
	s, err := f.reader.Read(context.Background(), f.tenant, in)
	if err != nil || !s.GlobalKeys[RowRef{"users", 12}] || !s.GlobalKeys[RowRef{"external_identities", 11}] {
		t.Fatal("global occupied keys hidden", err)
	}
	cl, is, err := Compare(context.Background(), in, s)
	if err != nil || *cl["total"].Conflict != 2 || is.Total() != 2 {
		t.Fatal("global conflict comparison", err)
	}
	for _, i := range is.Issues() {
		if i.Issue.Code != "GLOBAL_KEY_CONFLICT" || i.Stage != "database" {
			t.Fatal("unexpected occupancy output")
		}
	}
}
func TestComparePGDDLAndRevoke(t *testing.T) {
	for _, kind := range []string{"ddl", "revoke"} {
		t.Run(kind, func(t *testing.T) {
			f := newCompareFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			hold, err := f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer hold.Rollback(ctx)
			if _, err = hold.Exec(ctx, "LOCK TABLE "+pgx.Identifier{f.schema, "admin_grants"}.Sanitize()+" IN ACCESS EXCLUSIVE MODE"); err != nil {
				t.Fatal(err)
			}
			observer, err := pgx.Connect(ctx, f.dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer observer.Close(ctx)
			type result struct {
				s   Snapshot
				err error
			}
			done := make(chan result, 1)
			go func() { s, e := f.reader.Read(ctx, f.tenant, f.input); done <- result{s, e} }()
			for {
				var count int
				if err = observer.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE usename=$1 AND wait_event_type='Lock'", f.role).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count > 0 {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("missing structure lock wait")
				case r := <-done:
					t.Fatal("unexpected early reader result", r.err)
				case <-time.After(10 * time.Millisecond):
				}
			}
			if kind == "ddl" {
				short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
				_, err = observer.Exec(short, "ALTER TABLE "+pgx.Identifier{f.schema, "users"}.Sanitize()+" RENAME COLUMN display_name TO changed_name")
				stop()
				if err == nil {
					t.Fatal("concurrent DDL bypassed structure lock")
				}
			} else {
				if _, err = observer.Exec(ctx, "REVOKE SELECT ON "+pgx.Identifier{f.schema, "users"}.Sanitize()+" FROM "+pgx.Identifier{f.role}.Sanitize()); err != nil {
					t.Fatal(err)
				}
			}
			if err = hold.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			res := <-done
			if kind == "ddl" {
				if res.err != nil || !res.s.TenantFound {
					t.Fatal("stable profile after blocked DDL", res.err)
				}
			} else if res.err == nil || res.err.Error() != "DATABASE_ACCESS_UNSUPPORTED" {
				t.Fatal("revoked SELECT returned complete snapshot", res.err)
			}
		})
	}
}
func TestComparePGQueryBudget(t *testing.T) {
	f := newCompareFixture(t)
	ctx := context.Background()
	tx, err := f.owner.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	b := queryBudget{tx: tx, count: 1}
	for n := 0; n < 127; n++ {
		if err = b.exec(ctx, "SELECT 1"); err != nil {
			t.Fatal("bounded actual SQL", err)
		}
	}
	if err = b.exec(ctx, "SELECT 1"); err == nil || err.Error() != "DATABASE_LIMIT" || b.count != 128 {
		t.Fatal("129th actual SQL allowed")
	}
}
