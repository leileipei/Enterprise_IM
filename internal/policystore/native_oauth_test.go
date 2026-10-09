package policystore_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/nativeauth"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeOAuthIPWindowIsSharedAcrossNodes(t *testing.T) {
	conn := db(t)
	ctx := context.Background()
	migration, err := os.ReadFile("../../db/migrations/000026_native_oauth_rate.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(migration)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := conn.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	nodes := make([]policystore.Service, 2)
	for i := range nodes {
		cfg, err := pgxpool.ParseConfig(os.Getenv("IM_TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.RuntimeParams["search_path"] = schema + ", public"
		cfg.MaxConns = 4
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		nodes[i] = policystore.Service{DB: pool}
		if err := nodes[i].CheckNativeAuthRuntime(ctx); err != nil {
			rows, queryErr := conn.Query(ctx, "SELECT conname,pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='native_oauth_request_windows'::regclass ORDER BY conname")
			if queryErr == nil {
				for rows.Next() {
					var name, definition string
					_ = rows.Scan(&name, &definition)
					t.Log(name, definition)
				}
				rows.Close()
			}
			cols, queryErr := conn.Query(ctx, "SELECT attname,format_type(atttypid,atttypmod),attnotnull FROM pg_attribute WHERE attrelid='native_oauth_request_windows'::regclass AND attnum>0 AND NOT attisdropped ORDER BY attnum")
			if queryErr == nil {
				for cols.Next() {
					var name, typ string
					var nonnull bool
					_ = cols.Scan(&name, &typ, &nonnull)
					t.Log(name, typ, nonnull)
				}
				cols.Close()
			}
			t.Fatal(err)
		}
	}
	var allowed, denied atomic.Int32
	failures := make(chan error, 40)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(node int) {
			defer wg.Done()
			delay, err := nodes[node].AllowNativeOAuthRequest(ctx, "192.0.2.7")
			if err == nil {
				allowed.Add(1)
				return
			}
			if errors.Is(err, nativeauth.ErrRateLimited) && delay > 0 && delay <= time.Minute {
				denied.Add(1)
				return
			}
			failures <- err
		}(i % 2)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error("unexpected rate failure", err)
	}
	if allowed.Load() != 20 || denied.Load() != 20 {
		t.Fatalf("shared quota allowed=%d denied=%d", allowed.Load(), denied.Load())
	}
	if _, err := nodes[0].AllowNativeOAuthRequest(ctx, "::ffff:192.0.2.7"); !errors.Is(err, nativeauth.ErrRateLimited) {
		t.Fatal("IPv4-mapped address bypassed shared quota")
	}
	if _, err := nodes[1].AllowNativeOAuthRequest(ctx, "192.0.2.8"); err != nil {
		t.Fatal("unrelated peer was rate limited")
	}
	run(t, conn, "UPDATE native_oauth_request_windows SET window_start=clock_timestamp()-interval '2 minutes'")
	if _, err := nodes[0].AllowNativeOAuthRequest(ctx, "192.0.2.7"); err != nil {
		t.Fatal("expired window did not reset")
	}
	var count int
	if err := conn.QueryRow(ctx, "SELECT max(request_count) FROM native_oauth_request_windows").Scan(&count); err != nil || count != 1 {
		t.Fatal("expired counters retained")
	}
	for _, ip := range []string{"", "arbitrary", "192.0.2.7:443"} {
		if _, err := nodes[0].AllowNativeOAuthRequest(ctx, ip); err == nil {
			t.Fatal("invalid peer accepted")
		}
	}
	run(t, conn, "DROP TABLE native_oauth_request_windows")
	if err := nodes[0].CheckNativeAuthRuntime(ctx); err == nil {
		t.Fatal("missing rate migration accepted at startup")
	}
	if _, err := nodes[0].AllowNativeOAuthRequest(ctx, "192.0.2.7"); !errors.Is(err, nativeauth.ErrUnavailable) {
		t.Fatal("missing limiter failed open")
	}
}

func TestNativeOAuthStartupRejectsReadOnlyRole(t *testing.T) {
	conn := db(t)
	ctx := context.Background()
	migration, err := os.ReadFile("../../db/migrations/000026_native_oauth_rate.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.PgConn().Exec(ctx, string(migration)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err = conn.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	role := "native_reader_" + processRandom(t)
	quoted := pgx.Identifier{role}.Sanitize()
	run(t, conn, "CREATE ROLE "+quoted+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS")
	t.Cleanup(func() { run(t, conn, "DROP OWNED BY "+quoted); run(t, conn, "DROP ROLE "+quoted) })
	run(t, conn, "GRANT USAGE ON SCHEMA "+pgx.Identifier{schema}.Sanitize()+" TO "+quoted)
	run(t, conn, "GRANT SELECT ON native_oauth_request_windows TO "+quoted)
	cfg, err := pgxpool.ParseConfig(os.Getenv("IM_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ", public"
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error { _, err := c.Exec(ctx, "SET ROLE "+quoted); return err }
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = (policystore.Service{DB: pool}).CheckNativeAuthRuntime(ctx); err == nil {
		t.Fatal("SELECT-only role accepted for writable native limiter")
	}
}
