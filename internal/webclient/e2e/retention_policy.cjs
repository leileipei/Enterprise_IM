"use strict";
const assert = require("node:assert/strict"), http = require("node:http"), fs = require("node:fs"), path = require("node:path");
const { chromium } = require("playwright");
const assets = path.join(__dirname, "..", "assets");
const tenant = "00000000-0000-4000-8000-000000000001", user = "00000000-0000-4000-8000-000000000002";
const admin = "00000000-0000-4000-8000-000000000003", employee = "00000000-0000-4000-8000-000000000004";
const chat = "00000000-0000-4000-8000-000000000011";
let port, failure = "", readFailure = "", heldWrite, heldRead, holdRead = false, unauthorized = false;
let policy = { message_body_days: 365, version: 0, approval_reference: "", approved_by_user_id: "", approved_at: null };
const writes = [], commits = [];
const server = http.createServer((req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${port}`);
  const send = (status, value, type = "application/json") => {
    res.writeHead(status, { "Content-Type": type, "Cache-Control": "no-store" });
    res.end(typeof value === "string" ? value : JSON.stringify(value));
  };
  if (["/web/", "/web/app.js", "/web/retention.js", "/web/legal-holds.js", "/web/retention-policy.js", "/web/style.css"].includes(url.pathname)) {
    const name = url.pathname === "/web/" ? "index.html" : url.pathname.slice(5);
    if (!fs.existsSync(path.join(assets, name))) return send(404, {});
    return send(200, fs.readFileSync(path.join(assets, name), "utf8"), name.endsWith(".js") ? "text/javascript" : name.endsWith(".css") ? "text/css" : "text/html");
  }
  if (url.pathname === "/web/config") return send(200, { issuer: "https://sso.example.test/group", authorization_url: `http://127.0.0.1:${port}/authorize`, client_id: "enterprise-im-web", redirect_url: `http://127.0.0.1:${port}/web/`, scope: "openid profile" });
  if (url.pathname === "/authorize") { res.writeHead(302, { Location: `/web/?code=fixture&state=${url.searchParams.get("state")}` }); return res.end(); }
  if (url.pathname === "/web/oauth/token") return send(200, { access_token: "fixture-token", token_type: "Bearer", expires_in: 300 });
  if (req.headers.authorization !== "Bearer fixture-token" || unauthorized) return send(401, { error_code: "unauthorized" });
  if (url.pathname === "/api/v1/me") return send(200, { tenant_id: tenant, user_id: user, display_name: "测试管理员", global_employee_no: "A001", memberships: [
    { id: admin, organization_name: "集团总部", legal_entity_name: "总部法人", title: "管理员", is_primary: true },
    { id: employee, organization_name: "分公司", legal_entity_name: "分公司法人", title: "员工", is_primary: false }] });
  const actor = req.headers["x-acting-membership-id"];
  if (![admin, employee].includes(actor)) return send(403, { error_code: "invalid_identity" });
  if (url.pathname === "/api/v1/admin/retention-policy") {
    if (actor !== admin) return send(404, { error_code: "not_found" });
    if (req.method === "GET") {
      const result = { ...policy }, mode = readFailure; readFailure = "";
      const respond = () => mode === "503" ? send(503, {}) : mode === "malformed" ? send(200, { ...result, version: 9007199254740992 }) : mode === "numeric-time" ? send(200, { ...result, approved_at: "1" }) : mode === "bad-time" ? send(200, { ...result, approved_at: "2026-02-30T10:00:00Z" }) : send(200, result);
      if (holdRead) { holdRead = false; heldRead = respond; return; }
      return respond();
    }
    assert.equal(req.method, "PUT"); assert.equal(req.headers["content-type"], "application/json");
    let raw = ""; req.on("data", b => raw += b); req.on("end", () => {
      const input = JSON.parse(raw); assert.deepEqual(Object.keys(input).sort(), ["approval_reference", "expected_version", "message_body_days"]);
      writes.push({ input, actor }); const mode = failure; failure = "";
      if (["400", "403", "404", "409", "503"].includes(mode)) return send(Number(mode), { error_code: "rejected" });
      if (input.expected_version !== policy.version) return send(409, { error_code: "version_conflict" });
      policy = { message_body_days: input.message_body_days, version: input.expected_version + 1, approval_reference: input.approval_reference, approved_by_user_id: user, approved_at: "2026-10-03T10:00:00Z" };
      commits.push({ ...policy });
      if (mode === "drop") { res.writeHead(200, { "Content-Type": "application/json", "Content-Length": "1000", "Connection": "close" }); return res.end("{"); }
      if (mode === "hold") { const snapshot = { ...policy }; heldWrite = () => send(200, snapshot); return; }
      if (mode === "bad-time-ack") return send(200, { ...policy, approved_at: "2026-02-30T10:00:00Z" });
      if (mode === "wrong-actor") return send(200, { ...policy, approved_by_user_id: employee });
      send(200, policy);
    }); return;
  }
  if (url.pathname === `/api/v1/admin/conversations/${chat}/legal-holds`) return req.method === "GET" ? send(200, { holds: [], next_cursor: "" }) : send(503, {});
  if (url.pathname === "/api/v1/conversations") return send(200, { conversations: [{ id: chat, type: "direct", peer_visible: true, display_name: "项目会话", organization_name: "集团总部", last_seq: 0, updated_at: "2026-10-03T10:00:00Z" }], has_more: false });
  if (url.pathname === "/api/v1/groups") return send(200, { groups: [], has_more: false });
  if (/\/messages$/.test(url.pathname)) return send(200, { messages: [], next_after_seq: 0, has_more: false });
  send(404, { error_code: "not_found" });
});
server.listen(0, "127.0.0.1", async () => {
  port = server.address().port; let browser, page;
  try {
    browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_EXECUTABLE || undefined });
    page = await browser.newPage({ viewport: { width: 1280, height: 850 } }); page.setDefaultTimeout(3500);
    const errors = []; page.on("pageerror", e => errors.push(e.message));
    const login = async () => { await page.goto(`http://127.0.0.1:${port}/web/`); await page.locator("#login-button").click(); await page.locator("#workspace").waitFor({ state: "visible" }); await page.locator("#identity-options").getByRole("button", { name: /集团总部/ }).click(); };
    await login();
    const open = page.locator("#retention-policy-open"), dialog = page.locator("#retention-policy-dialog"), current = page.locator("#retention-policy-current");
    // Catches missing tenant-level entry or an incorrect conversation dependency.
    assert.equal(await open.count(), 1, "tenant retention settings entry must exist");
    await open.click(); await current.getByText("365 天", { exact: true }).waitFor();
    const days = page.locator("#retention-policy-days"), reference = page.locator("#retention-policy-reference"), confirm = page.locator("#retention-policy-confirm");
    const submit = page.locator("#retention-policy-submit"), hint = page.locator("#retention-policy-hint"), refresh = page.locator("#retention-policy-refresh"), retry = page.locator("#retention-policy-retry");
    const fill = async (n = "30", ref = "CAB-WEB-01", checked = true) => { await days.fill(n); await reference.fill(ref); if (checked) await confirm.check(); else await confirm.uncheck(); };
    await fill("30", "CAB-WEB-01", false); await submit.click(); assert.equal(writes.length, 0);
    for (const value of ["0", "3651", "1.5"]) { await fill(value); await submit.click(); assert.equal(writes.length, 0); }
    for (const value of [" ", "x".repeat(129), "bad\u0001ref", "\uFFFD"]) { await fill("30", value); await submit.click(); assert.equal(writes.length, 0); }
    await fill("30", "  CAB-WEB-01  "); await submit.click(); await hint.getByText(/服务器已确认/).waitFor();
    assert.deepEqual(writes[0], { actor: admin, input: { expected_version: 0, message_body_days: 30, approval_reference: "CAB-WEB-01" } });
    await current.getByText("30 天", { exact: true }).waitFor(); assert.equal(commits.length, 1); assert.equal(await confirm.isChecked(), false);
    // Commit succeeded but response body was lost: query, do not repeat PUT.
    await fill("20", "CAB-DROP"); failure = "drop"; await submit.click(); await hint.getByText(/结果待确认/).waitFor();
    assert.equal(await retry.isVisible(), false); assert.equal(await days.isDisabled(), true);
    const beforeQuery = writes.length; await refresh.click(); await hint.getByText(/当前配置与本次提交一致/).waitFor(); assert.equal(writes.length, beforeQuery); assert.equal(commits.length, 2);
    // No known commit after 503: query unchanged version before retrying.
    await fill("15", "CAB-RETRY"); failure = "503"; await submit.click(); await hint.getByText(/结果待确认/).waitFor();
    await refresh.click(); await retry.waitFor({ state: "visible" });
    const failedInput = writes.at(-1); await retry.click(); await hint.getByText(/服务器已确认/).waitFor(); assert.deepEqual(writes.at(-1), failedInput);
    assert.equal(commits.length, 3);
    // A concurrent change never becomes a new expected_version automatically.
    await fill("10", "CAB-CONFLICT"); failure = "503"; await submit.click(); await hint.getByText(/结果待确认/).waitFor();
    const count = writes.length; policy = { ...policy, version: policy.version + 2, message_body_days: 12, approval_reference: "CAB-OTHER" };
    await refresh.click(); await hint.getByText(/已变化/).waitFor(); assert.equal(writes.length, count); assert.equal(await retry.isVisible(), false); assert.equal(await confirm.isChecked(), false);
    for (const status of ["400", "409"]) { await fill("9", `CAB-${status}`); failure = status; await submit.click(); await hint.getByText(status === "400" ? /请求被拒绝/ : /操作冲突/).waitFor(); assert.equal(await submit.isDisabled(), true); await refresh.click(); await current.getByText("12 天", { exact: true }).waitFor(); }
    // Invalid ACK cannot unlock a fresh mutation; explicit read is required.
    await fill("8", "CAB-ACTOR"); failure = "wrong-actor"; await submit.click(); await hint.getByText(/结果待确认/).waitFor(); await refresh.click(); await hint.getByText(/当前配置与本次提交一致/).waitFor();
    // Network error, malformed GET and read races clear stale editable state.
    readFailure = "503"; await refresh.click(); await hint.getByText(/加载失败/).waitFor(); assert.equal(await submit.isDisabled(), true); assert.equal(await current.innerText(), "");
    readFailure = "malformed"; await refresh.click(); await hint.getByText(/加载失败/).waitFor(); assert.equal(await submit.isDisabled(), true);
    for (const mode of ["numeric-time", "bad-time"]) { readFailure = mode; await refresh.click(); await hint.getByText(/加载失败/).waitFor(); assert.equal(await submit.isDisabled(), true); }
    await refresh.click(); await current.getByText("8 天", { exact: true }).waitFor();
    await fill("7", "<img src=x onerror=alert(1)>CAB"); await submit.click(); await hint.getByText(/服务器已确认/).waitFor(); assert.equal(await current.locator("img").count(), 0);
    if (process.env.IM_TEST_POLICY_SCREENSHOT_DIR) { fs.mkdirSync(process.env.IM_TEST_POLICY_SCREENSHOT_DIR, { recursive: true }); await page.screenshot({ path: path.join(process.env.IM_TEST_POLICY_SCREENSHOT_DIR, "desktop.png") }); await page.setViewportSize({ width: 390, height: 844 }); await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))); await page.screenshot({ path: path.join(process.env.IM_TEST_POLICY_SCREENSHOT_DIR, "mobile.png") }); assert.equal(await dialog.evaluate(d => d.scrollWidth <= d.clientWidth + 1), true); await page.setViewportSize({ width: 1280, height: 850 }); }
    await page.locator("#retention-policy-close").click(); await page.locator("#conversation-list .conversation-button").click(); await open.click(); await current.getByText("7 天", { exact: true }).waitFor();
    // Unknown operation survives close and blocks manual identity/logout.
    await fill("6", "CAB-PENDING"); failure = "503"; await submit.click(); await hint.getByText(/结果待确认/).waitFor(); await page.locator("#retention-policy-close").click();
    await page.locator("#legal-holds-open").click();
    assert.equal(await page.locator("#legal-holds-dialog").isVisible(), false, "pending policy must prevent starting a legal hold write");
    assert.equal(await dialog.isVisible(), true); await page.locator("#retention-policy-close").click();
    await page.locator("#identity-options").getByRole("button", { name: /分公司/ }).click(); assert.equal(await dialog.isVisible(), true); assert.match(await page.locator("#retention-policy-context").innerText(), new RegExp(admin));
    await page.locator("#retention-policy-close").click(); await page.locator("#logout-button").click(); assert.equal(await page.locator("#workspace").isVisible(), true); assert.equal(await dialog.isVisible(), true);
    assert.equal(await page.evaluate(() => !window.dispatchEvent(new Event("beforeunload", { cancelable: true }))), true);
    const abandon = page.locator("#retention-policy-abandon"); page.once("dialog", d => d.dismiss()); await abandon.click(); assert.equal(await days.isDisabled(), true);
    page.once("dialog", d => d.accept()); await abandon.click(); await hint.getByText(/不代表撤销/).waitFor(); assert.equal(await submit.isDisabled(), true); await refresh.click(); await current.getByText("7 天", { exact: true }).waitFor();
    await page.locator("#retention-policy-close").click(); await page.locator("#legal-holds-open").click();
    await page.locator("#legal-holds-hint").getByText(/暂无保全/).waitFor();
    await page.locator("#legal-hold-reference").fill("CASE-BLOCK-POLICY"); await page.locator("#legal-hold-submit").click();
    await page.locator("#legal-hold-action-hint").getByText(/结果待确认/).waitFor(); await page.locator("#legal-holds-close").click();
    await open.click(); assert.equal(await dialog.isVisible(), false, "pending legal hold must prevent starting a policy write");
    assert.equal(await page.locator("#legal-holds-dialog").isVisible(), true);
    page.once("dialog", d => d.accept()); await page.locator("#legal-hold-abandon").click(); await page.locator("#legal-holds-close").click();
    await open.click(); await current.getByText("7 天", { exact: true }).waitFor();
    // In flight: frozen fields, duplicate submission suppressed, bounded timeout.
    await fill("5", "CAB-TIMEOUT"); failure = "hold"; await submit.click(); await hint.getByText(/正在提交/).waitFor();
    assert.equal(await abandon.isDisabled(), true); assert.equal(await refresh.isDisabled(), true); assert.equal(await submit.isDisabled(), true);
    await hint.getByText(/结果待确认/).waitFor({ timeout: 19000 }); const timedWrites = writes.length; await refresh.click(); await hint.getByText(/当前配置与本次提交一致/).waitFor(); assert.equal(writes.length, timedWrites); heldWrite(); heldWrite = null;
    // Stale GET on manual actor switch must not repopulate the old policy.
    holdRead = true; await refresh.click(); await page.waitForFunction(() => document.querySelector("#retention-policy-submit").disabled);
    await page.locator("#retention-policy-close").click(); await page.locator("#identity-options").getByRole("button", { name: /分公司/ }).click(); heldRead(); heldRead = null;
    await page.waitForFunction(() => document.querySelector("#retention-policy-open").classList.contains("hidden")); assert.equal(await current.innerText(), "");
    await page.locator("#identity-options").getByRole("button", { name: /集团总部/ }).click(); await open.click(); await current.getByText("5 天", { exact: true }).waitFor();
    await fill("5", "CAB-TIME-ACK"); failure = "bad-time-ack"; await submit.click(); await hint.getByText(/结果待确认/).waitFor();
    const timeWrites = writes.length;
    readFailure = "bad-time"; await refresh.click(); await hint.getByText(/配置加载失败，结果仍待确认/).waitFor();
    assert.equal(await days.isDisabled(), true); assert.equal(await retry.isVisible(), false);
    await refresh.click(); await hint.getByText(/当前配置与本次提交一致/).waitFor(); assert.equal(writes.length, timeWrites);
    // Real Go timestamps may have fractional seconds and a non-UTC offset.
    policy = { ...policy, approved_at: "2026-10-03T18:00:00.123456789+08:00" };
    await refresh.click(); await hint.getByText(/已加载当前配置/).waitFor(); assert.equal(await submit.isDisabled(), false);
    for (const status of ["403", "404"]) { await fill("4", `CAB-${status}`); failure = status; await submit.click(); await hint.getByText(/权限已失效/).waitFor(); assert.equal(await current.innerText(), ""); await page.locator("#retention-policy-close").click(); await page.locator("#identity-options").getByRole("button", { name: /分公司/ }).click(); await page.locator("#identity-options").getByRole("button", { name: /集团总部/ }).click(); await open.click(); await current.getByText("5 天", { exact: true }).waitFor(); }
    // Forced 401 while write is held discards pending data; late ACK stays invisible.
    await page.locator("#retention-policy-close").click(); await page.locator("#conversation-list .conversation-button").click(); await open.click(); await current.getByText("5 天", { exact: true }).waitFor();
    await fill("4", "CAB-LOGOUT"); failure = "hold"; await submit.click(); await hint.getByText(/正在提交/).waitFor(); unauthorized = true;
    await page.locator("#login-view").waitFor({ state: "visible", timeout: 8000 }); heldWrite(); heldWrite = null;
    assert.equal(await dialog.isVisible(), false); assert.equal(await current.innerText(), ""); unauthorized = false;
    assert.deepEqual(errors, []); process.stdout.write("tenant retention policy browser validation, CAS recovery, identity guards and timeout passed\n");
  } catch (error) { process.stderr.write(`${error.stack}\n`); if (page) process.stderr.write(JSON.stringify({ writes, policy, hint: await page.locator("#retention-policy-hint").textContent().catch(() => "missing") }) + "\n"); process.exitCode = 1; }
  finally { if (heldWrite) heldWrite(); if (heldRead) heldRead(); if (browser) await browser.close(); server.closeAllConnections(); server.close(); }
});
