package testfixtures

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationRegistryProof(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "registry.jsonl")
	seed, _ := json.Marshal(map[string]any{"schema_version": 1, "owner": strings.Repeat("b", 32), "source_commit": strings.Repeat("a", 40), "event": "reserve", "fingerprint": map[string]string{"private_root": root}})
	if err := os.WriteFile(path, append(seed, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IM_TEST_INTEGRATION_REGISTRY", path)
	t.Setenv("IM_TEST_INTEGRATION_OWNER", strings.Repeat("b", 32))
	t.Setenv("IM_TEST_INTEGRATION_SOURCE_SHA", strings.Repeat("a", 40))
	t.Setenv("IM_TEST_INTEGRATION_GATE", "full_repository")
	cmd := exec.Command("/bin/sleep", "30")
	cmd.Dir = root
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	t.Run("reject_foreign_source", func(t *testing.T) {
		t.Setenv("IM_TEST_INTEGRATION_SOURCE_SHA", strings.Repeat("c", 40))
		if _, err := RegisterIntegrationProcess(cmd, "full_repository", "TestParent"); err == nil {
			t.Fatal("unreserved source accepted")
		}
	})
	process, err := RegisterIntegrationProcess(cmd, "full_repository", "TestParent")
	if err != nil {
		t.Fatal(err)
	}
	if err = process.Ready(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if err = process.Exited("signal:killed", "signal:killed"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(rows) != 4 {
		t.Fatalf("lifecycle rows = %d, want 4", len(rows))
	}
	for i, want := range []string{"registered", "ready", "exited"} {
		var row map[string]any
		if err := json.Unmarshal([]byte(rows[i+1]), &row); err != nil {
			t.Fatal(err)
		}
		if row["event"] != want || row["source_commit"] != strings.Repeat("a", 40) {
			t.Fatalf("bad lifecycle: %#v", row)
		}
	}
	t.Setenv("IM_TEST_INTEGRATION_REGISTRY", "")
	disabled, err := RegisterIntegrationProcess(nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = disabled.Ready(); err != nil {
		t.Fatal(err)
	}
	if err = disabled.Exited("", ""); err != nil {
		t.Fatal(err)
	}
}
