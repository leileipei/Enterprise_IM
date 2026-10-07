package testfixtures

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func browserProtocolRegistry(t *testing.T) (string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(root, 0700)
	ledger := filepath.Join(root, "registry.jsonl")
	seed, _ := json.Marshal(map[string]any{"schema_version": 1, "owner": strings.Repeat("b", 32), "source_commit": strings.Repeat("a", 40), "event": "reserve", "fingerprint": map[string]string{"private_root": root}})
	if err = os.WriteFile(ledger, append(seed, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	node := os.Getenv("IM_TEST_BROWSER_NODE")
	if node == "" {
		node = "/Users/leo.cui/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/bin/node"
	}
	chrome := os.Getenv("CHROMIUM_EXECUTABLE")
	if chrome == "" {
		chrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	}
	modules := os.Getenv("NODE_PATH")
	if modules == "" {
		modules = filepath.Join(filepath.Dir(node), "../node_modules")
	}
	for key, value := range map[string]string{"IM_TEST_INTEGRATION_REGISTRY": ledger, "IM_TEST_INTEGRATION_OWNER": strings.Repeat("b", 32), "IM_TEST_INTEGRATION_SOURCE_SHA": strings.Repeat("a", 40), "IM_TEST_INTEGRATION_GATE": "race_repository", "IM_TEST_BROWSER_NODE": node, "CHROMIUM_EXECUTABLE": chrome, "NODE_PATH": modules} {
		t.Setenv(key, value)
	}
	return root, ledger
}

func TestIntegrationBrowserCancellation(t *testing.T) {
	root, ledger := browserProtocolRegistry(t)
	script := filepath.Join(root, "cancel.cjs")
	code := `const {chromium}=require('playwright');(async()=>{const browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_EXECUTABLE});const page=await browser.newPage();await page.goto('data:text/html,<title>actual cancellation protocol</title>');await new Promise(()=>{});await browser.close();})().catch(()=>process.exitCode=1);`
	if err := os.WriteFile(script, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Getenv("IM_TEST_BROWSER_NODE"), script)
	cmd.Env = BrowserEnvironment(nil)
	lifecycle, err := StartIntegrationBrowser(cmd, t.Name(), true)
	if err != nil {
		t.Fatal("actual browser startup failed", err)
	}
	select {
	case <-lifecycle.BrowsersReady:
	case <-time.After(15 * time.Second):
		cancel()
		lifecycle.Wait()
		t.Fatal("actual Chrome did not become ready")
	}
	cancel()
	started := time.Now()
	if err = lifecycle.Wait(); err == nil {
		t.Fatal("cancellation was reported as success")
	}
	if time.Since(started) > 7*time.Second {
		t.Fatal("actual cancellation exceeded cleanup allowance")
	}
	raw, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row map[string]any
		if json.Unmarshal([]byte(line), &row) != nil {
			t.Fatal("bad lifecycle receipt")
		}
		event := row["event"].(string)
		counts[event]++
		if event == "exited" {
			detail := row["detail"].(map[string]any)
			if detail["actual_exit"] != detail["expected_exit"] {
				t.Fatal("actual exit proof differs")
			}
		}
	}
	for _, event := range []string{"registered", "ready", "exited"} {
		if counts[event] != 2 {
			t.Errorf("cancelled Node/Chrome %s=%d want2", event, counts[event])
		}
	}
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 130 {
		t.Fatal("actual Node interrupt Wait status missing")
	}
}

func TestIntegrationBrowserForeignChildRejected(t *testing.T) {
	root, _ := browserProtocolRegistry(t)
	parent := exec.Command("/bin/sleep", "30")
	parent.Dir = root
	foreign := exec.Command("/bin/sleep", "30")
	foreign.Dir = root
	for _, cmd := range []*exec.Cmd{parent, foreign} {
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func(c *exec.Cmd) { c.Process.Kill(); c.Wait() }(cmd)
	}
	if _, err := RegisterIntegrationBrowserChild(parent, foreign.Process.Pid, "/bin/sleep", "race_repository", t.Name()); err == nil {
		t.Fatal("unrelated actual process accepted as Node browser child")
	}
}
