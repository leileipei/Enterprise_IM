//go:build darwin || linux

package webclient

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestWebRetentionRecordsBrowser(t *testing.T) {
	node := os.Getenv("IM_TEST_BROWSER_NODE")
	if node == "" {
		t.Skip("set IM_TEST_BROWSER_NODE to run browser interface tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "e2e/retention_records.cjs")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output, err := cmd.CombinedOutput()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if err != nil {
		t.Fatalf("retention browser: %v: %s", err, output)
	}
	t.Log(string(output))
}

func TestWebLegalHoldsBrowser(t *testing.T) {
	node := os.Getenv("IM_TEST_BROWSER_NODE")
	if node == "" {
		t.Skip("set IM_TEST_BROWSER_NODE to run browser interface tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "e2e/legal_holds.cjs")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output, err := cmd.CombinedOutput()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if err != nil {
		t.Fatalf("legal holds browser: %v: %s", err, output)
	}
	t.Log(string(output))
}

func TestWebLegalHoldActionsBrowser(t *testing.T) {
	node := os.Getenv("IM_TEST_BROWSER_NODE")
	if node == "" {
		t.Skip("set IM_TEST_BROWSER_NODE to run browser interface tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "e2e/legal_hold_actions.cjs")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output, err := cmd.CombinedOutput()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if err != nil {
		t.Fatalf("legal holds browser: %v: %s", err, output)
	}
	t.Log(string(output))
}
