"use strict";

const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require("playwright");

const assets = path.join(__dirname, "..", "assets");
const user = "00000000-0000-4000-8000-000000000002";
const primary = "00000000-0000-4000-8000-000000000003";
const source = "00000000-0000-4000-8000-00000000000a";
const directID = "00000000-0000-4000-8000-000000000005";
const groupID = "00000000-0000-4000-8000-000000000011";
const blockedID = "00000000-0000-4000-8000-000000000012";
let port;
let groupStatus = "active";
let rejectNextGroupSend = false;
let expireNextGroupSend = false;
let groupName = "跨任职项目群";
let holdNextGroupList = false;
let heldGroupListResponse;
let heldGroupListSnapshot;
let resolveHeldGroupList;
const groupPosts = [];
const directPosts = [];
const groupMessages = [];
const directMessages = [];
const seenGroupKeys = new Map();

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
    tenant_id: "00000000-0000-4000-8000-000000000001", user_id: user,
    display_name: "测试用户", global_employee_no: "A001",
    memberships: [
      { id: primary, organization_name: "集团总部", legal_entity_name: "总部法人", title: "工程师", is_primary: true },
      { id: source, organization_name: "分公司", legal_entity_name: "分公司法人", title: "顾问", is_primary: false },
    ],
  });
  if (![primary, source].includes(req.headers["x-acting-membership-id"])) {
    return send(403, { error_code: "invalid_identity" });
  }
  if (url.pathname === "/api/v1/realtime/tickets") return send(200, { ticket: "test-ticket" });
  if (url.pathname === "/api/v1/conversations" && req.method === "GET") return send(200, {
    conversations: [{ id: directID, type: "direct", last_seq: directMessages.length,
      updated_at: "2026-10-01T10:00:00Z", peer_visible: true,
      display_name: "已有同事", organization_name: "集团总部" }], has_more: false,
  });
  if (url.pathname === "/api/v1/groups" && req.method === "GET") {
    const snapshot = { groups: [
      { id: groupID, type: "group", name: groupName, status: groupStatus,
        role: "member", source_membership_id: source, last_seq: groupMessages.length,
        updated_at: "2026-10-01T10:02:00Z" },
      { id: blockedID, type: "group", name: "策略暂停群", status: "policy_blocked",
        role: "owner", source_membership_id: primary, last_seq: 0,
        updated_at: "2026-10-01T09:00:00Z" },
    ], has_more: false };
    if (holdNextGroupList) {
      holdNextGroupList = false;
      heldGroupListResponse = res;
      heldGroupListSnapshot = snapshot;
      resolveHeldGroupList();
      return;
    }
    return send(200, snapshot);
  }
  if (url.pathname === `/api/v1/groups/${groupID}/messages` && req.method === "GET") return send(200, {
    conversation_id: groupID, messages: groupMessages.filter((item) => item.seq > Number(url.searchParams.get("after_seq"))),
    next_after_seq: groupMessages.length, has_more: false,
  });
  if (url.pathname === `/api/v1/groups/${blockedID}/messages` && req.method === "GET") return send(200, {
    conversation_id: blockedID, messages: [], next_after_seq: 0, has_more: false,
  });
  if (url.pathname === `/api/v1/conversations/${directID}/messages` && req.method === "GET") return send(200, {
    conversation_id: directID, messages: directMessages.filter((item) => item.seq > Number(url.searchParams.get("after_seq"))),
    next_after_seq: directMessages.length, has_more: false,
  });
  if (req.method === "POST" && url.pathname.endsWith("/messages")) {
    let raw = "";
    req.on("data", (chunk) => { raw += chunk; });
    req.on("end", () => {
      const body = JSON.parse(raw);
      const actor = req.headers["x-acting-membership-id"];
      if (url.pathname === `/api/v1/groups/${groupID}/messages`) {
        groupPosts.push({ actor, body });
        if (expireNextGroupSend) {
          expireNextGroupSend = false;
          return send(410, { error_code: "retry_window_expired" });
        }
        if (rejectNextGroupSend) {
          rejectNextGroupSend = false;
          groupStatus = "policy_blocked";
          return send(409, { error_code: "group_policy_blocked" });
        }
        if (actor !== source || (groupStatus !== "active" && !seenGroupKeys.has(body.client_msg_id))) {
          return send(409, { error_code: "group_send_unavailable" });
        }
        const duplicate = seenGroupKeys.has(body.client_msg_id);
        let item = seenGroupKeys.get(body.client_msg_id);
        if (!duplicate) {
          if (body.text === "群内第一条") groupName = "已更新项目群";
          item = { seq: groupMessages.length + 1, sender_user_id: user, text: body.text,
            server_time: "2026-10-01T10:00:00Z" };
          seenGroupKeys.set(body.client_msg_id, item);
          groupMessages.push(item);
        }
        if (body.text === "待确认群消息" && !duplicate) return send(503, { error_code: "unavailable" });
        return send(200, { message_id: "00000000-0000-4000-8000-000000000013",
          conversation_id: groupID, seq: item.seq, server_time: item.server_time, duplicate });
      }
      if (url.pathname === `/api/v1/conversations/${directID}/messages`) {
        directPosts.push({ actor, body });
        const item = { seq: directMessages.length + 1, sender_user_id: user, text: body.text,
          server_time: "2026-10-01T10:00:00Z" };
        directMessages.push(item);
        return send(200, { message_id: "00000000-0000-4000-8000-000000000014",
          conversation_id: directID, seq: item.seq, server_time: item.server_time, duplicate: false });
      }
      return send(500, { error_code: "unexpected_post" });
    });
    return;
  }
  return send(404, { error_code: "not_found" });
});

function assert(value, message) { if (!value) throw new Error(message); }

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
      window.WebSocket = class {
        static OPEN = 1;
        constructor() { this.readyState = 1; setTimeout(() => this.onopen?.(), 0); }
        close() { this.readyState = 3; this.onclose?.(); }
      };
    });
    await page.goto(`http://127.0.0.1:${port}/web/`);
    await page.getByRole("button", { name: /使用企业账号登录/ }).click();
    await page.locator("#identity-options .identity-button").filter({ hasText: "集团总部" }).click();
    await page.locator("#group-list .group-card").first().waitFor({ timeout: 1500 });

    await page.locator("#group-list .group-card").filter({ hasText: "跨任职项目群" }).click();
    assert(await page.locator("#message-text").isDisabled(), "other acting membership could compose group text");
    await page.evaluate(() => { document.getElementById("message-text").value = "错误任职";
      document.getElementById("composer").requestSubmit(); });
    assert(groupPosts.length === 0, "other acting membership sent a group message");
    await page.locator("#group-list .group-card").filter({ hasText: "策略暂停群" }).click();
    assert(await page.locator("#message-text").isDisabled(), "policy blocked group could compose text");
    await page.evaluate(() => { document.getElementById("message-text").value = "策略暂停";
      document.getElementById("composer").requestSubmit(); });
    assert(groupPosts.length === 0, "policy blocked group sent a new message");

    await page.getByRole("button", { name: /分公司/ }).first().click();
    await page.locator("#group-list .group-card").filter({ hasText: "跨任职项目群" }).waitFor({ timeout: 1500 });
    await page.locator("#group-list .group-card").filter({ hasText: "跨任职项目群" }).click();
    assert(await page.locator("#message-text").isEnabled(), "source acting membership could not compose group text");
    await page.locator("#message-text").fill("中".repeat(5462));
    await page.locator("#send-button").click();
    await page.waitForTimeout(100);
    assert(groupPosts.length === 0 && await page.locator("#message-text").isEnabled(),
      "group text beyond the API byte limit was submitted");
    const groupListHeld = new Promise((resolve) => { resolveHeldGroupList = resolve; });
    holdNextGroupList = true;
    await page.evaluate(() => { refreshGroups().catch(report); });
    await Promise.race([groupListHeld, new Promise((_, reject) => setTimeout(() =>
      reject(new Error("pre-send group list was not held")), 1500))]);
    await page.locator("#message-text").fill("群内第一条");
    await page.locator("#send-button").click();
    await page.getByText("群内第一条").waitFor({ timeout: 1500 });
    heldGroupListResponse.writeHead(200, { "Content-Type": "application/json" });
    heldGroupListResponse.end(JSON.stringify(heldGroupListSnapshot));
    await page.getByRole("heading", { name: "已更新项目群" }).waitFor({ timeout: 1500 });
    assert(groupPosts.length === 1 && groupPosts[0].actor === source &&
      groupPosts[0].body.text === "群内第一条" &&
      /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(groupPosts[0].body.client_msg_id) &&
      await page.locator("#message-text").inputValue() === "", "group send did not use source membership, UUIDv7 and history pull");

    await page.locator("#message-text").fill("待确认群消息");
    await page.locator("#send-button").click();
    await page.locator("#discard-pending").waitFor({ timeout: 1500 });
    assert(groupPosts.length === 2 && await page.locator("#message-text").evaluate((input) => input.readOnly),
      "uncertain group send did not preserve retry state");
    await page.locator("#conversation-list .conversation-button").getByText("已有同事").click();
    await page.locator("#identity-options .identity-button").filter({ hasText: "集团总部" }).click();
    assert(await page.locator("#chat-title").textContent() === "已更新项目群" &&
      await page.locator("#discard-pending").isVisible(), "uncertain group send was lost by navigation");

    groupStatus = "policy_blocked";
    await page.evaluate(() => refreshGroups());
    assert(await page.locator("#send-button").isEnabled(), "policy change prevented retrying the same group message ID");
    await page.locator("#send-button").click();
    await page.getByText("待确认群消息").waitFor({ timeout: 1500 });
    assert(groupPosts.length === 3 &&
      groupPosts[1].body.client_msg_id === groupPosts[2].body.client_msg_id &&
      groupPosts[2].body.text === "待确认群消息" &&
      await page.locator("#message-text").isDisabled(), "group retry changed its id or reopened blocked composer");
    await page.evaluate(() => { document.getElementById("message-text").value = "新消息不得发送";
      document.getElementById("composer").requestSubmit(); });
    assert(groupPosts.length === 3, "blocked group sent a new message after retry");

    await page.locator("#conversation-list .conversation-button").getByText("已有同事").click();
    assert(await page.locator("#message-text").isEnabled(), "direct composer did not recover after group send");
    await page.locator("#message-text").fill("单聊仍可发送");
    await page.locator("#send-button").click();
    await page.getByText("单聊仍可发送").waitFor({ timeout: 1500 });
    assert(directPosts.length === 1 && directPosts[0].body.text === "单聊仍可发送" &&
      directPosts[0].actor === source, "direct send regressed after group send");

    groupStatus = "active";
    await page.evaluate(() => refreshGroups());
    await page.locator("#group-list .group-card").filter({ hasText: "已更新项目群" }).click();
    assert(await page.locator("#message-text").isEnabled(), "active group did not reopen after status refresh");
    const beforeRejected = groupMessages.length;
    rejectNextGroupSend = true;
    await page.locator("#message-text").fill("策略明确拒绝");
    await page.locator("#send-button").click();
    await page.locator("#chat-subtitle").getByText("策略暂停").waitFor({ timeout: 1500 });
    assert(groupMessages.length === beforeRejected &&
      !await page.locator("#discard-pending").isVisible() &&
      await page.locator("#message-text").isDisabled(),
      "definite policy rejection was incorrectly kept as an uncertain pending send");
    await page.locator("#conversation-list .conversation-button").getByText("已有同事").click();
    assert(await page.locator("#chat-title").textContent() === "已有同事",
      "definite policy rejection prevented switching back to direct chat");

    groupStatus = "active";
    await page.evaluate(() => refreshGroups());
    await page.locator("#group-list .group-card").filter({ hasText: "已更新项目群" }).click();
    expireNextGroupSend = true;
    await page.locator("#message-text").fill("过期请求");
    await page.locator("#send-button").click();
    await page.getByText("重试期限已过，请核对历史消息后重新发送。").waitFor({ timeout: 1500 });
    assert(!await page.locator("#discard-pending").isVisible() &&
      await page.locator("#message-text").isEnabled(),
      "expired group retry was left as a pending message that cannot succeed");
    process.stdout.write("group source, blocked state, send, retry and direct regression passed\n");
  } catch (error) {
    console.error(error);
    process.exitCode = 1;
  } finally {
    if (browser) await browser.close();
    server.close();
  }
});
