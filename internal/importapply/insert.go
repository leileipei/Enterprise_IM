package importapply

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"strings"
)

func insertPlan(ctx context.Context, tx pgx.Tx, schema string, plan Plan) error {
	schemas := map[p.Entity]p.TableSchema{}
	for _, s := range p.Schema() {
		schemas[s.Entity] = s
	}
	for _, row := range plan.Rows {
		if row.Ref.Entity == "tenants" || row.Ref.Entity == "external_identities" || row.Ref.Entity == "admin_grants" {
			return errPlan
		}
		s, ok := schemas[row.Ref.Entity]
		if !ok {
			return errPlan
		}
		cols := []string{}
		params := []string{}
		args := []any{}
		for i, f := range s.Fields {
			v, ok := row.Record.Values[f.Name]
			if !ok || !v.Valid {
				return errPlan
			}
			cols = append(cols, pgx.Identifier{string(f.Name)}.Sanitize())
			params = append(params, fmt.Sprintf("$%d", i+1))
			var a any
			if !v.IsNull {
				switch f.Kind {
				case "bool":
					a = v.Bool
				case "time":
					if v.Time == nil {
						return errPlan
					}
					a = *v.Time
				default:
					a = v.Text
				}
			}
			args = append(args, a)
		}
		tag, e := tx.Exec(ctx, "INSERT INTO "+tableName(schema, string(row.Ref.Entity))+" ("+strings.Join(cols, ",")+") VALUES("+strings.Join(params, ",")+")", args...)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return errPlan
		}
	}
	return nil
}
