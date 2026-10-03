"use strict";

const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require("playwright");

const assets = path.join(__dirname, "..", "assets");
const actor = "00000000-0000-4000-8000-000000000003";
const secondActor = "00000000-0000-4000-8000-000000000004";
const ownerGroup = "00000000-0000-4000-8000-000000000011";
const adminGroup = "00000000-0000-4000-8000-000000000012";
const memberGroup = "00000000-0000-4000-8000-000000000013";
const activeGroup = "00000000-0000-4000-8000-000000000014";
const statuses = new Map([[ownerGroup, "policy_blocked"], [adminGroup, "policy_blocked"],
  [memberGroup, "policy_blocked"], [activeGroup, "active"]]);
const roles = new Map([[ownerGroup, "owner"], [adminGroup, "admin"],
  [memberGroup, "member"], [activeGroup, "owner"]]);
const names = new Map([[ownerGroup, "群主暂停群"], [adminGroup, "管理员暂停群"],
  [memberGroup, "普通成员暂停群"], [activeGroup, "正常群"]]);
let port;
let ownerAttempts = 0;
let adminAttempts = 0;
let heldResponse;
let releaseHeld;
let holdNextOwner = false;
const recheckCalls = [];

const server = http.createServer((req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${port}`);
  const send = (status, value, type = "application/json") => {
    res.writeHead(status, { "Content-Type": type, "Cache-Control": "no-store" });
    res.end(typeof value === "string" ? value : JSON.stringify(value));
  };
  if (["/web/", "/web/app.js", "/web/retention.js", "/web/legal-holds.js", "/web/retention-policy.js", "/web/style.css"].includes(url.pathname)) {
    const name = url.pathname === "/web/" ? "index.html" : url.pathname.slice(5);
    return send(200, fs.readFileSync(path.join(assets, name), "utf8"),
      name.endsWith(".js") ? "text/javascript" : name.endsWith(".css") ? "text/css" : "text/html");
  }
  if (url.pathname === "/web/config") return send(200, {
    issuer: "https://sso.example.test/group", authorization_url: `http://127.0.0.1:${port}/authorize`,
    client_id: "enterprise-im-web", redirect_url: `http://127.0.0.1:${port}/web/`, scope: "openid profile",
  });
  if (url.pathname === "/authorize") {
    res.writeHead(302, { Location: `/web/?code=mock-code&state=${url.searchParams.get("state")}` });
    return res.end();
  }
  if (url.pathname === "/web/oauth/token") return send(200, {
    access_token: "mock-token", token_type: "Bearer", expires_in: 300,
  });
  if (req.headers.authorization !== "Bearer mock-token") return send(401, { error_code: "unauthorized" });
  if (url.pathname === "/api/v1/me") return send(200, {
    tenant_id: "00000000-0000-4000-8000-000000000001",
    user_id: "00000000-0000-4000-8000-000000000002", display_name: "测试用户",
    global_employee_no: "A001", memberships: [
      { id: actor, organization_name: "总部", legal_entity_name: "总部法人", title: "工程师", is_primary: true },
      { id: secondActor, organization_name: "分部", legal_entity_name: "分部法人", title: "顾问", is_primary: false },
    ],
  });
  const acting = req.headers["x-acting-membership-id"];
  if (![actor, secondActor].includes(acting)) return send(403, { error_code: "invalid_identity" });
  if (url.pathname === "/api/v1/realtime/tickets") return send(404, { error_code: "not_found" });
  if (url.pathname === "/api/v1/conversations") return send(200, { conversations: [], has_more: false });
  if (url.pathname === "/api/v1/groups" && req.method === "GET") {
    const ids = acting === actor ? [ownerGroup, adminGroup, memberGroup, activeGroup] : [memberGroup];
    return send(200, { groups: ids.map((id) => ({ id, type: "group", name: names.get(id),
      status: statuses.get(id), role: acting === actor ? roles.get(id) : "member",
      source_membership_id: actor, last_seq: 0, updated_at: "2026-10-02T10:00:00Z" })), has_more: false });
  }
  const match = url.pathname.match(/^\/api\/v1\/groups\/([^/]+)\/(messages|policy-rechecks)$/);
  if (match?.[2] === "messages" && req.method === "GET") return send(200, {
    conversation_id: match[1], messages: [], next_after_seq: 0, has_more: false,
  });
  if (match?.[2] === "policy-rechecks" && req.method === "POST") {
    let body = "";
    req.on("data", (chunk) => { body += chunk; });
    req.on("end", () => {
      recheckCalls.push({ groupID: match[1], actor: acting, body, query: url.search });
      if (acting !== actor || ![ownerGroup, adminGroup].includes(match[1]))
        return send(403, { error_code: "group_permission_denied" });
      if (match[1] === adminGroup) {
        adminAttempts++;
        if (adminAttempts >= 2) statuses.set(adminGroup, "active");
        return send(503, { error_code: "unavailable" });
      }
      if (holdNextOwner) {
        holdNextOwner = false;
        heldResponse = res;
        releaseHeld();
        return;
      }
      ownerAttempts++;
      if (ownerAttempts === 1) return send(409, { error_code: "group_policy_blocked" });
      statuses.set(ownerGroup, "active");
      return send(200, { status: "active", policy_version: 2 });
    });
    return;
  }
  return send(404, { error_code: "not_found" });
});

server.listen(0, "127.0.0.1", async () => {
  port = server.address().port;
  let browser;
  try {
    browser = await chromium.launch({ headless: true,
      executablePath: process.env.CHROMIUM_EXECUTABLE || undefined });
    const page = await browser.newPage();
    await page.addInitScript(() => {
      const originalTimeout = window.setTimeout.bind(window);
      window.setTimeout = (callback, delay, ...args) => originalTimeout(callback,
        delay === 15000 && window.__fastRecheckTimeout ? 100 : delay, ...args);
    });
    await page.goto(`http://127.0.0.1:${port}/web/`);
    await page.getByRole("button", { name: /使用企业账号登录/ }).click();
    await page.getByRole("heading", { name: "选择一位同事，开始沟通" }).waitFor();
    await page.getByRole("button", { name: /总部/ }).first().click();
    const cards = page.locator("#group-list .group-card");
    await cards.filter({ hasText: "群主暂停群" }).waitFor();
    const action = page.getByRole("button", { name: "复核群策略" });
    await cards.filter({ hasText: "普通成员暂停群" }).click();
    if (await action.isVisible()) throw new Error("ordinary member sees policy recheck action");
    await cards.filter({ hasText: "正常群" }).click();
    if (await action.isVisible()) throw new Error("active group shows policy recheck action");

    await cards.filter({ hasText: "管理员暂停群" }).click();
    await action.waitFor();
    await action.click();
    await page.locator("#group-policy-recheck-hint").getByText("复核结果未确认").waitFor();
    if (!await action.isEnabled() || await page.locator("#message-text").isEnabled() ||
        recheckCalls.length !== 1) throw new Error("unknown blocked result was not retryable");
    await action.click();
    await page.waitForFunction((id) => groups.get(id)?.status === "active", adminGroup);
    if (await action.isVisible() || !await page.locator("#message-text").isEnabled() ||
        recheckCalls.length !== 2 || recheckCalls[0].body !== "" || recheckCalls[0].query !== "" ||
        recheckCalls[0].actor !== actor || recheckCalls[1].body !== "") {
      throw new Error("uncertain administrator recheck did not reconcile current group state");
    }

    await cards.filter({ hasText: "群主暂停群" }).click();
    await action.waitFor();
    await action.click();
    await page.locator("#group-policy-recheck-hint").getByText("复核未通过").waitFor();
    if (await page.locator("#message-text").isEnabled() || !await action.isEnabled() ||
        recheckCalls.length !== 3) throw new Error("409 recheck allowed sending or blocked retry");
    statuses.set(ownerGroup, "active");
    await page.evaluate(() => refreshGroups());
    await page.locator("#group-policy-recheck-hint").getByText("群通信已恢复").waitFor();
    if (await action.isVisible()) throw new Error("restored group still offers recheck");
    statuses.set(ownerGroup, "policy_blocked");
    await page.evaluate(() => refreshGroups());
    await action.waitFor();
    await action.click();
    await page.waitForFunction((id) => groups.get(id)?.status === "active", ownerGroup);
    if (await action.isVisible() || !await page.locator("#message-text").isEnabled() ||
        recheckCalls.length !== 4) throw new Error("successful recheck did not restore UI");

    statuses.set(ownerGroup, "policy_blocked");
    statuses.set(adminGroup, "policy_blocked");
    await page.evaluate(() => refreshGroups());
    await action.waitFor();
    const heldAcrossGroup = new Promise((resolve) => { releaseHeld = resolve; });
    holdNextOwner = true;
    await action.click();
    await Promise.race([heldAcrossGroup, new Promise((_, reject) => setTimeout(() =>
      reject(new Error("cross-group recheck response was not held")), 1500))]);
    await cards.filter({ hasText: "管理员暂停群" }).click();
    await action.waitFor();
    if (!await action.isEnabled()) throw new Error("owner recheck blocked another group's action");
    await action.click();
    await page.waitForFunction((id) => groups.get(id)?.status === "active", adminGroup);
    if (recheckCalls.at(-1)?.groupID !== adminGroup)
      throw new Error("second group could not recheck while owner request was pending");
    await cards.filter({ hasText: "群主暂停群" }).click();
    statuses.set(ownerGroup, "active");
    heldResponse.writeHead(200, { "Content-Type": "application/json" });
    heldResponse.end(JSON.stringify({ status: "active", policy_version: 3 }));
    await page.waitForFunction((id) => groups.get(id)?.status === "active", ownerGroup);
    if (await page.locator("#group-policy-recheck-hint").getByText("复核通过").count())
      throw new Error("A-B-A switch displayed an old recheck result");

    statuses.set(ownerGroup, "policy_blocked");
    await page.evaluate(() => refreshGroups());
    await action.waitFor();
    const held = new Promise((resolve) => { releaseHeld = resolve; });
    holdNextOwner = true;
    await action.click();
    await Promise.race([held, new Promise((_, reject) => setTimeout(() =>
      reject(new Error("recheck response was not held")), 1500))]);
    await page.getByRole("button", { name: /分部/ }).first().click();
    await cards.filter({ hasText: "普通成员暂停群" }).waitFor();
    statuses.set(ownerGroup, "active");
    heldResponse.writeHead(200, { "Content-Type": "application/json" });
    heldResponse.end(JSON.stringify({ status: "active", policy_version: 3 }));
    await page.waitForTimeout(100);
    if (await action.isVisible() || await page.getByText("复核通过，群通信已恢复").count())
      throw new Error("late recheck response crossed acting identity");

    await page.getByRole("button", { name: /总部/ }).first().click();
    statuses.set(ownerGroup, "policy_blocked");
    await page.evaluate(() => refreshGroups());
    await cards.filter({ hasText: "群主暂停群" }).click();
    await action.waitFor();
    const timedOutRequest = new Promise((resolve) => { releaseHeld = resolve; });
    holdNextOwner = true;
    await page.evaluate(() => { window.__fastRecheckTimeout = true; });
    await action.click();
    await Promise.race([timedOutRequest, new Promise((_, reject) => setTimeout(() =>
      reject(new Error("timed-out recheck request was not held")), 1500))]);
    await page.locator("#group-policy-recheck-hint").getByText("复核结果未确认").waitFor({ timeout: 1500 });
    if (!await action.isEnabled()) throw new Error("hung recheck did not release retry action");

    process.stdout.write("group policy recheck roles, conflict, timeout, cross-group and identity isolation passed\n");
  } catch (error) {
    console.error(error);
    process.exitCode = 1;
  } finally {
    if (browser) await browser.close();
    server.close();
  }
});
