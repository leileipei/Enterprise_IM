"use strict";

// A missing sequence must leave the cursor in place. Both browser devices
// use the real Web client against one shared, deterministic HTTP fixture.
const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require("playwright");

const assets = path.join(__dirname, "..", "assets");
const user = "00000000-0000-4000-8000-000000000002";
const membership = "00000000-0000-4000-8000-000000000003";
const chat = "00000000-0000-4000-8000-000000000004";
const messages = [{ seq: 1, sender_user_id: "00000000-0000-4000-8000-000000000005",
  text: "原有消息", server_time: "2026-10-02T09:00:00Z" }];
let port;
let omitFirstUnseen = false;

const server = http.createServer((req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${port}`);
  const send = (status, value, type = "application/json") => {
    res.writeHead(status, { "Content-Type": type, "Cache-Control": "no-store" });
    res.end(typeof value === "string" ? value : JSON.stringify(value));
  };
  if (["/web/", "/web/app.js", "/web/retention.js", "/web/legal-holds.js", "/web/style.css"].includes(url.pathname)) {
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
    display_name: "测试用户", global_employee_no: "A001", memberships: [
      { id: membership, organization_name: "总部", legal_entity_name: "总部法人",
        title: "工程师", is_primary: true },
    ],
  });
  if (req.headers["x-acting-membership-id"] !== membership)
    return send(403, { error_code: "invalid_identity" });
  if (url.pathname === "/api/v1/realtime/tickets") return send(200, { ticket: "test-ticket" });
  if (url.pathname === "/api/v1/groups") return send(200, { groups: [], has_more: false });
  if (url.pathname === "/api/v1/conversations" && req.method === "GET") return send(200, {
    conversations: [{ id: chat, type: "direct", last_seq: messages.length,
      updated_at: "2026-10-02T09:00:00Z", peer_visible: true,
      display_name: "同事", organization_name: "总部" }], has_more: false,
  });
  if (url.pathname === `/api/v1/conversations/${chat}/messages` && req.method === "GET") {
    const after = Number(url.searchParams.get("after_seq"));
    let page = messages.filter((item) => item.seq > after);
    if (omitFirstUnseen && page.length >= 2) page = page.slice(1);
    return send(200, { conversation_id: chat, messages: page,
      next_after_seq: page.at(-1)?.seq || after, has_more: false });
  }
  if (url.pathname === `/api/v1/conversations/${chat}/messages` && req.method === "POST") {
    let raw = "";
    req.on("data", (chunk) => { raw += chunk; });
    req.on("end", () => {
      const body = JSON.parse(raw);
      const item = { seq: messages.length + 1, sender_user_id: user, text: body.text,
        server_time: "2026-10-02T09:01:00Z" };
      messages.push(item);
      send(200, { message_id: "00000000-0000-4000-8000-000000000006",
        conversation_id: chat, seq: item.seq, server_time: item.server_time, duplicate: false });
    });
    return;
  }
  return send(404, { error_code: "not_found" });
});

async function openDevice(browser) {
  const context = await browser.newContext();
  const page = await context.newPage();
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
    const originalInterval = window.setInterval.bind(window);
    window.setInterval = (callback, delay, ...args) => {
      if (delay === 5000) {
        window.__poll = () => callback(...args);
        return originalInterval(() => {}, 60 * 60 * 1000);
      }
      return originalInterval(callback, delay, ...args);
    };
  });
  await page.goto(`http://127.0.0.1:${port}/web/`);
  await page.getByRole("button", { name: /使用企业账号登录/ }).click();
  await page.getByRole("button", { name: /总部/ }).first().click();
  await page.locator(".conversation-button").getByText("同事").click();
  await page.getByText("原有消息").waitFor();
  await page.waitForFunction(() => window.__sockets.length === 1 && typeof window.__poll === "function");
  return { context, page };
}

server.listen(0, "127.0.0.1", async () => {
  port = server.address().port;
  let browser;
  try {
    browser = await chromium.launch({ headless: true,
      executablePath: process.env.CHROMIUM_EXECUTABLE || undefined });
    const first = await openDevice(browser);
    const second = await openDevice(browser);

    await first.page.locator("#message-text").fill("来自设备一");
    await first.page.locator("#send-button").click();
    await first.page.getByText("来自设备一").waitFor();
    await second.page.evaluate(() => window.__sockets[0].onmessage({
      data: JSON.stringify({ type: "sync_required" }),
    }));
    await second.page.getByText("来自设备一").waitFor();
    await second.page.evaluate(() => window.__sockets[0].onmessage({
      data: JSON.stringify({ type: "sync_required" }),
    }));
    await second.page.waitForFunction(() => syncPromise === null);
    if (await second.page.getByText("来自设备一").count() !== 1)
      throw new Error("duplicate sync signal duplicated a message on device two");

    await second.page.evaluate(() => { window.__sockets[0].close(); });
    messages.push({ seq: 3, sender_user_id: "00000000-0000-4000-8000-000000000005",
      text: "断线期间", server_time: "2026-10-02T09:02:00Z" });
    await second.page.evaluate(() => window.__poll());
    await second.page.getByText("断线期间").waitFor();
    if (await second.page.getByText("断线期间").count() !== 1)
      throw new Error("offline pull duplicated a message");

    messages.push({ seq: 4, redacted: true });
    messages.push({ seq: 5, sender_user_id: "00000000-0000-4000-8000-000000000005",
      text: "缺口之后", server_time: "2026-10-02T09:04:00Z" });
    omitFirstUnseen = true;
    await second.page.evaluate(() => syncMessages().catch(report));
    await second.page.waitForFunction(() => syncPromise === null);
    const stateAfterGap = await second.page.evaluate(() => ({ cursor: afterSeq,
      visible: [...document.querySelectorAll(".message-meta")].map((item) => item.textContent) }));
    if (stateAfterGap.cursor !== 3 || stateAfterGap.visible.some((item) => item.startsWith("#5")))
      throw new Error(`message gap was accepted: ${JSON.stringify(stateAfterGap)}`);
    omitFirstUnseen = false;
    await second.page.evaluate(() => syncMessages());
    await second.page.getByText("缺口之后").waitFor();
    if (await second.page.getByText("此消息当前不可见").count() !== 1 ||
        await second.page.getByText("缺口之后").count() !== 1)
      throw new Error("retry did not recover redacted and visible messages exactly once");

    await first.context.close();
    await second.context.close();
    process.stdout.write("two-device realtime, offline pull, gap recovery and deduplication passed\n");
  } catch (error) {
    console.error(error);
    process.exitCode = 1;
  } finally {
    if (browser) await browser.close();
    server.close();
  }
});
