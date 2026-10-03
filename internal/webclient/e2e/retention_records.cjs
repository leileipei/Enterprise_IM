"use strict";

const assert = require("node:assert/strict");
const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require("playwright");
const assets = path.join(__dirname, "..", "assets");
const tenant = "00000000-0000-4000-8000-000000000001";
const user = "00000000-0000-4000-8000-000000000002";
const admin = "00000000-0000-4000-8000-000000000003";
const employee = "00000000-0000-4000-8000-000000000004";
const chat = "00000000-0000-4000-8000-000000000011";
const other = "00000000-0000-4000-8000-000000000012";
const group = "00000000-0000-4000-8000-000000000013";
let port, holdNext = false, held = null, holdPolicy = false, heldPolicy = null;
let granted = true, nextFailure = "", emptyNext = false, failPolicy = false;
const calls = [];
function batch(id, conversation, kind, at = "2026-10-03T10:00:00Z") {
  return { id: `00000000-0000-4000-8000-00000000900${id}`, conversation_id: conversation,
    kind, processed_at: at, processed_count: 2, first_seq: 5, last_seq: 11,
    ...(kind === "body" ? { retention_days: 365, cutoff_at: "2025-10-03T10:00:00Z" } :
      { min_expires_at: "2026-10-01T10:00:00Z", max_expires_at: "2026-10-03T10:00:00Z" }),
    text: "MUST_NOT_RENDER_CONTENT", content_digest: "MUST_NOT_RENDER_DIGEST" };
}
const server = http.createServer((req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${port}`);
  const send = (status, value, type = "application/json") => {
    res.writeHead(status, { "Content-Type": type, "Cache-Control": "no-store" });
    res.end(typeof value === "string" ? value : JSON.stringify(value));
  };
  if (["/web/", "/web/app.js", "/web/retention.js", "/web/legal-holds.js", "/web/retention-policy.js", "/web/retention-history.js", "/web/style.css"].includes(url.pathname)) {
    const name = url.pathname === "/web/" ? "index.html" : url.pathname.slice(5);
    if (!fs.existsSync(path.join(assets, name))) return send(404, {});
    return send(200, fs.readFileSync(path.join(assets, name), "utf8"),
      name.endsWith(".js") ? "text/javascript" : name.endsWith(".css") ? "text/css" : "text/html");
  }
  if (url.pathname === "/web/config") return send(200, { issuer: "https://sso.example.test/group",
    authorization_url: `http://127.0.0.1:${port}/authorize`, client_id: "enterprise-im-web",
    redirect_url: `http://127.0.0.1:${port}/web/`, scope: "openid profile" });
  if (url.pathname === "/authorize") { res.writeHead(302, { Location: `/web/?code=fixture&state=${url.searchParams.get("state")}` }); return res.end(); }
  if (url.pathname === "/web/oauth/token") return send(200, { access_token: "fixture-token", token_type: "Bearer", expires_in: 300 });
  if (req.headers.authorization !== "Bearer fixture-token") return send(401, { error_code: "unauthorized" });
  if (url.pathname === "/api/v1/me") return send(200, { tenant_id: tenant, user_id: user,
    display_name: "测试管理员", global_employee_no: "A001", memberships: [
      { id: admin, organization_name: "集团总部", legal_entity_name: "总部法人", title: "管理员", is_primary: true },
      { id: employee, organization_name: "分公司", legal_entity_name: "分公司法人", title: "员工", is_primary: false }] });
  const actor = req.headers["x-acting-membership-id"];
  if (![admin, employee].includes(actor)) return send(403, { error_code: "invalid_identity" });
  if (url.pathname === "/api/v1/admin/retention-policy") {
    if (actor !== admin || !granted) return send(404, { error_code: "not_found" });
    if (failPolicy) { failPolicy = false; return send(503, { error_code: "unavailable" }); }
    const policy = { message_body_days: 365, version: 0, configured: false };
    if (holdPolicy) { holdPolicy = false; heldPolicy = () => send(200, policy); return; }
    return send(200, policy);
  }
  if (url.pathname === "/api/v1/conversations") return send(200, { conversations: [chat, other].map((id, n) =>
    ({ id, type: "direct", peer_visible: true, display_name: n ? "另一会话" : "<img src=x onerror=alert(1)>项目会话",
      organization_name: "集团总部", last_seq: 0, updated_at: "2026-10-03T10:00:00Z" })), has_more: false });
  if (url.pathname === "/api/v1/groups") return send(200, { groups: [{ id: group, type: "group", name: "项目群",
    status: "active", role: "member", source_membership_id: actor, last_seq: 0, updated_at: "2026-10-03T10:00:00Z" }], has_more: false });
  if (/\/messages$/.test(url.pathname)) return send(200, { messages: [], next_after_seq: 0, has_more: false });
  if (url.pathname === "/api/v1/realtime/tickets") return send(404, { error_code: "not_found" });
  const match = url.pathname.match(/^\/api\/v1\/admin\/conversations\/([^/]+)\/retention-batches$/);
  if (match) {
    calls.push({ conversation: match[1], kind: url.searchParams.get("kind"), cursor: url.searchParams.get("cursor"), actor });
    if (req.method !== "GET" || url.searchParams.get("limit") !== "20") return send(400, { error_code: "invalid_query" });
    if (actor !== admin || !granted) return send(404, { error_code: "not_found" });
    const kind = url.searchParams.get("kind"), cursor = url.searchParams.get("cursor");
    if (!["body", "digest"].includes(kind)) return send(400, { error_code: "invalid_query" });
    const fail = nextFailure; nextFailure = "";
    if (fail === "503" || fail === "404" || fail === "401") return send(Number(fail), { error_code: fail === "401" ? "unauthorized" : fail === "404" ? "not_found" : "unavailable" });
    let value = { batches: [batch(kind === "digest" ? 3 : cursor ? 1 : 2, match[1], kind)], next_cursor: kind === "body" && !cursor ? "body-page-2" : "" };
    if (fail === "malformed") value = { batches: [{ ...batch(2, match[1], kind), processed_count: -1 }], next_cursor: "" };
    if (fail === "wrong-context") value.batches[0].conversation_id = other;
    if (fail === "cursor-loop") value.next_cursor = cursor;
    if (emptyNext) { emptyNext = false; value = { batches: [], next_cursor: "" }; }
    if (holdNext) { holdNext = false; held = () => send(200, value); return; }
    return send(200, value);
  }
  send(404, { error_code: "not_found" });
});

server.listen(0, "127.0.0.1", async () => {
  port = server.address().port;
  let browser;
  try {
    browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_EXECUTABLE || undefined });
    const page = await browser.newPage({ viewport: { width: 1280, height: 850 } });
    page.setDefaultTimeout(3000);
    const errors = []; page.on("pageerror", e => errors.push(e.message));
    // Observe fetch settlement, including responses whose body is deliberately
    // abandoned by the app after its identity changed.
    await page.addInitScript(() => {
      const fetch = window.fetch.bind(window);
      window.fixtureSettled = {};
      window.fetch = async (...args) => {
        const url = String(args[0]);
        try { return await fetch(...args); }
        finally { window.fixtureSettled[url] = (window.fixtureSettled[url] || 0) + 1; }
      };
    });
    await page.goto(`http://127.0.0.1:${port}/web/`);
    await page.getByRole("button", { name: /使用企业账号登录/ }).click();
    await page.locator("#workspace").waitFor({ state: "visible" });
    const open = page.locator("#retention-records-open"), dialog = page.locator("#retention-records-dialog");
    const cards = dialog.locator(".retention-record-card"), hint = page.locator("#retention-records-hint");
    const close = page.locator("#retention-records-close"), refresh = page.locator("#retention-records-refresh");
    const more = page.locator("#retention-records-more"), kind = page.locator("#retention-records-kind");
    const selectActor = async (name) => { await page.locator("#identity-options").getByRole("button", { name: new RegExp(name) }).click(); await page.locator("#conversation-list .conversation-button").first().waitFor(); };
    const chooseChat = async (index = 0) => { await page.locator("#conversation-list .conversation-button").nth(index).click(); };
    const waitHeld = async (policy = false) => { for (let n = 0; n < 60; n++) { if (policy ? heldPolicy : held) return; await new Promise(r => setTimeout(r, 10)); } throw new Error("expected held request"); };
    const releaseHeld = async (policy = false) => {
      const path = policy ? "/retention-policy" : "/retention-batches";
      const before = await page.evaluate(path => Object.entries(window.fixtureSettled).filter(([url]) => url.includes(path)).reduce((n, [, count]) => n + count, 0), path);
      const fn = policy ? heldPolicy : held; if (policy) heldPolicy = null; else held = null; fn();
      await page.waitForFunction(({ path, before }) => Object.entries(window.fixtureSettled).filter(([url]) => url.includes(path)).reduce((n, [, count]) => n + count, 0) > before, { path, before });
      await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))));
    };
    await selectActor("分公司"); await chooseChat();
    assert.equal(await open.isVisible(), false, "ordinary member entry visible");
    assert.equal(calls.length, 0, "ordinary member queried batches");
    failPolicy = true; await selectActor("集团总部"); await chooseChat();
    const retryAccess = page.locator("#retention-access-retry");
    await retryAccess.waitFor({ state: "visible" });
    assert.equal(await open.isVisible(), false, "unverified admin entry visible");
    await retryAccess.click(); await open.waitFor({ state: "visible" });
    assert.equal(await retryAccess.isVisible(), false);
    await open.click(); await cards.first().waitFor();
    assert.equal(await cards.count(), 1); assert.match(await cards.first().innerText(), /2 条/);
    assert.match(await cards.first().innerText(), /5.*11/); assert.match(await cards.first().innerText(), /365/);
    assert.equal(await dialog.locator("img").count(), 0, "HTML injected into title");
    assert.equal((await dialog.innerText()).includes("MUST_NOT_RENDER"), false, "sensitive unknown fields rendered");
    await more.click(); await cards.nth(1).waitFor(); assert.equal(await more.isVisible(), false);
    assert.equal(calls.at(-1).cursor, "body-page-2", "pagination lost cursor");
    await kind.selectOption("digest"); await dialog.getByText("00000000-0000-4000-8000-000000009003", { exact: true }).waitFor();
    assert.equal(await cards.count(), 1); assert.match(await dialog.innerText(), /最早到期/);
    assert.equal(await more.isVisible(), false);
    await kind.selectOption("body"); await cards.first().waitFor();
    if (process.env.IM_TEST_RETENTION_SCREENSHOT_DIR) { fs.mkdirSync(process.env.IM_TEST_RETENTION_SCREENSHOT_DIR, { recursive: true }); await page.screenshot({ path: path.join(process.env.IM_TEST_RETENTION_SCREENSHOT_DIR, "desktop.png") }); }
    await page.setViewportSize({ width: 390, height: 844 });
    assert.equal(await dialog.evaluate(el => el.scrollWidth <= el.clientWidth), true, "mobile dialog overflows");
    if (process.env.IM_TEST_RETENTION_SCREENSHOT_DIR) await page.screenshot({ path: path.join(process.env.IM_TEST_RETENTION_SCREENSHOT_DIR, "mobile.png") });
    await page.setViewportSize({ width: 1280, height: 850 });
    nextFailure = "503"; await refresh.click(); await hint.getByText(/刷新重试/).waitFor(); assert.equal(await cards.count(), 0);
    await refresh.click(); await cards.first().waitFor();
    nextFailure = "malformed"; await refresh.click(); await hint.getByText(/刷新重试/).waitFor(); assert.equal(await cards.count(), 0);
    await refresh.click(); await cards.first().waitFor();
    nextFailure = "cursor-loop"; await more.click(); await hint.getByText(/刷新重试/).waitFor(); assert.equal(await cards.count(), 0);
    await refresh.click(); await cards.first().waitFor();
    nextFailure = "wrong-context"; await refresh.click(); await hint.getByText(/刷新重试/).waitFor(); assert.equal(await cards.count(), 0);
    emptyNext = true; await refresh.click(); await hint.getByText(/暂无/).waitFor(); assert.equal(await more.isVisible(), false);

    holdNext = true; await refresh.click(); await waitHeld();
    await kind.selectOption("digest"); await dialog.getByText("00000000-0000-4000-8000-000000009003", { exact: true }).waitFor();
    await releaseHeld(); assert.equal(await kind.inputValue(), "digest"); assert.equal(await cards.count(), 1); assert.match(await cards.first().innerText(), /009003/);
    holdNext = true; await refresh.click(); await waitHeld();
    await close.click(); await chooseChat(1); await open.click(); await cards.first().waitFor();
    await releaseHeld(); assert.equal((await dialog.innerText()).includes("项目会话"), false, "old chat title restored");
    assert.equal(calls.at(-1).conversation, other);
    await close.click(); await page.locator("#group-list .group-card").first().click(); await open.click(); await cards.first().waitFor(); assert.equal(calls.at(-1).conversation, group, "group used wrong route");
    holdNext = true; await refresh.click(); await waitHeld();
    await close.click(); await selectActor("分公司"); await chooseChat(); await releaseHeld();
    assert.equal(await open.isVisible(), false); assert.equal(await cards.count(), 0);
    holdPolicy = true; await selectActor("集团总部"); await waitHeld(true); await selectActor("分公司"); await chooseChat();
    await releaseHeld(true); assert.equal(await open.isVisible(), false, "stale admin probe restored access");
    await selectActor("集团总部"); await chooseChat(); await open.waitFor({ state: "visible" }); await open.click(); await cards.first().waitFor();
    nextFailure = "404"; await refresh.click(); await hint.getByText(/权限已失效|会话不可用/).waitFor();
    assert.equal(await cards.count(), 0); assert.equal(await open.isVisible(), false); assert.equal(await refresh.isDisabled(), true);
    await close.click(); await selectActor("分公司"); await selectActor("集团总部"); await chooseChat(); await open.waitFor({ state: "visible" }); await open.click(); await cards.first().waitFor();
    nextFailure = "401"; await refresh.click(); await page.locator("#login-view").waitFor({ state: "visible" });
    assert.equal(await cards.count(), 0); assert.equal(await dialog.isVisible(), false);
    assert.equal(errors.length, 0, `page errors: ${errors.join("; ")}`);
    console.log("PASS retention records: privilege, metadata, paging, retry, malformed data, kind/chat/identity/probe races, group, expiry, responsive layout");
  } catch (error) { console.error(error); process.exitCode = 1; }
  finally { if (held) held(); if (heldPolicy) heldPolicy(); await browser?.close(); server.closeAllConnections(); server.close(); }
});
