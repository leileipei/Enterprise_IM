"use strict";

// Run with Playwright available on NODE_PATH. CHROMIUM_EXECUTABLE may point
// to a preinstalled headless Chromium when Playwright has not downloaded one.

const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require("playwright");

const assets = path.join(__dirname, "..", "assets");
const tenant = "00000000-0000-4000-8000-000000000001";
const user = "00000000-0000-4000-8000-000000000002";
const membership = "00000000-0000-4000-8000-000000000003";
const otherMembership = "00000000-0000-4000-8000-00000000000a";
const chat = "00000000-0000-4000-8000-000000000005";
const secondChat = "00000000-0000-4000-8000-000000000007";
const thirdChat = "00000000-0000-4000-8000-000000000008";
const fourthChat = "00000000-0000-4000-8000-000000000009";
const messages = [];
let showSecondChat = false;
let showThirdChat = false;
let showFourthChat = false;
let failNextInbox = false;
let primaryMembershipInboxCalls = 0;
let otherMembershipInboxCalls = 0;
let holdPrimaryInbox = false;
let heldInboxResponse;
let resolveHeldInbox;
let port;

const server = http.createServer((req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${port}`);
  const send = (status, value, type = "application/json") => {
    res.writeHead(status, { "Content-Type": type, "Cache-Control": "no-store" });
    res.end(typeof value === "string" ? value : JSON.stringify(value));
  };
  if (["/web/", "/web/app.js", "/web/retention.js", "/web/legal-holds.js", "/web/retention-policy.js", "/web/retention-history.js", "/web/audit.js", "/web/style.css"].includes(url.pathname)) {
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
      { id: membership, organization_name: "集团总部", legal_entity_name: "总部法人",
        title: "工程师", is_primary: true },
      { id: otherMembership, organization_name: "分公司任职", legal_entity_name: "分公司法人",
        title: "顾问", is_primary: false },
    ],
  });
  if (![membership, otherMembership].includes(req.headers["x-acting-membership-id"])) {
    return send(403, { error_code: "invalid_identity" });
  }
  if (url.pathname === "/api/v1/realtime/tickets") return send(200, { ticket: "test-ticket" });
  if (url.pathname === "/api/v1/groups" && req.method === "GET") {
    return send(200, { groups: [], has_more: false });
  }
  if (url.pathname === "/api/v1/conversations" && req.method === "GET") {
    if (req.headers["x-acting-membership-id"] === otherMembership) {
      otherMembershipInboxCalls++;
      return send(200, { conversations: [], has_more: false });
    }
    primaryMembershipInboxCalls++;
    if (holdPrimaryInbox) {
      holdPrimaryInbox = false;
      heldInboxResponse = res;
      resolveHeldInbox();
      return;
    }
    if (failNextInbox) {
      failNextInbox = false;
      return send(503, { error_code: "unavailable" });
    }
    return send(200, {
    conversations: [
      { id: chat, type: "direct", last_seq: messages.length, updated_at: "2026-09-29T10:00:00Z",
        peer_visible: true, display_name: "已有同事", organization_name: "集团总部" },
      ...(showSecondChat ? [{ id: secondChat, type: "direct", last_seq: 1,
        updated_at: "2026-09-29T11:00:00Z", peer_visible: true,
        display_name: "新会话", organization_name: "集团总部" }] : []),
      ...(showThirdChat ? [{ id: thirdChat, type: "direct", last_seq: 1,
        updated_at: "2026-09-29T12:00:00Z", peer_visible: true,
        display_name: "重试后会话", organization_name: "集团总部" }] : []),
      ...(showFourthChat ? [{ id: fourthChat, type: "direct", last_seq: 1,
        updated_at: "2026-09-29T13:00:00Z", peer_visible: true,
        display_name: "发送后会话", organization_name: "集团总部" }] : []),
    ], has_more: false,
  });
  }
  if (url.pathname === `/api/v1/conversations/${chat}/messages` && req.method === "GET") {
    const after = Number(url.searchParams.get("after_seq"));
    return send(200, { conversation_id: chat, messages: messages.filter((item) => item.seq > after),
      next_after_seq: messages.length, has_more: false });
  }
  if (url.pathname === `/api/v1/conversations/${chat}/messages` && req.method === "POST") {
    return send(200, { message_id: "00000000-0000-4000-8000-000000000006",
      conversation_id: chat, seq: messages.length + 1, server_time: "2026-09-29T13:01:00Z" });
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
      const setIntervalOriginal = window.setInterval.bind(window);
      window.setInterval = (callback, ms, ...args) => {
        if (ms === 5000) {
          window.__poll = () => callback(...args);
          return setIntervalOriginal(() => {}, 60 * 60 * 1000);
        }
        return setIntervalOriginal(callback, ms, ...args);
      };
      const nowOriginal = Date.now;
      window.__clockOffset = 0;
      Date.now = () => nowOriginal() + window.__clockOffset;
      Math.random = () => 0.5;
    });
    await page.goto(`http://127.0.0.1:${port}/web/`);
    await page.getByRole("button", { name: /使用企业账号登录/ }).click();
    await page.getByRole("heading", { name: "选择一位同事，开始沟通" }).waitFor();
    await page.getByRole("button", { name: /集团总部/ }).click();
    await page.locator(".conversation-button").getByText("已有同事").waitFor();
    await page.locator(".conversation-button").getByText("已有同事").click();
    await page.waitForFunction(() => window.__sockets.length === 1 && typeof window.__poll === "function");

    await page.evaluate(() => {
      window.__clockOffset += 20000;
      window.__sockets[0].onmessage({ data: JSON.stringify({ type: "ready" }) });
    });
    await page.waitForFunction(() => inboxRefreshPromise === null);
    const requestsBeforeEarlyPoll = primaryMembershipInboxCalls;
    showSecondChat = true;
    messages.push({ seq: 1, sender_user_id: "00000000-0000-4000-8000-000000000004",
      text: "无通知但已保存", server_time: "2026-09-29T10:01:00Z" });
    await page.evaluate(() => { window.__clockOffset += 25000; window.__poll(); });
    await page.waitForTimeout(100);
    if (primaryMembershipInboxCalls !== requestsBeforeEarlyPoll ||
        await page.getByText("无通知但已保存").count()) {
      throw new Error("connected safety sync ignored the ready-frame reset");
    }

    await page.evaluate(() => { window.__clockOffset += 15000; window.__poll(); });
    await page.getByText("无通知但已保存").waitFor({ timeout: 1500 });
    await page.locator(".conversation-button").getByText("新会话").waitFor({ timeout: 1500 });

    showThirdChat = true;
    failNextInbox = true;
    await page.evaluate(() => { window.__clockOffset += 45000; window.__poll(); });
    await page.getByText("服务暂时不可用，请稍后重试").waitFor({ timeout: 1500 });
    await page.evaluate(() => { window.__clockOffset += 5000; window.__poll(); });
    await page.locator(".conversation-button").getByText("重试后会话").waitFor({ timeout: 1500 });

    showFourthChat = true;
    failNextInbox = true;
    await page.locator("#message-text").fill("触发发送后列表刷新");
    await page.locator("#send-button").click();
    await page.getByText("服务暂时不可用，请稍后重试").waitFor({ timeout: 1500 });
    await page.evaluate(() => { window.__clockOffset += 5000; window.__poll(); });
    await page.locator(".conversation-button").getByText("发送后会话").waitFor({ timeout: 1500 });

    messages.push({ seq: 2, sender_user_id: "00000000-0000-4000-8000-000000000004",
      text: "断线后补拉", server_time: "2026-09-29T10:02:00Z" });
    await page.evaluate(() => { window.__sockets[0].close(); window.__poll(); });
    await page.getByText("断线后补拉").waitFor({ timeout: 1500 });
    await page.waitForFunction(() => inboxRefreshPromise === null);

    const heldInboxReady = new Promise((resolve) => { resolveHeldInbox = resolve; });
    holdPrimaryInbox = true;
    await page.evaluate(() => window.__poll());
    await Promise.race([heldInboxReady, new Promise((_, reject) => setTimeout(() =>
      reject(new Error("old membership request was not held")), 1500))]);
    await page.getByRole("button", { name: /分公司任职/ }).click();
    await page.waitForFunction(() => window.__sockets.length === 2 && inboxRefreshPromise === null);
    heldInboxResponse.writeHead(200, { "Content-Type": "application/json" });
    heldInboxResponse.end(JSON.stringify({ conversations: [{ id: chat, type: "direct", last_seq: 2,
      updated_at: "2026-09-29T14:00:00Z", peer_visible: true,
      display_name: "旧任职会话", organization_name: "集团总部" }], has_more: false }));
    await page.waitForTimeout(100);
    if (await page.locator(".conversation-button").count()) {
      throw new Error("delayed old membership response replaced the new inbox");
    }
    const requestsBeforeOldFrame = otherMembershipInboxCalls;
    await page.evaluate(() => window.__sockets[0].onmessage?.({
      data: JSON.stringify({ type: "sync_required" }),
    }));
    await page.waitForTimeout(200);
    if (otherMembershipInboxCalls !== requestsBeforeOldFrame) {
      throw new Error("old socket refreshed the new membership inbox");
    }
    process.stdout.write("connected safety sync, retries, fallback and old socket isolation passed\n");
  } catch (error) {
    console.error(error);
    process.exitCode = 1;
  } finally {
    if (browser) await browser.close();
    server.close();
  }
});
