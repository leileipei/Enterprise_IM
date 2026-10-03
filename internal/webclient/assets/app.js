"use strict";

const element = (id) => document.getElementById(id);
const loginView = element("login-view");
const workspace = element("workspace");
const loginButton = element("login-button");
const loginHint = element("login-hint");
const noticeBox = element("notice");
const identityOptions = element("identity-options");
const results = element("people-results");
const conversationList = element("conversation-list");
const loadMoreConversationsButton = element("load-more-conversations");
const groupList = element("group-list");
const loadMoreGroupsButton = element("load-more-groups");
const groupCreateOpenButton = element("group-create-open");
const groupCreateDialog = element("group-create-dialog");
const groupCreateName = element("group-create-name");
const groupCreateQuery = element("group-create-query");
const groupCreateResults = element("group-create-results");
const groupCreateSelected = element("group-create-selected");
const groupCreateSubmit = element("group-create-submit");
const groupCreateDiscard = element("group-create-discard");
const groupInviteOpenButton = element("group-invite-open");
const groupInviteDialog = element("group-invite-dialog");
const groupInviteQuery = element("group-invite-query");
const groupInviteResults = element("group-invite-results");
const groupInviteSelected = element("group-invite-selected");
const groupInviteSubmit = element("group-invite-submit");
const groupInviteDiscard = element("group-invite-discard");
const pendingGroupInvites = element("pending-group-invites");
const pendingGroupLeaves = element("pending-group-leaves");
const groupLeaveOpenButton = element("group-leave-open");
const groupLeaveDialog = element("group-leave-dialog");
const groupLeaveConfirm = element("group-leave-confirm");
const groupLeaveRetry = element("group-leave-retry");
const groupLeaveDiscard = element("group-leave-discard");
const groupRosterOpenButton = element("group-roster-open");
const groupPolicyRecheckButton = element("group-policy-recheck");
const groupPolicyRecheckHint = element("group-policy-recheck-hint");
const groupRosterDialog = element("group-roster-dialog");
const groupRosterMembers = element("group-roster-members");
const groupRosterHint = element("group-roster-hint");
const groupRosterLoadMoreButton = element("group-roster-load-more");
const groupRosterRetryButton = element("group-roster-retry");
const pendingGroupRemovals = element("pending-group-removals");
const groupRemoveDialog = element("group-remove-dialog");
const groupRemoveConfirm = element("group-remove-confirm");
const groupRemoveRetry = element("group-remove-retry");
const groupRemoveDiscard = element("group-remove-discard");
const pendingGroupTransfers = element("pending-group-transfers");
const groupTransferDialog = element("group-transfer-dialog");
const groupTransferConfirm = element("group-transfer-confirm");
const groupTransferRetry = element("group-transfer-retry");
const groupTransferDiscard = element("group-transfer-discard");
const messages = element("messages");
const messageText = element("message-text");
const sendButton = element("send-button");
const discardPendingButton = element("discard-pending");
const connectionState = element("connection-state");

let config = null;
let accessToken = "";
let tokenExpiresAt = 0;
let self = null;
let actingMembership = "";
let identityEpoch = 0;
let activeConversation = null;
let activeConversationKind = "";
let conversationEpoch = 0;
let afterSeq = 0;
let syncPromise = null;
let syncAgain = false;
let syncRetryTimer = null;
let syncRetryDelay = 1000;
let pendingMessage = null;
let realtimeSocket = null;
let realtimeEpoch = 0;
let ticketInFlight = false;
let reconnectTimer = null;
let pollingTimer = null;
let nextSafetySyncAt = 0;
let nextGroupSyncAt = 0;
let realtimeUnsupported = false;
let noticeTimer = null;
const conversations = new Map();
let inboxCursor = "";
let inboxHasMore = false;
let inboxGeneration = 0;
let inboxRefreshPromise = null;
let inboxRefreshAgain = false;
let inboxPagePromise = null;
let openChatSerial = 0;
let openingChatSerial = 0;
const groups = new Map();
let groupCursor = "";
let groupHasMore = false;
let groupVisiblePages = 1;
let groupGeneration = 0;
let groupRefreshPromise = null;
let groupRefreshAgain = false;
let groupRefreshAfterPage = false;
let groupPagePromise = null;
const groupCreateMembers = new Map();
let groupCreateSearchResults = [];
let groupCreateSearchSerial = 0;
let pendingGroupCreate = null;
let groupInviteGroupID = "";
let groupInviteTarget = null;
let groupInviteSearchResults = [];
let groupInviteSearchSerial = 0;
let pendingGroupInvite = null;
let pendingGroupLeave = null;
let preparedGroupLeave = null;
let groupLeaveLookupSerial = 0;
let groupListNeedsRefreshID = "";
let groupRosterGroupID = "";
let groupRosterRole = "";
let groupRosterCursor = "";
let groupRosterHasMore = false;
let groupRosterLoading = false;
let groupRosterError = false;
let groupRosterGeneration = 0;
let preparedGroupRemoval = null;
let pendingGroupRemoval = null;
let preparedGroupTransfer = null;
let pendingGroupTransfer = null;
const groupPolicyRechecks = new Map();
let groupPolicyRecheckNotice = null;
const retentionContext = () => ({
  identityKey: accessToken && actingMembership ? `${identityEpoch}:${actingMembership}` : "",
  conversation: activeConversation,
  conversationEpoch,
  title: activeConversation ? element("chat-title").textContent : "",
});
const retentionRecords = new window.RetentionRecords(request, retentionContext);
const legalHoldRecords = new window.LegalHoldRecords(request, retentionContext);
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

function groupCreateStoragePrefix(actor = actingMembership) {
  return `enterprise-im-group-create:${self.tenant_id}:${self.user_id}:${actor}:`;
}

function groupCreateStorageKey(id, actor = actingMembership) {
  return groupCreateStoragePrefix(actor) + id;
}

function forgetGroupCreate(id, actor = actingMembership) {
  try { localStorage.removeItem(groupCreateStorageKey(id, actor)); } catch (_) { /* Storage may be unavailable. */ }
}

function restoreGroupCreate() {
  let saved = null;
  try {
    const prefix = groupCreateStoragePrefix();
    const keys = [];
    for (let index = 0; index < localStorage.length; index++) {
      const key = localStorage.key(index);
      if (key?.startsWith(prefix)) keys.push(key);
    }
    keys.sort();
    for (const key of keys) {
      try {
        const candidate = JSON.parse(localStorage.getItem(key) || "null");
        if (candidate?.id === key.slice(prefix.length)) { saved = candidate; break; }
      } catch (_) { /* Ignore an unreadable saved request. */ }
    }
  } catch (_) { /* Storage may be unavailable. */ }
  if (!saved || saved.actor !== actingMembership ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(saved.id) ||
      typeof saved.name !== "string" || !saved.name.trim() || [...saved.name].length > 120 ||
      !Array.isArray(saved.memberIDs) || saved.memberIDs.length < 1 || saved.memberIDs.length > 20 ||
      !saved.memberIDs.every((id) => typeof id === "string")) return;
  pendingGroupCreate = { id: saved.id, name: saved.name, memberIDs: saved.memberIDs,
    actor: saved.actor, sending: false };
  groupCreateName.value = saved.name;
  saved.memberIDs.forEach((membershipID, index) => {
    const member = Array.isArray(saved.members) ? saved.members[index] : null;
    groupCreateMembers.set(member?.userID || membershipID, {
      userID: member?.userID || membershipID, membershipID,
      name: typeof member?.name === "string" ? member.name : `成员 ${index + 1}`,
      organization: typeof member?.organization === "string" ? member.organization : "已保存任职",
    });
  });
  element("group-create-hint").textContent = "已恢复待确认建群请求；可用同一编号重试，或放弃后核对群列表。";
}

function notify(message) {
  noticeBox.textContent = message;
  noticeBox.classList.remove("hidden");
  clearTimeout(noticeTimer);
  noticeTimer = setTimeout(() => noticeBox.classList.add("hidden"), 4500);
}

function report(error) {
  if (!error.stale) notify(error.message);
}

function scheduleSafetySync() {
  nextSafetySyncAt = Date.now() + 30000 + Math.floor(Math.random() * 10000);
}

function scheduleGroupSync() {
  nextGroupSyncAt = Date.now() + 30000 + Math.floor(Math.random() * 10000);
}

function randomURLSafe(bytes = 32) {
  const value = crypto.getRandomValues(new Uint8Array(bytes));
  return btoa(String.fromCharCode(...value)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

async function challenge(verifier) {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier));
  return btoa(String.fromCharCode(...new Uint8Array(digest))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

function uuidV7() {
  const value = crypto.getRandomValues(new Uint8Array(16));
  let timestamp = BigInt(Date.now());
  for (let i = 5; i >= 0; i--) {
    value[i] = Number(timestamp & 255n);
    timestamp >>= 8n;
  }
  value[6] = (value[6] & 15) | 0x70;
  value[8] = (value[8] & 63) | 0x80;
  const hex = [...value].map((item) => item.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

async function request(path, options = {}, needsMembership = true) {
  if (!accessToken || Date.now() >= tokenExpiresAt) {
    logout("登录已过期，请重新登录。");
    throw new Error("登录已过期");
  }
  if (needsMembership && !actingMembership) throw new Error("请先选择任职");
  const headers = new Headers(options.headers || {});
  headers.set("Authorization", `Bearer ${accessToken}`);
  if (needsMembership) headers.set("X-Acting-Membership-ID", actingMembership);
  const requestToken = accessToken;
  const requestMembership = actingMembership;
  const requestEpoch = identityEpoch;
  const response = await fetch(path, { ...options, headers, cache: "no-store" });
  const checkIdentity = () => {
    if (requestToken === accessToken && requestEpoch === identityEpoch &&
        (!needsMembership || requestMembership === actingMembership)) return;
    const error = new Error("已切换身份");
    error.stale = true;
    throw error;
  };
  checkIdentity();
  if (response.status === 401) {
    logout("登录已过期，请重新登录。");
    throw new Error("登录已过期");
  }
  if (!response.ok) {
    let errorCode = "";
    try {
      const body = await response.json();
      if (typeof body.error_code === "string") errorCode = body.error_code;
    } catch (_) { /* Error responses can be empty or non-JSON at the proxy. */ }
    checkIdentity();
    if (response.status === 403 && needsMembership && errorCode === "invalid_identity") {
      selectMembership("");
      throw new Error("当前任职已失效，请重新选择");
    }
    const error = new Error(errorCode === "group_policy_blocked" ?
      (path.endsWith("/policy-rechecks") ? "复核未通过，群通信继续暂停。" :
        path.endsWith("/invitations") ? "群通信已暂停，无法邀请成员。" : "群通信已暂停，新消息未保存。") :
      errorCode === "group_permission_denied" ?
        (path.endsWith("/policy-rechecks") ? "当前账号无权复核该群策略。" :
          path.endsWith("/removals") ? "当前账号无权移除该群成员。" :
          path.endsWith("/owner-transfers") ? "当前账号无权转让此群群主。" :
          path.includes("/members") ? "当前账号无权查看群成员。" : "当前账号无权邀请群成员。") :
      errorCode === "retry_window_expired" ? "重试期限已过，请核对历史消息后重新发送。" :
        response.status === 404 ? "目标不可用或无权限" :
        response.status === 409 ? "当前会话状态已变化，请核对后重试" :
          response.status === 403 ? "当前操作无权限" :
            response.status === 429 ? "操作太频繁，请稍后再试" : "服务暂时不可用，请稍后重试");
    error.status = response.status;
    error.code = errorCode;
    throw error;
  }
  const data = await response.json();
  checkIdentity();
  return data;
}

async function startLogin() {
  if (!config) return;
  try {
    const verifier = randomURLSafe();
    const state = randomURLSafe();
    sessionStorage.setItem("enterprise-im-login", JSON.stringify({ state, verifier, started: Date.now() }));
    const url = new URL(config.authorization_url);
    url.searchParams.set("response_type", "code");
    url.searchParams.set("client_id", config.client_id);
    url.searchParams.set("redirect_uri", config.redirect_url);
    url.searchParams.set("scope", config.scope);
    url.searchParams.set("state", state);
    url.searchParams.set("code_challenge", await challenge(verifier));
    url.searchParams.set("code_challenge_method", "S256");
    window.location.assign(url.href);
  } catch (_) {
    loginHint.textContent = "无法启动企业登录，请检查浏览器安全设置。";
  }
}

async function finishLogin(callbackQuery) {
  const params = new URLSearchParams(callbackQuery);
  const transaction = sessionStorage.getItem("enterprise-im-login");
  sessionStorage.removeItem("enterprise-im-login");
  if (params.has("error")) throw new Error("企业登录未完成，请重试。");
  let saved;
  try { saved = JSON.parse(transaction || "null"); } catch (_) { saved = null; }
  if (!saved || params.getAll("code").length !== 1 || params.getAll("state").length !== 1 ||
      params.get("state") !== saved.state || !saved.verifier ||
      Date.now() - saved.started > 10 * 60 * 1000 || Date.now() < saved.started ||
      (window.location.origin !== new URL(config.redirect_url).origin ||
        window.location.pathname !== new URL(config.redirect_url).pathname) ||
      (params.has("iss") && (params.getAll("iss").length !== 1 || params.get("iss") !== config.issuer))) {
    throw new Error("登录状态已失效，请重新登录。");
  }
  const response = await fetch("/web/oauth/token", {
    method: "POST", cache: "no-store", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ code: params.get("code"), code_verifier: saved.verifier }),
  });
  if (!response.ok) throw new Error("身份验证未完成，请联系管理员确认账号已绑定。");
  const data = await response.json();
  accessToken = data.access_token;
  tokenExpiresAt = Date.now() + data.expires_in * 1000;
  self = await request("/api/v1/me", {}, false);
  showWorkspace();
}

function showWorkspace() {
  loginView.classList.add("hidden");
  workspace.classList.remove("hidden");
  element("account-name").textContent = self.display_name;
  element("account-number").textContent = self.global_employee_no;
  element("avatar").textContent = [...self.display_name].slice(0, 2).join("") || "IM";
  renderMemberships();
  if (!self.memberships.length) notify("账号当前没有有效任职，请联系管理员。");
  if (!pollingTimer) pollingTimer = setInterval(() => {
    const connected = realtimeSocket && realtimeSocket.readyState === WebSocket.OPEN;
    if (actingMembership && (!connected || Date.now() >= nextSafetySyncAt)) {
      if (connected) scheduleSafetySync();
      refreshInbox().catch(report);
      if (activeConversation) syncMessages().catch(report);
    }
    if (actingMembership && Date.now() >= nextGroupSyncAt) {
      nextGroupSyncAt = Number.MAX_SAFE_INTEGER;
      refreshGroups().catch(report);
    }
  }, 5000);
}

function logout(message = "已退出当前页面。") {
  closeGroupCreate(true);
  closeGroupInvite(true);
  closeGroupLeave(true);
  closeGroupRemoval(true);
  closeGroupTransfer(true);
  groupListNeedsRefreshID = "";
  groupPolicyRechecks.clear();
  groupPolicyRecheckNotice = null;
  stopRealtime();
  clearSyncRetry();
  identityEpoch++;
  if (pollingTimer) clearInterval(pollingTimer);
  pollingTimer = null;
  nextSafetySyncAt = 0;
  nextGroupSyncAt = 0;
  sessionStorage.removeItem("enterprise-im-login");
  accessToken = "";
  tokenExpiresAt = 0;
  self = null;
  actingMembership = "";
  activeConversation = null;
  activeConversationKind = "";
  conversationEpoch++;
  afterSeq = 0;
  syncPromise = null;
  syncAgain = false;
  pendingMessage = null;
  inboxGeneration++;
  inboxRefreshPromise = null;
  inboxRefreshAgain = false;
  inboxPagePromise = null;
  inboxCursor = "";
  inboxHasMore = false;
  openChatSerial++;
  openingChatSerial = 0;
  conversations.clear();
  resetGroups();
  identityOptions.replaceChildren();
  pendingGroupInvites.replaceChildren();
  pendingGroupInvites.classList.add("hidden");
  pendingGroupLeaves.replaceChildren();
  pendingGroupLeaves.classList.add("hidden");
  pendingGroupRemovals.replaceChildren();
  pendingGroupRemovals.classList.add("hidden");
  pendingGroupTransfers.replaceChildren();
  pendingGroupTransfers.classList.add("hidden");
  results.replaceChildren();
  conversationList.replaceChildren();
  loadMoreConversationsButton.classList.add("hidden");
  resetChat();
  workspace.classList.add("hidden");
  loginView.classList.remove("hidden");
  loginHint.textContent = message;
}

function renderMemberships() {
  groupCreateOpenButton.disabled = !actingMembership;
  identityOptions.replaceChildren();
  for (const membership of self.memberships) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "identity-button" + (membership.id === actingMembership ? " active" : "");
    const name = document.createElement("strong");
    name.textContent = membership.organization_name + (membership.is_primary ? " · 主任职" : "");
    const detail = document.createElement("small");
    detail.textContent = [membership.legal_entity_name, membership.title].filter(Boolean).join(" / ");
    button.append(name, detail);
    button.addEventListener("click", () => {
      if (canSwitchChat()) selectMembership(membership.id);
    });
    identityOptions.append(button);
  }
}

function selectMembership(id) {
  if (id === actingMembership) return;
  closeGroupCreate(true);
  closeGroupInvite(true);
  closeGroupLeave(true);
  closeGroupRemoval(true);
  closeGroupTransfer(true);
  groupListNeedsRefreshID = "";
  groupPolicyRechecks.clear();
  groupPolicyRecheckNotice = null;
  stopRealtime();
  clearSyncRetry();
  identityEpoch++;
  actingMembership = id;
  nextSafetySyncAt = 0;
  nextGroupSyncAt = 0;
  realtimeUnsupported = false;
  activeConversation = null;
  activeConversationKind = "";
  conversationEpoch++;
  afterSeq = 0;
  syncPromise = null;
  syncAgain = false;
  pendingMessage = null;
  inboxGeneration++;
  inboxRefreshPromise = null;
  inboxRefreshAgain = false;
  inboxPagePromise = null;
  inboxCursor = "";
  inboxHasMore = false;
  openChatSerial++;
  openingChatSerial = 0;
  conversations.clear();
  resetGroups();
  conversationList.replaceChildren();
  loadMoreConversationsButton.classList.add("hidden");
  results.replaceChildren();
  element("person-query").value = "";
  element("search-hint").textContent = id ? "输入姓名，查找当前任职下可见的同事。" : "请选择一个有效任职。";
  resetChat();
  renderMemberships();
  renderPendingGroupInvites();
  renderPendingGroupLeaves();
  renderPendingGroupRemovals();
  renderPendingGroupTransfers();
  if (id) {
    scheduleSafetySync();
    refreshInbox().catch(report);
    refreshGroups().catch(report);
    connectRealtime();
  }
}

function resetChat() {
  retentionRecords.contextChanged();
  legalHoldRecords.contextChanged();
  groupPolicyRecheckNotice = null;
  renderGroupPolicyRecheckAction();
  groupRosterOpenButton.classList.add("hidden");
  groupInviteOpenButton.classList.add("hidden");
  groupLeaveOpenButton.classList.add("hidden");
  messages.replaceChildren();
  const empty = document.createElement("div");
  empty.className = "empty-state";
  const title = document.createElement("strong");
  title.textContent = "沟通从这里开始";
  const detail = document.createElement("span");
  detail.textContent = "选择任职，查找同事，再发起单聊。";
  empty.append(title, detail);
  messages.append(empty);
  element("chat-mode").textContent = "DIRECT MESSAGE";
  element("chat-title").textContent = "选择一位同事，开始沟通";
  element("chat-subtitle").textContent = actingMembership ? "搜索可见人员并选择任职" : "选择任职后搜索可见人员";
  messageText.disabled = true;
  messageText.readOnly = false;
  sendButton.disabled = true;
  discardPendingButton.classList.add("hidden");
  messageText.value = "";
  element("send-hint").textContent = "服务端保存成功后显示“已保存”，不代表对方已收到。";
}

async function searchPeople() {
  if (!actingMembership) return notify("请先选择任职。");
  const query = element("person-query").value.trim();
  if ([...query].length < 2 || [...query].length > 100) return notify("请输入 2～100 个字符的姓名片段。");
  element("search-hint").textContent = "正在查找…";
  try {
    const page = await request(`/api/v1/directory/users?q=${encodeURIComponent(query)}&limit=20`);
    results.replaceChildren();
    if (!page.people.length) element("search-hint").textContent = "没有找到当前任职下可见的同事。";
    else element("search-hint").textContent = page.has_more ? "结果较多，请缩小姓名范围。" : `找到 ${page.people.length} 位同事`;
    for (const person of page.people) {
      const card = document.createElement("div");
      card.className = "person-card";
      const name = document.createElement("strong");
      name.textContent = person.display_name;
      const employee = document.createElement("small");
      employee.textContent = person.employee_no;
      card.append(name, employee);
      for (const member of person.memberships) {
        const button = document.createElement("button");
        button.type = "button";
        button.className = "person-button";
        const org = document.createElement("strong");
        org.textContent = member.organization_name;
        const title = document.createElement("small");
        title.textContent = member.title || "发起单聊";
        button.append(org, title);
        button.addEventListener("click", () => openChat(person, member));
        card.append(button);
      }
      results.append(card);
    }
  } catch (error) {
    if (error.stale) return;
    element("search-hint").textContent = error.message;
  }
}

function renderGroupCreate() {
  groupCreateSelected.replaceChildren();
  for (const member of groupCreateMembers.values()) {
    const card = document.createElement("div");
    card.className = "person-card";
    const name = document.createElement("strong");
    name.textContent = member.name;
    const organization = document.createElement("small");
    organization.textContent = member.organization;
    const remove = document.createElement("button");
    remove.className = "text-button";
    remove.type = "button";
    remove.textContent = "移除";
    remove.disabled = !!pendingGroupCreate;
    remove.addEventListener("click", () => {
      groupCreateMembers.delete(member.userID);
      renderGroupCreate();
    });
    card.append(name, organization, remove);
    groupCreateSelected.append(card);
  }
  element("group-create-count").textContent = String(groupCreateMembers.size);
  groupCreateResults.replaceChildren();
  for (const person of groupCreateSearchResults) {
    const card = document.createElement("div");
    card.className = "person-card";
    const name = document.createElement("strong");
    name.textContent = person.display_name;
    card.append(name);
    for (const membership of person.memberships) {
      const button = document.createElement("button");
      button.className = "person-button";
      button.type = "button";
      button.textContent = membership.organization_name +
        (membership.title ? " · " + membership.title : "");
      button.disabled = !!pendingGroupCreate || person.id === self.user_id ||
        groupCreateMembers.has(person.id) || groupCreateMembers.size >= 20;
      button.addEventListener("click", () => {
        if (pendingGroupCreate || groupCreateMembers.has(person.id) ||
            groupCreateMembers.size >= 20 || person.id === self.user_id) return;
        groupCreateMembers.set(person.id, {
          userID: person.id, membershipID: membership.membership_id,
          name: person.display_name, organization: membership.organization_name,
        });
        renderGroupCreate();
      });
      card.append(button);
    }
    groupCreateResults.append(card);
  }
  const locked = !!pendingGroupCreate;
  groupCreateName.disabled = locked;
  groupCreateQuery.disabled = locked;
  element("group-create-search").disabled = locked;
  element("group-create-cancel").disabled = locked;
  groupCreateDiscard.classList.toggle("hidden", !locked || !!pendingGroupCreate.sending);
  groupCreateSubmit.disabled = locked ? pendingGroupCreate.sending :
    !groupCreateName.value.trim() || groupCreateMembers.size < 1;
}

function closeGroupCreate(force = false) {
  if (pendingGroupCreate && !force) {
    notify("建群结果尚未确认，请先重试或放弃待确认请求。");
    return;
  }
  if (groupCreateDialog.open) groupCreateDialog.close();
  groupCreateSearchSerial++;
  groupCreateSearchResults = [];
  groupCreateMembers.clear();
  pendingGroupCreate = null;
  groupCreateName.value = "";
  groupCreateQuery.value = "";
  groupCreateResults.replaceChildren();
  groupCreateSelected.replaceChildren();
  element("group-create-hint").textContent = "";
  element("group-create-search-hint").textContent = "搜索当前任职下可见的同事。";
}

function openGroupCreate() {
  if (!actingMembership || !canSwitchChat()) return;
  if (messageText.value.trim()) return notify("请先发送或清空当前消息草稿，再新建群聊。");
  closeGroupCreate(true);
  restoreGroupCreate();
  groupCreateDialog.showModal();
  renderGroupCreate();
  if (pendingGroupCreate) groupCreateSubmit.focus();
  else groupCreateName.focus();
}

async function searchGroupCreatePeople() {
  if (!groupCreateDialog.open || pendingGroupCreate) return;
  const query = groupCreateQuery.value.trim();
  if ([...query].length < 2 || [...query].length > 100) {
    return notify("请输入 2～100 个字符的姓名片段。");
  }
  const serial = ++groupCreateSearchSerial;
  const selectedEpoch = identityEpoch;
  element("group-create-search-hint").textContent = "正在查找…";
  try {
    const page = await request(`/api/v1/directory/users?q=${encodeURIComponent(query)}&limit=20`);
    if (!groupCreateDialog.open || serial !== groupCreateSearchSerial ||
        selectedEpoch !== identityEpoch) return;
    groupCreateSearchResults = page.people;
    element("group-create-search-hint").textContent = !page.people.length ?
      "没有找到当前任职下可见的同事。" : page.has_more ?
        "结果较多，请缩小姓名范围。" : `找到 ${page.people.length} 位同事`;
    renderGroupCreate();
  } catch (error) {
    if (error.stale || serial !== groupCreateSearchSerial || !groupCreateDialog.open) return;
    element("group-create-search-hint").textContent = error.message;
  }
}

async function submitGroupCreate(event) {
  event.preventDefault();
  if (!groupCreateDialog.open || !actingMembership || pendingGroupCreate?.sending) return;
  if (!pendingGroupCreate) {
    const name = groupCreateName.value.trim();
    if (!name || [...name].length > 120 || /[\u0000-\u001f\u007f-\u009f]/u.test(name)) {
      return notify("请输入 1～120 个字符且不含控制字符的群名。");
    }
    if (groupCreateMembers.size < 1 || groupCreateMembers.size > 20) {
      return notify("请选择 1～20 位初始成员。");
    }
    const created = { id: uuidV7(), name,
      memberIDs: [...groupCreateMembers.values()].map((member) => member.membershipID),
      actor: actingMembership, sending: false };
    try {
      localStorage.setItem(groupCreateStorageKey(created.id), JSON.stringify({ ...created,
        members: [...groupCreateMembers.values()] }));
    } catch (_) {
      return notify("浏览器无法保存待确认建群请求，请检查浏览器存储设置后重试。");
    }
    pendingGroupCreate = created;
  }
  const submitted = pendingGroupCreate;
  const selectedEpoch = identityEpoch;
  const selectedConversationEpoch = conversationEpoch;
  const selectedOpenChatSerial = openChatSerial;
  submitted.sending = true;
  element("group-create-hint").textContent = "正在创建群聊…";
  renderGroupCreate();
  try {
    const group = await request("/api/v1/groups", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ client_request_id: submitted.id, name: submitted.name,
        member_membership_ids: submitted.memberIDs }),
    });
    if (selectedEpoch !== identityEpoch || submitted !== pendingGroupCreate ||
        submitted.actor !== actingMembership) return;
    forgetGroupCreate(submitted.id, submitted.actor);
    closeGroupCreate(true);
    try {
      await refreshGroups();
      if (selectedEpoch !== identityEpoch || submitted.actor !== actingMembership) return;
      if (!groups.has(group.id)) await refreshGroups();
      if (selectedEpoch !== identityEpoch || submitted.actor !== actingMembership ||
          selectedConversationEpoch !== conversationEpoch ||
          selectedOpenChatSerial !== openChatSerial || messageText.value.trim() ||
          groupCreateDialog.open || groupInviteDialog.open) return;
      if (groups.has(group.id)) activateGroupHistory(group.id);
      else notify("群已创建，群列表正在同步，请稍后查看。");
    } catch (error) {
      if (error.stale || selectedEpoch !== identityEpoch ||
          submitted.actor !== actingMembership ||
          selectedConversationEpoch !== conversationEpoch ||
          selectedOpenChatSerial !== openChatSerial || messageText.value.trim() ||
          groupCreateDialog.open || groupInviteDialog.open) return;
      report(error);
      notify("群已创建，群列表暂时无法同步，请稍后刷新。");
    }
  } catch (error) {
    if (error.stale || selectedEpoch !== identityEpoch ||
        submitted !== pendingGroupCreate || submitted.actor !== actingMembership) return;
    if ([400, 404, 409].includes(error.status)) {
      pendingGroupCreate = null;
      forgetGroupCreate(submitted.id, submitted.actor);
      element("group-create-hint").textContent = "建群请求未通过，请核对成员与群名。";
      renderGroupCreate();
    } else {
      submitted.sending = false;
      element("group-create-hint").textContent = "建群结果未确认；可用同一编号重试，或放弃后核对群列表。";
      renderGroupCreate();
    }
    report(error);
  } finally {
    submitted.sending = false;
    if (submitted === pendingGroupCreate) renderGroupCreate();
  }
}

function discardGroupCreate() {
  if (!pendingGroupCreate || pendingGroupCreate.sending) return;
  forgetGroupCreate(pendingGroupCreate.id, pendingGroupCreate.actor);
  closeGroupCreate(true);
  refreshGroups().catch(report);
  notify("已放弃待确认建群请求；群可能已创建，请先核对群列表。");
}

function groupInviteAccountPrefix(actor = actingMembership) {
  return `enterprise-im-group-invite:${self.tenant_id}:${self.user_id}:${actor}:`;
}

function groupInviteStoragePrefix(groupID, actor = actingMembership) {
  return groupInviteAccountPrefix(actor) + groupID + ":";
}

function groupInviteStorageKey(requestID, groupID, actor = actingMembership) {
  return groupInviteStoragePrefix(groupID, actor) + requestID;
}

function forgetGroupInvite(requestID, groupID, actor) {
  try { localStorage.removeItem(groupInviteStorageKey(requestID, groupID, actor)); }
  catch (_) { /* Storage may be unavailable. */ }
}

function savedGroupInvites() {
  if (!self || !actingMembership) return [];
  const found = [];
  try {
    const prefix = groupInviteAccountPrefix();
    const keys = [];
    for (let index = 0; index < localStorage.length; index++) {
      const key = localStorage.key(index);
      if (key?.startsWith(prefix)) keys.push(key);
    }
    keys.sort();
    for (const key of keys) {
      try {
        const suffix = key.slice(prefix.length).split(":");
        const saved = JSON.parse(localStorage.getItem(key) || "null");
        if (suffix.length === 2 && saved?.id === suffix[1] && saved.groupID === suffix[0] &&
            saved.actor === actingMembership &&
            /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(saved.groupID) &&
            /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(saved.id) &&
            /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(saved.targetID)) {
          found.push(saved);
        }
      } catch (_) { /* Ignore an unreadable saved request. */ }
    }
  } catch (_) { /* Storage may be unavailable. */ }
  return found;
}

function savedGroupInvite(groupID) {
  return savedGroupInvites().find((saved) => saved.groupID === groupID) || null;
}

function renderPendingGroupInvites() {
  pendingGroupInvites.replaceChildren();
  for (const saved of savedGroupInvites()) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "secondary-button";
    button.textContent = `待确认邀请 · ${saved.groupName || groups.get(saved.groupID)?.name || saved.groupID}`;
    button.addEventListener("click", () => showGroupInvite(saved.groupID,
      saved.groupName || groups.get(saved.groupID)?.name || saved.groupID, saved));
    pendingGroupInvites.append(button);
  }
  pendingGroupInvites.classList.toggle("hidden", !pendingGroupInvites.childElementCount);
}

function renderGroupInviteAction() {
  const group = activeConversationKind === "group" ? groups.get(activeConversation) : null;
  const canInvite = group && group.status === "active" &&
    ["owner", "admin"].includes(group.role);
  groupInviteOpenButton.classList.toggle("hidden", !actingMembership ||
    !(canInvite || (group && savedGroupInvite(group.id))));
}

function renderGroupInvite() {
  groupInviteSelected.replaceChildren();
  if (groupInviteTarget) {
    const card = document.createElement("div");
    card.className = "person-card";
    const name = document.createElement("strong");
    name.textContent = groupInviteTarget.name;
    const organization = document.createElement("small");
    organization.textContent = groupInviteTarget.organization;
    const remove = document.createElement("button");
    remove.className = "text-button";
    remove.type = "button";
    remove.textContent = "移除";
    remove.disabled = !!pendingGroupInvite;
    remove.addEventListener("click", () => {
      groupInviteTarget = null;
      renderGroupInvite();
    });
    card.append(name, organization, remove);
    groupInviteSelected.append(card);
  }
  groupInviteResults.replaceChildren();
  for (const person of groupInviteSearchResults) {
    const card = document.createElement("div");
    card.className = "person-card";
    const name = document.createElement("strong");
    name.textContent = person.display_name;
    card.append(name);
    for (const membership of person.memberships) {
      const button = document.createElement("button");
      button.className = "person-button";
      button.type = "button";
      button.textContent = membership.organization_name +
        (membership.title ? " · " + membership.title : "");
      button.disabled = !!pendingGroupInvite || person.id === self.user_id ||
        groupInviteTarget?.membershipID === membership.membership_id;
      button.addEventListener("click", () => {
        if (pendingGroupInvite || person.id === self.user_id) return;
        groupInviteTarget = { userID: person.id, membershipID: membership.membership_id,
          name: person.display_name, organization: membership.organization_name };
        renderGroupInvite();
      });
      card.append(button);
    }
    groupInviteResults.append(card);
  }
  const locked = !!pendingGroupInvite;
  groupInviteQuery.disabled = locked;
  element("group-invite-search").disabled = locked;
  element("group-invite-cancel").disabled = locked;
  groupInviteDiscard.classList.toggle("hidden", !locked || !!pendingGroupInvite.sending);
  groupInviteSubmit.disabled = locked ? pendingGroupInvite.sending :
    !groupInviteTarget || groups.get(groupInviteGroupID)?.status === "policy_blocked";
  groupInviteSubmit.textContent = locked ? "重试邀请" : "发送邀请";
}

function closeGroupInvite(force = false) {
  if (pendingGroupInvite && !force) {
    notify("邀请结果尚未确认，请先重试或放弃待确认请求。");
    return;
  }
  if (groupInviteDialog.open) groupInviteDialog.close();
  groupInviteSearchSerial++;
  groupInviteGroupID = "";
  groupInviteTarget = null;
  groupInviteSearchResults = [];
  pendingGroupInvite = null;
  groupInviteQuery.value = "";
  groupInviteResults.replaceChildren();
  groupInviteSelected.replaceChildren();
  element("group-invite-hint").textContent = "";
  element("group-invite-search-hint").textContent = "搜索当前任职下可见的同事。";
}

function openGroupInvite() {
  const group = activeConversationKind === "group" ? groups.get(activeConversation) : null;
  if (!group || !actingMembership) return;
  const saved = savedGroupInvite(group.id);
  if (!saved && (group.status !== "active" || !["owner", "admin"].includes(group.role))) return;
  showGroupInvite(group.id, group.name, saved);
}

function showGroupInvite(groupID, groupName, saved) {
  if (!self || !actingMembership) return;
  closeGroupInvite(true);
  groupInviteGroupID = groupID;
  if (saved) {
    pendingGroupInvite = { id: saved.id, groupID: saved.groupID, actor: saved.actor,
      targetID: saved.targetID, sending: false };
    groupInviteTarget = { membershipID: saved.targetID,
      name: typeof saved.name === "string" ? saved.name : "已保存成员",
      organization: typeof saved.organization === "string" ? saved.organization : "已保存任职" };
    element("group-invite-hint").textContent = "已恢复待确认邀请；可用同一编号重试，或放弃后核对成员状态。";
  }
  element("group-invite-group").textContent = `群聊：${groupName} · 当前任职发起邀请`;
  groupInviteDialog.showModal();
  renderGroupInvite();
  if (saved) groupInviteSubmit.focus();
  else groupInviteQuery.focus();
}

async function searchGroupInvitePeople() {
  if (!groupInviteDialog.open || pendingGroupInvite) return;
  const query = groupInviteQuery.value.trim();
  if ([...query].length < 2 || [...query].length > 100) {
    return notify("请输入 2～100 个字符的姓名片段。");
  }
  const serial = ++groupInviteSearchSerial;
  const selectedEpoch = identityEpoch;
  element("group-invite-search-hint").textContent = "正在查找…";
  try {
    const page = await request(`/api/v1/directory/users?q=${encodeURIComponent(query)}&limit=20`);
    if (!groupInviteDialog.open || serial !== groupInviteSearchSerial ||
        selectedEpoch !== identityEpoch) return;
    groupInviteSearchResults = page.people;
    element("group-invite-search-hint").textContent = !page.people.length ?
      "没有找到当前任职下可见的同事。" : page.has_more ?
        "结果较多，请缩小姓名范围。" : `找到 ${page.people.length} 位同事`;
    renderGroupInvite();
  } catch (error) {
    if (error.stale || serial !== groupInviteSearchSerial || !groupInviteDialog.open) return;
    element("group-invite-search-hint").textContent = error.message;
  }
}

async function submitGroupInvite(event) {
  event.preventDefault();
  if (!groupInviteDialog.open || !actingMembership || pendingGroupInvite?.sending) return;
  if (!pendingGroupInvite) {
    if (!groupInviteTarget || !groupInviteGroupID) return notify("请选择一位要邀请的成员任职。");
    const created = { id: uuidV7(), groupID: groupInviteGroupID,
      actor: actingMembership, targetID: groupInviteTarget.membershipID, sending: false };
    try {
      localStorage.setItem(groupInviteStorageKey(created.id, created.groupID),
        JSON.stringify({ ...created, name: groupInviteTarget.name,
          organization: groupInviteTarget.organization,
          groupName: groups.get(created.groupID)?.name || created.groupID }));
    } catch (_) {
      return notify("浏览器无法保存待确认邀请，请检查浏览器存储设置后重试。");
    }
    pendingGroupInvite = created;
    renderPendingGroupInvites();
  }
  const submitted = pendingGroupInvite;
  const selectedEpoch = identityEpoch;
  submitted.sending = true;
  element("group-invite-hint").textContent = "正在邀请成员…";
  renderGroupInvite();
  try {
    await request(`/api/v1/groups/${submitted.groupID}/invitations`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ client_request_id: submitted.id,
        target_membership_id: submitted.targetID }),
    });
    if (selectedEpoch !== identityEpoch || submitted !== pendingGroupInvite ||
        submitted.actor !== actingMembership) return;
    forgetGroupInvite(submitted.id, submitted.groupID, submitted.actor);
    closeGroupInvite(true);
    renderPendingGroupInvites();
    renderGroupInviteAction();
    notify("已邀请成员加入群聊。");
  } catch (error) {
    if (error.stale || selectedEpoch !== identityEpoch ||
        submitted !== pendingGroupInvite || submitted.actor !== actingMembership) return;
    if ([400, 404, 409].includes(error.status) ||
        (error.status === 403 && error.code === "group_permission_denied")) {
      forgetGroupInvite(submitted.id, submitted.groupID, submitted.actor);
      pendingGroupInvite = null;
      renderPendingGroupInvites();
      element("group-invite-hint").textContent = "邀请未通过，请核对群状态和目标任职。";
      if (error.code === "group_policy_blocked" && groups.has(submitted.groupID)) {
        groups.get(submitted.groupID).status = "policy_blocked";
        renderGroups();
        refreshGroups().catch(report);
      }
    } else {
      element("group-invite-hint").textContent = "邀请结果未确认；可用同一编号重试，或放弃后核对成员状态。";
    }
    submitted.sending = false;
    renderGroupInvite();
    report(error);
  } finally {
    submitted.sending = false;
    if (submitted === pendingGroupInvite) renderGroupInvite();
  }
}

function discardGroupInvite() {
  if (!pendingGroupInvite || pendingGroupInvite.sending) return;
  forgetGroupInvite(pendingGroupInvite.id, pendingGroupInvite.groupID,
    pendingGroupInvite.actor);
  closeGroupInvite(true);
  renderPendingGroupInvites();
  renderPendingGroupLeaves();
  renderGroupInviteAction();
  renderGroupLeaveAction();
  notify("已放弃待确认邀请；成员可能已加入，请先核对后再邀请。");
}

function groupLeaveStoragePrefix(actor = actingMembership) {
  return `enterprise-im-group-leave:${self.tenant_id}:${self.user_id}:${actor}:`;
}

function groupLeaveStorageKey(groupID, intervalID, actor = actingMembership) {
  return groupLeaveStoragePrefix(actor) + groupID + ":" + intervalID;
}

function forgetGroupLeave(leave) {
  try { localStorage.removeItem(groupLeaveStorageKey(leave.groupID, leave.intervalID, leave.actor)); }
  catch (_) { /* Storage may be unavailable. */ }
}

function savedGroupLeaves() {
  if (!self || !actingMembership) return [];
  const found = [];
  const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
  try {
    const prefix = groupLeaveStoragePrefix();
    for (let index = 0; index < localStorage.length; index++) {
      const key = localStorage.key(index);
      if (!key?.startsWith(prefix)) continue;
      try {
        const saved = JSON.parse(localStorage.getItem(key) || "null");
        if (saved?.actor === actingMembership && uuid.test(saved.groupID) &&
            uuid.test(saved.intervalID) && typeof saved.groupName === "string" &&
            key === groupLeaveStorageKey(saved.groupID, saved.intervalID)) found.push(saved);
      } catch (_) { /* Ignore an unreadable saved request. */ }
    }
  } catch (_) { /* Storage may be unavailable. */ }
  return found;
}

function renderPendingGroupLeaves() {
  pendingGroupLeaves.replaceChildren();
  for (const saved of savedGroupLeaves()) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "secondary-button";
    button.textContent = `待确认退群 · ${saved.groupName || saved.groupID}`;
    button.addEventListener("click", () => showGroupLeave(saved, true));
    pendingGroupLeaves.append(button);
  }
  pendingGroupLeaves.classList.toggle("hidden", !pendingGroupLeaves.childElementCount);
}

function renderGroupLeaveAction() {
  const group = activeConversationKind === "group" ? groups.get(activeConversation) : null;
  groupLeaveOpenButton.classList.toggle("hidden", !actingMembership || !group);
  groupLeaveOpenButton.disabled = !group || group.role === "owner";
  groupLeaveOpenButton.textContent = group?.role === "owner" ? "请先转让群主" : "退出群聊";
  groupLeaveOpenButton.title = group?.role === "owner" ? "请先转让群主，再退出群聊" : "";
}

function activeRosterGroup() {
  const group = activeConversationKind === "group" ? groups.get(activeConversation) : null;
  return actingMembership && group && ["owner", "admin"].includes(group.role) ? group : null;
}

function renderGroupRosterAction() {
  const group = activeRosterGroup();
  if (groupRosterDialog.open && (!group || group.id !== groupRosterGroupID ||
      group.role !== groupRosterRole)) closeGroupRoster();
  groupRosterOpenButton.classList.toggle("hidden", !group);
}

function renderGroupPolicyRecheckAction() {
  const group = activeConversationKind === "group" ? groups.get(activeConversation) : null;
  if (group && groupPolicyRecheckNotice?.groupID === group.id &&
      groupPolicyRecheckNotice.observedStatus !== group.status) {
    groupPolicyRecheckNotice = group.status === "active" ?
      { groupID: group.id, observedStatus: "active", text: "群通信已恢复。" } : null;
  }
  const canRecheck = !!actingMembership && group?.status === "policy_blocked" &&
    ["owner", "admin"].includes(group.role);
  groupPolicyRecheckButton.classList.toggle("hidden", !canRecheck);
  const pending = group ? groupPolicyRechecks.get(group.id) : null;
  groupPolicyRecheckButton.disabled = !!pending;
  groupPolicyRecheckButton.textContent = pending ? "正在复核…" : "复核群策略";
  const notice = groupPolicyRecheckNotice && groupPolicyRecheckNotice.groupID === group?.id ?
    groupPolicyRecheckNotice.text : "";
  groupPolicyRecheckHint.textContent = notice;
  groupPolicyRecheckHint.classList.toggle("hidden", !notice);
}

async function recheckGroupPolicy() {
  const group = activeRosterGroup();
  if (!group || group.status !== "policy_blocked" || groupPolicyRechecks.has(group.id)) return;
  const submitted = { groupID: group.id, actor: actingMembership, epoch: identityEpoch,
    conversationEpoch };
  groupPolicyRechecks.set(group.id, submitted);
  groupPolicyRecheckNotice = { groupID: group.id, observedStatus: group.status,
    text: "正在复核当前群成员与通信策略…" };
  renderGroupPolicyRecheckAction();
  const currentIdentity = () => groupPolicyRechecks.get(submitted.groupID) === submitted &&
    submitted.epoch === identityEpoch && submitted.actor === actingMembership;
  const currentView = () => currentIdentity() && submitted.conversationEpoch === conversationEpoch &&
    activeConversationKind === "group" && activeConversation === submitted.groupID;
  const showResult = (text) => {
    if (!currentView()) return;
    groupPolicyRecheckNotice = { groupID: submitted.groupID,
      observedStatus: groups.get(submitted.groupID)?.status, text };
    renderGroupPolicyRecheckAction();
  };
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 15000);
  try {
    const result = await request(`/api/v1/groups/${encodeURIComponent(submitted.groupID)}/policy-rechecks`, {
      method: "POST",
      signal: controller.signal,
    });
    if (!currentIdentity()) return;
    if (result?.status !== "active" || !Number.isSafeInteger(result.policy_version) ||
        result.policy_version < 0) throw new Error("复核响应无法确认，请核对群状态后重试。");
    try {
      await refreshGroups();
      if (!currentIdentity()) return;
      showResult(groups.get(submitted.groupID)?.status === "active" ?
        "复核通过，群通信已恢复。" : "复核已通过，但群状态又发生变化，请核对后重试。");
    } catch (refreshError) {
      if (!refreshError.stale) showResult("复核已通过；群列表尚未刷新，请稍后重载。");
    }
  } catch (error) {
    if (error.stale || !currentIdentity()) return;
    try { await refreshGroups(); } catch (refreshError) {
      if (refreshError.stale || !currentIdentity()) return;
    }
    if (!currentIdentity()) return;
    if (groups.get(submitted.groupID)?.status === "active") {
      showResult("已从群列表确认通信恢复。");
    } else if (error.status === 409 && error.code === "group_policy_blocked") {
      showResult("复核未通过，群通信继续暂停；请处理冲突成员或联系策略管理员。");
    } else if ([403, 404].includes(error.status)) {
      showResult(error.message);
    } else {
      showResult("复核结果未确认；群仍显示暂停，可重试或稍后刷新群列表。");
    }
  } finally {
    clearTimeout(timeout);
    if (groupPolicyRechecks.get(submitted.groupID) === submitted) {
      groupPolicyRechecks.delete(submitted.groupID);
      renderGroupPolicyRecheckAction();
    }
  }
}

function groupRemovalStoragePrefix(actor = actingMembership) {
  return `enterprise-im-group-remove:${self.tenant_id}:${self.user_id}:${actor}:`;
}

function groupRemovalStorageKey(groupID, intervalID, actor = actingMembership) {
  return groupRemovalStoragePrefix(actor) + groupID + ":" + intervalID;
}

function savedGroupRemovals() {
  if (!self || !actingMembership) return [];
  const found = [];
  try {
    const prefix = groupRemovalStoragePrefix();
    for (let index = 0; index < localStorage.length; index++) {
      const key = localStorage.key(index);
      if (!key?.startsWith(prefix)) continue;
      try {
        const saved = JSON.parse(localStorage.getItem(key) || "null");
        if (saved?.tenantID === self.tenant_id && saved.userID === self.user_id &&
            saved.actor === actingMembership && uuidPattern.test(saved.groupID) &&
            uuidPattern.test(saved.intervalID) &&
            key === groupRemovalStorageKey(saved.groupID, saved.intervalID)) found.push(saved);
      } catch (_) { /* Ignore an unreadable saved request. */ }
    }
  } catch (_) { /* Storage may be unavailable. */ }
  return found;
}

function forgetGroupRemoval(removal) {
  try { localStorage.removeItem(groupRemovalStorageKey(removal.groupID, removal.intervalID, removal.actor)); }
  catch (_) { /* Storage may be unavailable. */ }
}

function renderPendingGroupRemovals() {
  pendingGroupRemovals.replaceChildren();
  for (const saved of savedGroupRemovals()) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "secondary-button";
    button.textContent = `待确认移除 · ${groups.get(saved.groupID)?.name || saved.groupID}`;
    button.addEventListener("click", () => showGroupRemoval(saved, true));
    pendingGroupRemovals.append(button);
  }
  pendingGroupRemovals.classList.toggle("hidden", !pendingGroupRemovals.childElementCount);
}

function renderGroupRemovalDialog() {
  const saved = !!pendingGroupRemoval;
  groupRemoveConfirm.classList.toggle("hidden", saved);
  groupRemoveRetry.classList.toggle("hidden", !saved);
  groupRemoveDiscard.classList.toggle("hidden", !saved || pendingGroupRemoval.sending);
  groupRemoveConfirm.disabled = !preparedGroupRemoval;
  groupRemoveRetry.disabled = !saved || pendingGroupRemoval.sending;
  element("group-remove-cancel").disabled = !!pendingGroupRemoval?.sending;
}

function closeGroupRemoval(force = false) {
  if (pendingGroupRemoval?.sending && !force) return;
  if (groupRemoveDialog.open) groupRemoveDialog.close();
  preparedGroupRemoval = null;
  pendingGroupRemoval = null;
  element("group-remove-target").textContent = "";
  element("group-remove-hint").textContent = "";
}

function showGroupRemoval(removal, saved = false) {
  if (!self || !actingMembership || removal.actor !== actingMembership) return;
  if (groupRosterDialog.open) closeGroupRoster();
  closeGroupRemoval(true);
  if (saved) pendingGroupRemoval = { ...removal, sending: false };
  else preparedGroupRemoval = removal;
  const groupName = groups.get(removal.groupID)?.name || removal.groupID;
  element("group-remove-target").textContent = saved ?
    `群聊：${groupName} · 目标成员区间：${removal.intervalID}` :
    `群聊：${groupName} · ${removal.name} · ${removal.organization}`;
  element("group-remove-hint").textContent = saved ?
    "移除结果未确认；请用原成员区间重试，或放弃后核对成员状态。" :
    "请核对姓名和组织，确认后移除该成员。";
  renderGroupRemovalDialog();
  groupRemoveDialog.showModal();
  if (saved) groupRemoveRetry.focus();
  else groupRemoveConfirm.focus();
}

function prepareGroupRemoval(member) {
  const group = activeRosterGroup();
  if (!group || !groupRosterDialog.open || !uuidPattern.test(member.interval_id) ||
      (group.role === "owner" && member.role === "owner") ||
      (group.role === "admin" && member.role !== "member")) return;
  const saved = savedGroupRemovals().find((removal) => removal.groupID === group.id);
  if (saved) return showGroupRemoval(saved, true);
  if (savedGroupTransfers().some((transfer) => transfer.groupID === group.id)) {
    notify("此群有待确认群主转让，请先重试或放弃该请求。");
    return;
  }
  const removal = { tenantID: self.tenant_id, userID: self.user_id,
    actor: actingMembership, groupID: group.id, intervalID: member.interval_id,
    name: member.display_name, organization: member.organization_name };
  closeGroupRoster();
  showGroupRemoval(removal);
}

async function submitGroupRemoval() {
  if (!actingMembership || pendingGroupRemoval?.sending || !groupRemoveDialog.open) return;
  const fresh = !pendingGroupRemoval;
  if (fresh) {
    if (!preparedGroupRemoval) return;
    if (savedGroupTransfers().some((transfer) => transfer.groupID === preparedGroupRemoval.groupID))
      return notify("此群有待确认群主转让，请先处理该请求。");
    const { tenantID, userID, actor, groupID, intervalID } = preparedGroupRemoval;
    const saved = { tenantID, userID, actor, groupID, intervalID };
    try {
      localStorage.setItem(groupRemovalStorageKey(groupID, intervalID), JSON.stringify(saved));
    } catch (_) {
      return notify("浏览器无法保存待确认移除请求，请启用本地存储后重试。");
    }
    pendingGroupRemoval = { ...saved, sending: false };
    preparedGroupRemoval = null;
    renderPendingGroupRemovals();
  }
  const submitted = pendingGroupRemoval;
  const selectedEpoch = identityEpoch;
  submitted.sending = true;
  element("group-remove-hint").textContent = "正在移除成员…";
  renderGroupRemovalDialog();
  try {
    const result = await request(`/api/v1/groups/${encodeURIComponent(submitted.groupID)}/removals`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ interval_id: submitted.intervalID }),
    });
    if (selectedEpoch !== identityEpoch || submitted !== pendingGroupRemoval ||
        submitted.actor !== actingMembership) return;
    if (result.status !== "removed" || result.interval_id !== submitted.intervalID ||
        !Number.isSafeInteger(result.leave_seq) || result.leave_seq < 0)
      throw new Error("移除响应无法确认，请用原成员区间重试。");
    forgetGroupRemoval(submitted);
    closeGroupRemoval(true);
    renderPendingGroupRemovals();
    if (activeConversationKind === "group" && activeConversation === submitted.groupID &&
        activeRosterGroup()) openGroupRoster();
    refreshGroups().catch(report);
    notify("已移除群成员。");
  } catch (error) {
    if (error.stale || selectedEpoch !== identityEpoch || submitted !== pendingGroupRemoval ||
        submitted.actor !== actingMembership) return;
    const rejected = (error.status === 400 && error.code === "invalid_request") ||
      (error.status === 403 && error.code === "group_permission_denied") ||
      (error.status === 404 && error.code === "not_found") ||
      (error.status === 409 && error.code === "owner_transfer_required");
    if (fresh && rejected) {
      forgetGroupRemoval(submitted);
      closeGroupRemoval(true);
      renderPendingGroupRemovals();
      refreshGroups().catch(report);
      report(error);
      return;
    }
    element("group-remove-hint").textContent = error.code && error.status && error.status < 500 ?
      "当前请求被拒绝；此前请求的结果仍未确认，请核对成员状态或权限。" :
      "移除结果未确认；请用原成员区间重试，或放弃后核对成员状态。";
    report(error);
  } finally {
    submitted.sending = false;
    if (submitted === pendingGroupRemoval) renderGroupRemovalDialog();
  }
}

function discardGroupRemoval() {
  if (!pendingGroupRemoval || pendingGroupRemoval.sending) return;
  forgetGroupRemoval(pendingGroupRemoval);
  closeGroupRemoval(true);
  renderPendingGroupRemovals();
  if (groupRosterDialog.open) closeGroupRoster();
  notify("已放弃待确认移除；此前请求可能已成功，请核对群成员状态。");
}

function groupTransferStoragePrefix(actor = actingMembership) {
  return `enterprise-im-group-transfer:${self.tenant_id}:${self.user_id}:${actor}:`;
}

function groupTransferStorageKey(groupID, requestID, actor = actingMembership) {
  return groupTransferStoragePrefix(actor) + groupID + ":" + requestID;
}

function savedGroupTransfers() {
  if (!self || !actingMembership) return [];
  const found = [];
  try {
    const prefix = groupTransferStoragePrefix();
    for (let index = 0; index < localStorage.length; index++) {
      const key = localStorage.key(index);
      if (!key?.startsWith(prefix)) continue;
      try {
        const saved = JSON.parse(localStorage.getItem(key) || "null");
        if (saved?.tenantID === self.tenant_id && saved.userID === self.user_id &&
            saved.actor === actingMembership && uuidPattern.test(saved.groupID) &&
            uuidPattern.test(saved.requestID) && uuidPattern.test(saved.sourceIntervalID) &&
            uuidPattern.test(saved.targetIntervalID) &&
            key === groupTransferStorageKey(saved.groupID, saved.requestID)) found.push(saved);
      } catch (_) { /* Ignore an unreadable saved request. */ }
    }
  } catch (_) { /* Storage may be unavailable. */ }
  return found;
}

function forgetGroupTransfer(transfer) {
  try { localStorage.removeItem(groupTransferStorageKey(transfer.groupID, transfer.requestID, transfer.actor)); }
  catch (_) { /* Storage may be unavailable. */ }
}

function renderPendingGroupTransfers() {
  pendingGroupTransfers.replaceChildren();
  for (const saved of savedGroupTransfers()) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "secondary-button";
    button.textContent = `待确认转让 · ${groups.get(saved.groupID)?.name || saved.groupID}`;
    button.addEventListener("click", () => showGroupTransfer(saved, true));
    pendingGroupTransfers.append(button);
  }
  pendingGroupTransfers.classList.toggle("hidden", !pendingGroupTransfers.childElementCount);
}

function renderGroupTransferDialog() {
  const saved = !!pendingGroupTransfer;
  groupTransferConfirm.classList.toggle("hidden", saved);
  groupTransferRetry.classList.toggle("hidden", !saved);
  groupTransferDiscard.classList.toggle("hidden", !saved || pendingGroupTransfer.sending);
  groupTransferConfirm.disabled = !preparedGroupTransfer;
  groupTransferRetry.disabled = !saved || pendingGroupTransfer.sending;
  element("group-transfer-cancel").disabled = !!pendingGroupTransfer?.sending;
}

function closeGroupTransfer(force = false) {
  if (pendingGroupTransfer?.sending && !force) return;
  if (groupTransferDialog.open) groupTransferDialog.close();
  preparedGroupTransfer = null;
  pendingGroupTransfer = null;
  element("group-transfer-target").textContent = "";
  element("group-transfer-hint").textContent = "";
}

function showGroupTransfer(transfer, saved = false) {
  if (!self || !actingMembership || transfer.actor !== actingMembership) return;
  if (groupRosterDialog.open) closeGroupRoster();
  closeGroupTransfer(true);
  if (saved) pendingGroupTransfer = { ...transfer, sending: false };
  else preparedGroupTransfer = transfer;
  const groupName = groups.get(transfer.groupID)?.name || transfer.groupID;
  element("group-transfer-target").textContent = saved ?
    `群聊：${groupName} · 接任成员区间：${transfer.targetIntervalID}` :
    `群聊：${groupName} · 接任者：${transfer.name} · ${transfer.organization}`;
  element("group-transfer-hint").textContent = saved ?
    "转让结果未确认；请用原请求编号和区间重试，或放弃后核对群主。" :
    "确认后，您将成为普通成员，可自行退出群聊。";
  renderGroupTransferDialog();
  groupTransferDialog.showModal();
  if (saved) groupTransferRetry.focus();
  else groupTransferConfirm.focus();
}

async function prepareGroupTransfer(member) {
  const group = activeRosterGroup();
  if (!group || group.role !== "owner" || !groupRosterDialog.open ||
      member.role === "owner" || !uuidPattern.test(member.interval_id)) return;
  const saved = savedGroupTransfers().find((transfer) => transfer.groupID === group.id);
  if (saved) return showGroupTransfer(saved, true);
  if (savedGroupRemovals().some((removal) => removal.groupID === group.id)) {
    notify("此群有待确认成员移除，请先重试或放弃该请求。");
    return;
  }
  const selectedEpoch = identityEpoch;
  const selectedConversationEpoch = conversationEpoch;
  const selectedActor = actingMembership;
  closeGroupRoster();
  try {
    const membership = await request(`/api/v1/groups/${encodeURIComponent(group.id)}/membership`);
    if (selectedEpoch !== identityEpoch || selectedConversationEpoch !== conversationEpoch ||
        selectedActor !== actingMembership || activeConversationKind !== "group" ||
        activeConversation !== group.id) return;
    if (membership.role !== "owner" || !uuidPattern.test(membership.interval_id)) {
      notify("群主身份已变化，请刷新群列表后重试。");
      refreshGroups().catch(report);
      return;
    }
    showGroupTransfer({ tenantID: self.tenant_id, userID: self.user_id,
      actor: actingMembership, groupID: group.id, requestID: uuidV7(),
      sourceIntervalID: membership.interval_id, targetIntervalID: member.interval_id,
      name: member.display_name, organization: member.organization_name });
  } catch (error) { report(error); }
}

async function submitGroupTransfer() {
  if (!actingMembership || pendingGroupTransfer?.sending || !groupTransferDialog.open) return;
  const fresh = !pendingGroupTransfer;
  if (fresh) {
    if (!preparedGroupTransfer) return;
    if (savedGroupRemovals().some((removal) => removal.groupID === preparedGroupTransfer.groupID))
      return notify("此群有待确认成员移除，请先处理该请求。");
    const { tenantID, userID, actor, groupID, requestID, sourceIntervalID,
      targetIntervalID } = preparedGroupTransfer;
    const saved = { tenantID, userID, actor, groupID, requestID, sourceIntervalID, targetIntervalID };
    try {
      localStorage.setItem(groupTransferStorageKey(groupID, requestID), JSON.stringify(saved));
    } catch (_) {
      return notify("浏览器无法保存待确认转让请求，请启用本地存储后重试。");
    }
    pendingGroupTransfer = { ...saved, sending: false };
    preparedGroupTransfer = null;
    renderPendingGroupTransfers();
  }
  const submitted = pendingGroupTransfer;
  const selectedEpoch = identityEpoch;
  submitted.sending = true;
  element("group-transfer-hint").textContent = "正在转让群主…";
  renderGroupTransferDialog();
  try {
    const result = await request(`/api/v1/groups/${encodeURIComponent(submitted.groupID)}/owner-transfers`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ client_request_id: submitted.requestID,
        source_interval_id: submitted.sourceIntervalID, target_interval_id: submitted.targetIntervalID }),
    });
    if (selectedEpoch !== identityEpoch || submitted !== pendingGroupTransfer ||
        submitted.actor !== actingMembership) return;
    if (result.source_interval_id !== submitted.sourceIntervalID ||
        result.target_interval_id !== submitted.targetIntervalID)
      throw new Error("转让响应无法确认，请用原请求编号和区间重试。");
    forgetGroupTransfer(submitted);
    closeGroupTransfer(true);
    renderPendingGroupTransfers();
    try {
      await refreshGroups();
      if (selectedEpoch === identityEpoch) notify("已转让群主。您现在可退出该群。");
    } catch (refreshError) {
      if (!refreshError.stale && selectedEpoch === identityEpoch)
        notify("群主转让已确认；群列表尚未刷新，请稍后重载。");
    }
  } catch (error) {
    if (error.stale || selectedEpoch !== identityEpoch || submitted !== pendingGroupTransfer ||
        submitted.actor !== actingMembership) return;
    const rejected = (error.status === 400 && error.code === "invalid_request") ||
      (error.status === 403 && error.code === "group_permission_denied") ||
      (error.status === 404 && error.code === "not_found") ||
      (error.status === 409 && error.code === "idempotency_conflict");
    if (fresh && rejected) {
      forgetGroupTransfer(submitted);
      closeGroupTransfer(true);
      renderPendingGroupTransfers();
      refreshGroups().catch(report);
      report(error);
      return;
    }
    element("group-transfer-hint").textContent = error.code && error.status && error.status < 500 ?
      "当前请求被拒绝；此前转让结果仍未确认，请核对群主状态。" :
      "转让结果未确认；请用原请求编号和区间重试，或放弃后核对群主。";
    report(error);
  } finally {
    submitted.sending = false;
    if (submitted === pendingGroupTransfer) renderGroupTransferDialog();
  }
}

function discardGroupTransfer() {
  if (!pendingGroupTransfer || pendingGroupTransfer.sending) return;
  forgetGroupTransfer(pendingGroupTransfer);
  closeGroupTransfer(true);
  renderPendingGroupTransfers();
  refreshGroups().catch(report);
  notify("已放弃待确认转让；此前请求可能已成功，请核对当前群主。");
}

function renderGroupRosterControls() {
  groupRosterLoadMoreButton.classList.toggle("hidden", !groupRosterHasMore || groupRosterError);
  groupRosterLoadMoreButton.disabled = groupRosterLoading;
  groupRosterRetryButton.classList.toggle("hidden", !groupRosterError);
  groupRosterRetryButton.disabled = groupRosterLoading;
}

function closeGroupRoster() {
  groupRosterGeneration++;
  if (groupRosterDialog.open) groupRosterDialog.close();
  groupRosterGroupID = "";
  groupRosterRole = "";
  groupRosterCursor = "";
  groupRosterHasMore = false;
  groupRosterLoading = false;
  groupRosterError = false;
  groupRosterMembers.replaceChildren();
  groupRosterHint.textContent = "";
  element("group-roster-group").textContent = "";
  renderGroupRosterControls();
}

function renderGroupRosterMember(member) {
  const item = document.createElement("div");
  item.className = "group-roster-member";
  item.setAttribute("role", "listitem");
  const identity = document.createElement("div");
  const name = document.createElement("strong");
  name.textContent = member.display_name;
  const organization = document.createElement("small");
  organization.textContent = member.organization_name;
  identity.append(name, organization);
  const role = document.createElement("span");
  role.className = "group-roster-role";
  role.textContent = { owner: "群主", admin: "管理员", member: "成员" }[member.role];
  const actions = document.createElement("div");
  actions.className = "group-roster-actions";
  actions.append(role);
  const group = activeRosterGroup();
  if (group?.role === "owner" && member.role !== "owner") {
    const transfer = document.createElement("button");
    transfer.type = "button";
    transfer.className = "secondary-button";
    transfer.textContent = `转让给${member.display_name}`;
    transfer.disabled = savedGroupTransfers().some((saved) => saved.groupID === group.id) ||
      savedGroupRemovals().some((saved) => saved.groupID === group.id);
    transfer.addEventListener("click", () => prepareGroupTransfer(member));
    actions.append(transfer);
  }
  if (group && ((group.role === "owner" && member.role !== "owner") ||
      (group.role === "admin" && member.role === "member"))) {
    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "secondary-button";
    remove.textContent = `移除${member.display_name}`;
    remove.disabled = savedGroupRemovals().some((saved) => saved.groupID === group.id) ||
      savedGroupTransfers().some((saved) => saved.groupID === group.id);
    remove.addEventListener("click", () => prepareGroupRemoval(member));
    actions.append(remove);
  }
  item.append(identity, actions);
  return item;
}

async function loadGroupRosterPage(cursor = "") {
  if (!groupRosterDialog.open || !groupRosterGroupID || groupRosterLoading ||
      (cursor && (!groupRosterHasMore || cursor !== groupRosterCursor))) return;
  const generation = groupRosterGeneration;
  const groupID = groupRosterGroupID;
  const selectedEpoch = identityEpoch;
  const current = () => generation === groupRosterGeneration && groupRosterDialog.open &&
    groupRosterGroupID === groupID && selectedEpoch === identityEpoch &&
    activeConversationKind === "group" && activeConversation === groupID && activeRosterGroup();
  groupRosterLoading = true;
  groupRosterError = false;
  groupRosterHint.textContent = cursor ? "正在加载更多成员…" : "正在加载成员…";
  renderGroupRosterControls();
  try {
    const path = `/api/v1/groups/${encodeURIComponent(groupID)}/members?limit=20` +
      (cursor ? `&cursor=${encodeURIComponent(cursor)}` : "");
    const page = await request(path);
    if (!current()) return;
    if (!Array.isArray(page.members) || typeof page.has_more !== "boolean" ||
        (page.has_more && !page.next_cursor) || page.members.some((member) =>
          typeof member.display_name !== "string" || typeof member.organization_name !== "string" ||
          typeof member.interval_id !== "string" || !uuidPattern.test(member.interval_id) ||
          !["owner", "admin", "member"].includes(member.role))) {
      const invalid = new Error("群成员数据暂不可用，请稍后重试。");
      invalid.invalidRoster = true;
      throw invalid;
    }
    const items = document.createDocumentFragment();
    for (const member of page.members) items.append(renderGroupRosterMember(member));
    groupRosterMembers.append(items);
    groupRosterCursor = page.next_cursor || "";
    groupRosterHasMore = page.has_more;
    groupRosterHint.textContent = groupRosterMembers.childElementCount ?
      `已加载 ${groupRosterMembers.childElementCount} 位成员。` : "暂无当前成员。";
  } catch (error) {
    if (error.stale || !current()) return;
    if (error.invalidRoster || [403, 404].includes(error.status)) {
      groupRosterMembers.replaceChildren();
      groupRosterCursor = "";
      groupRosterHasMore = false;
    }
    groupRosterError = true;
    groupRosterHint.textContent = error.message;
  } finally {
    if (generation === groupRosterGeneration) {
      groupRosterLoading = false;
      renderGroupRosterControls();
    }
  }
}

function openGroupRoster() {
  const group = activeRosterGroup();
  if (!group) return;
  closeGroupRoster();
  groupRosterGroupID = group.id;
  groupRosterRole = group.role;
  element("group-roster-group").textContent = `群聊：${group.name}`;
  groupRosterDialog.showModal();
  loadGroupRosterPage();
}

function renderGroupLeaveDialog() {
  const saved = !!pendingGroupLeave;
  groupLeaveConfirm.classList.toggle("hidden", saved);
  groupLeaveRetry.classList.toggle("hidden", !saved);
  groupLeaveDiscard.classList.toggle("hidden", !saved || pendingGroupLeave.sending);
  groupLeaveConfirm.disabled = !preparedGroupLeave;
  groupLeaveRetry.disabled = !saved || pendingGroupLeave.sending;
}

function closeGroupLeave(force = false) {
  if (pendingGroupLeave?.sending && !force) return;
  groupLeaveLookupSerial++;
  if (groupLeaveDialog.open) groupLeaveDialog.close();
  pendingGroupLeave = null;
  preparedGroupLeave = null;
  element("group-leave-hint").textContent = "";
  renderGroupLeaveAction();
}

function showGroupLeave(leave, saved = false) {
  if (!actingMembership || !canSwitchChat() ||
      savedGroupInvites().some((invite) => invite.groupID === leave.groupID)) {
    if (actingMembership && savedGroupInvites().some((invite) => invite.groupID === leave.groupID))
      notify("此群有待确认邀请，请先重试或放弃该邀请。");
    return;
  }
  closeGroupLeave(true);
  if (saved) pendingGroupLeave = { ...leave, sending: false };
  else preparedGroupLeave = leave;
  element("group-leave-group").textContent = leave.groupName;
  element("group-leave-hint").textContent = saved ?
    "退群结果未确认；可用原成员区间重试，或放弃后核对群成员状态。" :
    "退出后群聊将从当前列表移除。请确认退群。";
  renderGroupLeaveDialog();
  groupLeaveDialog.showModal();
  if (saved) groupLeaveRetry.focus();
  else groupLeaveConfirm.focus();
}

async function openGroupLeave() {
  const group = activeConversationKind === "group" ? groups.get(activeConversation) : null;
  if (!group || !actingMembership || groupLeaveDialog.open) return;
  if (group.role === "owner") return notify("请先转让群主，再退出群聊。");
  const saved = savedGroupLeaves().find((leave) => leave.groupID === group.id);
  if (saved) return showGroupLeave(saved, true);
  if (!canSwitchChat() || savedGroupInvites().some((invite) => invite.groupID === group.id)) {
    if (savedGroupInvites().some((invite) => invite.groupID === group.id))
      notify("此群有待确认邀请，请先重试或放弃该邀请。");
    return;
  }
  const serial = ++groupLeaveLookupSerial;
  const selectedEpoch = identityEpoch;
  groupLeaveOpenButton.disabled = true;
  try {
    const membership = await request(`/api/v1/groups/${encodeURIComponent(group.id)}/membership`);
    if (serial !== groupLeaveLookupSerial || selectedEpoch !== identityEpoch ||
        activeConversationKind !== "group" || activeConversation !== group.id) return;
    if (membership.role === "owner") return notify("请先转让群主，再退出群聊。");
    if (typeof membership.interval_id !== "string") throw new Error("群成员状态不可用，请稍后重试。");
    showGroupLeave({ groupID: group.id, groupName: group.name,
      intervalID: membership.interval_id, actor: actingMembership });
  } catch (error) { report(error); }
  finally { if (serial === groupLeaveLookupSerial) renderGroupLeaveAction(); }
}

async function submitGroupLeave() {
  if (!actingMembership || pendingGroupLeave?.sending) return;
  const fresh = !pendingGroupLeave;
  if (fresh) {
    if (!preparedGroupLeave) return;
    try {
      localStorage.setItem(groupLeaveStorageKey(preparedGroupLeave.groupID,
        preparedGroupLeave.intervalID), JSON.stringify(preparedGroupLeave));
    } catch (_) {
      return notify("浏览器无法保存待确认退群请求，请启用本地存储后重试。");
    }
    pendingGroupLeave = { ...preparedGroupLeave, sending: false };
    preparedGroupLeave = null;
    renderPendingGroupLeaves();
  }
  const submitted = pendingGroupLeave;
  const selectedEpoch = identityEpoch;
  submitted.sending = true;
  renderGroupLeaveDialog();
  try {
    const result = await request(`/api/v1/groups/${encodeURIComponent(submitted.groupID)}/leave`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ interval_id: submitted.intervalID }),
    });
    if (selectedEpoch !== identityEpoch || submitted !== pendingGroupLeave ||
        submitted.actor !== actingMembership) return;
    if (result.status !== "left" || result.interval_id !== submitted.intervalID)
      throw new Error("退群响应无法确认，请使用原成员区间重试。");
    forgetGroupLeave(submitted);
    const openGroup = activeConversationKind === "group" && activeConversation === submitted.groupID;
    let membershipState = "unknown";
    if (openGroup) {
      messageText.disabled = true;
      sendButton.disabled = true;
      try {
        const current = await request(`/api/v1/groups/${encodeURIComponent(submitted.groupID)}/membership`);
        membershipState = typeof current.interval_id === "string" &&
          current.interval_id !== submitted.intervalID ? "rejoined" : "old";
      } catch (error) {
        if (error.stale) return;
        if (error.status === 404) membershipState = "absent";
        else report(error);
      }
    }
    if (selectedEpoch !== identityEpoch || submitted !== pendingGroupLeave) return;
    let refreshed = false;
    try { await refreshGroups(); refreshed = true; } catch (error) { report(error); }
    if (selectedEpoch !== identityEpoch) return;
    if (openGroup && activeConversationKind === "group" &&
        activeConversation === submitted.groupID) {
      if (membershipState === "rejoined" && (!refreshed || !groups.has(submitted.groupID)))
        groupListNeedsRefreshID = submitted.groupID;
      const left = membershipState === "absent" || membershipState === "old" ||
        (membershipState !== "rejoined" && (!refreshed || !groups.has(submitted.groupID)));
      if (left) {
        clearSyncRetry();
        activeConversation = null;
        activeConversationKind = "";
        conversationEpoch++;
        resetChat();
        renderGroups();
      }
    }
    closeGroupLeave(true);
    renderPendingGroupLeaves();
    if (openGroup && activeConversationKind === "group" &&
        activeConversation === submitted.groupID) updateGroupComposer();
    notify("已退出群聊。");
  } catch (error) {
    if (error.stale || selectedEpoch !== identityEpoch || submitted !== pendingGroupLeave ||
        submitted.actor !== actingMembership) return;
    if ([400, 404, 409].includes(error.status)) {
      forgetGroupLeave(submitted);
      pendingGroupLeave = null;
      renderPendingGroupLeaves();
      preparedGroupLeave = null;
      element("group-leave-hint").textContent = error.code === "owner_transfer_required" ?
        "请先转让群主，再退出群聊。" : "当前成员区间不可用，请刷新群列表核对状态。";
      refreshGroups().catch(report);
    } else {
      element("group-leave-hint").textContent =
        "退群结果未确认；可用原成员区间重试，或放弃后核对群成员状态。";
    }
    report(error);
  } finally {
    submitted.sending = false;
    if (submitted === pendingGroupLeave || groupLeaveDialog.open) renderGroupLeaveDialog();
  }
}

function discardGroupLeave() {
  if (!pendingGroupLeave || pendingGroupLeave.sending) return;
  forgetGroupLeave(pendingGroupLeave);
  closeGroupLeave(true);
  renderPendingGroupLeaves();
  refreshGroups().catch(report);
  notify("已放弃待确认退群；此前请求可能已成功，请先核对群成员状态。");
}

function canSwitchChat() {
  if (!pendingMessage) return true;
  notify("当前消息结果尚未确认，请先重试或放弃待确认消息。");
  return false;
}

async function openChat(person, member) {
  if (!actingMembership) return notify("请先选择任职。");
  if (!canSwitchChat()) return;
  if (openingChatSerial) return;
  const serial = ++openChatSerial;
  openingChatSerial = serial;
  const selectedEpoch = identityEpoch;
  const selectedConversationEpoch = conversationEpoch;
  const priorTextDisabled = messageText.disabled;
  const priorSendDisabled = sendButton.disabled;
  messageText.disabled = true;
  sendButton.disabled = true;
  try {
    const chat = await request("/api/v1/conversations", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ target_membership_id: member.membership_id }),
    });
    if (serial !== openChatSerial || selectedEpoch !== identityEpoch ||
        selectedConversationEpoch !== conversationEpoch) return;
    conversations.set(chat.id, { id: chat.id, name: person.display_name, organization: member.organization_name });
    renderConversations();
    activateConversation(chat.id);
    refreshInbox().catch(report);
  } catch (error) { report(error); }
  finally {
    if (openingChatSerial === serial) openingChatSerial = 0;
    if (serial === openChatSerial && selectedEpoch === identityEpoch &&
        selectedConversationEpoch === conversationEpoch) {
      messageText.disabled = priorTextDisabled;
      sendButton.disabled = priorSendDisabled;
    }
  }
}

function inboxConversation(item) {
  return { id: item.id, name: item.peer_visible ? item.display_name : "联系人不可见",
    organization: item.peer_visible ? item.organization_name : "资料当前不可见",
    peerVisible: item.peer_visible, updatedAt: item.updated_at };
}

async function refreshInbox() {
  if (!actingMembership) return;
  if (inboxPagePromise) {
    inboxRefreshAgain = true;
    return inboxPagePromise;
  }
  if (inboxRefreshPromise) {
    inboxRefreshAgain = true;
    return inboxRefreshPromise;
  }
  const generation = inboxGeneration;
  const work = (async () => {
    do {
      inboxRefreshAgain = false;
      const page = await request("/api/v1/conversations?limit=20");
      if (generation !== inboxGeneration) return;
      const activeID = activeConversation;
      conversations.clear();
      for (const item of page.conversations) conversations.set(item.id, inboxConversation(item));
      if (activeConversationKind === "direct" && activeID && !conversations.has(activeID)) {
        conversations.set(activeID, { id: activeID, name: "当前会话", organization: "历史会话", peerVisible: false });
      }
      inboxCursor = page.next_cursor || "";
      inboxHasMore = page.has_more;
      renderConversations();
    } while (inboxRefreshAgain && generation === inboxGeneration);
  })();
  inboxRefreshPromise = work;
  try { await work; }
  catch (error) {
    if (generation === inboxGeneration && !error.stale) nextSafetySyncAt = 0;
    throw error;
  }
  finally {
    if (inboxRefreshPromise === work) {
      inboxRefreshPromise = null;
      if (inboxRefreshAgain && generation === inboxGeneration) refreshInbox().catch(report);
    }
  }
}

async function loadMoreInbox() {
  if (inboxRefreshPromise) {
    try { await inboxRefreshPromise; } catch (error) { report(error); return; }
  }
  if (!actingMembership || !inboxHasMore || !inboxCursor) return;
  if (inboxPagePromise) return inboxPagePromise;
  const generation = inboxGeneration;
  const cursor = inboxCursor;
  loadMoreConversationsButton.disabled = true;
  const work = (async () => {
    const page = await request(`/api/v1/conversations?limit=20&cursor=${encodeURIComponent(cursor)}`);
    if (generation !== inboxGeneration) return;
    for (const item of page.conversations) conversations.set(item.id, inboxConversation(item));
    inboxCursor = page.next_cursor || "";
    inboxHasMore = page.has_more;
    renderConversations();
  })();
  inboxPagePromise = work;
  try { await work; } catch (error) { report(error); }
  finally {
    if (inboxPagePromise === work) {
      inboxPagePromise = null;
      loadMoreConversationsButton.disabled = false;
      if (inboxRefreshAgain && generation === inboxGeneration) refreshInbox().catch(report);
    }
  }
}

function resetGroups() {
  closeGroupRoster();
  groupGeneration++;
  groupRefreshPromise = null;
  groupRefreshAgain = false;
  groupRefreshAfterPage = false;
  groupPagePromise = null;
  groupCursor = "";
  groupHasMore = false;
  groupVisiblePages = 1;
  groups.clear();
  groupList.replaceChildren();
  loadMoreGroupsButton.disabled = false;
  loadMoreGroupsButton.classList.add("hidden");
}

function renderGroups() {
  groupList.replaceChildren();
  if (!groups.size) {
    const empty = document.createElement("p");
    empty.className = "group-empty";
    empty.textContent = "暂无已加入的群聊。";
    groupList.append(empty);
  }
  for (const group of groups.values()) {
    const card = document.createElement("button");
    card.type = "button";
    card.className = "group-card" + (group.status === "policy_blocked" ? " policy-blocked" : "") +
      (activeConversationKind === "group" && activeConversation === group.id ? " active" : "");
    card.setAttribute("aria-pressed", activeConversationKind === "group" && activeConversation === group.id ? "true" : "false");
    const name = document.createElement("strong");
    name.textContent = group.name;
    const detail = document.createElement("small");
    const role = { owner: "群主", admin: "管理员", member: "成员" }[group.role] || "成员";
    const source = self.memberships.find((membership) => membership.id === group.source_membership_id);
    detail.textContent = [group.status === "policy_blocked" ? "策略暂停" : "正常",
      role, source ? source.organization_name : "来源任职已失效"].join(" · ");
    card.append(name, detail);
    card.addEventListener("click", () => activateGroupHistory(group.id));
    groupList.append(card);
  }
  loadMoreGroupsButton.classList.toggle("hidden", !groupHasMore);
  renderPendingGroupInvites();
  renderPendingGroupLeaves();
  renderPendingGroupRemovals();
  renderPendingGroupTransfers();
  renderGroupRosterAction();
  renderGroupPolicyRecheckAction();
  renderGroupInviteAction();
  renderGroupLeaveAction();
  if (activeConversationKind === "group") {
    if (groups.has(activeConversation)) {
      const active = groups.get(activeConversation);
      element("chat-title").textContent = active.name;
      element("chat-subtitle").textContent = "群聊历史" +
        (active.status === "policy_blocked" ? " · 策略暂停" : "");
    } else {
      element("chat-subtitle").textContent = "群聊历史 · 群已不在当前列表";
    }
    updateGroupComposer();
  }
}

async function refreshGroups() {
  if (!actingMembership) return;
  if (groupPagePromise) {
    groupRefreshAfterPage = true;
    await groupPagePromise;
    return refreshGroups();
  }
  if (groupRefreshPromise) {
    groupRefreshAgain = true;
    return groupRefreshPromise;
  }
  const generation = groupGeneration;
  const work = (async () => {
    do {
      groupRefreshAgain = false;
      const refreshed = new Map();
      const visiblePages = groupVisiblePages;
      let cursor = "";
      let hasMore = true;
      let pagesRead = 0;
      while (hasMore && pagesRead < visiblePages) {
        const path = cursor ? `/api/v1/groups?limit=20&cursor=${encodeURIComponent(cursor)}` :
          "/api/v1/groups?limit=20";
        const page = await request(path);
        if (generation !== groupGeneration) return;
        for (const group of page.groups) refreshed.set(group.id, group);
        cursor = page.next_cursor || "";
        hasMore = page.has_more;
        pagesRead++;
        if (hasMore && !cursor) throw new Error("群列表同步中断，请稍后重试。");
      }
      groups.clear();
      for (const group of refreshed.values()) groups.set(group.id, group);
      groupCursor = cursor;
      groupHasMore = hasMore;
      groupVisiblePages = pagesRead;
      renderGroups();
    } while (groupRefreshAgain && generation === groupGeneration);
  })();
  groupRefreshPromise = work;
  try {
    await work;
    if (generation === groupGeneration) {
      scheduleGroupSync();
      if (groupListNeedsRefreshID && groups.has(groupListNeedsRefreshID)) {
        groupListNeedsRefreshID = "";
        if (activeConversationKind === "group") updateGroupComposer();
      }
    }
  } catch (error) {
    if (generation === groupGeneration && !error.stale) nextGroupSyncAt = 0;
    throw error;
  } finally {
    if (groupRefreshPromise === work) {
      groupRefreshPromise = null;
      if (groupRefreshAgain && generation === groupGeneration) refreshGroups().catch(report);
    }
  }
}

async function loadMoreGroups() {
  if (groupRefreshPromise) {
    try { await groupRefreshPromise; } catch (error) { report(error); return; }
  }
  if (!actingMembership || !groupHasMore || !groupCursor) return;
  if (groupPagePromise) return groupPagePromise;
  const generation = groupGeneration;
  const cursor = groupCursor;
  loadMoreGroupsButton.disabled = true;
  const work = (async () => {
    const page = await request(`/api/v1/groups?limit=20&cursor=${encodeURIComponent(cursor)}`);
    if (generation !== groupGeneration) return;
    for (const group of page.groups) groups.set(group.id, group);
    groupCursor = page.next_cursor || "";
    groupHasMore = page.has_more;
    groupVisiblePages++;
    renderGroups();
  })();
  groupPagePromise = work;
  try { await work; } catch (error) { report(error); }
  finally {
    if (groupPagePromise === work) {
      groupPagePromise = null;
      loadMoreGroupsButton.disabled = false;
      if (groupRefreshAfterPage && generation === groupGeneration) {
        groupRefreshAfterPage = false;
        refreshGroups().catch(report);
      }
    }
  }
}

function renderConversations() {
  conversationList.replaceChildren();
  for (const chat of conversations.values()) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "conversation-button" + (activeConversationKind === "direct" && activeConversation === chat.id ? " active" : "");
    const name = document.createElement("strong");
    name.textContent = chat.name;
    const org = document.createElement("small");
    org.textContent = chat.organization;
    button.append(name, org);
    button.addEventListener("click", () => activateConversation(chat.id));
    conversationList.append(button);
  }
  loadMoreConversationsButton.classList.toggle("hidden", !inboxHasMore);
  if (activeConversationKind === "direct" && activeConversation && conversations.has(activeConversation)) {
    const active = conversations.get(activeConversation);
    element("chat-title").textContent = active.name;
    element("chat-subtitle").textContent = active.organization + " · 单聊";
  }
}

function activateConversation(id) {
  if (!conversations.has(id) || !canSwitchChat()) return;
  closeGroupRoster();
  clearSyncRetry();
  activeConversation = id;
  activeConversationKind = "direct";
  conversationEpoch++;
  afterSeq = 0;
  syncPromise = null;
  syncAgain = false;
  pendingMessage = null;
  messages.replaceChildren();
  const chat = conversations.get(id);
  element("chat-mode").textContent = "DIRECT MESSAGE";
  element("chat-title").textContent = chat.name;
  element("chat-subtitle").textContent = chat.organization + " · 单聊";
  messageText.disabled = false;
  messageText.readOnly = false;
  sendButton.disabled = false;
  discardPendingButton.classList.add("hidden");
  messageText.value = "";
  element("send-hint").textContent = "服务端保存成功后显示“已保存”，不代表对方已收到。";
  renderConversations();
  renderGroups();
  retentionRecords.contextChanged();
  legalHoldRecords.contextChanged();
  syncMessages().catch(report);
}

function activateGroupHistory(id) {
  if (!groups.has(id) || !actingMembership || !canSwitchChat()) return;
  closeGroupRoster();
  clearSyncRetry();
  activeConversation = id;
  activeConversationKind = "group";
  conversationEpoch++;
  afterSeq = 0;
  syncPromise = null;
  syncAgain = false;
  pendingMessage = null;
  groupPolicyRecheckNotice = null;
  messages.replaceChildren();
  const group = groups.get(id);
  element("chat-mode").textContent = "GROUP HISTORY";
  element("chat-title").textContent = group.name;
  element("chat-subtitle").textContent = "群聊历史" +
    (group.status === "policy_blocked" ? " · 策略暂停" : "");
  messageText.value = "";
  discardPendingButton.classList.add("hidden");
  renderConversations();
  renderGroups();
  retentionRecords.contextChanged();
  legalHoldRecords.contextChanged();
  syncMessages().catch(report);
}

function updateGroupComposer() {
  if (activeConversationKind !== "group") return;
  const group = groups.get(activeConversation);
  const canSendNew = !!group && group.status === "active" &&
    group.source_membership_id === actingMembership && !groupLeaveDialog.open &&
    groupListNeedsRefreshID !== activeConversation;
  element("chat-mode").textContent = canSendNew ? "GROUP CHAT" : "GROUP HISTORY";
  if (pendingMessage && pendingMessage.chatKind === "group" &&
      pendingMessage.chatID === activeConversation) {
    messageText.disabled = !!pendingMessage.sending;
    messageText.readOnly = true;
    sendButton.disabled = !!pendingMessage.sending;
    return;
  }
  messageText.disabled = !canSendNew;
  messageText.readOnly = false;
  sendButton.disabled = !canSendNew;
  discardPendingButton.classList.add("hidden");
  element("send-hint").textContent = !group ? "群已不在当前列表，无法发送新消息。" :
    group.status !== "active" ? "群通信已暂停，仅可查看历史消息。" :
      !canSendNew ? "请先切换到此群的来源任职，再发送群消息。" :
        "服务端保存成功后显示“已保存”，不代表群成员已收到。";
}

function appendMessage(message) {
  const empty = messages.querySelector(".empty-state");
  if (empty) empty.remove();
  const item = document.createElement("div");
  item.className = "message" + (message.redacted ? " redacted" : "") +
    (message.sender_user_id === self.user_id ? " mine" : "");
  const meta = document.createElement("span");
  meta.className = "message-meta";
  meta.textContent = `#${message.seq}` + (message.server_time ? ` · ${new Date(message.server_time).toLocaleString("zh-CN")}` : "");
  const bubble = document.createElement("div");
  bubble.className = "message-bubble";
  bubble.textContent = message.redacted ? "此消息当前不可见" : message.text;
  item.append(meta, bubble);
  messages.append(item);
  messages.scrollTop = messages.scrollHeight;
}

async function syncMessages() {
  if (!activeConversation || !actingMembership) return;
  if (syncPromise) {
    syncAgain = true;
    return syncPromise;
  }
  const chatID = activeConversation;
  const chatKind = activeConversationKind;
  const selectedMembership = actingMembership;
  const selectedEpoch = identityEpoch;
  const selectedConversationEpoch = conversationEpoch;
  const currentSync = (async () => {
    do {
      syncAgain = false;
      let more = true;
      while (more && activeConversation === chatID && actingMembership === selectedMembership &&
          identityEpoch === selectedEpoch && conversationEpoch === selectedConversationEpoch) {
        const resource = chatKind === "group" ? "groups" : "conversations";
        const page = await request(`/api/v1/${resource}/${encodeURIComponent(chatID)}/messages?after_seq=${afterSeq}&limit=100`);
        if (activeConversation !== chatID || actingMembership !== selectedMembership ||
            identityEpoch !== selectedEpoch || conversationEpoch !== selectedConversationEpoch) break;
        let expectedSeq = afterSeq + 1;
        if (!Array.isArray(page.messages)) throw new Error("消息同步中断，请稍后重试。");
        for (const message of page.messages) {
          if (!Number.isSafeInteger(message.seq) || message.seq !== expectedSeq++) {
            throw new Error("消息同步中断，请稍后重试。");
          }
        }
        if (page.next_after_seq !== expectedSeq - 1) {
          throw new Error("消息同步中断，请稍后重试。");
        }
        for (const message of page.messages) {
          if (message.seq > afterSeq) appendMessage(message);
        }
        if (page.next_after_seq < afterSeq || (page.has_more && page.next_after_seq === afterSeq)) {
          throw new Error("消息同步中断，请稍后重试。");
        }
        afterSeq = page.next_after_seq;
        more = page.has_more;
      }
    } while (syncAgain && activeConversation === chatID && actingMembership === selectedMembership &&
        identityEpoch === selectedEpoch && conversationEpoch === selectedConversationEpoch);
  })();
  syncPromise = currentSync;
  try {
    await currentSync;
    if (activeConversation === chatID && actingMembership === selectedMembership &&
        identityEpoch === selectedEpoch && conversationEpoch === selectedConversationEpoch) clearSyncRetry();
  } catch (error) {
    if (!error.stale && activeConversation === chatID && actingMembership === selectedMembership &&
        identityEpoch === selectedEpoch && conversationEpoch === selectedConversationEpoch) {
      scheduleSyncRetry(chatID, selectedMembership, selectedEpoch, selectedConversationEpoch);
    }
    throw error;
  } finally { if (syncPromise === currentSync) syncPromise = null; }
}

function clearSyncRetry() {
  if (syncRetryTimer) clearTimeout(syncRetryTimer);
  syncRetryTimer = null;
  syncRetryDelay = 1000;
}

function scheduleSyncRetry(chatID, membershipID, selectedEpoch, selectedConversationEpoch) {
  if (syncRetryTimer) return;
  const delay = syncRetryDelay;
  syncRetryDelay = Math.min(delay * 2, 30000);
  syncRetryTimer = setTimeout(() => {
    syncRetryTimer = null;
    if (activeConversation === chatID && actingMembership === membershipID &&
        identityEpoch === selectedEpoch && conversationEpoch === selectedConversationEpoch) {
      syncMessages().catch(report);
    }
  }, delay);
}

async function sendMessage(event) {
  event.preventDefault();
  if (!activeConversation || !["direct", "group"].includes(activeConversationKind)) return;
  const chatID = activeConversation;
  const chatKind = activeConversationKind;
  if (pendingMessage && pendingMessage.sending) return;
  if (!pendingMessage || pendingMessage.chatID !== chatID || pendingMessage.chatKind !== chatKind) {
    if (chatKind === "group") {
      const group = groups.get(chatID);
      if (!group || group.status !== "active" || group.source_membership_id !== actingMembership) return;
    }
    const text = messageText.value.trim();
    if (!text) return notify("请输入消息内容。");
    if (new TextEncoder().encode(text).length > 16384) return notify("消息正文最多 16384 字节。");
    pendingMessage = { id: uuidV7(), text, chatID, chatKind, sending: false };
  }
  const submitted = pendingMessage;
  const selectedMembership = actingMembership;
  submitted.sending = true;
  sendButton.disabled = true;
  messageText.disabled = true;
  element("send-hint").textContent = "正在保存…";
  try {
    const resource = chatKind === "group" ? "groups" : "conversations";
    await request(`/api/v1/${resource}/${encodeURIComponent(chatID)}/messages`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ client_msg_id: submitted.id, text: submitted.text }),
    });
    if (actingMembership !== selectedMembership || activeConversation !== chatID ||
        activeConversationKind !== chatKind || pendingMessage !== submitted) return;
    messageText.value = "";
    messageText.readOnly = false;
    discardPendingButton.classList.add("hidden");
    pendingMessage = null;
    if (chatKind === "group") {
      updateGroupComposer();
      if (!messageText.disabled) element("send-hint").textContent = "已保存到服务器；群成员送达和已读状态尚不可用。";
      refreshGroups().catch(report);
    } else {
      messageText.disabled = false;
      element("send-hint").textContent = "已保存到服务器；对方送达和已读状态尚不可用。";
      refreshInbox().catch(report);
    }
    try { await syncMessages(); } catch (error) { report(error); }
  } catch (error) {
    if (error.stale || actingMembership !== selectedMembership || activeConversation !== chatID ||
        activeConversationKind !== chatKind || pendingMessage !== submitted) return;
    if (chatKind === "group" && [400, 404, 409, 410, 429].includes(error.status)) {
      pendingMessage = null;
      messageText.readOnly = false;
      discardPendingButton.classList.add("hidden");
      messageText.disabled = true;
      sendButton.disabled = true;
      if (error.code === "group_policy_blocked" && groups.has(chatID)) {
        groups.get(chatID).status = "policy_blocked";
        renderGroups();
      }
      try { await refreshGroups(); } catch (refreshError) { report(refreshError); }
      if (error.status === 410) syncMessages().catch(report);
      report(error);
      return;
    }
    messageText.disabled = false;
    messageText.readOnly = true;
    discardPendingButton.classList.remove("hidden");
    element("send-hint").textContent = "发送结果未确认；点击发送将用同一编号重试。";
    if (chatKind === "group") refreshGroups().catch(report);
    report(error);
  } finally {
    submitted.sending = false;
    if (actingMembership === selectedMembership && activeConversation === chatID &&
        activeConversationKind === chatKind) {
      if (chatKind === "group") updateGroupComposer();
      else {
        messageText.disabled = false;
        sendButton.disabled = false;
      }
    }
  }
}

function discardPending() {
  if (!pendingMessage || pendingMessage.sending) return;
  pendingMessage = null;
  messageText.readOnly = false;
  discardPendingButton.classList.add("hidden");
  if (activeConversationKind === "group") updateGroupComposer();
  element("send-hint").textContent = "已放弃待确认消息；此前请求可能已保存，可先补拉核对。";
  syncMessages().catch(report);
}

function stopRealtime() {
  realtimeEpoch++;
  ticketInFlight = false;
  if (reconnectTimer) clearTimeout(reconnectTimer);
  reconnectTimer = null;
  if (realtimeSocket) realtimeSocket.close();
  realtimeSocket = null;
  connectionState.textContent = "未连接";
  connectionState.classList.remove("online");
}

async function connectRealtime() {
  if (!accessToken || !actingMembership || realtimeUnsupported || realtimeSocket || ticketInFlight) return;
  const selectedMembership = actingMembership;
  const selectedEpoch = realtimeEpoch;
  ticketInFlight = true;
  try {
    const ticket = await request("/api/v1/realtime/tickets", { method: "POST" });
    if (selectedEpoch !== realtimeEpoch || selectedMembership !== actingMembership || !accessToken) return;
    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    const socket = new WebSocket(`${protocol}//${window.location.host}/api/v1/realtime`,
      ["enterprise-im.v1", `ticket.${ticket.ticket}`]);
    realtimeSocket = socket;
    socket.onopen = () => {
      connectionState.textContent = "实时通知已连接";
      connectionState.classList.add("online");
    };
    socket.onmessage = (event) => {
      if (realtimeSocket !== socket || selectedEpoch !== realtimeEpoch ||
          selectedMembership !== actingMembership) return;
      let frame;
      try { frame = JSON.parse(event.data); } catch (_) { return; }
      if (frame.type === "ready" || frame.type === "sync_required") {
        scheduleSafetySync();
        refreshInbox().catch(report);
        refreshGroups().catch(report);
        syncMessages().catch(report);
      }
    };
    socket.onclose = () => {
      if (realtimeSocket !== socket) return;
      realtimeSocket = null;
      nextSafetySyncAt = 0;
      connectionState.textContent = "定时同步中";
      connectionState.classList.remove("online");
      if (actingMembership && accessToken) reconnectTimer = setTimeout(connectRealtime, 3000);
    };
  } catch (error) {
    if (error.stale || selectedEpoch !== realtimeEpoch || selectedMembership !== actingMembership) return;
    if (error.status === 404) realtimeUnsupported = true;
    connectionState.textContent = "定时同步中";
    connectionState.classList.remove("online");
    if (!realtimeUnsupported && actingMembership && accessToken) reconnectTimer = setTimeout(connectRealtime, 5000);
  } finally {
    if (selectedEpoch === realtimeEpoch) ticketInFlight = false;
  }
}

loginButton.disabled = true;
loginButton.addEventListener("click", startLogin);
element("logout-button").addEventListener("click", () => logout());
element("search-button").addEventListener("click", searchPeople);
loadMoreConversationsButton.addEventListener("click", loadMoreInbox);
loadMoreGroupsButton.addEventListener("click", loadMoreGroups);
groupCreateOpenButton.addEventListener("click", openGroupCreate);
element("group-create-cancel").addEventListener("click", () => closeGroupCreate());
groupCreateDiscard.addEventListener("click", discardGroupCreate);
element("group-create-search").addEventListener("click", searchGroupCreatePeople);
groupCreateQuery.addEventListener("keydown", (event) => {
  if (event.key === "Enter") { event.preventDefault(); searchGroupCreatePeople(); }
});
groupCreateQuery.addEventListener("input", () => {
  groupCreateSearchSerial++;
  groupCreateSearchResults = [];
  renderGroupCreate();
});
groupCreateName.addEventListener("input", renderGroupCreate);
element("group-create-form").addEventListener("submit", submitGroupCreate);
groupCreateDialog.addEventListener("cancel", (event) => {
  event.preventDefault();
  closeGroupCreate();
});
groupInviteOpenButton.addEventListener("click", openGroupInvite);
element("group-invite-cancel").addEventListener("click", () => closeGroupInvite());
groupInviteDiscard.addEventListener("click", discardGroupInvite);
element("group-invite-search").addEventListener("click", searchGroupInvitePeople);
groupInviteQuery.addEventListener("keydown", (event) => {
  if (event.key === "Enter") { event.preventDefault(); searchGroupInvitePeople(); }
});
groupInviteQuery.addEventListener("input", () => {
  groupInviteSearchSerial++;
  groupInviteSearchResults = [];
  renderGroupInvite();
});
element("group-invite-form").addEventListener("submit", submitGroupInvite);
groupInviteDialog.addEventListener("cancel", (event) => {
  event.preventDefault();
  closeGroupInvite();
});
groupLeaveOpenButton.addEventListener("click", openGroupLeave);
element("group-leave-cancel").addEventListener("click", () => closeGroupLeave());
groupLeaveConfirm.addEventListener("click", submitGroupLeave);
groupLeaveRetry.addEventListener("click", submitGroupLeave);
groupLeaveDiscard.addEventListener("click", discardGroupLeave);
groupLeaveDialog.addEventListener("cancel", (event) => {
  event.preventDefault();
  closeGroupLeave();
});
groupRosterOpenButton.addEventListener("click", openGroupRoster);
groupPolicyRecheckButton.addEventListener("click", recheckGroupPolicy);
groupRosterLoadMoreButton.addEventListener("click", () => loadGroupRosterPage(groupRosterCursor));
groupRosterRetryButton.addEventListener("click", () => loadGroupRosterPage(groupRosterCursor));
element("group-roster-close").addEventListener("click", closeGroupRoster);
groupRosterDialog.addEventListener("cancel", (event) => {
  event.preventDefault();
  closeGroupRoster();
});
element("group-remove-cancel").addEventListener("click", () => closeGroupRemoval());
groupRemoveConfirm.addEventListener("click", submitGroupRemoval);
groupRemoveRetry.addEventListener("click", submitGroupRemoval);
groupRemoveDiscard.addEventListener("click", discardGroupRemoval);
groupRemoveDialog.addEventListener("cancel", (event) => {
  event.preventDefault();
  closeGroupRemoval();
});
element("group-transfer-cancel").addEventListener("click", () => closeGroupTransfer());
groupTransferConfirm.addEventListener("click", submitGroupTransfer);
groupTransferRetry.addEventListener("click", submitGroupTransfer);
groupTransferDiscard.addEventListener("click", discardGroupTransfer);
groupTransferDialog.addEventListener("cancel", (event) => {
  event.preventDefault();
  closeGroupTransfer();
});
window.addEventListener("storage", (event) => {
  if (self && actingMembership && groupRosterDialog.open &&
      (!event.key || event.key.startsWith(groupRemovalStoragePrefix()) ||
        event.key.startsWith(groupTransferStoragePrefix()))) closeGroupRoster();
  renderPendingGroupInvites();
  renderGroupInviteAction();
  renderPendingGroupLeaves();
  renderPendingGroupRemovals();
  renderPendingGroupTransfers();
});
element("person-query").addEventListener("keydown", (event) => {
  if (event.key === "Enter") { event.preventDefault(); searchPeople(); }
});
element("composer").addEventListener("submit", sendMessage);
discardPendingButton.addEventListener("click", discardPending);
messageText.addEventListener("keydown", (event) => {
  if (event.key === "Enter" && !event.shiftKey) { event.preventDefault(); element("composer").requestSubmit(); }
});

(async () => {
  const callbackQuery = window.location.search;
  if (callbackQuery) history.replaceState({}, "", "/web/");
  try {
    const response = await fetch("/web/config", { cache: "no-store" });
    if (!response.ok) throw new Error("登录配置暂不可用。");
    config = await response.json();
    if (window.location.origin !== new URL(config.redirect_url).origin ||
        window.location.pathname !== new URL(config.redirect_url).pathname) {
      throw new Error("当前访问地址与企业登录回调地址不一致。");
    }
    loginButton.disabled = false;
    loginHint.textContent = "使用集团统一身份安全登录。";
    if (callbackQuery) await finishLogin(callbackQuery);
  } catch (error) { loginHint.textContent = error.message; }
})();
