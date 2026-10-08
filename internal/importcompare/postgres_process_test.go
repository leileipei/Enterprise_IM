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
	return processCallWithin(t, f, input, tenant, extra, 10*time.Second)
}
func processCallWithin(t *testing.T, f *compareFixture, input, tenant string, extra []string, limit time.Duration) (int, []byte, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
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
	t.Log("COMPARISON_REPORT " + string(normal))
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

func assertProcessIncomplete(t *testing.T, code int, raw, stderr []byte, expected string) {
	t.Helper()
	r, err := DecodeReport(context.Background(), raw)
	if code != 2 || err != nil || len(stderr) != 0 || r.Status != "incomplete" || r.ChecksComplete || r.DatabaseChecked {
		t.Fatal("process did not return an honest incomplete report", code, err)
	}
	for _, c := range r.ClassificationCounts {
		if c.New != nil || c.Identical != nil || c.Conflict != nil {
			t.Fatal("partial classification escaped")
		}
	}
	if expected != "" {
		found := false
		for _, issue := range r.Issues {
			found = found || string(issue.Issue.Code) == expected
		}
		if !found {
			t.Fatal("expected fixed diagnostic missing", expected)
		}
	}
}

func TestCompareProcessTLS(t *testing.T) {
	f := newCompareFixture(t)
	input := processInput(t, f.input)
	root, wrong := os.Getenv("IM_COMPARE_TEST_CA"), os.Getenv("IM_COMPARE_TEST_WRONG_CA")
	if root == "" || wrong == "" {
		t.Fatal("owned TLS certificate fixture required")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(home, ".postgresql"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"root.crt", "postgresql.crt", "postgresql.key"} {
		if err = syscall.Mkfifo(filepath.Join(home, ".postgresql", name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// A forbidden implicit credential/certificate read would block on one of these FIFOs.
	for _, name := range []string{".pgpass", ".pg_service.conf"} {
		if err = syscall.Mkfifo(filepath.Join(home, name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	poison := []string{"HOME=" + home, "PGSSLROOTCERT=" + filepath.Join(home, ".postgresql", "root.crt"),
		"PGSSLCERT=" + filepath.Join(home, ".postgresql", "postgresql.crt"),
		"PGSSLKEY=" + filepath.Join(home, ".postgresql", "postgresql.key"), "PGSSLMODE=disable",
		"PGPASSFILE=" + filepath.Join(home, ".pgpass"), "PGSERVICEFILE=" + filepath.Join(home, ".pg_service.conf"),
		"PGSERVICE=HOME_PRIVATE_CERT_MARKER", "PGPASSWORD=HOME_PRIVATE_CERT_MARKER"}
	settings := connectionSettings{}
	for k, v := range f.reader.Config.connection {
		settings[k] = v
	}
	settings["host"], settings["sslmode"], settings["sslrootcert"] = "127.0.0.1", "verify-full", root
	f.reader.Config.connection = settings
	for _, name := range []string{"verify_full_home_traps", "wrong_ca", "wrong_hostname", "downgrade"} {
		t.Run(name, func(t *testing.T) {
			settings["host"], settings["sslmode"], settings["sslrootcert"] = "127.0.0.1", "verify-full", root
			expected := "DATABASE_CONNECT_FAILED"
			switch name {
			case "wrong_ca":
				settings["sslrootcert"] = wrong
			case "wrong_hostname":
				settings["host"] = "localhost"
			case "downgrade":
				settings["sslmode"] = "disable"
				expected = "DATABASE_CONFIG_INVALID"
			}
			code, raw, stderr := processCall(t, f, input, f.tenant, poison)
			if bytes.Contains(raw, []byte(home)) || bytes.Contains(raw, []byte("HOME_PRIVATE_CERT_MARKER")) || bytes.Contains(raw, []byte(settings.canonical())) {
				t.Fatal("private configuration escaped report")
			}
			if name == "verify_full_home_traps" {
				r, err := DecodeReport(context.Background(), raw)
				if code != 0 || err != nil || len(stderr) != 0 || !r.DatabaseChecked || *r.ClassificationCounts["total"].Identical != 66 {
					t.Fatal("actual TLS process failed with HOME certificate traps", code, err)
				}
			} else {
				assertProcessIncomplete(t, code, raw, stderr, expected)
			}
		})
	}
}

func TestCompareProcessResourceEdges(t *testing.T) {
	f := newCompareFixture(t)
	input := processInput(t, f.input)
	users := pgx.Identifier{f.schema, "users"}.Sanitize()
	check := func(name, expected string) {
		t.Run(name, func(t *testing.T) {
			started := time.Now()
			code, raw, stderr := processCallWithin(t, f, input, f.tenant, nil, 33*time.Second)
			elapsed := time.Since(started)
			r, err := DecodeReport(context.Background(), raw)
			if err != nil || elapsed > 31*time.Second {
				t.Fatal("full process exceeded shared execution/cleanup budget", elapsed, err)
			}
			t.Logf("CLI_RESOURCE_RESULT %s elapsed_ms=%d status=%s exit=%d", name, elapsed.Milliseconds(), r.Status, code)
			if expected != "" {
				assertProcessIncomplete(t, code, raw, stderr, expected)
				return
			}
			if r.Status == "incomplete" {
				assertProcessIncomplete(t, code, raw, stderr, "TIMEOUT")
				return
			}
			if code != 0 || len(stderr) != 0 || !r.DatabaseChecked || r.Status != "valid" || *r.Counts["total"] != 66 {
				t.Fatal("maximum valid stock produced an incorrect complete report", code, r.Status)
			}
		})
	}
	f.exec(t, "INSERT INTO "+users+" (id,tenant_id,global_employee_no,display_name) SELECT ('88888888-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid,$1::uuid,'limit-'||n,'' FROM generate_series(1,19934) n", f.tenant)
	check("rows_20000", "")
	f.exec(t, "INSERT INTO "+users+" (id,tenant_id,global_employee_no,display_name) VALUES ($1::uuid,$2::uuid,'over-row','')", newID(70001), f.tenant)
	check("rows_20001", "DATABASE_LIMIT")
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
		t.Fatal("exact stock byte fixture arithmetic")
	}
	f.exec(t, "UPDATE "+users+" SET display_name=repeat('x',$1::int) WHERE global_employee_no LIKE '00008115%'", 4096-excess)
	check("bytes_64_mib", "")
	f.exec(t, "UPDATE "+users+" SET display_name=display_name||'x' WHERE global_employee_no LIKE '00008115%'")
	check("bytes_plus_one", "DATABASE_LIMIT")
	f.exec(t, "DELETE FROM "+users)
	f.exec(t, "INSERT INTO "+users+" (id,tenant_id,global_employee_no,display_name) VALUES ($1::uuid,$2::uuid,'oversized-cell',repeat('x',4097))", newID(70002), f.tenant)
	check("cell_4097", "DATABASE_DATA_UNSUPPORTED")
}
