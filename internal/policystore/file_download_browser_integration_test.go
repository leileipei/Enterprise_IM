//go:build darwin || linux

package policystore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"github.com/leileipei/Enterprise_IM/internal/testfixtures"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestFileDownloadRealBrowserLegacy(t *testing.T) {
	node := os.Getenv("IM_TEST_BROWSER_NODE")
	if node == "" {
		t.Skip("requires real browser runtime")
	}
	f := realFileMessageFixture(t)
	m := f.scanned(t, directA)
	caption := "P423_PRIVATE_BROWSER_CAPTION"
	req := map[string]string{"client_msg_id": clientUUIDv7(time.Now(), 8630), "message_type": "file", "file_id": m.ID, "caption": caption}
	// The fixture sends via the authorized typed service; the actual production
	// Web/API child remains closed for file sending and uses legacy pull.
	if _, e := f.repo.SendMessage(context.Background(), publisher(), directA, policystore.MessageSendRequest{ClientMessageID: req["client_msg_id"], MessageType: "file", FileID: m.ID, Caption: caption}); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(node, "../webclient/e2e/file_download_legacy.cjs")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = testfixtures.BrowserEnvironment(map[string]string{"IM_TEST_WEB_URL": f.web, "IM_TEST_PRIVATE_NAME": m.OriginalFilename, "IM_TEST_PRIVATE_CAPTION": caption})
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	lifecycle, startErr := testfixtures.StartIntegrationBrowser(cmd, t.Name(), true)
	if startErr != nil {
		t.Fatal(startErr)
	}
	done := make(chan error, 1)
	go func() { done <- lifecycle.Wait() }()
	timer := time.NewTimer(90 * time.Second)
	defer timer.Stop()
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	select {
	case e := <-done:
		if e != nil {
			t.Fatal("browser flow", e, output.String())
		}
	case <-timer.C:
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatal("browser flow timeout")
	}
	if f.logins.Load() != 2 || f.exchanges.Load() != 2 {
		t.Fatal("normal PKCE flow did not execute", f.logins.Load(), f.exchanges.Load())
	}
	var facts map[string]bool
	if e := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &facts); e != nil || len(facts) != 5 {
		t.Fatal(output.String(), e)
	}
	for k, v := range facts {
		if !v {
			t.Fatal(k)
		}
	}
	var count int
	if e := f.conn.QueryRow(context.Background(), "SELECT count(*) FROM messages WHERE message_type='text' AND text_body='P424 real browser text'").Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
	logs, _ := os.ReadFile(f.apiLog)
	assertFileMessagePrivate(t, logs, caption, m.OriginalFilename)
	t.Log("real PKCE browser: legacy placeholder, text send, reload pull and privacy passed")
}
