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
const intervalA = "00000000-0000-4000-8000-000000000021";
const intervalB = "00000000-0000-4000-8000-000000000022";
const intervalC = "00000000-0000-4000-8000-000000000023";
const group = (id, name, status, role) => ({ id, type: "group", name, status, role,
  source_membership_id: actor, last_seq: 1, updated_at: "2026-10-02T10:00:00Z" });
let port;
let rosterCalls = 0;
let failNextRoster = false;
let holdNextRoster = false;
let heldRosterResponse;
let resolveHeldRoster;
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
    user_id: "00000000-0000-4000-8000-000000000002", display_name: "测试用户",
    global_employee_no: "A001", memberships: [
      { id: actor, organization_name: "集团总部", legal_entity_name: "总部法人", title: "工程师", is_primary: true },
      { id: secondActor, organization_name: "分公司", legal_entity_name: "分公司法人", title: "顾问", is_primary: false },
    ],
  });
  if (![actor, secondActor].includes(req.headers["x-acting-membership-id"]))
    return send(403, { error_code: "invalid_identity" });
  if (url.pathname === "/api/v1/realtime/tickets") return send(404, { error_code: "not_found" });
  if (url.pathname === "/api/v1/conversations" && req.method === "GET")
    return send(200, { conversations: [], has_more: false });
  if (url.pathname === "/api/v1/groups" && req.method === "GET") return send(200, {
    groups: req.headers["x-acting-membership-id"] === secondActor ? [] : [
      group(ownerGroup, "群主群", "active", "owner"),
      group(adminGroup, "暂停群", "policy_blocked", "admin"),
      group(memberGroup, "普通成员群", "active", "member"),
    ], has_more: false,
  });
  const match = url.pathname.match(/^\/api\/v1\/groups\/([^/]+)\/(members|messages)$/);
  if (match?.[2] === "messages" && req.method === "GET") return send(200, {
    conversation_id: match[1], messages: [], next_after_seq: 0, has_more: false,
  });
  if (match?.[2] === "members" && req.method === "GET") {
    if (req.headers["x-acting-membership-id"] !== actor ||
        ![ownerGroup, adminGroup].includes(match[1])) return send(403, { error_code: "group_permission_denied" });
    if (url.searchParams.get("limit") !== "20") return send(400, { error_code: "invalid_request" });
    rosterCalls++;
    if (failNextRoster) {
      failNextRoster = false;
      return send(503, { error_code: "unavailable" });
    }
    if (holdNextRoster) {
      holdNextRoster = false;
      heldRosterResponse = res;
      resolveHeldRoster();
      return;
    }
    if (url.searchParams.has("cursor")) {
      if (url.searchParams.get("cursor") !== "next") return send(400, { error_code: "invalid_request" });
      return send(200, { members: [{ interval_id: intervalC, display_name: "第三位",
        role: "member", organization_name: "分公司" }], has_more: false });
    }
    return send(200, { members: [
      { interval_id: intervalA, display_name: "<img src=x onerror=alert(1)>成员",
        role: "owner", organization_name: "集团总部" },
      { interval_id: intervalB, display_name: "第二位", role: "admin", organization_name: "公司 B" },
    ], has_more: true, next_cursor: "next" });
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
    await page.goto(`http://127.0.0.1:${port}/web/`);
    await page.getByRole("button", { name: /使用企业账号登录/ }).click();
    await page.getByRole("heading", { name: "选择一位同事，开始沟通" }).waitFor();
    await page.getByRole("button", { name: /集团总部/ }).first().click();
    await page.locator("#group-list .group-card").first().waitFor();
    const open = page.getByRole("button", { name: "查看成员" });
    if (await open.isVisible()) throw new Error("roster action visible without a selected group");

    await page.locator("#group-list .group-card").filter({ hasText: "群主群" }).click();
    await open.waitFor();
    await open.click();
    const dialog = page.locator("#group-roster-dialog");
    await dialog.getByText("<img src=x onerror=alert(1)>成员").waitFor();
    if (await dialog.locator("img").count() || await dialog.locator(".group-roster-member").count() !== 2 ||
        !await dialog.getByText("集团总部").count()) throw new Error("first roster page unsafe or incomplete");
    failNextRoster = true;
    await dialog.getByRole("button", { name: "加载更多成员" }).click();
    await dialog.getByText("服务暂时不可用，请稍后重试").waitFor();
    if (await dialog.locator(".group-roster-member").count() !== 2)
      throw new Error("transient page failure discarded previously loaded members");
    await dialog.getByRole("button", { name: "重试" }).click();
    await dialog.getByText("第三位").waitFor();
    if (await dialog.getByRole("list", { name: "当前群成员" }).getByRole("listitem").count() !== 3)
      throw new Error("roster does not expose accessible list semantics");
    if (!await dialog.locator("#group-roster-hint").getByText("已加载 3 位成员").count())
      throw new Error("roster result was not announced");
    if (await dialog.locator(".group-roster-member").count() !== 3 ||
        await dialog.getByRole("button", { name: "加载更多成员" }).isVisible())
      throw new Error("roster pagination did not append or finish");
    await dialog.getByRole("button", { name: "关闭" }).click();
    if (await dialog.locator(".group-roster-member").count()) throw new Error("roster remained in DOM after close");
    const priorCalls = rosterCalls;
    await open.click();
    await dialog.getByText("第二位").waitFor();
    if (rosterCalls !== priorCalls + 1) throw new Error("reopened roster reused stale data");
    await dialog.getByRole("button", { name: "关闭" }).click();

    await page.locator("#group-list .group-card").filter({ hasText: "普通成员群" }).click();
    if (await open.isVisible()) throw new Error("ordinary member sees roster action");
    await page.locator("#group-list .group-card").filter({ hasText: "暂停群" }).click();
    await open.waitFor();
    failNextRoster = true;
    await open.click();
    await dialog.getByText("服务暂时不可用，请稍后重试").waitFor();
    if (await dialog.locator(".group-roster-member").count()) throw new Error("failed roster retained member data");
    await dialog.getByRole("button", { name: "重试" }).click();
    await dialog.getByText("第二位").waitFor();
    await dialog.getByRole("button", { name: "关闭" }).click();

    await page.locator("#group-list .group-card").filter({ hasText: "群主群" }).click();
    const held = new Promise((resolve) => { resolveHeldRoster = resolve; });
    holdNextRoster = true;
    await open.click();
    await Promise.race([held, new Promise((_, reject) => setTimeout(() => reject(new Error("roster was not held")), 1500))]);
    await page.evaluate((groupID) => activateGroupHistory(groupID), memberGroup);
    heldRosterResponse.writeHead(200, { "Content-Type": "application/json" });
    heldRosterResponse.end(JSON.stringify({ members: [{ interval_id: intervalA,
      display_name: "过期群响应", role: "owner", organization_name: "集团总部" }], has_more: false }));
    await page.waitForTimeout(100);
    if (await dialog.isVisible() || await dialog.getByText("过期群响应").count())
      throw new Error("late roster response crossed group boundary");

    await page.evaluate((groupID) => activateGroupHistory(groupID), ownerGroup);
    const heldIdentity = new Promise((resolve) => { resolveHeldRoster = resolve; });
    holdNextRoster = true;
    await open.click();
    await Promise.race([heldIdentity, new Promise((_, reject) => setTimeout(() => reject(new Error("identity roster was not held")), 1500))]);
    await page.evaluate((membershipID) => selectMembership(membershipID), secondActor);
    heldRosterResponse.writeHead(200, { "Content-Type": "application/json" });
    heldRosterResponse.end(JSON.stringify({ members: [{ interval_id: intervalA,
      display_name: "过期任职响应", role: "owner", organization_name: "集团总部" }], has_more: false }));
    await page.waitForTimeout(100);
    if (await dialog.isVisible() || await dialog.getByText("过期任职响应").count() ||
        await open.isVisible()) throw new Error("late roster response crossed identity boundary");
    if (await page.evaluate(() => Object.keys(localStorage).some((key) => key.includes("roster"))))
      throw new Error("roster data was saved in local storage");

    await page.evaluate((membershipID) => selectMembership(membershipID), actor);
    await page.locator("#group-list .group-card").filter({ hasText: "群主群" }).waitFor();
    await page.locator("#group-list .group-card").filter({ hasText: "群主群" }).click();
    await open.click();
    await dialog.getByText("第二位").waitFor();
    await page.evaluate(() => logout());
    if (await dialog.isVisible() || await dialog.locator(".group-roster-member").count())
      throw new Error("logout retained roster data");

    process.stdout.write("group roster access, pagination, errors and stale response isolation passed\n");
  } catch (error) {
    console.error(error);
    process.exitCode = 1;
  } finally {
    if (browser) await browser.close();
    server.close();
  }
});
