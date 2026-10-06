package importpreflight

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPreflightPostgresOracle(t *testing.T) {
	dsn := os.Getenv("IM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires dedicated PostgreSQL oracle fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, e := pgx.Connect(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(context.Background())
	schema := fmt.Sprintf("im_preflight_%d", time.Now().UnixNano())
	if _, e = conn.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	defer conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	if _, e = conn.Exec(ctx, "SET search_path TO "+schema+", public"); e != nil {
		t.Fatal(e)
	}
	manifestBytes, err := os.ReadFile("testdata/sample_manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Hashes map[string]string `json:"migration_sha256"`
	}
	if json.Unmarshal(manifestBytes, &manifest) != nil || len(manifest.Hashes) != 21 {
		t.Fatal("migration manifest")
	}
	paths, e := filepath.Glob("../../db/migrations/*.up.sql")
	if e != nil || len(paths) != 21 {
		t.Fatal("frozen migration count")
	}
	for _, path := range paths {
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		if fmt.Sprintf("%x", sha256.Sum256(b)) != manifest.Hashes["db/migrations/"+filepath.Base(path)] {
			t.Fatal("migration source differs from approved sample baseline")
		}
		if _, e = conn.PgConn().Exec(ctx, string(b)).ReadAll(); e != nil {
			t.Fatal(e)
		}
		t.Logf("migration %s sha256=%x", filepath.Base(path), sha256.Sum256(b))
	}
	doc := sampleDocument(t)
	for _, ent := range entities {
		fs := fields[ent]
		names := []string{}
		params := []string{}
		for i, f := range fs {
			names = append(names, string(f))
			params = append(params, fmt.Sprintf("$%d", i+1))
		}
		query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", ent, strings.Join(names, ","), strings.Join(params, ","))
		for _, r := range doc.Tables[ent] {
			args := []any{}
			for _, f := range fs {
				v := r.Values[f]
				var arg any = v.Text
				if v.IsNull {
					arg = nil
				} else if specification(ent, f).kind == "bool" {
					arg = v.Bool
				} else if specification(ent, f).kind == "time" {
					arg = v.Time
				}
				args = append(args, arg)
			}
			if _, e = conn.Exec(ctx, query, args...); e != nil {
				t.Fatalf("sample table %s row %d: %v", ent, r.Ordinal, e)
			}
		}
		var n int
		if e = conn.QueryRow(ctx, "SELECT count(*) FROM "+string(ent)).Scan(&n); e != nil || n != len(doc.Tables[ent]) {
			t.Fatal("sample count")
		}
	}
	t.Run("positive", func(t *testing.T) {
		if Evaluate(ctx, sampleBytes(t)).ExitCode() != 0 {
			t.Fatal("database accepted but preflight rejected")
		}
	})
	b, e := os.ReadFile("testdata/database_constraint_cases.json")
	if e != nil {
		t.Fatal(e)
	}
	var cases []struct {
		ID    string `json:"case_id"`
		SQL   string `json:"sql"`
		State string `json:"expected_sqlstate"`
	}
	if json.Unmarshal(b, &cases) != nil || len(cases) != 11 {
		t.Fatal("oracle cases")
	}
	for i, tc := range mutations() {
		t.Run(tc.id, func(t *testing.T) {
			sql := cases[i]
			if sql.ID != tc.id || sql.State != tc.state {
				t.Fatal("oracle pairing")
			}
			tx, e := conn.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(ctx)
			_, e = tx.Exec(ctx, sql.SQL)
			var pe *pgconn.PgError
			if !errors.As(e, &pe) || pe.Code != tc.state {
				t.Fatalf("database SQLSTATE=%v wanted %s", e, tc.state)
			}
			r := Evaluate(ctx, mutateBytes(t, sampleBytes(t), tc.mutate))
			if r.ExitCode() != 1 || !r.ChecksComplete || !hasCode(r, tc.code) {
				t.Fatalf("database/preflight mismatch %+v", r.Issues)
			}
		})
	}
}
