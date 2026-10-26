package policystore_test

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

// Registration failure must leave the created, unstarted container available
// for the parent's label-bound recovery rather than disposing unknown mounts.
func TestReceiptRegistrationFailurePreservesRecovery(t *testing.T) {
	if os.Getenv("IM_MOUNT_FAILURE_CHILD") == "1" {
		startReceiptChild(t, os.Getenv("MOUNT_FAILURE_ROOT"), "worker", os.Args[0], "", nil)
		t.Fatal("unproven registration unexpectedly succeeded")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	owner, source, cid := strings.Repeat("b", 32), strings.Repeat("a", 40), strings.Repeat("d", 64)
	private := filepath.Join(root, owner)
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(root, "registry.jsonl")
	seed, _ := json.Marshal(map[string]any{"schema_version": 1, "event": "reserve", "owner": owner, "source_commit": source, "fingerprint": map[string]string{"private_root": private}})
	if err := os.WriteFile(ledger, append(seed, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	actual, _ := json.Marshal([]any{map[string]any{"Id": cid, "Config": map[string]any{"Labels": map[string]string{"im.integration.owner": owner, "im.integration.source": source}}, "Mounts": nil, "HostConfig": map[string]any{}}})
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$1\" in\ncreate) /usr/bin/touch \"$MOUNT_FAILURE_ROOT/present\"; printf '%s\\n' '" + cid + "';;\ninspect) /bin/cat <<'INSPECT_FIXTURE'\n" + string(actual) + "\nINSPECT_FIXTURE\n;;\nrm) /bin/rm -f \"$MOUNT_FAILURE_ROOT/present\";;\nstart) /usr/bin/touch \"$MOUNT_FAILURE_ROOT/started\";;\n*) exit 92;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestReceiptRegistrationFailurePreservesRecovery$", "-test.count=1")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "IM_MOUNT_FAILURE_CHILD=1", "MOUNT_FAILURE_ROOT="+root, "IM_TEST_INTEGRATION_REGISTRY="+ledger, "IM_TEST_INTEGRATION_OWNER="+owner, "IM_TEST_INTEGRATION_SOURCE_SHA="+source, "IM_TEST_INTEGRATION_GATE=full_repository")
	output, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("registration must fail in child: %v, %s", err, output)
	}
	if _, err = os.Stat(filepath.Join(root, "present")); err != nil {
		t.Fatal("registration failure discarded container before a durable mount ticket")
	}
	if _, err = os.Stat(filepath.Join(root, "started")); !os.IsNotExist(err) {
		t.Fatal("unproven child was started")
	}
	data, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Split(strings.TrimSpace(string(data)), "\n")) != 1 {
		t.Fatal("invalid mounts were published as a container ticket")
	}
}
