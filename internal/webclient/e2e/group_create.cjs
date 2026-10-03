"use strict";

const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require("playwright");

const assets = path.join(__dirname, "..", "assets");
const primary = "00000000-0000-4000-8000-000000000003";
const secondary = "00000000-0000-4000-8000-00000000000a";
const user = "00000000-0000-4000-8000-000000000002";
const memberA1 = "00000000-0000-4000-8000-000000000021";
const memberA2 = "00000000-0000-4000-8000-000000000022";
const memberB = "00000000-0000-4000-8000-000000000023";
const createdID = "00000000-0000-4000-8000-000000000031";
const laterCreatedID = "00000000-0000-4000-8000-000000000032";
let port;
let failFirstCreate = true;
let denyNextCreate = false;
let holdNextSearch = false;
let heldSearchResponse;
let resolveHeldSearch;
let forbidNextSearch = false;
let holdNextGroupList = false;
let heldGroupListResponse;
let resolveHeldGroupList;
let holdNextChat = false;
let heldChatResponse;
let resolveHeldChat;
let chatCreated = false;
const createCalls = [];
const created = new Map();
const person = (id, name, memberships) => ({ id, display_name: name, employee_no: name,
  memberships: memberships.map(([membershipID, organizationName]) => ({
    membership_id: membershipID, organization_id: membershipID,
    organization_name: organizationName, title: "工程师", is_primary: false, departments: [],
  })) });
const peopleByQuery = {
  "张三": [person("00000000-0000-4000-8000-000000000041", "张三", [
    [memberA1, "上海公司"], [memberA2, "苏州公司"]])],
  "李四": [person("00000000-0000-4000-8000-000000000042", "李四", [[memberB, "日本子公司"]])],
  "批量": Array.from({ length: 20 }, (_, index) => person(
    `00000000-0000-4000-8000-${String(100 + index).padStart(12, "0")}`,
    `成员${index + 1}`, [[`00000000-0000-4000-8000-${String(200 + index).padStart(12, "0")}`, "集团总部"]])),
  "额外": [person("00000000-0000-4000-8000-000000000301", "额外成员", [
    ["00000000-0000-4000-8000-000000000302", "集团总部"]])],
};
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
    tenant_id: "00000000-0000-4000-8000-000000000001", user_id: user,
    display_name: "测试用户", global_employee_no: "A001",
    memberships: [
      { id: primary, organization_name: "集团总部", legal_entity_name: "总部法人", title: "工程师", is_primary: true },
      { id: secondary, organization_name: "分公司", legal_entity_name: "分公司法人", title: "顾问", is_primary: false },
    ],
  });
  if (![primary, secondary].includes(req.headers["x-acting-membership-id"])) {
    return send(403, { error_code: "invalid_identity" });
  }
  if (url.pathname === "/api/v1/realtime/tickets") return send(404, { error_code: "not_found" });
  if (url.pathname === "/api/v1/conversations" && req.method === "GET") return send(200, {
    conversations: chatCreated ? [{ id: "00000000-0000-4000-8000-000000000051",
      peer_visible: true, display_name: "李四", organization_name: "日本子公司",
      updated_at: "2026-10-01T10:00:00Z" }] : [], has_more: false,
  });
  if (url.pathname === "/api/v1/conversations" && req.method === "POST") {
    if (holdNextChat) {
      holdNextChat = false;
      heldChatResponse = res;
      resolveHeldChat();
      return;
    }
    return send(201, { id: "00000000-0000-4000-8000-000000000051", type: "direct" });
  }
  if (url.pathname === "/api/v1/directory/users" && req.method === "GET") {
    if (forbidNextSearch) {
      forbidNextSearch = false;
      return send(403, { error_code: "invalid_identity" });
    }
    const page = { people: peopleByQuery[url.searchParams.get("q")] || [], has_more: false };
    if (holdNextSearch) {
      holdNextSearch = false;
      heldSearchResponse = res;
      resolveHeldSearch();
      return;
    }
    return send(200, page);
  }
  if (url.pathname === "/api/v1/groups" && req.method === "GET") {
    const page = { groups: (req.headers["x-acting-membership-id"] === secondary ? [] :
      [...created.values()]).map((group) => ({
      id: group.id, type: "group", name: group.name, status: "active", role: "owner",
      source_membership_id: primary, last_seq: 0, updated_at: "2026-10-01T10:00:00Z",
    })), has_more: false };
    if (holdNextGroupList) {
      holdNextGroupList = false;
      heldGroupListResponse = res;
      resolveHeldGroupList();
      return;
    }
    return send(200, page);
  }
  if (url.pathname === "/api/v1/groups" && req.method === "POST") {
    let raw = "";
    req.on("data", (chunk) => { raw += chunk; });
    req.on("end", () => {
      const body = JSON.parse(raw);
      createCalls.push({ actor: req.headers["x-acting-membership-id"], body });
      if (denyNextCreate) {
        denyNextCreate = false;
        return send(404, { error_code: "not_found" });
      }
      const prior = created.get(body.client_request_id);
      if (prior) return send(200, { id: prior.id, type: "group", last_seq: 0,
        policy_version: 1, member_count: prior.member_membership_ids.length + 1 });
      const group = { ...body, id: `00000000-0000-4000-8000-${String(31 + created.size).padStart(12, "0")}` };
      created.set(body.client_request_id, group);
      if (failFirstCreate) {
        failFirstCreate = false;
        return send(503, { error_code: "unavailable" });
      }
      return send(201, { id: group.id, type: "group", last_seq: 0,
        policy_version: 1, member_count: body.member_membership_ids.length + 1 });
    });
    return;
  }
  if ([createdID, laterCreatedID].some((id) => url.pathname === `/api/v1/groups/${id}/messages`) &&
      req.method === "GET") return send(200, {
    conversation_id: url.pathname.split("/")[4], messages: [], next_after_seq: 0, has_more: false,
  });
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
    await page.goto(`http://127.0.0.1:${port}/web/`);
    await page.getByRole("button", { name: /使用企业账号登录/ }).click();
    await page.locator("#identity-options .identity-button").filter({ hasText: "集团总部" }).click();
    await page.locator("#group-create-open").click({ timeout: 1500 });
    assert(await page.locator("#group-create-dialog").isVisible(), "group create dialog did not open");
    await page.locator("#group-create-name").fill("跨组织项目群");
    await page.locator("#group-create-query").fill("张三");
    await page.locator("#group-create-search").click();
    await page.locator("#group-create-results .person-button").first().waitFor({ timeout: 1500 });
    await page.locator("#group-create-results .person-button").first().click();
    assert(await page.locator("#group-create-results .person-button").nth(1).isDisabled() &&
      await page.locator("#group-create-selected").getByText("张三").count() === 1,
      "same person could be selected through a second membership");
    await page.locator("#group-create-query").fill("李四");
    await page.locator("#group-create-search").click();
    await page.locator("#group-create-results .person-button").first().waitFor({ timeout: 1500 });
    await page.locator("#group-create-results .person-button").first().click();
    assert(await page.locator("#group-create-selected").getByText("李四").count() === 1,
      "selected members were lost between searches");
    await page.locator("#group-create-submit").click();
    await page.locator("#group-create-discard").waitFor({ timeout: 1500 });
    assert(await page.locator("#group-create-name").isDisabled() &&
      await page.locator("#group-create-selected button").first().isDisabled() &&
      createCalls.length === 1, "uncertain group create did not freeze its payload");
    await page.reload();
    await page.getByRole("button", { name: /使用企业账号登录/ }).click();
    await page.locator("#identity-options .identity-button").filter({ hasText: "集团总部" }).click();
    await page.locator("#group-create-open").click();
    assert(await page.locator("#group-create-name").inputValue() === "跨组织项目群" &&
      await page.locator("#group-create-name").isDisabled() &&
      await page.locator("#group-create-discard").isVisible(),
      "uncertain group create was not recoverable after reload");
    await page.evaluate(({ primary, memberA1 }) => {
      const id = "00000000-0000-7000-8000-000000000099";
      const key = `enterprise-im-group-create:00000000-0000-4000-8000-000000000001:` +
        `00000000-0000-4000-8000-000000000002:${primary}:${id}`;
      localStorage.setItem(key, JSON.stringify({ id, name: "并发待确认群", actor: primary,
        memberIDs: [memberA1], members: [{ userID: "00000000-0000-4000-8000-000000000041",
          membershipID: memberA1, name: "张三", organization: "上海公司" }] }));
    }, { primary, memberA1 });
    await page.locator("#group-create-submit").click();
    await page.locator("#group-create-dialog").waitFor({ state: "hidden", timeout: 1500 });
    await page.locator("#group-list .group-card").getByText("跨组织项目群").waitFor({ timeout: 1500 });
    assert(createCalls.length === 2 && createCalls.every((call) => call.actor === primary) &&
      createCalls[0].body.client_request_id === createCalls[1].body.client_request_id &&
      createCalls[0].body.name === "跨组织项目群" &&
      JSON.stringify(createCalls[0].body.member_membership_ids) === JSON.stringify([memberA1, memberB]) &&
      JSON.stringify(createCalls[0].body) === JSON.stringify(createCalls[1].body) &&
      /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(createCalls[0].body.client_request_id) &&
      await page.locator("#chat-title").textContent() === "跨组织项目群",
      "group create did not reuse frozen UUIDv7 payload or open the created group");
    await page.locator("#group-create-open").click();
    assert(await page.locator("#group-create-name").inputValue() === "并发待确认群" &&
      await page.locator("#group-create-discard").isVisible(),
      "completing one tab erased another pending group create request");
    await page.locator("#group-create-discard").click();

    await page.locator("#group-create-open").click();
    await page.locator("#group-create-name").fill("第二项目群");
    await page.locator("#group-create-query").fill("张三");
    await page.locator("#group-create-search").click();
    await page.locator("#group-create-results .person-button").first().waitFor({ timeout: 1500 });
    await page.locator("#group-create-results .person-button").first().click();
    const listHeld = new Promise((resolve) => { resolveHeldGroupList = resolve; });
    holdNextGroupList = true;
    await page.locator("#group-create-submit").click();
    await Promise.race([listHeld, new Promise((_, reject) => setTimeout(() =>
      reject(new Error("post-create group list was not held")), 1500))]);
    await page.locator("#message-text").fill("不要覆盖的草稿");
    heldGroupListResponse.writeHead(200, { "Content-Type": "application/json" });
    heldGroupListResponse.end(JSON.stringify({ groups: [...created.values()].map((group) => ({
      id: group.id, type: "group", name: group.name, status: "active", role: "owner",
      source_membership_id: primary, last_seq: 0, updated_at: "2026-10-01T10:00:00Z",
    })), has_more: false }));
    await page.locator("#group-list .group-card").getByText("第二项目群").waitFor({ timeout: 1500 });
    assert(await page.locator("#chat-title").textContent() === "跨组织项目群" &&
      await page.locator("#message-text").inputValue() === "不要覆盖的草稿",
      "late group refresh displaced the active chat or unsent draft");
    await page.locator("#message-text").fill("");

    await page.locator("#group-create-open").click();
    await page.locator("#group-create-name").fill("邀请弹窗竞态群");
    await page.locator("#group-create-query").fill("张三");
    await page.locator("#group-create-search").click();
    await page.locator("#group-create-results .person-button").first().waitFor({ timeout: 1500 });
    await page.locator("#group-create-results .person-button").first().click();
    const inviteRaceListHeld = new Promise((resolve) => { resolveHeldGroupList = resolve; });
    holdNextGroupList = true;
    await page.locator("#group-create-submit").click();
    await Promise.race([inviteRaceListHeld, new Promise((_, reject) => setTimeout(() =>
      reject(new Error("invite race group list was not held")), 1500))]);
    await page.locator("#group-invite-open").click();
    heldGroupListResponse.writeHead(200, { "Content-Type": "application/json" });
    heldGroupListResponse.end(JSON.stringify({ groups: [...created.values()].map((group) => ({
      id: group.id, type: "group", name: group.name, status: "active", role: "owner",
      source_membership_id: primary, last_seq: 0, updated_at: "2026-10-01T10:00:00Z",
    })), has_more: false }));
    await page.locator("#group-list .group-card").getByText("邀请弹窗竞态群").waitFor({ timeout: 1500 });
    await page.waitForTimeout(50);
    assert(await page.locator("#group-invite-dialog").isVisible() &&
      await page.locator("#chat-title").textContent() === "跨组织项目群",
      "late group create replaced the chat behind an invitation dialog");
    await page.locator("#group-invite-cancel").click();

    await page.locator("#group-create-open").click();
    await page.locator("#group-create-name").fill("单聊竞态群");
    await page.locator("#group-create-query").fill("张三");
    await page.locator("#group-create-search").click();
    await page.locator("#group-create-results .person-button").first().waitFor({ timeout: 1500 });
    await page.locator("#group-create-results .person-button").first().click();
    const chatRaceListHeld = new Promise((resolve) => { resolveHeldGroupList = resolve; });
    holdNextGroupList = true;
    await page.locator("#group-create-submit").click();
    await Promise.race([chatRaceListHeld, new Promise((_, reject) => setTimeout(() =>
      reject(new Error("chat race group list was not held")), 1500))]);
    await page.locator("#person-query").fill("李四");
    await page.locator("#search-button").click();
    await page.locator("#people-results .person-button").first().waitFor({ timeout: 1500 });
    const chatHeld = new Promise((resolve) => { resolveHeldChat = resolve; });
    holdNextChat = true;
    await page.locator("#people-results .person-button").first().click();
    await Promise.race([chatHeld, new Promise((_, reject) => setTimeout(() =>
      reject(new Error("direct chat request was not held")), 1500))]);
    heldGroupListResponse.writeHead(200, { "Content-Type": "application/json" });
    heldGroupListResponse.end(JSON.stringify({ groups: [...created.values()].map((group) => ({
      id: group.id, type: "group", name: group.name, status: "active", role: "owner",
      source_membership_id: primary, last_seq: 0, updated_at: "2026-10-01T10:00:00Z",
    })), has_more: false }));
    await page.locator("#group-list .group-card").getByText("单聊竞态群").waitFor({ timeout: 1500 });
    chatCreated = true;
    heldChatResponse.writeHead(201, { "Content-Type": "application/json" });
    heldChatResponse.end(JSON.stringify({ id: "00000000-0000-4000-8000-000000000051", type: "direct" }));
    await page.locator("#chat-title").getByText("李四").waitFor({ timeout: 1500 });

    await page.locator("#group-create-open").click();
    await page.locator("#group-create-name").fill("切换前创建的群");
    await page.locator("#group-create-query").fill("张三");
    await page.locator("#group-create-search").click();
    await page.locator("#group-create-results .person-button").first().waitFor({ timeout: 1500 });
    await page.locator("#group-create-results .person-button").first().click();
    const identityListHeld = new Promise((resolve) => { resolveHeldGroupList = resolve; });
    holdNextGroupList = true;
    await page.locator("#group-create-submit").click();
    await Promise.race([identityListHeld, new Promise((_, reject) => setTimeout(() =>
      reject(new Error("identity race group list was not held")), 1500))]);
    await page.locator("#identity-options .identity-button").filter({ hasText: "分公司" }).click();
    heldGroupListResponse.writeHead(200, { "Content-Type": "application/json" });
    heldGroupListResponse.end(JSON.stringify({ groups: [...created.values()].map((group) => ({
      id: group.id, type: "group", name: group.name, status: "active", role: "owner",
      source_membership_id: primary, last_seq: 0, updated_at: "2026-10-01T10:00:00Z",
    })), has_more: false }));
    await page.locator("#group-list").getByText("暂无已加入的群聊").waitFor({ timeout: 1500 });
    assert(!((await page.locator("#notice").textContent()) || "").includes("群已创建"),
      "old identity group create notice leaked after membership switch");
    await page.locator("#identity-options .identity-button").filter({ hasText: "集团总部" }).click();

    await page.locator("#group-create-open").click();
    await page.locator("#group-create-name").fill("隔离群");
    await page.locator("#group-create-query").fill("张三");
    await page.locator("#group-create-search").click();
    await page.locator("#group-create-results .person-button").first().waitFor({ timeout: 1500 });
    await page.locator("#group-create-results .person-button").first().click();
    denyNextCreate = true;
    await page.locator("#group-create-submit").click();
    await page.getByText("目标不可用或无权限").waitFor({ timeout: 1500 });
    await page.waitForFunction(() => !document.getElementById("group-create-name").disabled);
    assert(await page.locator("#group-create-dialog").isVisible() &&
      await page.locator("#group-create-name").isEnabled() &&
      !await page.locator("#group-create-discard").isVisible() && created.size === 5,
      "definite policy denial retained pending state or created a partial group");
    const allowedName = "😀".repeat(120);
    await page.locator("#group-create-name").fill(allowedName);
    assert(await page.locator("#group-create-name").inputValue() === allowedName,
      "group name field truncated 120 Unicode characters because it counted UTF-16 units");
    await page.locator("#group-create-name").fill("隔离群");
    await page.locator("#group-create-selected button").first().click();
    await page.locator("#group-create-query").fill("批量");
    await page.locator("#group-create-search").click();
    await page.locator("#group-create-results .person-button").first().waitFor({ timeout: 1500 });
    for (let index = 0; index < 20; index++) {
      await page.locator("#group-create-results .person-button:enabled").first().click();
    }
    assert(await page.locator("#group-create-selected .person-card").count() === 20,
      "group create did not allow the API maximum of 20 initial members");
    await page.locator("#group-create-query").fill("额外");
    await page.locator("#group-create-search").click();
    await page.locator("#group-create-results").getByText("额外成员").waitFor({ timeout: 1500 });
    assert(await page.locator("#group-create-results .person-button").first().isDisabled(),
      "group create allowed more than 20 initial members");

    const searchHeld = new Promise((resolve) => { resolveHeldSearch = resolve; });
    holdNextSearch = true;
    await page.locator("#group-create-query").fill("张三");
    await page.locator("#group-create-search").click();
    await Promise.race([searchHeld, new Promise((_, reject) => setTimeout(() =>
      reject(new Error("stale directory search was not held")), 1500))]);
    await page.locator("#group-create-query").fill("李四");
    await page.locator("#group-create-search").click();
    await page.locator("#group-create-results").getByText("李四").waitFor({ timeout: 1500 });
    heldSearchResponse.writeHead(200, { "Content-Type": "application/json" });
    heldSearchResponse.end(JSON.stringify({ people: peopleByQuery["张三"], has_more: false }));
    await page.waitForTimeout(50);
    assert(!await page.locator("#group-create-results").getByText("张三").count(),
      "old directory search replaced newer member choices");

    forbidNextSearch = true;
    await page.locator("#group-create-query").fill("张三");
    await page.locator("#group-create-search").click();
    await page.locator("#group-create-dialog").waitFor({ state: "hidden", timeout: 1500 });
    assert(await page.locator("#identity-options .identity-button.active").count() === 0,
      "invalid acting membership left group create dialog open");
    process.stdout.write("group create selection, idempotent retry, visibility and policy denial passed\n");
  } catch (error) {
    console.error(error);
    process.exitCode = 1;
  } finally {
    if (browser) await browser.close();
    server.close();
  }
});
