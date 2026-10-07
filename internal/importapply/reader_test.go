package importapply

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	c "github.com/leileipei/Enterprise_IM/internal/importcompare"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"strings"
	"testing"
	"time"
)

func readerTenant(t *testing.T, f *appendFixture) {
	t.Helper()
	if _, e := f.Admin.Exec(context.Background(), "INSERT INTO tenants(id,code,name) VALUES($1,'fixture','Fixture')", fixtureTenant); e != nil {
		t.Fatal(e)
	}
}
func readAppend(t *testing.T, f *appendFixture) (c.Snapshot, error) {
	t.Helper()
	ctx := context.Background()
	tx, e := f.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite})
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	return c.ReadAppendSnapshot(ctx, tx, f.Schema, fixtureTenant, p.Document{Tables: map[p.Entity][]p.Record{}})
}
func TestAppendPGSnapshotLocks(t *testing.T) {
	f := appendDB(t, 22)
	readerTenant(t, f)
	ctx := context.Background()
	tx, e := f.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite})
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	s, e := c.ReadAppendSnapshot(ctx, tx, f.Schema, fixtureTenant, p.Document{Tables: map[p.Entity][]p.Record{}})
	if e != nil || !s.TenantFound {
		t.Fatal("append snapshot", e)
	}
	other, e := f.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer other.Rollback(ctx)
	other.Exec(ctx, "SET LOCAL lock_timeout='100ms'")
	_, e = other.Exec(ctx, "UPDATE "+tableName(f.Schema, "tenants")+" SET name='Concurrent' WHERE id=$1", fixtureTenant)
	if e == nil {
		t.Fatal("existing row was not locked")
	}
	if _, e = tx.Exec(ctx, "SELECT 1"); e != nil {
		t.Fatal("reader ended tx")
	}
}
func TestAppendPGWriterProfile(t *testing.T) {
	for _, scenario := range []string{"writer", "audit_insert_only", "column_insert", "readonly", "superuser", "bypassrls", "rls", "missing_migration", "wrong_type", "nondeterministic"} {
		t.Run(scenario, func(t *testing.T) {
			f := appendDB(t, 22)
			ctx := context.Background()
			if scenario == "audit_insert_only" {
				if _, e := f.Admin.Exec(ctx, "REVOKE SELECT ON audit_events FROM "+pgx.Identifier{f.Pool.Config().ConnConfig.User}.Sanitize()); e != nil {
					t.Fatal(e)
				}
			}
			if scenario == "column_insert" {
				if _, e := f.Admin.Exec(ctx, "REVOKE INSERT ON users FROM "+pgx.Identifier{f.Pool.Config().ConnConfig.User}.Sanitize()); e != nil {
					t.Fatal(e)
				}
				if _, e := f.Admin.Exec(ctx, "GRANT INSERT(id,tenant_id,global_employee_no,display_name,status) ON users TO "+pgx.Identifier{f.Pool.Config().ConnConfig.User}.Sanitize()); e != nil {
					t.Fatal(e)
				}
			}
			if scenario == "readonly" {
				if _, e := f.Admin.Exec(ctx, "REVOKE UPDATE ON ALL TABLES IN SCHEMA "+pgx.Identifier{f.Schema}.Sanitize()+" FROM "+pgx.Identifier{f.Pool.Config().ConnConfig.User}.Sanitize()); e != nil {
					t.Fatal(e)
				}
			}
			if scenario == "rls" {
				f.Admin.Exec(ctx, "ALTER TABLE users ENABLE ROW LEVEL SECURITY")
			}
			if scenario == "missing_migration" {
				f.Admin.Exec(ctx, "DROP TABLE import_batches")
			}
			if scenario == "wrong_type" {
				f.Admin.Exec(ctx, "ALTER TABLE users ALTER COLUMN display_name TYPE varchar")
			}
			if scenario == "nondeterministic" {
				_, e := f.Admin.Exec(ctx, "CREATE COLLATION nondet(provider=icu,locale='und-u-ks-level2',deterministic=false)")
				if e != nil {
					t.Fatal(e)
				}
				_, e = f.Admin.Exec(ctx, "ALTER TABLE users ALTER COLUMN global_employee_no TYPE text COLLATE nondet")
				if e != nil {
					t.Fatal(e)
				}
			}
			var tx pgx.Tx
			var e error
			if scenario == "superuser" || scenario == "bypassrls" {
				tx, e = f.Admin.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite})
				if scenario == "bypassrls" {
					role := f.Schema + "_bypass"
					_, e = f.Admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" BYPASSRLS")
					if e != nil {
						t.Fatal(e)
					}
					t.Cleanup(func() { f.Admin.Exec(ctx, "DROP ROLE "+pgx.Identifier{role}.Sanitize()) })
					_, e = tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize())
				}
			} else {
				tx, e = f.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite})
			}
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(ctx)
			e = c.CheckAppendProfile(ctx, tx, f.Schema)
			if (scenario == "writer" || scenario == "audit_insert_only" || scenario == "column_insert") && e != nil {
				t.Fatal(e)
			}
			if scenario != "writer" && scenario != "audit_insert_only" && scenario != "column_insert" && e == nil {
				t.Fatal("unsafe profile accepted")
			}
		})
	}
}
func TestAppendPGStoredLimits(t *testing.T) {
	for _, scenario := range []string{"rows20000", "rows20001", "cell4096", "cell4097", "bytes64m", "bytes64mplus1", "infinity"} {
		t.Run(scenario, func(t *testing.T) {
			f := appendDB(t, 22)
			readerTenant(t, f)
			ctx := context.Background()
			n, display := 1, 4096
			if scenario == "rows20000" {
				n = 19999
				display = 1
			}
			if scenario == "rows20001" {
				n = 20000
				display = 1
			}
			if scenario == "cell4097" {
				display = 4097
			}
			sql := "INSERT INTO users(id,tenant_id,global_employee_no,display_name) SELECT ('92000000-0000-4000-8000-'||lpad(i::text,12,'0'))::uuid,$1,('92000000-0000-4000-8000-'||lpad(i::text,12,'0')),repeat('x',$3) FROM generate_series(1,$2) i"
			if strings.HasPrefix(scenario, "bytes64m") {
				n = (64*1024*1024 - 56 - 114) / 4210
				display = 4096
			}
			if _, e := f.Admin.Exec(ctx, sql, fixtureTenant, n, display); e != nil {
				t.Fatal(e)
			}
			if strings.HasPrefix(scenario, "bytes64m") {
				tail := 64*1024*1024 - 56 - n*4210 - 114
				if scenario == "bytes64mplus1" {
					tail++
				}
				if _, e := f.Admin.Exec(ctx, "INSERT INTO users(id,tenant_id,global_employee_no,display_name) VALUES('93000000-0000-4000-8000-000000000001',$1,'93000000-0000-4000-8000-000000000001',repeat('x',$2))", fixtureTenant, tail); e != nil {
					t.Fatal(e)
				}
			}
			if scenario == "infinity" {
				for _, s := range []string{"INSERT INTO legal_entities(id,tenant_id,code,name) VALUES('94000000-0000-4000-8000-000000000001',$1,'l','L')", "INSERT INTO organizations(id,tenant_id,legal_entity_id,org_type,code,name) VALUES('94000000-0000-4000-8000-000000000002',$1,'94000000-0000-4000-8000-000000000001','company','o','O')", "INSERT INTO user_organizations(id,tenant_id,user_id,organization_id,effective_from) VALUES('94000000-0000-4000-8000-000000000003',$1,'92000000-0000-4000-8000-000000000001','94000000-0000-4000-8000-000000000002','infinity')"} {
					if _, e := f.Admin.Exec(ctx, s, fixtureTenant); e != nil {
						t.Fatal(e)
					}
				}
			}
			s, e := readAppend(t, f)
			valid := scenario == "rows20000" || scenario == "cell4096" || scenario == "bytes64m"
			if valid && e != nil {
				t.Fatalf("valid boundary: %v", e)
			}
			if !valid && (e == nil || s.TenantFound) {
				t.Fatalf("invalid boundary yielded partial snapshot: %v", e)
			}
		})
	}
}
func TestAppendBudget(t *testing.T) {
	start := time.Now()
	expiry := start.Add(20 * time.Second)
	ctx, cancel, e := RequestContext(context.Background(), start, expiry)
	if e != nil {
		t.Fatal(e)
	}
	defer cancel()
	if got, _ := ctx.Deadline(); !got.Equal(expiry) {
		t.Fatal("deadline reset")
	}
	b := budgetFrom(ctx)
	for i := 0; i < 256; i++ {
		if e := b.charge(ctx, readSQL); e != nil {
			t.Fatal(e)
		}
	}
	if e := b.charge(ctx, readSQL); e == nil {
		t.Fatal("257th read accepted")
	}
	if _, _, e := RequestContext(ctx, start, time.Time{}); e == nil {
		t.Fatal("zero token expiry")
	}
	f := appendDB(t, 22)
	tx, e := f.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	m := newMeteredTx(tx, &sqlBudget{Deadline: expiry})
	rows, e := m.Query(ctx, "SELECT 1")
	if e != nil {
		t.Fatal(e)
	}
	if !rows.Next() {
		t.Fatal("query canceled before consume")
	}
	var n int
	if e = rows.Scan(&n); e != nil || n != 1 {
		t.Fatal(e)
	}
	rows.Close()
	if e = m.QueryRow(ctx, "SELECT 1").Scan(&n); e != nil {
		t.Fatal("row canceled before scan", e)
	}
	_ = fmt.Sprint(n)
}

func TestAppendBudgetInsertClassification(t *testing.T) {
	if sqlClass(`INSERT INTO "public"."users" (id) VALUES($1)`) != insertSQL {
		t.Fatal("business inserts escaped insert budget")
	}
	if sqlClass(`INSERT INTO "public"."import_batches" (request_id) VALUES($1)`) != controlSQL {
		t.Fatal("receipt counted as business insert")
	}
}
