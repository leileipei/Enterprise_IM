package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/retention"
)

func testEnv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}
func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestCleanerConfigDefaultsAndRejections(t *testing.T) {
	cfg, err := configFromEnv(testEnv(nil))
	if err != nil || cfg.Enabled || cfg.BatchSize != 100 {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	for _, size := range []string{"1", "1000"} {
		cfg, err := configFromEnv(testEnv(map[string]string{"IM_DATABASE_URL": "postgres://localhost/db", "IM_BODY_CLEANER_ENABLED": "true", "IM_BODY_CLEANER_BATCH_SIZE": size}))
		if err != nil || !cfg.Enabled {
			t.Fatalf("valid config: %+v %v", cfg, err)
		}
	}
	for _, values := range []map[string]string{
		{"IM_BODY_CLEANER_ENABLED": "true"},
		{"IM_BODY_CLEANER_ENABLED": "TRUE"},
		{"IM_BODY_CLEANER_ENABLED": "1"},
		{"IM_BODY_CLEANER_ENABLED": " true "},
		{"IM_BODY_CLEANER_BATCH_SIZE": "0"},
		{"IM_BODY_CLEANER_BATCH_SIZE": "-1"},
		{"IM_BODY_CLEANER_BATCH_SIZE": "1001"},
		{"IM_BODY_CLEANER_BATCH_SIZE": "bad"},
	} {
		if _, err := configFromEnv(testEnv(values)); err == nil {
			t.Fatalf("invalid config accepted: %v", values)
		}
	}
	cfg, err = configFromEnv(testEnv(map[string]string{"IM_BODY_CLEANER_ENABLED": "false"}))
	if err != nil || cfg.Enabled {
		t.Fatalf("explicit false: %+v %v", cfg, err)
	}
}

func TestCleanerDisabledDoesNotConnect(t *testing.T) {
	cfg, err := configFromEnv(testEnv(map[string]string{"IM_DATABASE_URL": "intentionally invalid connection"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), cfg, testLogger()); err != nil {
		t.Fatalf("disabled cleaner used database: %v", err)
	}
}

type fakeSweepWorker struct {
	ids     []string
	listErr error
	calls   []string
	process func(context.Context, string) (retention.BatchResult, error)
}

func (w *fakeSweepWorker) ListActiveTenants(context.Context) ([]string, error) {
	return w.ids, w.listErr
}
func (w *fakeSweepWorker) ProcessTenant(ctx context.Context, id string) (retention.BatchResult, error) {
	w.calls = append(w.calls, id)
	return w.process(ctx, id)
}

func TestCleanerSweepFairnessAndCancellation(t *testing.T) {
	t.Run("partial failure", func(t *testing.T) {
		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))
		worker := &fakeSweepWorker{ids: []string{"a", "b"}, process: func(ctx context.Context, id string) (retention.BatchResult, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 5*time.Second {
				t.Fatal("unbounded tenant context")
			}
			if id == "a" {
				return retention.BatchResult{}, errors.New("sensitive-message-body-and-DSN")
			}
			return retention.BatchResult{TenantID: id, ConversationID: "c", BatchID: "batch", ClearedCount: 1}, nil
		}}
		count, err := runSweep(context.Background(), worker, logger)
		if count != 1 || err == nil || strings.Join(worker.calls, ",") != "a,b" {
			t.Fatalf("unfair sweep: %d %v %v", count, worker.calls, err)
		}
		if strings.Contains(logs.String(), "sensitive-message-body-and-DSN") {
			t.Fatalf("logged database details: %s", logs.String())
		}
	})
	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		worker := &fakeSweepWorker{ids: []string{"a", "b"}, process: func(context.Context, string) (retention.BatchResult, error) {
			cancel()
			return retention.BatchResult{}, nil
		}}
		if _, err := runSweep(ctx, worker, testLogger()); !errors.Is(err, context.Canceled) || len(worker.calls) != 1 {
			t.Fatalf("processed after cancellation: %v %v", worker.calls, err)
		}
	})
	t.Run("database unavailable", func(t *testing.T) {
		worker := &fakeSweepWorker{listErr: errors.New("private DSN")}
		if _, err := runSweep(context.Background(), worker, testLogger()); err == nil {
			t.Fatal("database outage ignored")
		}
		worker.listErr = nil
		worker.ids = []string{}
		if count, err := runSweep(context.Background(), worker, testLogger()); err != nil || count != 0 {
			t.Fatalf("did not recover: %d %v", count, err)
		}
	})
}

func TestCleanerBackoff(t *testing.T) {
	for streak, want := range map[int]time.Duration{0: time.Second, 1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 6: 30 * time.Second, 100: 30 * time.Second} {
		if got := sweepDelay(streak); got != want {
			t.Fatalf("backoff %d: %v want %v", streak, got, want)
		}
	}
}

func TestCleanerProductionProcess(t *testing.T) {
	dsn := os.Getenv("IM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set IM_TEST_DATABASE_URL for PostgreSQL process test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	schema := fmt.Sprintf("im_cleaner_process_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	if _, err := conn.Exec(ctx, "SET search_path TO "+schema+",public"); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob("../../db/migrations/*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.PgConn().Exec(ctx, string(data)).ReadAll(); err != nil {
			t.Fatal(err)
		}
	}
	_, err = conn.Exec(ctx, `INSERT INTO tenants(id,code,name) VALUES ('00000000-0000-4000-8000-000000003001','t','T');
 INSERT INTO legal_entities(id,tenant_id,code,name) VALUES ('00000000-0000-4000-8000-000000003002','00000000-0000-4000-8000-000000003001','l','L');
 INSERT INTO organizations(id,tenant_id,legal_entity_id,org_type,code,name) VALUES ('00000000-0000-4000-8000-000000003003','00000000-0000-4000-8000-000000003001','00000000-0000-4000-8000-000000003002','company','o','O');
 INSERT INTO users(id,tenant_id,global_employee_no,display_name) VALUES ('00000000-0000-4000-8000-000000003004','00000000-0000-4000-8000-000000003001','a','A'),('00000000-0000-4000-8000-000000003005','00000000-0000-4000-8000-000000003001','b','B');
 INSERT INTO user_organizations(id,tenant_id,user_id,organization_id,effective_from) VALUES
 ('00000000-0000-4000-8000-000000003006','00000000-0000-4000-8000-000000003001','00000000-0000-4000-8000-000000003004','00000000-0000-4000-8000-000000003003','2020-01-01'),
 ('00000000-0000-4000-8000-000000003007','00000000-0000-4000-8000-000000003001','00000000-0000-4000-8000-000000003005','00000000-0000-4000-8000-000000003003','2020-01-01');
 INSERT INTO conversations(id,tenant_id,direct_user_low_id,direct_user_high_id,direct_low_membership_id,direct_high_membership_id,created_by_user_id,last_seq) VALUES
 ('00000000-0000-4000-8000-000000003008','00000000-0000-4000-8000-000000003001','00000000-0000-4000-8000-000000003004','00000000-0000-4000-8000-000000003005','00000000-0000-4000-8000-000000003006','00000000-0000-4000-8000-000000003007','00000000-0000-4000-8000-000000003004',1);
 INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest,accepted_at) VALUES
 ('00000000-0000-4000-8000-000000003001','00000000-0000-4000-8000-000000003008',1,'00000000-0000-4000-8000-000000003004','00000000-0000-4000-8000-000000003006','0199f04a-0000-7000-8000-000000000001','process-private-body',decode(repeat('ab',32),'hex'),clock_timestamp()-INTERVAL '8784 hours')`)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema+",public")
	parsed.RawQuery = query.Encode()
	binary := filepath.Join(t.TempDir(), "im-retention-worker")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	disabled := exec.CommandContext(ctx, binary)
	disabled.Env = append(os.Environ(), "IM_DATABASE_URL="+parsed.String(), "IM_BODY_CLEANER_ENABLED=", "IM_BODY_CLEANER_BATCH_SIZE=100")
	if out, err := disabled.CombinedOutput(); err != nil {
		t.Fatalf("disabled process: %s %v", out, err)
	}
	var count int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM messages WHERE text_body IS NOT NULL").Scan(&count); err != nil || count != 1 {
		t.Fatalf("default cleaner changed data: %d %v", count, err)
	}
	logPath := filepath.Join(t.TempDir(), "process.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	child := exec.CommandContext(ctx, binary)
	child.Env = append(os.Environ(), "IM_DATABASE_URL="+parsed.String(), "IM_BODY_CLEANER_ENABLED=true", "IM_BODY_CLEANER_BATCH_SIZE=100")
	child.Stdout = logFile
	child.Stderr = logFile
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	for {
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM message_body_clear_batches").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("process exited before cleaning: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SIGTERM exit: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM messages WHERE text_body IS NULL AND body_cleared_at IS NOT NULL").Scan(&count); err != nil || count != 1 {
		t.Fatalf("enabled process did not clear: %d %v", count, err)
	}
	logs, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logs), "process-private-body") || strings.Contains(string(logs), "local_p4_04_test") {
		t.Fatalf("process leaked private data: %s", logs)
	}
}
