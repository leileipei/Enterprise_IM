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
let port, holdNext = false, held = null, holdPolicy = 0, heldPolicy = null;
let granted = true, nextFailure = "", emptyNext = false, failPolicy = 0;
const calls = [];
const writes = [], ledger = new Map();
const rows = [hold(1, chat), hold(2, chat, true)];
let writeFailure = "", heldWrite = null, nextHoldID = 3;
function hold(id, conversation, released = false) {
  return { id: `00000000-0000-4000-8000-00000000900${id}`, conversation_id: conversation,
    case_reference: id === 2 ? "<img src=x onerror=alert(1)>CASE-A" : "CASE-B",
    placed_at: "2026-10-01T10:00:00Z", placed_by_user_id: user, placed_by_membership_id: admin,
    ...(released ? { released_at: "2026-10-03T10:00:00Z", release_approval_reference: "CAB-2026-01",
      released_by_user_id: user, released_by_membership_id: admin } : {}),
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
    if (failPolicy > 0) { failPolicy--; return send(503, { error_code: "unavailable" }); }
    const policy = { message_body_days: 365, version: 0, configured: false };
    if (holdPolicy > 0 && --holdPolicy === 0) { heldPolicy = () => send(200, policy); return; }
    return send(200, policy);
  }
  if (url.pathname === "/api/v1/conversations") return send(200, { conversations: [chat, other].map((id, n) =>
    ({ id, type: "direct", peer_visible: true, display_name: n ? "另一会话" : "<img src=x onerror=alert(1)>项目会话",
      organization_name: "集团总部", last_seq: 0, updated_at: "2026-10-03T10:00:00Z" })), has_more: false });
  if (url.pathname === "/api/v1/groups") return send(200, { groups: [{ id: group, type: "group", name: "项目群",
    status: "active", role: "member", source_membership_id: actor, last_seq: 0, updated_at: "2026-10-03T10:00:00Z" }], has_more: false });
  if (/\/messages$/.test(url.pathname)) return send(200, { messages: [], next_after_seq: 0, has_more: false });
  if (url.pathname === "/api/v1/realtime/tickets") return send(404, { error_code: "not_found" });
  const match = url.pathname.match(/^\/api\/v1\/admin\/conversations\/([^/]+)\/legal-holds(?:\/([^/]+)\/release)?$/);
  if (match) {
    if (actor !== admin || !granted) return send(404, { error_code: "not_found" });
    if (req.method === "GET") { calls.push(match[1]); return send(200, { holds: rows.filter(h => h.conversation_id === match[1]), next_cursor: "" }); }
    if (req.method !== "POST" || req.headers["content-type"] !== "application/json") return send(400, {});
    let raw = ""; req.on("data", bytes => raw += bytes);
    req.on("end", () => {
      const input = JSON.parse(raw), type = match[2] ? "release" : "place";
      const key = type === "place" ? "case_reference" : "approval_reference";
      assert.deepEqual(Object.keys(input).sort(), [key, "request_id"].sort());
      assert.match(input.request_id, /^[0-9a-f-]{36}$/); assert.equal(typeof input[key], "string");
      writes.push({ type, conversation: match[1], hold: match[2], input, actor });
      const failure = writeFailure; writeFailure = "";
      if (["400", "403", "404", "409", "503"].includes(failure)) return send(Number(failure), { error_code: "rejected" });
      const existing = ledger.get(input.request_id);
      if (existing) { assert.equal(existing.signature, JSON.stringify(writes.at(-1))); return send(200, existing.row); }
      let row;
      if (type === "place") {
        row = { ...hold(nextHoldID++, match[1]), case_reference: input[key] }; rows.push(row);
      } else {
        row = rows.find(h => h.id === match[2] && h.conversation_id === match[1]);
        if (!row || row.released_at) return send(409, {});
        Object.assign(row, { released_at: "2026-10-03T10:00:00Z", release_approval_reference: input[key], released_by_user_id: user, released_by_membership_id: admin });
      }
      ledger.set(input.request_id, { row, signature: JSON.stringify(writes.at(-1)) });
      if (failure === "drop") {
        // Interrupt an acknowledged response body. Closing before headers can
        // cause Chrome itself to replay the POST and transparently recover.
        res.writeHead(200, { "Content-Type": "application/json", "Content-Length": "1000", "Connection": "close" });
        return res.end("{");
      }
      if (failure === "malformed") return send(200, { ...row, conversation_id: other });
      if (failure === "wrong-actor") return send(200, { ...row, placed_by_user_id: employee });
      if (failure === "hold") { heldWrite = () => send(type === "place" ? 201 : 200, row); return; }
      send(type === "place" ? 201 : 200, row);
    });
    return;
  }
  send(404, { error_code: "not_found" });
});

server.listen(0, "127.0.0.1", async () => {
  port = server.address().port;
  let browser, page;
  try {
    browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_EXECUTABLE || undefined });
    page = await browser.newPage({ viewport: { width: 1280, height: 850 } });
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
    const open = page.locator("#legal-holds-open"), dialog = page.locator("#legal-holds-dialog");
    const cards = dialog.locator(".legal-holds-card"), hint = page.locator("#legal-hold-action-hint");
    const reference = page.locator("#legal-hold-reference"), submit = page.locator("#legal-hold-submit");
    const retry = page.locator("#legal-hold-retry"), abandon = page.locator("#legal-hold-abandon");
    const close = page.locator("#legal-holds-close");
    const chooseChat = async (index = 0) => page.locator("#conversation-list .conversation-button").nth(index).click();
    const selectActor = async name => page.locator("#identity-options").getByRole("button", { name: new RegExp(name) }).click();
    await selectActor("集团总部"); await chooseChat(); await open.click(); await cards.first().waitFor();
    const releaseButtons = dialog.getByRole("button", { name: "解除此项保全", exact: true });
    assert.equal(await releaseButtons.count(), 1, "released row offers release");
    for (const value of [" ", "BAD\u0001", "界".repeat(129)]) {
      await reference.fill(value); await submit.click(); assert.equal(writes.length, 0, "invalid reference sent");
    }
    await reference.fill("CASE-WEB-NEW"); writeFailure = "drop"; await submit.click();
    await hint.getByText(/结果待确认/).waitFor(); assert.equal(await reference.isDisabled(), true);
    const firstRequest = writes[0].input.request_id; assert.equal(ledger.size, 1);
    assert.equal(await page.evaluate(() => { const event = new Event("beforeunload", { cancelable: true }); window.dispatchEvent(event); return event.defaultPrevented; }), true, "pending write does not warn before unload");
    await close.click(); await chooseChat(1); await dialog.waitFor({ state: "visible" });
    assert.match(await hint.innerText(), /结果待确认/); assert.equal(calls.at(-1), chat, "switched despite pending write");
    assert.match(await page.locator("#legal-holds-conversation").innerText(), /项目会话/, "pending context title lost");
    await close.click(); await selectActor("分公司"); await dialog.waitFor({ state: "visible" });
    assert.match(await page.locator("#legal-holds-conversation").innerText(), /项目会话/);
    await retry.click(); await hint.getByText(/服务器已确认/).waitFor();
    assert.equal(writes.length, 2); assert.equal(writes[1].input.request_id, firstRequest); assert.equal(ledger.size, 1);
    await cards.filter({ hasText: "CASE-WEB-NEW" }).waitFor();
    await releaseButtons.first().click(); await reference.fill("CAB-WEB-RELEASE");
    if (process.env.IM_TEST_HOLD_ACTION_SCREENSHOT_DIR) {
      fs.mkdirSync(process.env.IM_TEST_HOLD_ACTION_SCREENSHOT_DIR, { recursive: true });
      await page.screenshot({ path: path.join(process.env.IM_TEST_HOLD_ACTION_SCREENSHOT_DIR, "desktop.png") });
      await page.setViewportSize({ width: 390, height: 844 });
      assert.equal(await dialog.evaluate(el => el.scrollWidth <= el.clientWidth), true, "mobile action form overflows");
      await page.screenshot({ path: path.join(process.env.IM_TEST_HOLD_ACTION_SCREENSHOT_DIR, "mobile.png") });
      await page.setViewportSize({ width: 1280, height: 850 });
    }
    await submit.click(); assert.equal(writes.length, 2, "release without confirmation sent");
    await page.locator("#legal-hold-confirm").check(); writeFailure = "503"; await submit.click();
    await hint.getByText(/结果待确认/).waitFor(); const releaseID = writes.at(-1).input.request_id;
    await retry.click(); await hint.getByText(/服务器已确认/).waitFor();
    assert.equal(writes.at(-1).input.request_id, releaseID); assert.equal(ledger.size, 2);
    await cards.filter({ hasText: "CAB-WEB-RELEASE" }).waitFor();
    await reference.fill("CASE-WEB-UNKNOWN"); writeFailure = "malformed"; await submit.click();
    await hint.getByText(/结果待确认/).waitFor(); const malformedID = writes.at(-1).input.request_id;
    await close.click(); await page.locator("#logout-button").click(); await dialog.waitFor({ state: "visible" });
    assert.equal(await page.locator("#workspace").isVisible(), true, "logout discarded pending write");
    await retry.click(); await hint.getByText(/服务器已确认/).waitFor(); assert.equal(writes.at(-1).input.request_id, malformedID);
    await reference.fill("CASE-WEB-ACTOR"); writeFailure = "wrong-actor"; await submit.click();
    await hint.getByText(/结果待确认/).waitFor(); const actorID = writes.at(-1).input.request_id;
    await retry.click(); await hint.getByText(/服务器已确认/).waitFor(); assert.equal(writes.at(-1).input.request_id, actorID);
    await reference.fill("CASE-WEB-BAD-REQUEST"); writeFailure = "400"; await submit.click();
    await hint.getByText(/被拒绝/).waitFor(); assert.equal(await retry.isVisible(), false);
    await reference.fill("CASE-WEB-CONFLICT"); writeFailure = "409"; await submit.click();
    await hint.getByText(/冲突/).waitFor(); assert.equal(await reference.isDisabled(), false); assert.equal(await retry.isVisible(), false);
    await reference.fill("CASE-WEB-ABANDON"); writeFailure = "503"; await submit.click(); await hint.getByText(/结果待确认/).waitFor();
    page.once("dialog", d => d.dismiss()); await abandon.click(); assert.equal(await retry.isVisible(), true);
    page.once("dialog", d => d.accept()); await abandon.click(); await hint.getByText(/不代表撤销/).waitFor(); assert.equal(await retry.isVisible(), false);
    await reference.fill("CASE-WEB-SENDING"); writeFailure = "hold"; await submit.click();
    await page.waitForFunction(() => document.getElementById("legal-hold-submit").disabled);
    assert.equal(await abandon.isDisabled(), true); assert.equal(await retry.isDisabled(), true);
    const before = writes.length; await reference.press("Enter"); assert.equal(writes.length, before, "double submit wrote again");
    for (let n = 0; n < 60 && !heldWrite; n++) await new Promise(r => setTimeout(r, 10));
    assert.ok(heldWrite); const response = page.waitForResponse(r => r.url().includes("legal-holds") && r.request().method() === "POST");
    heldWrite(); heldWrite = null; await response; await hint.getByText(/服务器已确认/).waitFor();
    await reference.fill("CASE-WEB-TIMEOUT"); writeFailure = "hold"; await submit.click();
    await hint.getByText(/结果待确认/).waitFor({ timeout: 18000 });
    const timedID = writes.at(-1).input.request_id;
    await retry.click(); await hint.getByText(/服务器已确认/).waitFor();
    assert.equal(writes.at(-1).input.request_id, timedID, "timeout replaced original request ID");
    heldWrite(); heldWrite = null;
    await reference.fill("CASE-WEB-EXPIRY"); writeFailure = "hold"; await submit.click();
    for (let n = 0; n < 60 && !heldWrite; n++) await new Promise(r => setTimeout(r, 10));
    assert.ok(heldWrite);
    await page.route("**/api/v1/conversations/*/messages?*", r => r.fulfill({ status: 401, contentType: "application/json", body: "{}" }));
    await page.locator("#login-view").waitFor({ state: "visible", timeout: 8000 });
    const expiredResponse = page.waitForResponse(r => r.url().includes("legal-holds") && r.request().method() === "POST");
    heldWrite(); heldWrite = null; await expiredResponse;
    await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))));
    assert.equal(await dialog.isVisible(), false); assert.equal(await reference.inputValue(), ""); assert.equal(await cards.count(), 0);
    await page.unroute("**/api/v1/conversations/*/messages?*");
    await page.getByRole("button", { name: /使用企业账号登录/ }).click();
    await page.locator("#workspace").waitFor({ state: "visible" }); await selectActor("集团总部"); await chooseChat(); await open.click(); await cards.first().waitFor();
    for (const status of ["403", "404"]) {
      await reference.fill("CASE-WEB-REVOKED"); writeFailure = status; await submit.click();
      await hint.getByText(/权限已失效|会话不可用/).waitFor(); assert.equal(await open.isVisible(), false);
      assert.equal(await submit.isDisabled(), true); assert.equal(await cards.count(), 0);
      if (status === "403") { await close.click(); await selectActor("分公司"); await selectActor("集团总部"); await chooseChat(); await open.click(); await cards.first().waitFor(); }
    }
    assert.equal(errors.length, 0, `page errors: ${errors.join("; ")}`);
    console.log("PASS legal hold actions: validation, explicit release confirmation, committed response loss, stable retry IDs, unknown DTO, switching/logout guard, conflict, abandon, duplicate suppression, revocation");
  } catch (error) { console.error("diagnostics", { writes, errors: await page?.locator("#legal-hold-action-hint").innerText(), submit: await page?.locator("#legal-hold-submit").isDisabled() }); console.error(error); process.exitCode = 1; }
  finally { if (heldWrite) heldWrite(); if (held) held(); if (heldPolicy) heldPolicy(); await browser?.close(); server.closeAllConnections(); server.close(); }
});
