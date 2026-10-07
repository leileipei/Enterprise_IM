package importcompare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func processBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("IM_COMPARE_TEST_BINARY")
	if bin == "" {
		t.Fatal("process gate needs actual comparison binary")
	}
	return bin
}
func documentBytes(t *testing.T, d p.Document) []byte {
	t.Helper()
	tables := map[p.Entity][]map[p.Field]json.RawMessage{}
	for _, s := range p.Schema() {
		tables[s.Entity] = []map[p.Field]json.RawMessage{}
		for _, r := range d.Tables[s.Entity] {
			raw, e := rawRecord(r, s)
			if e != nil {
				t.Fatal(e)
			}
			tables[s.Entity] = append(tables[s.Entity], raw)
		}
	}
	b, e := json.Marshal(map[string]any{"format_version": 1, "baseline_commit": d.BaselineCommit, "data_origin": "synthetic_process_fixture", "reference_time": "2026-10-07T00:00:00Z", "identity_source_selected": false, "tables": tables})
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func processInput(t *testing.T, d p.Document) string {
	t.Helper()
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	name := filepath.Join(dir, "private-input-marker")
	if e = os.WriteFile(name, documentBytes(t, d), 0600); e != nil {
		t.Fatal(e)
	}
	return name
}
func processCall(t *testing.T, f *compareFixture, input, tenant string, extra []string) (int, []byte, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, processBinary(t), "--input", input, "--tenant-id", tenant)
	cmd.Env = append(os.Environ(), "IM_IMPORT_COMPARE_DATABASE_URL="+f.reader.Config.connection.canonical(), "IM_IMPORT_COMPARE_SCHEMA="+f.schema)
	cmd.Env = append(cmd.Env, extra...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	exit := 0
	if err != nil {
		var e *exec.ExitError
		if !errors.As(err, &e) {
			t.Fatal("process run")
		}
		exit = e.ExitCode()
	}
	return exit, out.Bytes(), stderr.Bytes()
}
func TestCompareProcessEnvIsolation(t *testing.T) {
	f := newCompareFixture(t)
	path := processInput(t, f.input)
	code, normal, stderr := processCall(t, f, path, f.tenant, nil)
	if code != 0 || len(stderr) != 0 {
		t.Fatal("actual process positive path")
	}
	r, err := DecodeReport(context.Background(), normal)
	if err != nil || *r.ClassificationCounts["total"].Identical != 66 || r.ImportAuthorized {
		t.Fatal("actual report contract", err)
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	var accepted atomic.Int64
	go func() {
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{".pgpass", ".pg_service.conf"} {
		if e = syscall.Mkfifo(filepath.Join(dir, name), 0600); e != nil {
			t.Fatal(e)
		}
	}

	host, port, _ := net.SplitHostPort(listener.Addr().String())
	poison := []string{"HOME=" + dir, "PGHOST=" + host, "PGPORT=" + port, "PGUSER=private-marker", "PGPASSWORD=private-marker", "PGSERVICE=private-marker", "PGSERVICEFILE=" + filepath.Join(dir, ".pg_service.conf"), "PGPASSFILE=" + filepath.Join(dir, ".pgpass"), "PGOPTIONS=-c search_path=private_marker", "IM_DATABASE_URL=postgres://private-marker", "GODEBUG=private-marker"}
	code, dirty, stderr := processCall(t, f, path, f.tenant, poison)
	if code != 0 || !bytes.Equal(normal, dirty) || len(stderr) != 0 || accepted.Load() != 0 {
		t.Fatal("environment defaults changed actual target")
	}
	code, raw, stderr := processCall(t, f, path, newID(92300), poison)
	r, err = DecodeReport(context.Background(), raw)
	if code != 1 || err != nil || r.DatabaseChecked || r.ClassificationCounts["total"].New != nil || len(stderr) != 0 {
		t.Fatal("single-tenant selection mismatch")
	}
}
func TestCompareProcessActualSIGTERM(t *testing.T) {
	f := newCompareFixture(t)
	path := processInput(t, f.input)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	hold, e := f.owner.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer hold.Rollback(ctx)
	_, e = hold.Exec(ctx, "LOCK TABLE "+pgx.Identifier{f.schema, "admin_grants"}.Sanitize()+" IN ACCESS EXCLUSIVE MODE")
	if e != nil {
		t.Fatal(e)
	}
	observer, e := pgx.Connect(ctx, f.dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer observer.Close(ctx)
	cmd := exec.Command(processBinary(t), "--input", path, "--tenant-id", f.tenant)
	cmd.Env = append(os.Environ(), "IM_IMPORT_COMPARE_DATABASE_URL="+f.reader.Config.connection.canonical(), "IM_IMPORT_COMPARE_SCHEMA="+f.schema)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	for {
		var n int
		if e = observer.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE usename=$1 AND wait_event_type='Lock'", f.role).Scan(&n); e != nil {
			t.Fatal(e)
		}
		if n > 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("command did not wait on actual database")
		case <-time.After(10 * time.Millisecond):
		}
	}
	start := time.Now()
	cmd.Process.Signal(syscall.SIGTERM)
	e = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(e, &exit) || exit.ExitCode() != 2 || time.Since(start) > 1500*time.Millisecond || len(stderr.Bytes()) != 0 {
		t.Fatal("actual SIGTERM did not terminate bounded command")
	}
	r, e := DecodeReport(context.Background(), out.Bytes())
	if e != nil || r.Status != "incomplete" || r.DatabaseChecked || r.ClassificationCounts["total"].New != nil {
		t.Fatal("signal returned partial pass", e)
	}
	for {
		var n int
		e = observer.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE usename=$1", f.role).Scan(&n)
		if e != nil {
			t.Fatal(e)
		}
		if n == 0 {
			break
		}
		if time.Now().After(start.Add(time.Second)) {
			t.Fatal("canceled DB connection remained beyond cleanup budget")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
