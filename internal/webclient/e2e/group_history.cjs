"use strict";

const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require("playwright");

const assets = path.join(__dirname, "..", "assets");
const user = "00000000-0000-4000-8000-000000000002";
const primaryMembership = "00000000-0000-4000-8000-000000000003";
const secondaryMembership = "00000000-0000-4000-8000-00000000000a";
const directID = "00000000-0000-4000-8000-000000000005";
const groupID = "00000000-0000-4000-8000-000000000011";
const blockedID = "00000000-0000-4000-8000-000000000012";
let port;
let holdNextGroupHistory = false;
let heldHistoryResponse;
let resolveHeldHistory;
let unexpectedPost = false;
let holdNextDirectSend = false;
let heldDirectSendResponse;
let resolveHeldDirectSend;
let groupName = "跨任职项目群";
let groupStatus = "active";
let forbidNextGroupHistory = false;
const groupHistoryActors = [];
const groupMessages = [
  { seq: 1, sender_user_id: user, text: "<img src=x onerror=alert(1)>项目同步", server_time: "2026-10-01T10:00:00Z" },
  { seq: 2, redacted: true },
  { seq: 3, sender_user_id: "00000000-0000-4000-8000-000000000004", text: "确认计划", server_time: "2026-10-01T10:02:00Z" },
];
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
    tenant_id: "00000000-0000-4000-8000-000000000001", user_id: user,
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
  if (url.pathname === "/api/v1/realtime/tickets") return send(200, { ticket: "test-ticket" });
  if (url.pathname === "/api/v1/conversations" && req.method === "GET") return send(200, {
    conversations: [{ id: directID, type: "direct", last_seq: 1, updated_at: "2026-10-01T10:00:00Z",
      peer_visible: true, display_name: "已有同事", organization_name: "集团总部" }], has_more: false,
  });
  if (url.pathname === "/api/v1/groups" && req.method === "GET") return send(200, {
    groups: [
      { id: groupID, type: "group", name: groupName, status: groupStatus, role: "member",
        source_membership_id: secondaryMembership, last_seq: 3, updated_at: "2026-10-01T10:02:00Z" },
      { id: blockedID, type: "group", name: "策略暂停群", status: "policy_blocked", role: "owner",
        source_membership_id: primaryMembership, last_seq: 1, updated_at: "2026-10-01T09:00:00Z" },
    ], has_more: false,
  });
  if (url.pathname === `/api/v1/conversations/${directID}/messages` && req.method === "POST") {
    if (holdNextDirectSend) {
      holdNextDirectSend = false;
      heldDirectSendResponse = res;
      resolveHeldDirectSend();
      return;
    }
    return send(200, { message_id: "00000000-0000-4000-8000-000000000006",
      conversation_id: directID, seq: 2, server_time: "2026-10-01T10:05:00Z" });
  }
  if (req.method === "POST" && url.pathname.endsWith("/messages")) {
    unexpectedPost = true;
    return send(500, { error_code: "unexpected_post" });
  }
  if (url.pathname === `/api/v1/conversations/${directID}/messages` && req.method === "GET") return send(200, {
    conversation_id: directID, messages: [{ seq: 1, sender_user_id: user, text: "单聊内容",
      server_time: "2026-10-01T10:01:00Z" }], next_after_seq: 1, has_more: false,
  });
  if (url.pathname === `/api/v1/groups/${blockedID}/messages` && req.method === "GET") return send(200, {
    conversation_id: blockedID, messages: [{ seq: 1, sender_user_id: user, text: "受限群旧消息",
      server_time: "2026-10-01T09:00:00Z" }], next_after_seq: 1, has_more: false,
  });
  if (url.pathname === `/api/v1/groups/${groupID}/messages` && req.method === "GET") {
    groupHistoryActors.push(req.headers["x-acting-membership-id"]);
    if (forbidNextGroupHistory) {
      forbidNextGroupHistory = false;
      return send(403, { error_code: "invalid_identity" });
    }
    const after = Number(url.searchParams.get("after_seq"));
    if (holdNextGroupHistory && after === 0) {
      holdNextGroupHistory = false;
      heldHistoryResponse = res;
      resolveHeldHistory();
      return;
    }
    const pending = groupMessages.filter((item) => item.seq > after);
    const items = after === 0 ? pending.slice(0, 2) : pending;
    const next = items.length ? items[items.length - 1].seq : after;
    return send(200, { conversation_id: groupID, messages: items,
      next_after_seq: next, has_more: pending.length > items.length });
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
      window.setInterval = (callback, ms, ...args) => ms === 5000 ?
        originalSetInterval(() => {}, 60 * 60 * 1000) : originalSetInterval(callback, ms, ...args);
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
    });
    await page.goto(`http://127.0.0.1:${port}/web/`);
    await page.getByRole("button", { name: /使用企业账号登录/ }).click();
    await page.getByRole("heading", { name: "选择一位同事，开始沟通" }).waitFor();
    await page.getByRole("button", { name: /集团总部/ }).click();
    await page.locator("#group-list .group-card").first().waitFor({ timeout: 1500 });
    await page.locator("#group-list .group-card").filter({ hasText: "跨任职项目群" }).click();
    await page.getByText("确认计划").waitFor({ timeout: 1500 });
    if (await page.locator(".chat-header .eyebrow").textContent() !== "GROUP HISTORY") {
      throw new Error("group history kept the direct-message heading");
    }
    if (!groupHistoryActors.length || groupHistoryActors.some((id) => id !== primaryMembership) ||
        !await page.getByText("<img src=x onerror=alert(1)>项目同步").count() ||
        await page.locator("#messages img").count() ||
        !await page.getByText("此消息当前不可见").count() ||
        !await page.locator("#message-text").isDisabled() ||
        !await page.locator("#send-button").isDisabled()) {
      throw new Error("group history did not use the selected acting membership, safe text and read-only composer");
    }
    await page.evaluate(() => { document.getElementById("message-text").value = "不得发送";
      document.getElementById("composer").requestSubmit(); });
    if (unexpectedPost) throw new Error("read-only group attempted to send a message");

    await page.locator("#group-list .group-card").filter({ hasText: "策略暂停群" }).click();
    await page.getByText("受限群旧消息").waitFor({ timeout: 1500 });
    if (!await page.locator("#message-text").isDisabled()) {
      throw new Error("policy blocked group unexpectedly enabled sending");
    }
    await page.locator("#conversation-list .conversation-button").getByText("已有同事").click();
    await page.getByText("单聊内容").waitFor({ timeout: 1500 });
    if (!await page.locator("#message-text").isEnabled() ||
        await page.locator(".chat-header .eyebrow").textContent() !== "DIRECT MESSAGE" ||
        await page.locator("#send-hint").textContent() !== "服务端保存成功后显示“已保存”，不代表对方已收到。") {
      throw new Error("direct composer or hint did not recover after group history");
    }

    const directSendHeld = new Promise((resolve) => { resolveHeldDirectSend = resolve; });
    holdNextDirectSend = true;
    await page.locator("#message-text").fill("结果尚未确认");
    await page.locator("#send-button").click();
    await Promise.race([directSendHeld, new Promise((_, reject) => setTimeout(() =>
      reject(new Error("direct send request was not held")), 1500))]);
    await page.locator("#group-list .group-card").filter({ hasText: "跨任职项目群" }).click();
    if (await page.locator("#chat-title").textContent() !== "已有同事") {
      throw new Error("group navigation discarded an in-flight direct send");
    }
    heldDirectSendResponse.writeHead(503, { "Content-Type": "application/json" });
    heldDirectSendResponse.end(JSON.stringify({ error_code: "unavailable" }));
    await page.locator("#discard-pending").waitFor({ timeout: 1500 });
    await page.locator("#group-list .group-card").filter({ hasText: "跨任职项目群" }).click();
    await page.locator("#conversation-list .conversation-button").getByText("已有同事").click();
    await page.getByRole("button", { name: /分公司/ }).first().click();
    if (await page.locator("#chat-title").textContent() !== "已有同事" ||
        !await page.locator("#discard-pending").isVisible()) {
      throw new Error("navigation discarded a pending idempotent retry");
    }
    await page.locator("#discard-pending").click();

    const heldReady = new Promise((resolve) => { resolveHeldHistory = resolve; });
    holdNextGroupHistory = true;
    await page.locator("#group-list .group-card").filter({ hasText: "跨任职项目群" }).click();
    await Promise.race([heldReady, new Promise((_, reject) => setTimeout(() =>
      reject(new Error("group history request was not held")), 1500))]);
    await page.locator("#conversation-list .conversation-button").getByText("已有同事").click();
    heldHistoryResponse.writeHead(200, { "Content-Type": "application/json" });
    heldHistoryResponse.end(JSON.stringify({ conversation_id: groupID, messages: [{ seq: 1,
      sender_user_id: user, text: "迟到的群消息", server_time: "2026-10-01T10:03:00Z" }],
      next_after_seq: 1, has_more: false }));
    await page.waitForTimeout(100);
    if (await page.getByText("迟到的群消息").count() || !await page.getByText("单聊内容").count()) {
      throw new Error("late group history response crossed the direct chat selection");
    }

    await page.locator("#group-list .group-card").filter({ hasText: "跨任职项目群" }).click();
    await page.getByText("确认计划").waitFor({ timeout: 1500 });
    groupMessages.push({ seq: 4, sender_user_id: user, text: "实时补拉到群",
      server_time: "2026-10-01T10:04:00Z" });
    await page.waitForFunction(() => window.__sockets.length === 1);
    await page.evaluate(() => window.__sockets[0].onmessage({ data: JSON.stringify({ type: "sync_required" }) }));
    await page.getByText("实时补拉到群").waitFor({ timeout: 1500 });
    groupName = "已更名项目群";
    groupStatus = "policy_blocked";
    await page.evaluate(() => refreshGroups());
    if (await page.locator("#chat-title").textContent() !== "已更名项目群" ||
        !await page.locator("#chat-subtitle").getByText("策略暂停").count()) {
      throw new Error("active group heading did not reflect refreshed name and status");
    }
    if (await page.locator("#conversation-list .conversation-button").count() !== 1) {
      throw new Error("group selection appeared as a phantom direct conversation");
    }

    await page.getByRole("button", { name: /分公司/ }).first().click();
    if (await page.getByText("实时补拉到群").count() || await page.locator("#message-text").isEnabled()) {
      throw new Error("membership switch retained group history or enabled composer");
    }
    await page.locator("#group-list .group-card").first().waitFor({ timeout: 1500 });
    forbidNextGroupHistory = true;
    await page.locator("#group-list .group-card").first().click();
    await page.getByText("当前任职已失效，请重新选择").waitFor({ timeout: 1500 });
    if (await page.locator("#identity-options .identity-button.active").count() ||
        await page.locator("#message-text").isEnabled()) {
      throw new Error("403 group history did not clear the invalid acting membership");
    }
    process.stdout.write("group history read-only, redaction, direct switch and realtime pull passed\n");
  } catch (error) {
    console.error(error);
    process.exitCode = 1;
  } finally {
    if (browser) await browser.close();
    server.close();
  }
});
