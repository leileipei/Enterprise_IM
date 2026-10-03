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
const adminGroup = "00000000-0000-4000-8000-000000000012";
const memberGroup = "00000000-0000-4000-8000-000000000013";
const transferGroup = "00000000-0000-4000-8000-000000000014";
const intervalA = "00000000-0000-4000-8000-000000000021";
const intervalB = "00000000-0000-4000-8000-000000000022";
const intervalC = "00000000-0000-4000-8000-000000000023";
const group = (id, name, status, role) => ({ id, type: "group", name, status, role,
  source_membership_id: actor, last_seq: 1, updated_at: "2026-10-02T10:00:00Z" });
let port;
let rosterCalls = 0;
let removalCalls = [];
let failNextRemoval = false;
let incompleteNextRemoval = false;
let deniedNextRemoval = false;
let unknownNextRemoval = false;
const removed = new Set();
let transferRole = "owner";
let transferStored = null;
let transferNextOutcome = "";
const transferCalls = [];
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
  if (["/web/", "/web/app.js", "/web/retention.js", "/web/legal-holds.js", "/web/retention-policy.js", "/web/retention-history.js", "/web/audit.js", "/web/message-search.js", "/web/style.css"].includes(url.pathname)) {
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
    tenant_id: tenant,
    user_id: user, display_name: "测试用户",
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
      group(transferGroup, "转让测试群", "policy_blocked", transferRole),
    ], has_more: false,
  });
  const match = url.pathname.match(/^\/api\/v1\/groups\/([^/]+)\/(members|messages|removals|membership|owner-transfers)$/);
  if (match?.[2] === "messages" && req.method === "GET") return send(200, {
    conversation_id: match[1], messages: [], next_after_seq: 0, has_more: false,
  });
  if (match?.[2] === "members" && req.method === "GET") {
    if (req.headers["x-acting-membership-id"] !== actor ||
        (![ownerGroup, adminGroup].includes(match[1]) &&
          !(match[1] === transferGroup && transferRole === "owner")))
      return send(403, { error_code: "group_permission_denied" });
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
      return send(200, { members: removed.has(`${match[1]}:${intervalC}`) ? [] :
        [{ interval_id: intervalC, display_name: "第三位",
          role: "member", organization_name: "分公司" }], has_more: false });
    }
    if (match[1] === adminGroup) return send(200, { members: [
      { interval_id: intervalA, display_name: "群主", role: "owner", organization_name: "集团总部" },
      { interval_id: intervalB, display_name: "第二位", role: "admin", organization_name: "公司 B" },
      { interval_id: intervalC, display_name: "第三位", role: "member", organization_name: "分公司" },
    ].filter((member) => !removed.has(`${match[1]}:${member.interval_id}`)), has_more: false });
    if (match[1] === transferGroup) return send(200, { members: [
      { interval_id: intervalA, display_name: "群主", role: "owner", organization_name: "集团总部" },
      { interval_id: intervalB, display_name: "接任者", role: "admin", organization_name: "公司 B" },
    ], has_more: false });
    return send(200, { members: [
      { interval_id: intervalA, display_name: "<img src=x onerror=alert(1)>成员",
        role: "owner", organization_name: "集团总部" },
      { interval_id: intervalB, display_name: "第二位", role: "admin", organization_name: "公司 B" },
    ].filter((member) => !removed.has(`${match[1]}:${member.interval_id}`)), has_more: true, next_cursor: "next" });
  }
  if (match?.[2] === "removals" && req.method === "POST") {
    let body = "";
    req.on("data", (part) => { body += part; });
    req.on("end", () => {
      removalCalls.push({ groupID: match[1], actor: req.headers["x-acting-membership-id"], body: JSON.parse(body) });
      if (failNextRemoval) {
        failNextRemoval = false;
        return send(503, { error_code: "unavailable" });
      }
      if (deniedNextRemoval) {
        deniedNextRemoval = false;
        return send(403, { error_code: "group_permission_denied" });
      }
      if (unknownNextRemoval) {
        unknownNextRemoval = false;
        return send(403, "<html>proxy rejected</html>", "text/html");
      }
      if (incompleteNextRemoval) {
        incompleteNextRemoval = false;
        return send(200, { interval_id: JSON.parse(body).interval_id, status: "removed" });
      }
      removed.add(`${match[1]}:${JSON.parse(body).interval_id}`);
      return send(200, { interval_id: JSON.parse(body).interval_id, status: "removed", leave_seq: 2 });
    });
    return;
  }
  if (match?.[2] === "membership" && req.method === "GET" && match[1] === transferGroup)
    return send(200, { interval_id: intervalA, role: transferRole, join_seq: 1,
      group_status: "policy_blocked" });
  if (match?.[2] === "owner-transfers" && req.method === "POST" && match[1] === transferGroup) {
    let raw = "";
    req.on("data", (part) => { raw += part; });
    req.on("end", () => {
      const body = JSON.parse(raw);
      transferCalls.push({ actor: req.headers["x-acting-membership-id"], body });
      const outcome = transferNextOutcome;
      transferNextOutcome = "";
      if (outcome === "denied") return send(403, { error_code: "group_permission_denied" });
      if (outcome === "proxy") return send(403, "<html>proxy rejected</html>", "text/html");
      if (outcome === "incomplete") return send(200, { source_interval_id: intervalA });
      if (transferStored) {
        if (JSON.stringify(body) !== JSON.stringify(transferStored))
          return send(409, { error_code: "idempotency_conflict" });
        return send(200, { source_interval_id: intervalA, target_interval_id: intervalB });
      }
      if (transferRole !== "owner") return send(403, { error_code: "group_permission_denied" });
      transferStored = body;
      transferRole = "member";
      return send(503, { error_code: "unavailable" });
    });
    return;
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
    await dialog.getByText("第三位", { exact: true }).waitFor();
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
    await dialog.getByText("第二位", { exact: true }).waitFor();
    if (rosterCalls !== priorCalls + 1) throw new Error("reopened roster reused stale data");
    await dialog.getByRole("button", { name: "关闭" }).click();

    await open.click();
    await dialog.getByText("第二位", { exact: true }).waitFor();
    if (await dialog.getByRole("button", { name: "移除群主" }).count() ||
        !await dialog.getByRole("button", { name: "移除第二位" }).count())
      throw new Error("owner removal actions do not match target roles");
    await dialog.getByRole("button", { name: "移除第二位" }).click();
    const removeDialog = page.locator("#group-remove-dialog");
    await removeDialog.getByText("第二位").waitFor();
    if (!await removeDialog.getByText("公司 B").count()) throw new Error("confirmation omitted target organization");
    await removeDialog.getByRole("button", { name: "稍后处理" }).click();
    if (removalCalls.length) throw new Error("cancel submitted a removal");
    await open.click();
    await dialog.getByRole("button", { name: "移除第二位" }).click();
    failNextRemoval = true;
    await removeDialog.getByRole("button", { name: "确认移除" }).click();
    await removeDialog.getByText("移除结果未确认").waitFor();
    if (removalCalls.length !== 1 || removalCalls[0].body.interval_id !== intervalB)
      throw new Error("removal did not submit the selected interval");
    const saved = await page.evaluate(() => Object.entries(localStorage).filter(([key]) => key.includes("group-remove")));
    if (saved.length !== 1 || saved[0][1].includes("第二位") || saved[0][1].includes("公司 B") ||
        saved[0][1].includes("群主群")) throw new Error("pending removal persisted roster PII");
    await removeDialog.getByRole("button", { name: "稍后处理" }).click();
    await page.evaluate((membershipID) => selectMembership(membershipID), secondActor);
    if (await page.locator("#pending-group-removals button").count())
      throw new Error("pending removal crossed acting identity");
    await page.evaluate((membershipID) => selectMembership(membershipID), actor);
    await page.locator("#pending-group-removals button").waitFor();
    await page.reload();
    await page.getByRole("button", { name: /使用企业账号登录/ }).click();
    await page.getByRole("button", { name: /集团总部/ }).first().click();
    await page.locator("#pending-group-removals button").waitFor();
    await page.locator("#group-list .group-card").filter({ hasText: "群主群" }).click();
    await page.locator("#pending-group-removals button").click();
    await removeDialog.getByRole("button", { name: "重试移除" }).click();
    await page.locator("#pending-group-removals button").waitFor({ state: "detached" });
    if (removalCalls.length !== 2 || removalCalls[1].body.interval_id !== intervalB ||
        removalCalls[1].groupID !== ownerGroup || removalCalls[1].actor !== actor)
      throw new Error("recovered removal did not replay the original interval and identity");
    await dialog.getByText("<img src=x onerror=alert(1)>成员", { exact: true }).waitFor();
    if (await dialog.getByText("第二位", { exact: true }).count())
      throw new Error("confirmed removal remained in refreshed roster");
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
    await dialog.getByText("第二位", { exact: true }).waitFor();
    if (await dialog.getByRole("button", { name: "移除第二位" }).count() ||
        !await dialog.getByRole("button", { name: "移除第三位" }).count() ||
        await dialog.getByRole("button", { name: /转让给/ }).count())
      throw new Error("administrator removal actions do not match target roles");
    await dialog.getByRole("button", { name: "移除第三位" }).click();
    await removeDialog.getByRole("button", { name: "确认移除" }).click();
    await page.locator("#pending-group-removals button").waitFor({ state: "detached" });
    if (removalCalls.length !== 3 || removalCalls[2].groupID !== adminGroup ||
        removalCalls[2].body.interval_id !== intervalC)
      throw new Error("policy blocked group administrator could not remove member");
    await dialog.getByText("第二位", { exact: true }).waitFor();
    if (await dialog.getByText("第三位", { exact: true }).count())
      throw new Error("policy blocked group roster retained removed member");
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
    await dialog.getByText("<img src=x onerror=alert(1)>成员", { exact: true }).waitFor();
    await dialog.getByRole("button", { name: "加载更多成员" }).click();
    await dialog.getByText("第三位", { exact: true }).waitFor();
    await dialog.getByRole("button", { name: "移除第三位" }).click();
    deniedNextRemoval = true;
    await removeDialog.getByRole("button", { name: "确认移除" }).click();
    await removeDialog.waitFor({ state: "hidden" });
    if (await page.locator("#pending-group-removals button").count())
      throw new Error("definitive first-attempt denial retained pending removal");
    await open.click();
    await dialog.getByRole("button", { name: "加载更多成员" }).click();
    await dialog.getByText("第三位", { exact: true }).waitFor();
    await dialog.getByRole("button", { name: "移除第三位" }).click();
    unknownNextRemoval = true;
    await removeDialog.getByRole("button", { name: "确认移除" }).click();
    await removeDialog.getByText("移除结果未确认").waitFor();
    if (!await page.locator("#pending-group-removals button").count())
      throw new Error("unknown proxy rejection cleared pending removal");
    deniedNextRemoval = true;
    await removeDialog.getByRole("button", { name: "重试移除" }).click();
    await removeDialog.getByText("此前请求的结果仍未确认").waitFor();
    if (!await page.locator("#pending-group-removals button").count())
      throw new Error("later denial cleared uncertain prior attempt");
    await removeDialog.getByRole("button", { name: "放弃待确认" }).click();
    if (await page.locator("#pending-group-removals button").count())
      throw new Error("discard retained pending removal");
    await open.click();
    await dialog.getByRole("button", { name: "加载更多成员" }).click();
    await dialog.getByText("第三位", { exact: true }).waitFor();
    await dialog.getByRole("button", { name: "移除第三位" }).click();
    incompleteNextRemoval = true;
    await removeDialog.getByRole("button", { name: "确认移除" }).click();
    await removeDialog.getByText("移除结果未确认").waitFor();
    if (!await page.locator("#pending-group-removals button").count())
      throw new Error("incomplete success response cleared pending removal");
    await removeDialog.getByRole("button", { name: "重试移除" }).click();
    await page.locator("#pending-group-removals button").waitFor({ state: "detached" });
    await dialog.getByText("<img src=x onerror=alert(1)>成员", { exact: true }).waitFor();
    await dialog.getByRole("button", { name: "加载更多成员" }).click();
    if (await dialog.getByText("第三位", { exact: true }).count())
      throw new Error("retried removal remained in roster");
    await dialog.getByRole("button", { name: "关闭" }).click();

    await page.locator("#group-list .group-card").filter({ hasText: "转让测试群" }).click();
    await open.click();
    await dialog.getByText("接任者", { exact: true }).waitFor();
    if (!await dialog.getByRole("button", { name: "转让给接任者" }).count())
      throw new Error("owner cannot select a successor from roster");
    await dialog.getByRole("button", { name: "转让给接任者" }).click();
    const transferDialog = page.locator("#group-transfer-dialog");
    await transferDialog.getByText("接任者").waitFor();
    if (!await transferDialog.getByText("公司 B").count())
      throw new Error("transfer confirmation omitted target organization");
    await transferDialog.getByRole("button", { name: "稍后处理" }).click();
    if (transferCalls.length) throw new Error("cancel submitted owner transfer");
    await open.click();
    await dialog.getByText("接任者", { exact: true }).waitFor();
    const crossTabRemovalKey = await page.evaluate(({ tenantID, userID, actorID, groupID, intervalID }) => {
      const key = `enterprise-im-group-remove:${tenantID}:${userID}:${actorID}:${groupID}:${intervalID}`;
      localStorage.setItem(key, JSON.stringify({ tenantID, userID, actor: actorID, groupID, intervalID }));
      return key;
    }, { tenantID: tenant, userID: user, actorID: actor, groupID: transferGroup, intervalID: intervalB });
    await dialog.getByRole("button", { name: "转让给接任者" }).click();
    await page.getByText("此群有待确认成员移除").waitFor();
    if (await transferDialog.isVisible() || transferCalls.length)
      throw new Error("stale roster action bypassed pending removal guard");
    await page.evaluate((key) => window.dispatchEvent(new StorageEvent("storage", { key })), crossTabRemovalKey);
    if (await dialog.isVisible()) throw new Error("cross-tab pending change kept stale roster actions");
    await page.evaluate((key) => {
      localStorage.removeItem(key);
      window.dispatchEvent(new StorageEvent("storage", { key }));
    }, crossTabRemovalKey);
    await open.click();
    await dialog.getByRole("button", { name: "转让给接任者" }).click();
    transferNextOutcome = "denied";
    await transferDialog.getByRole("button", { name: "确认转让" }).click();
    await transferDialog.waitFor({ state: "hidden" });
    if (await page.locator("#pending-group-transfers button").count())
      throw new Error("first definitive transfer denial retained pending request");
    await open.click();
    await dialog.getByRole("button", { name: "转让给接任者" }).click();
    transferNextOutcome = "proxy";
    await transferDialog.getByRole("button", { name: "确认转让" }).click();
    await transferDialog.getByText("转让结果未确认").waitFor();
    if (!await page.locator("#pending-group-transfers button").count())
      throw new Error("unknown proxy response cleared transfer request");
    const unknownTransfer = transferCalls.at(-1).body;
    transferNextOutcome = "denied";
    await transferDialog.getByRole("button", { name: "重试转让" }).click();
    await transferDialog.getByText("此前转让结果仍未确认").waitFor();
    if (!await page.locator("#pending-group-transfers button").count() ||
        JSON.stringify(transferCalls.at(-1).body) !== JSON.stringify(unknownTransfer))
      throw new Error("later denial cleared or changed uncertain transfer request");
    await transferDialog.getByRole("button", { name: "放弃待确认" }).click();
    if (await page.locator("#pending-group-transfers button").count())
      throw new Error("discard retained uncertain transfer request");
    await open.click();
    await dialog.getByRole("button", { name: "转让给接任者" }).click();
    transferNextOutcome = "incomplete";
    await transferDialog.getByRole("button", { name: "确认转让" }).click();
    await transferDialog.getByText("转让结果未确认").waitFor();
    if (!await page.locator("#pending-group-transfers button").count())
      throw new Error("incomplete transfer response cleared pending request");
    await transferDialog.getByRole("button", { name: "放弃待确认" }).click();
    if (await page.locator("#pending-group-transfers button").count())
      throw new Error("discard retained incomplete transfer request");
    const firstCommittedTransfer = transferCalls.length;
    await open.click();
    await dialog.getByRole("button", { name: "转让给接任者" }).click();
    await transferDialog.getByRole("button", { name: "确认转让" }).click();
    await transferDialog.getByText("转让结果未确认").waitFor();
    if (transferCalls.length !== firstCommittedTransfer + 1 ||
        transferCalls[firstCommittedTransfer].actor !== actor ||
        transferCalls[firstCommittedTransfer].body.source_interval_id !== intervalA ||
        transferCalls[firstCommittedTransfer].body.target_interval_id !== intervalB ||
        !/^[0-9a-f]{8}-[0-9a-f]{4}-7/i.test(transferCalls[firstCommittedTransfer].body.client_request_id))
      throw new Error("transfer did not submit exact source, target and request ID");
    const savedTransfer = await page.evaluate(() => Object.entries(localStorage).filter(([key]) => key.includes("group-transfer")));
    if (savedTransfer.length !== 1 || savedTransfer[0][1].includes("接任者") ||
        savedTransfer[0][1].includes("公司 B") || savedTransfer[0][1].includes("转让测试群"))
      throw new Error("pending owner transfer persisted roster PII");
    await transferDialog.getByRole("button", { name: "稍后处理" }).click();
    await page.evaluate((membershipID) => selectMembership(membershipID), secondActor);
    if (await page.locator("#pending-group-transfers button").count())
      throw new Error("pending owner transfer crossed acting identity");
    await page.evaluate((membershipID) => selectMembership(membershipID), actor);
    await page.locator("#pending-group-transfers button").waitFor();
    await page.reload();
    await page.getByRole("button", { name: /使用企业账号登录/ }).click();
    await page.getByRole("button", { name: /集团总部/ }).first().click();
    await page.locator("#pending-group-transfers button").waitFor();
    await page.locator("#group-list .group-card").filter({ hasText: "转让测试群" }).click();
    if (await open.isVisible()) throw new Error("former owner retained roster action");
    await page.locator("#pending-group-transfers button").click();
    await transferDialog.getByRole("button", { name: "重试转让" }).click();
    await page.locator("#pending-group-transfers button").waitFor({ state: "detached" });
    if (transferCalls.length !== firstCommittedTransfer + 2 ||
        JSON.stringify(transferCalls[firstCommittedTransfer + 1].body) !==
        JSON.stringify(transferCalls[firstCommittedTransfer].body))
      throw new Error("transfer recovery changed request ID or interval IDs");
    const leaveAction = page.locator("#group-leave-open");
    await leaveAction.getByText("退出群聊").waitFor();
    if (await leaveAction.isDisabled()) throw new Error("former owner cannot leave after transfer");
    await page.evaluate(() => logout());
    if (await dialog.isVisible() || await dialog.locator(".group-roster-member").count())
      throw new Error("logout retained roster data");

    process.stdout.write("group roster, removal and owner transfer access, recovery and isolation passed\n");
  } catch (error) {
    console.error(error);
    process.exitCode = 1;
  } finally {
    if (browser) await browser.close();
    server.close();
  }
});
