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
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestFileMessageRealBrowserLegacy(t *testing.T) {
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
	script := filepath.Join(t.TempDir(), "file-message-browser.cjs")
	const code = `"use strict";
const {chromium}=require("playwright");const assert=require("node:assert/strict");let stage="startup";
(async()=>{const browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_EXECUTABLE||undefined});try{
const context=await browser.newContext({ignoreHTTPSErrors:true});const page=await context.newPage();
stage="login";await page.goto(process.env.IM_TEST_WEB_URL+"/web/");await page.getByRole("button",{name:/使用企业账号登录/}).click();
await page.locator("#workspace").waitFor({state:"visible",timeout:15000});assert.equal(new URL(page.url()).search,"");
stage="select identity and conversation";await page.locator("#identity-options .identity-button").first().click();await page.locator(".conversation-button").first().click();
stage="legacy placeholder";await page.getByText("附件消息（当前客户端不支持查看）",{exact:true}).waitFor({timeout:15000});
let body=await page.locator("body").textContent();assert(!body.includes(process.env.IM_TEST_PRIVATE_NAME));assert(!body.includes(process.env.IM_TEST_PRIVATE_CAPTION));
stage="text send";await page.locator("#message-text").fill("P423 real browser text");await page.locator("#send-button").click();await page.getByText("P423 real browser text",{exact:true}).waitFor({timeout:10000});
stage="reload and fresh PKCE login";await page.reload();await page.getByRole("button",{name:/使用企业账号登录/}).click();await page.locator("#workspace").waitFor({state:"visible",timeout:15000});await page.locator("#identity-options .identity-button").first().click();await page.locator(".conversation-button").first().click();
await page.getByText("附件消息（当前客户端不支持查看）",{exact:true}).waitFor({timeout:15000});await page.getByText("P423 real browser text",{exact:true}).waitFor({timeout:10000});
body=await page.locator("body").textContent();assert(!body.includes(process.env.IM_TEST_PRIVATE_NAME));assert(!body.includes(process.env.IM_TEST_PRIVATE_CAPTION));
console.log(JSON.stringify({legacy_placeholder:true,text_send:true,reload_pull:true,private_values_hidden:true}));await context.close();
}finally{await browser.close()}})().catch(e=>{console.error(stage+": "+e.message);process.exit(1)});`
	if e := os.WriteFile(script, []byte(code), 0600); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(node, script)
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
	if e := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &facts); e != nil || len(facts) != 4 {
		t.Fatal(output.String(), e)
	}
	for k, v := range facts {
		if !v {
			t.Fatal(k)
		}
	}
	var count int
	if e := f.conn.QueryRow(context.Background(), "SELECT count(*) FROM messages WHERE message_type='text' AND text_body='P423 real browser text'").Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
	logs, _ := os.ReadFile(f.apiLog)
	assertFileMessagePrivate(t, logs, caption, m.OriginalFilename)
	t.Log("real PKCE browser: legacy placeholder, text send, reload pull and privacy passed")
}
