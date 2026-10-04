"use strict";

const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require("playwright");

const assets = path.join(__dirname, "..", "assets");
const tenant = "00000000-0000-4000-8000-000000000001";
const user = "00000000-0000-4000-8000-000000000002";
const actor = "00000000-0000-4000-8000-000000000003";
const secondActor = "00000000-0000-4000-8000-000000000004";
const ownerGroup = "00000000-0000-4000-8000-000000000011";
const memberGroup = "00000000-0000-4000-8000-000000000012";
const blockedGroup = "00000000-0000-4000-8000-000000000013";
const oldInterval = "00000000-0000-4000-8000-000000000021";
const newInterval = "00000000-0000-4000-8000-000000000022";
const blockedInterval = "00000000-0000-4000-8000-000000000023";
const ownerInterval = "00000000-0000-4000-8000-000000000024";
const group = (id, name, status, role) => ({ id, type: "group", name, status, role,
  source_membership_id: actor, last_seq: 8, updated_at: "2026-10-01T10:00:00Z" });
let port;
let memberVisible = true;
let blockedVisible = true;
let memberInterval = oldInterval;
let blockNextLeave = true;
let blockFirstBlockedLeave = true;
let memberCommitted = false;
let denyNextIdentity = false;
let failNextMembershipLookup = false;
let enablePagination = false;
let holdNextPage = false;
let holdNextRefresh = false;
let pendingPageReply = null;
let pendingRefreshReply = null;
let failNextGroupListRefresh = false;
const leaveCalls = [];
const membershipCalls = [];

const server = http.createServer((req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${port}`);
  const send = (status, value, type = "application/json") => {
    res.writeHead(status, { "Content-Type": type, "Cache-Control": "no-store" });
    res.end(typeof value === "string" ? value : JSON.stringify(value));
  };
  if (["/web/", "/web/app.js", "/web/file-transport.js", "/web/file-transfer.js", "/web/file-messages.js", "/web/file-download.js", "/web/file-policy.js", "/web/file-search.js", "/web/retention.js", "/web/legal-holds.js", "/web/retention-policy.js", "/web/retention-history.js", "/web/audit.js", "/web/message-search.js", "/web/cross-message-search.js", "/web/style.css"].includes(url.pathname)) {
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
    tenant_id: tenant, user_id: user, display_name: "测试用户", global_employee_no: "A001",
    memberships: [
      { id: actor, organization_name: "集团总部", legal_entity_name: "总部法人",
        title: "工程师", is_primary: true },
      { id: secondActor, organization_name: "上海公司", legal_entity_name: "上海法人",
        title: "顾问", is_primary: false },
    ],
  });
  if (![actor, secondActor].includes(req.headers["x-acting-membership-id"]))
    return send(403, { error_code: "invalid_identity" });
  if (url.pathname === "/api/v1/realtime/tickets") return send(404, { error_code: "not_found" });
  if (url.pathname === "/api/v1/conversations" && req.method === "GET") return send(200, {
    conversations: [], has_more: false,
  });
  if (url.pathname === "/api/v1/groups" && req.method === "GET") {
    if (url.searchParams.has("cursor")) {
      if (holdNextPage) {
        holdNextPage = false;
        pendingPageReply = () => send(200, { groups: [], has_more: false });
        return;
      }
      return send(200, { groups: [], has_more: false });
    }
    if (failNextGroupListRefresh) {
      failNextGroupListRefresh = false;
      return send(503, { error_code: "unavailable" });
    }
    const respond = () => send(200, { groups: [
      group(ownerGroup, "群主所在群", "active", "owner"),
      ...(memberVisible ? [group(memberGroup, "普通成员群", "active", "member")] : []),
      ...(blockedVisible ? [group(blockedGroup, "策略暂停群", "policy_blocked", "member")] : []),
    ], has_more: enablePagination, next_cursor: enablePagination ? "next" : null });
    if (holdNextRefresh) {
      holdNextRefresh = false;
      pendingRefreshReply = respond;
      return;
    }
    return respond();
  }
  const match = url.pathname.match(/^\/api\/v1\/groups\/([^/]+)\/(membership|leave|messages)$/);
  if (match && match[2] === "messages" && req.method === "GET") return send(200, {
    conversation_id: match[1], messages: [], next_after_seq: 0, has_more: false,
  });
  if (match && match[2] === "membership" && req.method === "GET") {
    membershipCalls.push({ groupID: match[1], actor: req.headers["x-acting-membership-id"] });
    if (failNextMembershipLookup) {
      failNextMembershipLookup = false;
      return send(503, { error_code: "unavailable" });
    }
    if (match[1] === memberGroup && !memberVisible) return send(404, { error_code: "not_found" });
    if (match[1] === blockedGroup && !blockedVisible) return send(404, { error_code: "not_found" });
    const interval = match[1] === ownerGroup ? ownerInterval :
      match[1] === blockedGroup ? blockedInterval : memberInterval;
    return send(200, { interval_id: interval, role: match[1] === ownerGroup ? "owner" : "member",
      join_seq: 1, group_status: match[1] === blockedGroup ? "policy_blocked" : "active" });
  }
  if (match && match[2] === "leave" && req.method === "POST") {
    let raw = "";
    req.on("data", (chunk) => { raw += chunk; });
    req.on("end", () => {
      const body = JSON.parse(raw);
      leaveCalls.push({ groupID: match[1], actor: req.headers["x-acting-membership-id"], body });
      if (denyNextIdentity) {
        denyNextIdentity = false;
        return send(403, { error_code: "invalid_identity" });
      }
      if (match[1] === ownerGroup) return send(409, { error_code: "owner_transfer_required" });
      if (match[1] === memberGroup) {
        if (body.interval_id !== oldInterval) return send(404, { error_code: "not_found" });
        if (blockNextLeave) {
          blockNextLeave = false;
          memberCommitted = true;
          memberVisible = false;
          return send(503, { error_code: "unavailable" });
        }
        return send(200, { interval_id: oldInterval, status: "left", leave_seq: 8 });
      }
      blockedVisible = false;
      if (blockFirstBlockedLeave) {
        blockFirstBlockedLeave = false;
        return send(503, { error_code: "unavailable" });
      }
      return send(200, { interval_id: blockedInterval, status: "left", leave_seq: 8 });
    });
    return;
  }
  return send(404, { error_code: "not_found" });
});

function assert(value, message) { if (!value) throw new Error(message); }
async function waitUntil(check) {
  for (let attempt = 0; attempt < 100 && !check(); attempt++)
    await new Promise((resolve) => setTimeout(resolve, 10));
  assert(check(), "timed out waiting for held group response");
}

server.listen(0, "127.0.0.1", async () => {
  port = server.address().port;
  let browser;
  try {
    browser = await chromium.launch({ headless: true,
      executablePath: process.env.CHROMIUM_EXECUTABLE || undefined });
    const page = await browser.newPage();
    await page.addInitScript(() => {
      const setInterval = window.setInterval.bind(window);
      window.setInterval = (callback, ms, ...args) => ms === 5000 ?
        setInterval(() => {}, 60 * 60 * 1000) : setInterval(callback, ms, ...args);
    });
    const login = async () => {
      await page.goto(`http://127.0.0.1:${port}/web/`);
      await page.getByRole("button", { name: /使用企业账号登录/ }).click();
      await page.locator("#identity-options .identity-button").first().click();
      await page.locator("#group-list .group-card").first().waitFor({ timeout: 1500 });
    };
    await login();
    await page.locator("#group-list .group-card").getByText("群主所在群").click();
    assert(await page.locator("#group-leave-open").isDisabled({ timeout: 1500 }) &&
      (await page.locator("#group-leave-open").textContent()).includes("转让群主"),
      "owner did not see the transfer requirement before self-leave");
    await page.locator("#group-list .group-card").getByText("普通成员群").click();
    await page.locator("#group-leave-open").click();
    await page.locator("#group-leave-dialog").waitFor({ state: "visible", timeout: 1500 });
    assert(await page.locator("#group-leave-dialog").evaluate((dialog) =>
      dialog.getBoundingClientRect().width <= 470), "leave confirmation dialog was too wide");
    await page.locator("#group-leave-confirm").click();
    await page.locator("#group-leave-retry").waitFor({ timeout: 1500 });
    assert(memberCommitted && membershipCalls.some((call) => call.groupID === memberGroup) &&
      leaveCalls.length === 1 && leaveCalls[0].body.interval_id === oldInterval &&
      leaveCalls[0].actor === actor, "leave did not target the fetched own interval");
    await page.locator("#group-leave-cancel").click();
    await page.locator("#identity-options .identity-button").nth(1).click();
    assert(!await page.locator("#pending-group-leaves button").count(),
      "another acting membership could see the pending leave");
    await page.locator("#identity-options .identity-button").first().click();
    await page.locator("#pending-group-leaves button").first().waitFor({ timeout: 1500 });
    await login();
    assert(!await page.locator("#group-list").getByText("普通成员群").count(),
      "fixture did not hide committed leave from group list");
    await page.locator("#pending-group-leaves button").first().click({ timeout: 1500 });
    assert(await page.locator("#group-leave-retry").isVisible(),
      "uncertain leave was not restored without a group card");
    await page.locator("#group-leave-cancel").click();
    memberVisible = true;
    memberInterval = newInterval;
    enablePagination = true;
    await page.locator("#identity-options .identity-button").nth(1).click();
    await page.locator("#identity-options .identity-button").first().click();
    await page.locator("#group-list .group-card").getByText("普通成员群").waitFor({ timeout: 1500 });
    await page.locator("#group-list .group-card").getByText("普通成员群").click();
    holdNextPage = true;
    await page.locator("#load-more-groups").click();
    await waitUntil(() => pendingPageReply);
    holdNextRefresh = true;
    await page.locator("#pending-group-leaves button").first().click();
    failNextMembershipLookup = true;
    await page.locator("#group-leave-retry").click();
    await waitUntil(() => leaveCalls.length === 2);
    assert(await page.locator("#group-leave-dialog").isVisible(),
      "leave dialog closed before the in-flight group page was reconciled");
    pendingPageReply();
    pendingPageReply = null;
    await waitUntil(() => pendingRefreshReply);
    await page.waitForTimeout(30);
    assert(await page.locator("#group-leave-dialog").isVisible() &&
      await page.locator("#send-button").isDisabled(),
      "leave retry enabled sending before full group refresh");
    pendingRefreshReply();
    pendingRefreshReply = null;
    await page.locator("#group-leave-dialog").waitFor({ state: "hidden", timeout: 1500 });
    assert(leaveCalls.length === 2 && leaveCalls[1].body.interval_id === oldInterval &&
      memberInterval === newInterval &&
      !await page.locator("#pending-group-leaves button").count() &&
      await page.locator("#chat-title").textContent() === "普通成员群",
      "old interval retry changed a new membership or remained pending");
    await page.locator("#group-list .group-card").getByText("普通成员群").waitFor({ timeout: 1500 });
    await page.locator("#group-list .group-card").getByText("普通成员群").click();
    assert(await page.locator("#chat-title").textContent() === "普通成员群",
      "old interval retry hid the rejoined group from the Web client");
    await page.evaluate(({ tenant, user, actor, groupID, intervalID }) => {
      localStorage.setItem(`enterprise-im-group-leave:${tenant}:${user}:${actor}:${groupID}:${intervalID}`,
        JSON.stringify({ actor, groupID, intervalID, groupName: "普通成员群" }));
      window.dispatchEvent(new Event("storage"));
    }, { tenant, user, actor, groupID: memberGroup, intervalID: oldInterval });
    await page.locator("#pending-group-leaves button").first().click();
    failNextGroupListRefresh = true;
    await page.locator("#group-leave-retry").click();
    await page.locator("#group-leave-dialog").waitFor({ state: "hidden", timeout: 1500 });
    assert(await page.locator("#chat-title").textContent() === "普通成员群" &&
      await page.locator("#send-button").isDisabled(),
      "group list outage closed a confirmed rejoined chat or enabled stale sending");
    await page.evaluate(() => refreshGroups());
    assert(await page.locator("#chat-title").textContent() === "普通成员群" &&
      await page.locator("#send-button").isEnabled(),
      "successful group refresh did not restore the confirmed rejoined chat");
    await page.locator("#group-list .group-card").getByText("策略暂停群").click();
    assert(await page.locator("#group-leave-open").isEnabled(),
      "policy-blocked member could not leave");
    await page.locator("#group-leave-open").click();
    await page.locator("#group-leave-confirm").click();
    await page.locator("#group-leave-retry").waitFor({ timeout: 1500 });
    await page.locator("#group-leave-retry").click();
    await page.locator("#group-leave-dialog").waitFor({ state: "hidden", timeout: 1500 });
    assert(leaveCalls.length === 5 && leaveCalls[3].body.interval_id === blockedInterval &&
      leaveCalls[4].body.interval_id === blockedInterval &&
      await page.locator("#chat-title").textContent() === "选择一位同事，开始沟通",
      "in-session retry of policy-blocked leave did not reset current chat");
    process.stdout.write("group leave interval recovery, owner restriction and blocked-group exit passed\n");
  } catch (error) {
    console.error(error);
    process.exitCode = 1;
  } finally {
    if (browser) await browser.close();
    server.close();
  }
});
