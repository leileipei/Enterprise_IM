package policystore_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Real protocol processes verify fixture lifecycle accounting, not IM behavior.
func TestIntegrationProductProcessRegistration(t *testing.T) {
	root := t.TempDir()
	os.Chmod(root, 0700)
	ledger := filepath.Join(root, "registry.jsonl")
	seed, _ := json.Marshal(map[string]any{"schema_version": 1, "owner": strings.Repeat("b", 32), "source_commit": strings.Repeat("a", 40), "event": "reserve", "fingerprint": map[string]string{"private_root": root}})
	if err := os.WriteFile(ledger, append(seed, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"IM_TEST_INTEGRATION_REGISTRY": ledger, "IM_TEST_INTEGRATION_OWNER": strings.Repeat("b", 32), "IM_TEST_INTEGRATION_SOURCE_SHA": strings.Repeat("a", 40), "IM_TEST_INTEGRATION_GATE": "file_components"} {
		t.Setenv(key, value)
	}
	source := filepath.Join(root, "main.go")
	binary := filepath.Join(root, "protocol-product")
	code := `package main
import("fmt";"net";"net/http";"os";"os/signal";"syscall")
func main(){l,e:=net.Listen("tcp","127.0.0.1:0");if e!=nil{panic(e)};fmt.Printf("{\"msg\":\"im api listening\",\"address\":\"%s\"}\n",l.Addr());fmt.Println("file worker started");go http.Serve(l,http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){w.WriteHeader(200)}));c:=make(chan os.Signal,1);signal.Notify(c,os.Interrupt,syscall.SIGTERM);<-c;l.Close()}`
	if err := os.WriteFile(source, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", binary, source)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("protocol build failed: %v %s", err, output)
	}
	t.Cleanup(func() {
		data, err := os.ReadFile(ledger)
		if err != nil {
			t.Error(err)
			return
		}
		counts := map[string]int{}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var row map[string]any
			if json.Unmarshal([]byte(line), &row) != nil {
				t.Error("bad lifecycle ledger")
				return
			}
			counts[row["event"].(string)]++
		}
		for _, kind := range []string{"registered", "ready", "exited"} {
			if counts[kind] != 3 {
				t.Errorf("protocol lifecycle %s=%d want3", kind, counts[kind])
			}
		}
	})
	startProductionAPI(t, binary, nil)
	startFileMessageProduction(t, binary, nil)
	worker := startFileWorkerProcess(t, binary, nil)
	data, _ := os.ReadFile(ledger)
	if strings.Count(string(data), `"event":"ready"`) != 3 {
		t.Fatal("ready official product helpers are missing central lifecycle records")
	}
	if err := worker.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := worker.Wait(); err != nil {
		t.Fatal("actual graceful protocol worker Wait failed")
	}
}

func TestFileBusinessOneShotLifecycle(t *testing.T) {
	root := t.TempDir()
	os.Chmod(root, 0700)
	ledger := filepath.Join(root, "registry.jsonl")
	seed, _ := json.Marshal(map[string]any{"schema_version": 1, "owner": strings.Repeat("b", 32), "source_commit": strings.Repeat("a", 40), "event": "reserve", "fingerprint": map[string]string{"private_root": root}})
	if err := os.WriteFile(ledger, append(seed, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"IM_TEST_INTEGRATION_REGISTRY": ledger, "IM_TEST_INTEGRATION_OWNER": strings.Repeat("b", 32), "IM_TEST_INTEGRATION_SOURCE_SHA": strings.Repeat("a", 40), "IM_TEST_INTEGRATION_GATE": "file_components"} {
		t.Setenv(key, value)
	}
	source := filepath.Join(root, "main.go")
	binary := filepath.Join(root, "protocol-once")
	if err := os.WriteFile(source, []byte("package main\nfunc main(){}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("go", "build", "-o", binary, source).CombinedOutput(); err != nil {
		t.Fatalf("protocol build failed: %v %s", err, output)
	}
	f := &fileBusinessProcessFixture{privateRoot: root, binaries: map[string]string{"im-file-cleaner": binary}, processes: map[string]*exec.Cmd{}, processPIDs: map[string]int{}, logs: map[string]*os.File{}, waits: map[string]chan error{}}
	f.restartRepair(t, true)
	data, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), `"event":"exited"`) != 1 {
		t.Fatal("actual successful one-shot has no terminal lifecycle proof")
	}
	if strings.Contains(string(data), `"event":"ready"`) {
		t.Fatal("one-shot terminal was reported as live readiness")
	}
	if !strings.Contains(string(data), `"actual_exit":"exit:0"`) {
		t.Fatal("one-shot actual Wait success missing")
	}
}

func TestWebFileBrowserEnvironmentIsolation(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "browser.go")
	binary := filepath.Join(root, "protocol-browser")
	code := `package main
import("fmt";"os")
func main(){fmt.Printf("{\"no_management_environment\":%t}\n",os.Getenv("IM_TEST_S3_ADMIN_SECRET_KEY")=="" && os.Getenv("PGPASSWORD")=="")}`
	if err := os.WriteFile(source, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("go", "build", "-o", binary, source).CombinedOutput(); err != nil {
		t.Fatalf("protocol build failed: %v %s", err, output)
	}
	t.Setenv("IM_TEST_BROWSER_NODE", binary)
	t.Setenv("IM_TEST_S3_ADMIN_SECRET_KEY", "controlled-management-probe")
	t.Setenv("PGPASSWORD", "controlled-password-probe")
	f := &webFileFixture{baseURL: "http://127.0.0.1:1"}
	f.browser(t, "file_search", map[string]any{})
}
