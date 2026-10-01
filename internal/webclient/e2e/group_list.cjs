"use strict";

const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require("playwright");

const assets = path.join(__dirname, "..", "assets");
const primaryMembership = "00000000-0000-4000-8000-000000000003";
const secondaryMembership = "00000000-0000-4000-8000-00000000000a";
const firstGroup = "00000000-0000-4000-8000-000000000011";
const secondGroup = "00000000-0000-4000-8000-000000000012";
const thirdGroup = "00000000-0000-4000-8000-000000000013";
const refreshedGroup = "00000000-0000-4000-8000-000000000014";
const group = (id, name, status, role, sourceMembershipID) => ({
  id, type: "group", name, status, role, source_membership_id: sourceMembershipID,
  last_seq: 2, updated_at: "2026-10-01T10:00:00Z",
});
let port;
let refreshGroups = false;
let holdNextGroupResponse = false;
let heldResponse;
let resolveHeld;
let otherMembershipGroupRequests = 0;
let primaryGroupRequests = 0;
let realtimeEnabled = false;
let secondaryHasMore = false;
let holdSecondaryPage = false;
let heldSecondaryPageResponse;
let resolveSecondaryPageHeld;
const server = http.createServer((req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${port}`);
  const send = (status, value, type = "application/json") => {
    res.writeHead(status, { "Content-Type": type, "Cache-Control": "no-store" });
    res.end(typeof value === "string" ? value : JSON.stringify(value));
  };
  if (["/web/", "/web/app.js", "/web/style.css"].includes(url.pathname)) {
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
    user_id: "00000000-0000-4000-8000-000000000002",
    display_name: "测试用户", global_employee_no: "A001",
    memberships: [
      { id: primaryMembership, organization_name: "集团总部", legal_entity_name: "总部法人",
        title: "工程师", is_primary: true },
      { id: secondaryMembership, organization_name: "分公司", legal_entity_name: "分公司法人",
        title: "顾问", is_primary: false },
    ],
  });
  if (![primaryMembership, secondaryMembership].includes(req.headers["x-acting-membership-id"])) {
    return send(403, { error_code: "invalid_identity" });
  }
  if (url.pathname === "/api/v1/realtime/tickets") return realtimeEnabled ?
    send(200, { ticket: "test-ticket" }) : send(404, { error_code: "not_found" });
  if (url.pathname === "/api/v1/conversations" && req.method === "GET") {
    return send(200, { conversations: [], has_more: false });
  }
  if (url.pathname === "/api/v1/groups" && req.method === "GET") {
    if (req.headers["x-acting-membership-id"] === secondaryMembership) {
      otherMembershipGroupRequests++;
      if (url.searchParams.has("cursor") && holdSecondaryPage) {
        holdSecondaryPage = false;
        heldSecondaryPageResponse = res;
        resolveSecondaryPageHeld();
        return;
      }
      return send(200, { groups: [group(secondGroup, "分公司项目群", "policy_blocked", "member", secondaryMembership)],
        has_more: secondaryHasMore, next_cursor: secondaryHasMore ? "secondary-next" : undefined });
    }
    primaryGroupRequests++;
    if (holdNextGroupResponse) {
      holdNextGroupResponse = false;
      heldResponse = res;
      resolveHeld();
      return;
    }
    if (url.searchParams.has("cursor")) {
      if (url.searchParams.get("cursor") !== "next-page") return send(400, { error_code: "invalid_request" });
      return send(200, { groups: [group(thirdGroup, "第三个群", "active", "member", primaryMembership)], has_more: false });
    }
    if (refreshGroups) return send(200, {
      groups: [group(refreshedGroup, "新加入的群", "active", "member", primaryMembership)], has_more: false,
    });
    return send(200, {
      groups: [
        group(firstGroup, "<img src=x onerror=alert(1)>项目群", "active", "owner", primaryMembership),
        group(secondGroup, "分公司项目群", "policy_blocked", "member", secondaryMembership),
      ], has_more: true, next_cursor: "next-page",
    });
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
      const originalSetInterval = window.setInterval.bind(window);
      window.setInterval = (callback, ms, ...args) => {
        if (ms === 5000) {
          window.__poll = () => callback(...args);
          return originalSetInterval(() => {}, 60 * 60 * 1000);
        }
        return originalSetInterval(callback, ms, ...args);
      };
      window.__sockets = [];
      window.WebSocket = class {
        static OPEN = 1;
        constructor() {
          this.readyState = 1;
          window.__sockets.push(this);
          setTimeout(() => this.onopen?.(), 0);
        }
        close() { this.readyState = 3; this.onclose?.(); }
      };
      const originalNow = Date.now;
      window.__clockOffset = 0;
      Date.now = () => originalNow() + window.__clockOffset;
      Math.random = () => 0.5;
    });
    await page.goto(`http://127.0.0.1:${port}/web/`);
    await page.getByRole("button", { name: /使用企业账号登录/ }).click();
    await page.getByRole("heading", { name: "选择一位同事，开始沟通" }).waitFor();
    await page.getByRole("button", { name: /集团总部/ }).click();

    const cards = page.locator("#group-list .group-card");
    await cards.first().waitFor({ timeout: 1500 });
    if (await cards.count() !== 2 ||
        !await cards.first().getByText("<img src=x onerror=alert(1)>项目群").count() ||
        await page.locator("#group-list img").count() ||
        !await cards.first().getByText(/群主.*集团总部/).count() ||
        !await cards.nth(1).getByText(/策略暂停.*分公司/).count()) {
      throw new Error("group cards did not render safe names, status, role and source organization");
    }
    await page.getByRole("button", { name: "加载更多群聊" }).click();
    await page.locator("#group-list .group-card").getByText("第三个群").waitFor({ timeout: 1500 });
    if (await cards.count() !== 3 || await page.getByRole("button", { name: "加载更多群聊" }).isVisible()) {
      throw new Error("group pagination did not append and finish");
    }

    const callsBeforeSlowRefresh = primaryGroupRequests;
    const slowRefreshHeld = new Promise((resolve) => { resolveHeld = resolve; });
    holdNextGroupResponse = true;
    await page.evaluate(() => { window.__clockOffset += 40000; window.__poll(); });
    await Promise.race([slowRefreshHeld, new Promise((_, reject) => setTimeout(() =>
      reject(new Error("slow group refresh was not held")), 1500))]);
    await page.evaluate(() => { window.__clockOffset += 40000; window.__poll(); });
    heldResponse.writeHead(200, { "Content-Type": "application/json" });
    heldResponse.end(JSON.stringify({ groups: [
      group(firstGroup, "<img src=x onerror=alert(1)>项目群", "active", "owner", primaryMembership),
      group(secondGroup, "分公司项目群", "policy_blocked", "member", secondaryMembership),
    ], has_more: true, next_cursor: "next-page" }));
    await page.waitForFunction(() => groupRefreshPromise === null);
    if (primaryGroupRequests - callsBeforeSlowRefresh !== 2) {
      throw new Error("polling queued an unnecessary repeated multi-page refresh");
    }
    if (await cards.count() !== 3 || !await cards.getByText("第三个群").count()) {
      throw new Error("periodic refresh collapsed already loaded group pages");
    }

    refreshGroups = true;
    await page.evaluate(() => refreshGroups());
    await page.locator("#group-list .group-card").getByText("新加入的群").waitFor({ timeout: 1500 });
    if (await cards.count() !== 1) throw new Error("group refresh did not replace stale pages");

    const heldReady = new Promise((resolve) => { resolveHeld = resolve; });
    holdNextGroupResponse = true;
    await page.evaluate(() => { refreshGroups().catch(() => {}); });
    await Promise.race([heldReady, new Promise((_, reject) => setTimeout(() =>
      reject(new Error("group refresh request was not held")), 1500))]);
    await page.getByRole("button", { name: /分公司/ }).first().click();
    await page.locator("#group-list .group-card").getByText("分公司项目群").waitFor({ timeout: 1500 });
    heldResponse.writeHead(200, { "Content-Type": "application/json" });
    heldResponse.end(JSON.stringify({ groups: [group(firstGroup, "旧任职响应", "active", "owner", primaryMembership)], has_more: false }));
    await page.waitForTimeout(100);
    if (!otherMembershipGroupRequests || await page.getByText("旧任职响应").count()) {
      throw new Error("late group response crossed the acting membership boundary");
    }

    secondaryHasMore = true;
    await page.evaluate(() => refreshGroups());
    await page.getByRole("button", { name: "加载更多群聊" }).waitFor({ timeout: 1500 });
    const secondaryPageHeld = new Promise((resolve) => { resolveSecondaryPageHeld = resolve; });
    holdSecondaryPage = true;
    await page.getByRole("button", { name: "加载更多群聊" }).click();
    await Promise.race([secondaryPageHeld, new Promise((_, reject) => setTimeout(() =>
      reject(new Error("secondary group page request was not held")), 1500))]);
    refreshGroups = false;
    realtimeEnabled = true;
    await page.getByRole("button", { name: /集团总部/ }).click();
    await page.locator("#group-list .group-card").getByText("<img src=x onerror=alert(1)>项目群").waitFor({ timeout: 1500 });
    if (!await page.getByRole("button", { name: "加载更多群聊" }).isEnabled()) {
      throw new Error("old page request left the new membership pagination disabled");
    }
    heldSecondaryPageResponse.writeHead(200, { "Content-Type": "application/json" });
    heldSecondaryPageResponse.end(JSON.stringify({ groups: [group(thirdGroup, "旧分页响应", "active", "member", secondaryMembership)], has_more: false }));
    await page.waitForTimeout(100);
    if (await page.getByText("旧分页响应").count()) throw new Error("late page crossed the acting membership boundary");

    await page.waitForFunction(() => window.__sockets.length === 1);
    const callsBeforeReady = primaryGroupRequests;
    await page.evaluate(() => window.__sockets[0].onmessage({ data: JSON.stringify({ type: "ready" }) }));
    await page.waitForFunction(() => groupRefreshPromise === null);
    if (primaryGroupRequests <= callsBeforeReady) throw new Error("ready did not refresh groups");
    refreshGroups = true;
    await page.evaluate(() => window.__sockets[0].onmessage({ data: JSON.stringify({ type: "sync_required" }) }));
    await page.locator("#group-list .group-card").getByText("新加入的群").waitFor({ timeout: 1500 });
    refreshGroups = false;
    await page.evaluate(() => { window.__clockOffset += 40000; window.__poll(); });
    await page.locator("#group-list .group-card").getByText("<img src=x onerror=alert(1)>项目群").waitFor({ timeout: 1500 });

    await page.getByRole("button", { name: "退出" }).click();
    if (await cards.count()) throw new Error("logout retained group data");
    process.stdout.write("group list rendering, pagination, refresh and identity isolation passed\n");
  } catch (error) {
    console.error(error);
    process.exitCode = 1;
  } finally {
    if (browser) await browser.close();
    server.close();
  }
});
