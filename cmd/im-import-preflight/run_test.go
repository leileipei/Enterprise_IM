package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var binaryOnce sync.Once
var testBinary string
var binaryError error

func binary(t *testing.T) string {
	t.Helper()
	binaryOnce.Do(func() {
		if s := os.Getenv("IM_PREFLIGHT_TEST_BINARY"); s != "" {
			testBinary = s
			return
		}
		dir, e := os.MkdirTemp("", "im-preflight-build-")
		if e != nil {
			binaryError = e
			return
		}
		testBinary = filepath.Join(dir, "im-import-preflight")
		cmd := exec.Command("go", "build", "-mod=readonly", "-o", testBinary, ".")
		binaryError = cmd.Run()
	})
	if binaryError != nil {
		t.Fatal(binaryError)
	}
	return testBinary
}
func sample(t *testing.T) []byte {
	t.Helper()
	b, e := os.ReadFile("../../internal/importpreflight/testdata/sample_data_v1.json")
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestCLIReportsAndExitCodes(t *testing.T) {
	d := t.TempDir()
	d, e := filepath.EvalSymlinks(d)
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		data   []byte
		code   int
		status string
	}{{sample(t), 0, "valid"}, {[]byte(`{}`), 1, "invalid"}, {nil, 2, "incomplete"}} {
		path := filepath.Join(d, "private-marker")
		if tc.data != nil {
			os.WriteFile(path, tc.data, 0600)
		} else {
			os.Remove(path)
		}
		cmd := exec.Command(binary(t), "--input", path)
		cmd.Env = append(os.Environ(), "IM_DATABASE_URL=postgres://private-marker.invalid/no", "IM_OIDC_ISSUER=http://private-marker.invalid", "IM_REDIS_URL=redis://private-marker.invalid", "IM_S3_ENDPOINT=http://private-marker.invalid")
		var out, err bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &err
		e := cmd.Run()
		code := 0
		if e != nil {
			code = e.(*exec.ExitError).ExitCode()
		}
		if code != tc.code {
			t.Fatalf("exit %d %s", code, err.String())
		}
		var r map[string]any
		dec := json.NewDecoder(&out)
		if dec.Decode(&r) != nil || r["status"] != tc.status {
			t.Fatal("no report")
		}
		var tail any
		if dec.Decode(&tail) != io.EOF {
			t.Fatal("multiple outputs")
		}
		if strings.Contains(out.String()+err.String(), "private-marker") {
			t.Fatal("path leaked")
		}
	}
	for _, arg := range []string{"--help", "--version"} {
		out, e := exec.Command(binary(t), arg).Output()
		if e != nil || len(out) == 0 || bytes.Contains(out, []byte("input_sha256")) {
			t.Fatal("exclusive mode")
		}
	}
}

type failWriter struct{ short bool }

func (w failWriter) Write(b []byte) (int, error) {
	if w.short {
		return len(b) - 1, nil
	}
	return 0, errors.New("private-output-marker")
}
func TestCLIOutputFailure(t *testing.T) {
	reader := func(context.Context, string) ([]byte, *p.Issue, error) { return sample(t), nil, nil }
	for _, w := range []io.Writer{failWriter{}, failWriter{true}} {
		var err bytes.Buffer
		code := runWithReader(context.Background(), []string{"--input=a"}, w, &err, reader)
		if code != 2 || err.String() != "OUTPUT_WRITE_FAILED\n" {
			t.Fatal("write failed but success")
		}
	}
}
func TestCLISharedDeadlineAndCancellation(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Second))
	defer cancel()
	deadline, _ := ctx.Deadline()
	reader := func(got context.Context, s string) ([]byte, *p.Issue, error) {
		d, ok := got.Deadline()
		if !ok || !d.Equal(deadline) {
			t.Fatal("deadline reset")
		}
		return sample(t), nil, nil
	}
	var out, err bytes.Buffer
	if runWithReader(ctx, []string{"--input=a"}, &out, &err, reader) != 0 {
		t.Fatal("shared deadline")
	}
	out.Reset()
	cancel()
	if runWithReader(ctx, []string{"--input=a"}, &out, &err, reader) != 2 {
		t.Fatal("canceled success")
	}
}
func TestCLISignalHelper(t *testing.T) {
	if os.Getenv("IM_PREFLIGHT_SIGNAL_HELPER") != "1" {
		return
	}
	reader := func(ctx context.Context, s string) ([]byte, *p.Issue, error) {
		os.Stderr.WriteString("READY\n")
		<-ctx.Done()
		return nil, nil, p.ContextFailure(ctx)
	}
	os.Exit(runWithSignals([]string{"--input=a"}, os.Stdout, os.Stderr, reader))
}
func TestCLISIGTERM(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLISignalHelper$")
	cmd.Env = append(os.Environ(), "IM_PREFLIGHT_SIGNAL_HELPER=1")
	var out bytes.Buffer
	cmd.Stdout = &out
	errPipe, e := cmd.StderrPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	ready := make(chan bool, 1)
	go func() {
		b := make([]byte, 6)
		_, e := io.ReadFull(errPipe, b)
		ready <- e == nil && string(b) == "READY\n"
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("signal helper not ready")
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatal("helper timeout")
	}
	cmd.Process.Signal(syscall.SIGTERM)
	e = cmd.Wait()
	if e == nil || e.(*exec.ExitError).ExitCode() != 2 {
		t.Fatal("SIGTERM exit")
	}
	var r map[string]any
	if json.Unmarshal(out.Bytes(), &r) != nil || r["status"] != "incomplete" || r["checks_complete"] != false {
		t.Fatal("SIGTERM incomplete")
	}
}
func TestMain(m *testing.M) {
	code := m.Run()
	if testBinary != "" && os.Getenv("IM_PREFLIGHT_TEST_BINARY") == "" {
		_ = os.RemoveAll(filepath.Dir(testBinary))
	}
	os.Exit(code)
}
func TestCLIBlockedOutputCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := func(context.Context, string) ([]byte, *p.Issue, error) { return sample(t), nil, nil }
	w := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan int, 1)
	go func() { var err bytes.Buffer; done <- runWithReader(ctx, []string{"--input=a"}, w, &err, reader) }()
	<-w.started
	cancel()
	select {
	case code := <-done:
		if code != 2 {
			t.Fatal("blocked output success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked output ignored cancellation")
	}
	close(w.release)
}

type blockingWriter struct {
	started chan struct{}
	release chan struct{}
}

func (w *blockingWriter) Write(b []byte) (int, error) {
	close(w.started)
	<-w.release
	return len(b), nil
}
