//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"errors"
	c "github.com/leileipei/Enterprise_IM/internal/importcompare"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type limitedOutput struct{ bytes.Buffer }

func (w *limitedOutput) Write(b []byte) (int, error) {
	remaining := p.MaxReport + 1 - w.Len()
	if remaining <= 0 {
		return 0, p.Failure{Code: "OUTPUT_WRITE_FAILED"}
	}
	if len(b) > remaining {
		n, _ := w.Buffer.Write(b[:remaining])
		return n, p.Failure{Code: "OUTPUT_WRITE_FAILED"}
	}
	return w.Buffer.Write(b)
}
func controlDeadline(ctx context.Context) (time.Time, error) {
	bad := func() (time.Time, error) { return time.Time{}, p.Failure{Code: "DATABASE_READ_FAILED"} }
	var stat unix.Stat_t
	if err := unix.Fstat(3, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFIFO {
		return bad()
	}
	flags, err := unix.FcntlInt(3, unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC != 0 {
		return bad()
	}
	f := os.NewFile(3, "compare-control")
	if f == nil {
		return bad()
	}
	defer f.Close()
	type result struct {
		raw []byte
		err error
	}
	done := make(chan result, 1)
	go func() { b, e := io.ReadAll(io.LimitReader(f, 129)); done <- result{b, e} }()
	var got result
	select {
	case <-ctx.Done():
		return time.Time{}, p.ContextFailure(ctx)
	case got = <-done:
	}
	if got.err != nil || len(got.raw) > 128 || !bytes.HasSuffix(got.raw, []byte{'\n'}) {
		return bad()
	}
	raw := strings.TrimSuffix(string(got.raw), "\n")
	n, e := strconv.ParseInt(raw, 10, 64)
	if e != nil || strconv.FormatInt(n, 10) != raw {
		return bad()
	}
	d := time.Unix(0, n)
	if d.After(time.Now().Add(30 * time.Second)) {
		return bad()
	}
	return d, nil
}
func runProcess(ctx context.Context, args []string, cleanup *cleanupBudget) (c.Report, error) {
	bad := func() (c.Report, error) { return c.Report{}, p.Failure{Code: "DATABASE_READ_FAILED"} }
	path, err := os.Executable()
	if err != nil {
		return bad()
	}
	read, write, err := os.Pipe()
	if err != nil {
		return bad()
	}
	defer read.Close()
	defer write.Close()
	deadline, _ := ctx.Deadline()
	cmd := exec.Command(path, args...)
	cmd.Args[0] = workerName
	cmd.ExtraFiles = []*os.File{read}
	cmd.Env = []string{"TZ=UTC"}
	for _, k := range []string{"IM_IMPORT_COMPARE_DATABASE_URL", "IM_IMPORT_COMPARE_SCHEMA"} {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	out := &limitedOutput{}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	if err = cmd.Start(); err != nil {
		return bad()
	}
	read.Close()
	if _, err = write.Write([]byte(strconv.FormatInt(deadline.UnixNano(), 10) + "\n")); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return bad()
	}
	write.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		clean := cleanup.get()
		_ = cmd.Process.Signal(syscall.SIGTERM)
		timer := time.NewTimer(750 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-clean.Done():
				return bad()
			}
		}
		return c.Report{}, p.ContextFailure(ctx)
	}
	if err = p.ContextFailure(ctx); err != nil {
		return c.Report{}, err
	}
	code := 0
	if waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) {
			return bad()
		}
		code = exit.ExitCode()
	}
	if out.Len() > p.MaxReport {
		return bad()
	}
	report, err := c.DecodeReport(ctx, out.Bytes())
	if err != nil || report.ExitCode() != code {
		return bad()
	}
	return report, nil
}
