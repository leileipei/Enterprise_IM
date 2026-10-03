"use strict";

const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require("playwright");

const assets = path.join(__dirname, "..", "assets");
const tenant = "00000000-0000-4000-8000-000000000001";
const user = "00000000-0000-4000-8000-000000000002";
const actor = "00000000-0000-4000-8000-000000000003";
const ownerGroup = "00000000-0000-4000-8000-000000000011";
const memberGroup = "00000000-0000-4000-8000-000000000012";
const blockedGroup = "00000000-0000-4000-8000-000000000013";
const targetA = "00000000-0000-4000-8000-000000000021";
const targetB = "00000000-0000-4000-8000-000000000022";
const group = (id, name, status, role) => ({ id, type: "group", name, status, role,
  source_membership_id: actor, last_seq: 0, updated_at: "2026-10-01T10:00:00Z" });
const person = { id: "00000000-0000-4000-8000-000000000041", display_name: "张三",
  employee_no: "A041", memberships: [
    { membership_id: targetA, organization_id: targetA, organization_name: "上海公司",
      title: "工程师", is_primary: true, departments: [] },
    { membership_id: targetB, organization_id: targetB, organization_name: "苏州公司",
      title: "顾问", is_primary: false, departments: [] },
  ] };
let port;
let failFirstInvite = true;
let denyNextInvite = false;
let invalidateNextInvite = false;
let blockNextInvite = false;
let hideOwnerGroup = false;
let ownerRole = "owner";
let ownerStatus = "active";
const invitationCalls = [];
const invitations = new Map();
const server = http.createServer((req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${port}`);
  const send = (status, value, type = "application/json") => {
    res.writeHead(status, { "Content-Type": type, "Cache-Control": "no-store" });
    res.end(typeof value === "string" ? value : JSON.stringify(value));
  };
  if (["/web/", "/web/app.js", "/web/retention.js", "/web/legal-holds.js", "/web/retention-policy.js", "/web/retention-history.js", "/web/audit.js", "/web/message-search.js", "/web/cross-message-search.js", "/web/style.css"].includes(url.pathname)) {
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
    memberships: [{ id: actor, organization_name: "集团总部", legal_entity_name: "总部法人",
      title: "工程师", is_primary: true }],
  });
  if (req.headers["x-acting-membership-id"] !== actor) return send(403, { error_code: "invalid_identity" });
  if (url.pathname === "/api/v1/realtime/tickets") return send(404, { error_code: "not_found" });
  if (url.pathname === "/api/v1/conversations" && req.method === "GET") return send(200, {
    conversations: [], has_more: false,
  });
  if (url.pathname === "/api/v1/groups" && req.method === "GET") return send(200, { groups: [
    group(ownerGroup, "项目群", ownerStatus, ownerRole),
    group(memberGroup, "普通成员群", "active", "member"),
    group(blockedGroup, "暂停群", "policy_blocked", "owner"),
  ].filter((item) => !hideOwnerGroup || item.id !== ownerGroup), has_more: false });
  if (url.pathname === "/api/v1/directory/users" && req.method === "GET") return send(200, {
    people: url.searchParams.get("q") === "张三" ? [person] : [], has_more: false,
  });
  if ([ownerGroup, memberGroup, blockedGroup].some((id) =>
    url.pathname === `/api/v1/groups/${id}/messages`) && req.method === "GET") return send(200, {
    conversation_id: url.pathname.split("/")[4], messages: [], next_after_seq: 0, has_more: false,
  });
  if (url.pathname === `/api/v1/groups/${ownerGroup}/invitations` && req.method === "POST") {
    let raw = "";
    req.on("data", (chunk) => { raw += chunk; });
    req.on("end", () => {
      const body = JSON.parse(raw);
      invitationCalls.push({ actor: req.headers["x-acting-membership-id"], body });
      if (invalidateNextInvite) {
        invalidateNextInvite = false;
        return send(403, { error_code: "invalid_identity" });
      }
      if (denyNextInvite) {
        denyNextInvite = false;
        return send(403, { error_code: "group_permission_denied" });
      }
      if (blockNextInvite) {
        blockNextInvite = false;
        ownerStatus = "policy_blocked";
        return send(409, { error_code: "group_policy_blocked" });
      }
      const old = invitations.get(body.client_request_id);
      if (old) return send(200, { interval_id: old, join_seq: 1, policy_version: 1 });
      const interval = "00000000-0000-4000-8000-000000000031";
      invitations.set(body.client_request_id, interval);
      if (failFirstInvite) {
        failFirstInvite = false;
        return send(503, { error_code: "unavailable" });
      }
      return send(201, { interval_id: interval, join_seq: 1, policy_version: 1 });
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
    });
    const login = async () => {
      await page.goto(`http://127.0.0.1:${port}/web/`);
      await page.getByRole("button", { name: /使用企业账号登录/ }).click();
      await page.locator("#identity-options .identity-button").first().click();
      await page.locator("#group-list .group-card").first().waitFor({ timeout: 1500 });
    };
    await login();
    await page.locator("#group-list .group-card").getByText("普通成员群").click();
    assert(!await page.locator("#group-invite-open").isVisible(), "ordinary member could invite");
    await page.locator("#group-list .group-card").getByText("暂停群").click();
    assert(!await page.locator("#group-invite-open").isVisible(), "blocked group could invite");
    await page.locator("#group-list .group-card").getByText("项目群").click();
    await page.locator("#group-invite-open").click({ timeout: 1500 });
    await page.locator("#group-invite-query").fill("张三");
    await page.locator("#group-invite-search").click();
    await page.locator("#group-invite-results .person-button").nth(1).waitFor({ timeout: 1500 });
    await page.locator("#group-invite-results .person-button").nth(1).click();
    await page.locator("#group-invite-submit").click();
    await page.locator("#group-invite-discard").waitFor({ timeout: 1500 });
    assert(await page.locator("#group-invite-query").isDisabled() && invitationCalls.length === 1,
      "uncertain invitation did not freeze target and request ID");
    hideOwnerGroup = true;
    await login();
    assert(!await page.locator("#group-list .group-card").getByText("项目群").count(),
      "test fixture still exposed the lost group");
    await page.locator("#pending-group-invites button").first().click({ timeout: 1500 });
    assert(await page.locator("#group-invite-discard").isVisible() &&
      await page.locator("#group-invite-selected").getByText("苏州公司").count() === 1,
      "uncertain invitation was not restored after reload");
    await page.locator("#group-invite-submit").click();
    await page.locator("#group-invite-dialog").waitFor({ state: "hidden", timeout: 1500 });
    assert(invitationCalls.length === 2 && invitationCalls.every((call) => call.actor === actor) &&
      invitationCalls[0].body.client_request_id === invitationCalls[1].body.client_request_id &&
      invitationCalls[0].body.target_membership_id === targetB &&
      JSON.stringify(invitationCalls[0].body) === JSON.stringify(invitationCalls[1].body) &&
      /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(
        invitationCalls[0].body.client_request_id),
      "invitation retry did not use the same UUIDv7 request and target");
    hideOwnerGroup = false;
    ownerRole = "admin";
    await login();
    await page.locator("#group-list .group-card").getByText("项目群").click();
    assert(await page.locator("#group-invite-open").isVisible(),
      "group administrator could not access invitations");
    denyNextInvite = true;
    await page.locator("#group-invite-open").click();
    await page.locator("#group-invite-query").fill("张三");
    await page.locator("#group-invite-search").click();
    await page.locator("#group-invite-results .person-button").first().waitFor({ timeout: 1500 });
    await page.locator("#group-invite-results .person-button").first().click();
    await page.locator("#group-invite-submit").click();
    await page.waitForFunction(() => !document.getElementById("group-invite-query").disabled);
    assert(await page.locator("#identity-options .identity-button.active").count() === 1 &&
      !await page.locator("#group-invite-discard").isVisible() && invitations.size === 1,
      "invitation permission denial cleared valid acting membership or retained pending request");
    invalidateNextInvite = true;
    await page.locator("#group-invite-submit").click();
    await page.locator("#group-invite-dialog").waitFor({ state: "hidden", timeout: 1500 });
    assert(await page.locator("#identity-options .identity-button.active").count() === 0,
      "invalid acting membership did not clear selection");
    await page.locator("#identity-options .identity-button").first().click();
    await page.locator("#group-list .group-card").getByText("项目群").click();
    await page.locator("#group-invite-open").click();
    assert(await page.locator("#group-invite-discard").isVisible(),
      "identity reset lost the uncertain invitation request");
    await page.locator("#group-invite-discard").click();

    blockNextInvite = true;
    await page.locator("#group-invite-open").click();
    await page.locator("#group-invite-query").fill("张三");
    await page.locator("#group-invite-search").click();
    await page.locator("#group-invite-results .person-button").first().waitFor({ timeout: 1500 });
    await page.locator("#group-invite-results .person-button").first().click();
    await page.locator("#group-invite-submit").click();
    await page.waitForFunction(() => !document.getElementById("group-invite-query").disabled);
    assert(!await page.locator("#group-invite-discard").isVisible() &&
      await page.locator("#identity-options .identity-button.active").count() === 1 &&
      (await page.locator("#notice").textContent()).includes("无法邀请成员"),
      "definite group policy block retained retry state or cleared identity");
    await page.locator("#group-list .group-card").filter({ hasText: "项目群" }).
      getByText("策略暂停").waitFor({ timeout: 1500 });
    assert(!await page.locator("#group-invite-open").isVisible() &&
      await page.locator("#group-invite-submit").isDisabled(),
      "policy-blocked group still offered a fresh invitation");
    process.stdout.write("group invite role, idempotent recovery and permission isolation passed\n");
  } catch (error) {
    console.error(error);
    process.exitCode = 1;
  } finally {
    if (browser) await browser.close();
    server.close();
  }
});
