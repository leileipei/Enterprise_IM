"use strict";

// Invoked by TestRealBrowserLoginRealtimeAndOfflinePull. The page, OIDC
// redirect, token exchange, API and WebSocket all use live local processes.
const { chromium } = require("playwright");
const assert = require("node:assert/strict");

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
    const searchMessages = async (query, count) => {
      await first.page.locator("#message-search-open").click();
      await first.page.locator("#message-search-query").fill(query);
      await first.page.locator("#message-search-apply").click();
      await first.page.locator("#message-search-hint").getByText(new RegExp(`已显示 ${count} 条`)).waitFor();
      const matches=first.page.locator(".message-search-card");
      assert.equal(await matches.count(),count);
      assert.equal(await first.page.locator("#message-search-more").isVisible(),false);
      const text=await matches.allTextContents();
      await first.page.locator("#message-search-close").click();
      assert.equal(await matches.count(),0);
      return text;
    };
    assert.match((await searchMessages("\u0085已有\u0085",1))[0],/已有消息/);

    await first.page.locator("#retention-records-open").click();
    const evidence = first.page.locator("#retention-records-dialog");
    await evidence.getByText("00000000-0000-4000-8000-000000009b07", { exact: true }).waitFor();
    assert.equal(await evidence.locator(".retention-record-card").count(), 1);
    assert.match(await evidence.innerText(), /2 条/);
    assert.match(await evidence.innerText(), /5 ～ 11/);
    assert.match(await evidence.innerText(), /365 天/);
    assert.equal(await first.page.locator("#retention-records-more").isVisible(), false);
    await first.page.locator("#retention-records-kind").selectOption("digest");
    await evidence.getByText("00000000-0000-4000-8000-000000009d07", { exact: true }).waitFor();
    assert.equal(await evidence.locator(".retention-record-card").count(), 1);
    assert.match(await evidence.innerText(), /最早到期时间/);
    assert.match(await evidence.innerText(), /最晚到期时间/);
    await first.page.locator("#retention-records-close").click();
    assert.equal(await evidence.locator(".retention-record-card").count(), 0);
    await first.page.locator("#legal-holds-open").click();
    const holds = first.page.locator("#legal-holds-dialog");
    await holds.getByText("CASE-BROWSER-ACTIVE", { exact: true }).waitFor();
    assert.equal(await holds.locator(".legal-holds-card").count(), 2);
    assert.match(await holds.locator(".legal-holds-card").filter({ hasText: "CASE-BROWSER-ACTIVE" }).innerText(), /保全中/);
    assert.match(await holds.locator(".legal-holds-card").filter({ hasText: "CASE-BROWSER-RELEASED" }).innerText(), /已解除/);
    await holds.getByText("CAB-BROWSER-RELEASE", { exact: true }).waitFor();
    assert.equal(await first.page.locator("#legal-holds-more").isVisible(), false);
    const reference = first.page.locator("#legal-hold-reference");
    const actionHint = first.page.locator("#legal-hold-action-hint");
    await reference.fill("CASE-BROWSER-WEB-NEW");
    await first.page.locator("#legal-hold-submit").click();
    await actionHint.getByText(/服务器已确认/).waitFor();
    const webHold = holds.locator(".legal-holds-card").filter({ hasText: "CASE-BROWSER-WEB-NEW" });
    await webHold.waitFor(); assert.match(await webHold.innerText(), /保全中/);
    await webHold.getByRole("button", { name: "解除此项保全" }).click();
    await reference.fill("CAB-BROWSER-WEB-RELEASE");
    await first.page.locator("#legal-hold-confirm").check();
    await first.page.locator("#legal-hold-submit").click();
    await actionHint.getByText(/服务器已确认/).waitFor();
    await holds.getByText("CAB-BROWSER-WEB-RELEASE", { exact: true }).waitFor();
    assert.match(await webHold.innerText(), /已解除/);
    assert.equal(await webHold.getByRole("button", { name: "解除此项保全" }).count(), 0);
    assert.match(await holds.locator(".legal-holds-card").filter({ hasText: "CASE-BROWSER-ACTIVE" }).innerText(), /保全中/);
    await first.page.locator("#legal-holds-close").click();
    assert.equal(await holds.locator(".legal-holds-card").count(), 0);
    await first.page.locator("#retention-policy-open").click();
    const policyCurrent = first.page.locator("#retention-policy-current");
    const policyHint = first.page.locator("#retention-policy-hint");
    await policyCurrent.getByText("365 天", { exact: true }).waitFor();
    await first.page.locator("#retention-policy-days").fill("180");
    await first.page.locator("#retention-policy-reference").fill("CAB-BROWSER-RETENTION");
    await first.page.locator("#retention-policy-confirm").check();
    await first.page.locator("#retention-policy-submit").click();
    await policyHint.getByText(/服务器已确认/).waitFor();
    await policyCurrent.getByText("180 天", { exact: true }).waitFor();
    await policyCurrent.getByText("CAB-BROWSER-RETENTION", { exact: true }).waitFor();
    // Existing message history must reject extension at the real server.
    await first.page.locator("#retention-policy-days").fill("366");
    await first.page.locator("#retention-policy-reference").fill("CAB-BROWSER-EXTEND");
    await first.page.locator("#retention-policy-confirm").check();
    await first.page.locator("#retention-policy-submit").click();
    await policyHint.getByText(/操作冲突/).waitFor();
    assert.equal(await first.page.locator("#retention-policy-submit").isDisabled(), true);
    await first.page.locator("#retention-policy-refresh").click();
    await policyCurrent.getByText("180 天", { exact: true }).waitFor();
    await first.page.locator("#retention-history-toggle").click();
    const history = first.page.locator("#retention-history-list");
    await history.getByText("CAB-BROWSER-RETENTION", { exact: true }).waitFor();
    assert.equal(await history.locator(".retention-history-card").count(), 1);
    assert.match(await history.innerText(), /版本 1/);
    assert.match(await history.innerText(), /180 天/);
    assert.equal(await first.page.locator("#retention-history-more").isVisible(), false);
    await first.page.locator("#retention-policy-close").click();
    assert.equal(await policyCurrent.innerText(), "");
    assert.equal(await history.locator(".retention-history-card").count(), 0);
    await first.page.locator("#audit-open").click();
    const audits = first.page.locator("#audit-list");
    const auditHint = first.page.locator("#audit-hint");
    await auditHint.getByText(/已显示/).waitFor();
    const actualTenant = await audits.locator(".audit-record-card dd").nth(4).innerText();
    assert.match(actualTenant,/^[0-9a-f-]{36}$/);
    const actualActor = await audits.locator(".audit-record-card dd").nth(1).innerText();
    assert.match(actualActor,/^[0-9a-f-]{36}$/);
    await first.page.locator("#audit-actor").fill(actualActor.toUpperCase());
    assert.equal(await audits.locator(".audit-record-card").count(),0);
    await first.page.locator("#audit-from").fill(new Date(Date.now()-3600000).toISOString());
    await first.page.locator("#audit-until").fill(new Date(Date.now()+3600000).toISOString());
    await first.page.locator("#audit-resource-type").fill("tenant");
    await first.page.locator("#audit-resource-id").fill(actualTenant.toUpperCase());
    await first.page.locator("#audit-action").fill("retention_policy_update");
    await first.page.locator("#audit-outcome").selectOption("allow");
    await first.page.locator("#audit-apply").click();
    await audits.getByText("approved_retention_change", { exact: true }).waitFor();
    assert.equal(await audits.locator(".audit-record-card").count(), 1);
    assert.match(await audits.innerText(), /允许/);
    await first.page.locator("#audit-outcome").selectOption("deny");
    assert.equal(await audits.locator(".audit-record-card").count(), 0);
    await first.page.locator("#audit-apply").click();
    await audits.getByText("retention_extension_requires_empty_history", { exact: true }).waitFor();
    assert.equal(await audits.locator(".audit-record-card").count(), 1);
    assert.match(await audits.innerText(), /拒绝/);
    await first.page.locator("#audit-close").click();
    assert.equal(await audits.locator(".audit-record-card").count(), 0);
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


    const found = await searchMessages("来自",2);
    assert.equal(found.some(text=>text.includes("来自真实浏览器一")),true);
    assert.equal(found.some(text=>text.includes("断线期间来自浏览器一")),true);
    await send(first.page,"前缀\uFEFFİ工单（真实 Unicode）");
    assert.match((await searchMessages("\uFEFFİ工单",1))[0],/İ工单（真实 Unicode）/);
    const crossSearch = async (query,kind,count) => {
      await first.page.locator("#cross-message-search-open").click();
      await first.page.locator("#cross-message-search-kind").selectOption(kind);
      await first.page.locator("#cross-message-search-query").fill(query);
      await first.page.locator("#cross-message-search-apply").click();
      await first.page.locator("#cross-message-search-hint").getByText(new RegExp(`已显示 ${count} 条`)).waitFor();
      const cards=first.page.locator(".cross-message-search-card");assert.equal(await cards.count(),count);
      assert.equal(await first.page.locator("#cross-message-search-more").isVisible(),false);
      const texts=await cards.allTextContents();
      await first.page.locator("#cross-message-search-close").click();assert.equal(await cards.count(),0);return texts;
    };
    const all=await crossSearch("来自","all",3);
    assert.equal(all.some(t=>t.includes("来自真实浏览器一")),true);
    assert.equal(all.some(t=>t.includes("断线期间来自浏览器一")),true);
    assert.equal(all.some(t=>t.includes("来自真实浏览器群")),true);
    const directs=await crossSearch("来自","direct",2);assert.equal(directs.some(t=>t.includes("来自真实浏览器群")),false);
    assert.match((await crossSearch("来自","group",1))[0],/来自真实浏览器群/);
    assert.match((await crossSearch("\uFEFFİ工单","all",1))[0],/İ工单（真实 Unicode）/);
    await first.context.close();
    await second.context.close();
    process.stdout.write("real browser OIDC, retention evidence, legal hold administration, retention configuration and approval history, conversation and cross-conversation text search, filters and Unicode, realtime sync and offline recovery passed\n");
  } finally {
    await browser.close();
  }
})().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
