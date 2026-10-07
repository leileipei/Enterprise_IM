package importapply

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type appendFixture struct {
	Admin  *pgx.Conn
	Pool   *pgxpool.Pool
	Schema string
}

func appendDB(t *testing.T, last int) *appendFixture {
	t.Helper()
	adminURL := os.Getenv("IM_IMPORT_APPLY_TEST_ADMIN_URL")
	writerURL := os.Getenv("IM_IMPORT_APPLY_TEST_DATABASE_URL")
	if adminURL == "" || writerURL == "" {
		t.Skip("dedicated append PostgreSQL fixture required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal("admin fixture unavailable")
	}
	schema := fmt.Sprintf("im_append_%d", time.Now().UnixNano())
	q := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+q); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(ctx, "DROP SCHEMA "+q+" CASCADE"); admin.Close(ctx) })
	if _, err = admin.Exec(ctx, "SET search_path TO "+q+", public"); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob("../../db/migrations/*.up.sql")
	for _, path := range paths {
		var n int
		fmt.Sscanf(filepath.Base(path), "%d_", &n)
		if n > last {
			continue
		}
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = admin.PgConn().Exec(ctx, string(b)).ReadAll(); e != nil {
			t.Fatalf("migration %s: %v", filepath.Base(path), e)
		}
	}
	cfg, err := pgxpool.ParseConfig(writerURL)
	if err != nil {
		t.Fatal("writer config invalid")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var role string
	if err = pool.QueryRow(ctx, "SELECT current_user").Scan(&role); err != nil {
		t.Fatal(err)
	}
	quotedRole := pgx.Identifier{role}.Sanitize()
	for _, sql := range []string{"GRANT USAGE ON SCHEMA " + q + " TO " + quotedRole, "GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA " + q + " TO " + quotedRole, "GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA " + q + " TO " + quotedRole} {
		if _, err = admin.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	return &appendFixture{admin, pool, schema}
}
