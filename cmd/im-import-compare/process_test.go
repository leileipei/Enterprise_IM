//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	c "github.com/leileipei/Enterprise_IM/internal/importcompare"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var buildOnce sync.Once
var commandBinary, buildFailure string

func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		commandBinary = os.Getenv("IM_COMPARE_TEST_BINARY")
		if commandBinary != "" {
			return
		}
		dir, err := os.MkdirTemp("", "im-compare-bin-")
		if err != nil {
			buildFailure = err.Error()
			return
		}
		commandBinary = filepath.Join(dir, "im-import-compare")
		b, e := exec.Command("go", "build", "-mod=readonly", "-o", commandBinary, ".").CombinedOutput()
		if e != nil {
			buildFailure = string(b)
		}
	})
	if buildFailure != "" {
		t.Fatal(buildFailure)
	}
	return commandBinary
}
func physicalFile(t *testing.T, raw []byte) string {
	t.Helper()
	d, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(d, "private-path-marker")
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	return path
}
func call(t *testing.T, bin string, args []string, env []string) (int, []byte, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	var out, err bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &err
	e := cmd.Run()
	code := 0
	if e != nil {
		var exit *exec.ExitError
		if !errors.As(e, &exit) {
			t.Fatal("actual command failed to run")
		}
		code = exit.ExitCode()
	}
	return code, out.Bytes(), err.Bytes()
}
func TestCompareProcessHelpVersion(t *testing.T) {
	for _, flag := range []string{"--help", "--version"} {
		code, b, e := call(t, binary(t), []string{flag}, []string{"IM_IMPORT_COMPARE_DATABASE_URL=service=/secret", "HOME=/private/marker", "PGHOST=/private/marker"})
		if code != 0 || len(b) == 0 || len(e) != 0 || bytes.Contains(b, []byte("marker")) {
			t.Fatal("exclusive help/version touched configuration")
		}
	}
}
func TestCompareProcessNoNetwork(t *testing.T) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	accepted := make(chan bool, 1)
	go func() {
		c, e := listener.Accept()
		if e == nil {
			c.Close()
			accepted <- true
		}
	}()
	sample, e := os.ReadFile("../../internal/importpreflight/testdata/sample_data_v1.json")
	if e != nil {
		t.Fatal(e)
	}
	cases := [][]byte{[]byte(`{}`), sample}
	for _, raw := range cases {
		path := physicalFile(t, raw)
		code, b, stderr := call(t, binary(t), []string{"--input", path, "--tenant-id", "00000000-0000-4000-8000-000000000001"}, append(os.Environ(), "IM_IMPORT_COMPARE_DATABASE_URL=host="+listener.Addr().String()+" service=marker", "PGHOST="+listener.Addr().String()))
		var report map[string]any
		if code != 1 || json.Unmarshal(b, &report) != nil || report["database_checked"] != false || len(stderr) != 0 || bytes.Contains(b, []byte("private-path-marker")) {
			t.Fatal("invalid input contacted configuration or leaked input")
		}
	}
	select {
	case <-accepted:
		t.Fatal("invalid file connected")
	default:
	}
}

type failWriter struct{ short bool }

func (w failWriter) Write(b []byte) (int, error) {
	if w.short {
		return len(b) - 1, nil
	}
	return 0, errors.New("secret-writer")
}
func TestCompareProcessSignalAndPipe(t *testing.T) {
	for _, w := range []io.Writer{failWriter{}, failWriter{true}} {
		var stderr bytes.Buffer
		code := Run(context.Background(), []string{"--version"}, w, &stderr)
		if code != 2 || stderr.String() != "OUTPUT_WRITE_FAILED\n" {
			t.Fatal("short write success")
		}
	}
	cmd := exec.Command(binary(t), "--version")
	read, write, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	read.Close()
	cmd.Stdout = write
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	write.Close()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || stderr.String() != "OUTPUT_WRITE_FAILED\n" {
		t.Fatal("stdout close bypassed write failure")
	}
}
func TestCompareProcessDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	defer cancel()
	path := physicalFile(t, []byte(`{}`))
	var out, stderr bytes.Buffer
	code := Run(ctx, []string{"--input", path, "--tenant-id", "00000000-0000-4000-8000-000000000001"}, &out, &stderr)
	if code != 2 || !strings.Contains(out.String(), "TIMEOUT") || !strings.Contains(out.String(), "incomplete") {
		t.Fatal("canceled parent emitted no fixed incomplete report")
	}
}

func TestMain(m *testing.M) {
	if os.Args[0] == workerName {
		switch os.Getenv("IM_IMPORT_COMPARE_SCHEMA") {
		case "panic_fixture":
			panic("private-child-marker")
		case "malformed_fixture":
			os.Stdout.WriteString(`{"secret":"private-child-marker"}`)
			os.Exit(0)
		case "deadline_fixture":
			ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
			defer stop()
			deadline, err := controlDeadline(ctx)
			if err != nil {
				os.Exit(2)
			}
			ctx, done := context.WithDeadline(ctx, deadline)
			defer done()
			os.Exit(runWorker(ctx, os.Args[1:], os.Stdout, os.Stderr, func(got context.Context, path string) ([]byte, *p.Issue, error) {
				d, _ := got.Deadline()
				os.WriteFile(path, []byte(strconv.FormatInt(d.UnixNano(), 10)), 0600)
				<-got.Done()
				return nil, nil, p.ContextFailure(got)
			}, os.Getenv, func(c.Config) c.SnapshotReader { panic("unexpected provider") }))
		}
		os.Exit(commandMain())
	}
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "--") {
		os.Exit(commandMain())
	}
	code := m.Run()
	if commandBinary != "" && os.Getenv("IM_COMPARE_TEST_BINARY") == "" {
		os.RemoveAll(filepath.Dir(commandBinary))
	}
	os.Exit(code)
}
func TestCompareProcessBadPrivateWorker(t *testing.T) {
	for _, schema := range []string{"panic_fixture", "malformed_fixture"} {
		t.Setenv("IM_IMPORT_COMPARE_SCHEMA", schema)
		var out, stderr bytes.Buffer
		path := physicalFile(t, []byte(`{}`))
		exit := Run(context.Background(), []string{"--input", path, "--tenant-id", "00000000-0000-4000-8000-000000000001"}, &out, &stderr)
		if exit != 2 || !bytes.Contains(out.Bytes(), []byte("DATABASE_READ_FAILED")) || bytes.Contains(out.Bytes(), []byte("private-child-marker")) || len(stderr.Bytes()) != 0 {
			t.Fatal("private worker failure leaked or passed")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary(t), "--input=a", "--tenant-id=00000000-0000-4000-8000-000000000001")
	cmd.Args[0] = workerName
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || !bytes.Contains(out.Bytes(), []byte("DATABASE_READ_FAILED")) {
		t.Fatal("missing private control descriptor accepted")
	}
}
func TestCompareProcessSharedAbsoluteDeadline(t *testing.T) {
	t.Setenv("IM_IMPORT_COMPARE_SCHEMA", "deadline_fixture")
	path := physicalFile(t, []byte(`{}`))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()
	var out, stderr bytes.Buffer
	start := time.Now()
	exit := Run(ctx, []string{"--input", path, "--tenant-id", "00000000-0000-4000-8000-000000000001"}, &out, &stderr)
	raw, _ := os.ReadFile(path)
	if string(raw) != strconv.FormatInt(deadline.UnixNano(), 10) || exit != 2 || !bytes.Contains(out.Bytes(), []byte("TIMEOUT")) || time.Since(start) > 1200*time.Millisecond {
		t.Fatal("private deadline extended or not shared")
	}
}
