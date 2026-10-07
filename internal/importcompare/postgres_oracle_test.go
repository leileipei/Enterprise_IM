package importcompare

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"os"
	"strings"
	"testing"
)

func tryInsert(ctx context.Context, tx pgx.Tx, schema string, table p.TableSchema, r p.Record) error {
	cols, params := []string{}, []string{}
	args := []any{}
	for n, f := range table.Fields {
		cols = append(cols, pgx.Identifier{string(f.Name)}.Sanitize())
		params = append(params, fmt.Sprintf("$%d", n+1))
		v := r.Values[f.Name]
		var arg any = v.Text
		if v.IsNull {
			arg = nil
		} else if f.Kind == "bool" {
			arg = v.Bool
		} else if f.Kind == "time" {
			arg = v.Time
		}
		args = append(args, arg)
	}
	_, err := tx.Exec(ctx, "INSERT INTO "+qualified(schema, table.Entity)+" ("+strings.Join(cols, ",")+") VALUES ("+strings.Join(params, ",")+")", args...)
	return err
}
func TestComparePGOracle(t *testing.T) {
	for _, kind := range []string{"new", "pk", "unique", "interval", "dependency_fk", "additional_check"} {
		t.Run(kind, func(t *testing.T) {
			f := newCompareFixture(t)
			ctx := context.Background()
			in := cloneDoc(f.input)
			entity := p.Entity("users")
			candidate := cloneRecord(in.Tables[entity][0])
			text(&candidate, "id", newID(93001))
			text(&candidate, "global_employee_no", "oracle-new-user")
			expected, code := "", ""
			switch kind {
			case "pk":
				candidate = cloneRecord(in.Tables[entity][0])
				text(&candidate, "display_name", "proposed-difference")
				in.Tables[entity][0] = candidate
				expected, code = "23505", "STORED_VALUE_DIFFERS"
			case "unique":
				text(&candidate, "global_employee_no", in.Tables[entity][0].Values["global_employee_no"].Text)
				for _, e := range []p.Entity{"user_organizations", "user_departments", "external_identities", "admin_grants"} {
					in.Tables[e] = nil
				}
				in.Tables["users"] = []p.Record{candidate}
				expected, code = "23505", "STORED_UNIQUE_CONFLICT"
			case "interval":
				entity = "user_organizations"
				candidate = cloneRecord(in.Tables[entity][0])
				text(&candidate, "id", newID(93002))
				candidate.Values["is_primary"] = p.Value{Valid: true}
				in.Tables[entity] = []p.Record{candidate}
				in.Tables["user_departments"] = nil
				in.Tables["admin_grants"] = nil
				expected, code = "23P01", "INTERVAL_OVERLAP"
			case "dependency_fk":
				entity = "user_organizations"
				candidate = cloneRecord(in.Tables[entity][0])
				orgID := candidate.Values["organization_id"].Text
				var organization p.Record
				for _, org := range in.Tables["organizations"] {
					if org.Values["id"].Text == orgID {
						organization = cloneRecord(org)
					}
				}
				text(&organization, "id", newID(93003))
				organization.Values["parent_id"] = p.Value{Valid: true, IsNull: true}
				text(&candidate, "id", newID(93004))
				text(&candidate, "organization_id", organization.Values["id"].Text)
				candidate.Values["is_primary"] = p.Value{Valid: true}
				in.Tables["organizations"] = []p.Record{organization}
				in.Tables["departments"] = nil
				in.Tables["user_departments"] = nil
				in.Tables["admin_grants"] = nil
				in.Tables[entity] = []p.Record{candidate}
				expected, code = "23503", "REF_NOT_FOUND"
			case "additional_check":
				f.exec(t, "ALTER TABLE "+qualified(f.schema, "users")+" ADD CONSTRAINT synthetic_extra CHECK (display_name<>'blocked_new')")
				text(&candidate, "display_name", "blocked_new")
				expected = "23514"
			}
			if kind == "new" || kind == "additional_check" {
				in.Tables[entity] = append(in.Tables[entity], candidate)
			}
			renumber(&in)
			check := p.NewCollector()
			if err := p.ValidateModel(ctx, in, check); err != nil || p.BuildReport(p.Outcome{Status: "valid", ChecksComplete: true}, check).ErrorsTotal != 0 {
				t.Fatal("oracle input is not independently closed")
			}
			s, err := f.reader.Read(ctx, f.tenant, in)
			if err != nil {
				t.Fatal(err)
			}
			classes, issues, err := Compare(ctx, in, s)
			if err != nil {
				t.Fatal(err)
			}
			if code != "" && !found(issues, p.Code(code), entity, 0) {
				t.Fatal("comparison failed to block stored constraint")
			}
			if kind == "new" || kind == "additional_check" {
				if *classes["users"].New != 1 {
					t.Fatal("candidate not new")
				}
				file := p.Evaluate(ctx, documentBytes(t, in))
				report := BuildReport(file, "valid", true, classes, issues)
				if report.AdditionalDatabaseRulesChecked || report.ImportAuthorized {
					t.Fatal("known profile claimed full insert approval")
				}
			}
			before := f.digest(t)
			tx, err := f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			for _, table := range p.Schema() {
				if table.Entity == entity {
					err = tryInsert(ctx, tx, f.schema, table, candidate)
					break
				}
			}
			if expected == "" {
				if err != nil {
					t.Fatal("compatible candidate rejected", err)
				}
			} else {
				var pg *pgconn.PgError
				if !errors.As(err, &pg) || pg.Code != expected {
					t.Fatal("actual insertion did not match expected known boundary", err)
				}
			}
			tx.Rollback(ctx)
			if before != f.digest(t) {
				t.Fatal("oracle rollback changed fixture")
			}
		})
	}
}
func TestComparePGTLS(t *testing.T) {
	f := newCompareFixture(t)
	root := os.Getenv("IM_COMPARE_TEST_CA")
	wrongRoot := os.Getenv("IM_COMPARE_TEST_WRONG_CA")
	if root == "" || wrongRoot == "" {
		t.Fatal("TLS gate requires owned certificates")
	}
	cfg := f.reader.Config
	settings := connectionSettings{}
	for k, v := range cfg.connection {
		settings[k] = v
	}
	settings["host"] = "127.0.0.1"
	settings["port"] = "5432"
	settings["sslmode"] = "verify-full"
	settings["sslrootcert"] = root
	cfg.connection = settings
	t.Run("verify_full", func(t *testing.T) {
		s, err := (PGReader{Config: cfg}).Read(context.Background(), f.tenant, f.input)
		if err != nil || !s.TenantFound {
			t.Fatal("verify-full owned TLS", err)
		}
	})
	t.Run("wrong_ca", func(t *testing.T) {
		settings["sslrootcert"] = wrongRoot
		_, err := (PGReader{Config: cfg}).Read(context.Background(), f.tenant, f.input)
		if err == nil || err.Error() != "DATABASE_CONNECT_FAILED" {
			t.Fatal("wrong CA allowed")
		}
	})
	t.Run("wrong_hostname", func(t *testing.T) {
		settings["sslrootcert"] = root
		settings["host"] = "localhost"
		_, err := (PGReader{Config: cfg}).Read(context.Background(), f.tenant, f.input)
		if err == nil || err.Error() != "DATABASE_CONNECT_FAILED" {
			t.Fatal("wrong hostname allowed")
		}
	})
}
