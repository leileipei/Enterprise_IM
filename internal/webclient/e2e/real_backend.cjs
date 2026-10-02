"use strict";

// Invoked by TestRealBrowserLoginRealtimeAndOfflinePull. The page, OIDC
// redirect, token exchange, API and WebSocket all use live local processes.
const { chromium } = require("playwright");

const webURL = process.env.IM_TEST_WEB_URL;
if (!webURL) throw new Error("IM_TEST_WEB_URL is required");

async function waitUntil(predicate, timeoutMs = 6000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  throw new Error("timed out waiting for a WebSocket frame");
}

async function openDevice(browser, disablePolling = false) {
  const context = await browser.newContext({ ignoreHTTPSErrors: true });
  if (disablePolling) await context.addInitScript(() => {
    const original = window.setInterval.bind(window);
    window.setInterval = (callback, delay, ...args) => delay === 5000 ?
      original(() => {}, 60 * 60 * 1000) : original(callback, delay, ...args);
  });
  const page = await context.newPage();
  const frames = [];
  const messageRequests = [];
  const failedRequests = [];
  page.on("requestfailed", (request) => failedRequests.push(`${request.url()}: ${request.failure()?.errorText}`));
  page.on("request", (request) => {
    if (request.method() === "GET" && request.url().includes("/messages?after_seq="))
      messageRequests.push(request.url());
  });
  page.on("websocket", (socket) => {
    socket.on("framereceived", ({ payload }) => frames.push(String(payload)));
  });
  await page.goto(`${webURL}/web/`);
  await page.getByRole("button", { name: /使用企业账号登录/ }).click();
  try {
    await page.locator("#workspace").waitFor({ state: "visible", timeout: 8000 });
  } catch (error) {
    const callbackURL = new URL(page.url());
    callbackURL.search = "";
    console.error("login diagnostics", { url: callbackURL.toString(),
      title: await page.title().catch(() => "unavailable"),
      body: (await page.locator("body").textContent().catch(() => "unavailable")).slice(0, 400),
      hint: await page.locator("#login-hint").textContent().catch(() => "unavailable"),
      failedRequests });
    throw error;
  }
  if (new URL(page.url()).search) throw new Error("authorization code remained in browser URL");
  await page.locator("#identity-options .identity-button").first().click();
  await page.waitForFunction(() => document.getElementById("connection-state")?.textContent ===
    "实时通知已连接", null, { timeout: 15000 });
  await page.locator(".conversation-button").first().click();
  await page.getByText("已有消息", { exact: true }).waitFor({ timeout: 15000 });
  return { context, page, frames, messageRequests };
}

async function send(page, text) {
  await page.locator("#message-text").fill(text);
  await page.locator("#send-button").click();
  await page.getByText(text, { exact: true }).waitFor({ timeout: 10000 });
}

(async () => {
  const browser = await chromium.launch({ headless: true,
    executablePath: process.env.CHROMIUM_EXECUTABLE || undefined });
  try {
    const first = await openDevice(browser);
    const second = await openDevice(browser, true);
    const before = second.frames.length;

    await send(first.page, "来自真实浏览器一");
    await waitUntil(() => second.frames.slice(before).some((frame) =>
      frame.includes('"type":"sync_required"')));
    await second.page.getByText("来自真实浏览器一", { exact: true }).waitFor({ timeout: 6000 });
    if (await second.page.getByText("来自真实浏览器一", { exact: true }).count() !== 1)
      throw new Error("second browser duplicated the first message");

    await second.context.setOffline(true);
    await second.page.evaluate(() => realtimeSocket?.close());
    await second.page.waitForFunction(() => document.getElementById("connection-state")?.textContent !==
      "实时通知已连接", null, { timeout: 10000 });
    await send(first.page, "断线期间来自浏览器一");
    if (await second.page.getByText("断线期间来自浏览器一", { exact: true }).count() !== 0)
      throw new Error("offline browser received a message");

    const beforeReconnectFrames = second.frames.length;
    const beforeReconnectRequests = second.messageRequests.length;
    await second.context.setOffline(false);
    await second.page.waitForFunction(() => document.getElementById("connection-state")?.textContent ===
      "实时通知已连接", null, { timeout: 15000 });
    await waitUntil(() => second.frames.slice(beforeReconnectFrames).some((frame) =>
      frame.includes('"type":"ready"')), 15000);
    await waitUntil(() => second.messageRequests.slice(beforeReconnectRequests).some((request) =>
      request.includes("after_seq=2")), 15000);
    await second.page.getByText("断线期间来自浏览器一", { exact: true }).waitFor({ timeout: 15000 });
    if (await second.page.getByText("断线期间来自浏览器一", { exact: true }).count() !== 1)
      throw new Error("reconnected browser duplicated the offline message");

    await first.context.close();
    await second.context.close();
    process.stdout.write("real browser OIDC, realtime sync and offline recovery passed\n");
  } finally {
    await browser.close();
  }
})().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
