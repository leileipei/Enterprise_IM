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

function notify(message) {
  noticeBox.textContent = message;
  noticeBox.classList.remove("hidden");
  clearTimeout(noticeTimer);
  noticeTimer = setTimeout(() => noticeBox.classList.add("hidden"), 4500);
}

function report(error) {
  if (!error.stale) notify(error.message);
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
  if (response.status === 403 && needsMembership) {
    selectMembership("");
    throw new Error("当前任职已失效，请重新选择");
  }
  if (!response.ok) {
    const error = new Error(response.status === 404 ? "目标不可用或无权限" :
      response.status === 429 ? "操作太频繁，请稍后再试" : "服务暂时不可用，请稍后重试");
    error.status = response.status;
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
    if (actingMembership && (!realtimeSocket || realtimeSocket.readyState !== WebSocket.OPEN)) {
      refreshInbox().catch(report);
      if (activeConversation) syncMessages().catch(report);
    }
  }, 5000);
}

function logout(message = "已退出当前页面。") {
  stopRealtime();
  clearSyncRetry();
  identityEpoch++;
  if (pollingTimer) clearInterval(pollingTimer);
  pollingTimer = null;
  sessionStorage.removeItem("enterprise-im-login");
  accessToken = "";
  tokenExpiresAt = 0;
  self = null;
  actingMembership = "";
  activeConversation = null;
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
  identityOptions.replaceChildren();
  results.replaceChildren();
  conversationList.replaceChildren();
  loadMoreConversationsButton.classList.add("hidden");
  resetChat();
  workspace.classList.add("hidden");
  loginView.classList.remove("hidden");
  loginHint.textContent = message;
}

function renderMemberships() {
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
    button.addEventListener("click", () => selectMembership(membership.id));
    identityOptions.append(button);
  }
}

function selectMembership(id) {
  if (id === actingMembership) return;
  stopRealtime();
  clearSyncRetry();
  identityEpoch++;
  actingMembership = id;
  realtimeUnsupported = false;
  activeConversation = null;
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
  conversationList.replaceChildren();
  loadMoreConversationsButton.classList.add("hidden");
  results.replaceChildren();
  element("person-query").value = "";
  element("search-hint").textContent = id ? "输入姓名，查找当前任职下可见的同事。" : "请选择一个有效任职。";
  resetChat();
  renderMemberships();
  if (id) {
    refreshInbox().catch(report);
    connectRealtime();
  }
}

function resetChat() {
  messages.replaceChildren();
  const empty = document.createElement("div");
  empty.className = "empty-state";
  const title = document.createElement("strong");
  title.textContent = "沟通从这里开始";
  const detail = document.createElement("span");
  detail.textContent = "选择任职，查找同事，再发起单聊。";
  empty.append(title, detail);
  messages.append(empty);
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

async function openChat(person, member) {
  if (!actingMembership) return notify("请先选择任职。");
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
      if (activeID && !conversations.has(activeID)) {
        conversations.set(activeID, { id: activeID, name: "当前会话", organization: "历史会话", peerVisible: false });
      }
      inboxCursor = page.next_cursor || "";
      inboxHasMore = page.has_more;
      renderConversations();
    } while (inboxRefreshAgain && generation === inboxGeneration);
  })();
  inboxRefreshPromise = work;
  try { await work; }
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

function renderConversations() {
  conversationList.replaceChildren();
  for (const chat of conversations.values()) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "conversation-button" + (activeConversation === chat.id ? " active" : "");
    const name = document.createElement("strong");
    name.textContent = chat.name;
    const org = document.createElement("small");
    org.textContent = chat.organization;
    button.append(name, org);
    button.addEventListener("click", () => activateConversation(chat.id));
    conversationList.append(button);
  }
  loadMoreConversationsButton.classList.toggle("hidden", !inboxHasMore);
  if (activeConversation && conversations.has(activeConversation)) {
    const active = conversations.get(activeConversation);
    element("chat-title").textContent = active.name;
    element("chat-subtitle").textContent = active.organization + " · 单聊";
  }
}

function activateConversation(id) {
  if (!conversations.has(id)) return;
  clearSyncRetry();
  activeConversation = id;
  conversationEpoch++;
  afterSeq = 0;
  syncPromise = null;
  syncAgain = false;
  pendingMessage = null;
  messages.replaceChildren();
  const chat = conversations.get(id);
  element("chat-title").textContent = chat.name;
  element("chat-subtitle").textContent = chat.organization + " · 单聊";
  messageText.disabled = false;
  messageText.readOnly = false;
  sendButton.disabled = false;
  discardPendingButton.classList.add("hidden");
  messageText.value = "";
  renderConversations();
  syncMessages().catch(report);
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
  const selectedMembership = actingMembership;
  const selectedEpoch = identityEpoch;
  const selectedConversationEpoch = conversationEpoch;
  const currentSync = (async () => {
    do {
      syncAgain = false;
      let more = true;
      while (more && activeConversation === chatID && actingMembership === selectedMembership &&
          identityEpoch === selectedEpoch && conversationEpoch === selectedConversationEpoch) {
        const page = await request(`/api/v1/conversations/${encodeURIComponent(chatID)}/messages?after_seq=${afterSeq}&limit=100`);
        if (activeConversation !== chatID || actingMembership !== selectedMembership ||
            identityEpoch !== selectedEpoch || conversationEpoch !== selectedConversationEpoch) break;
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
  if (!activeConversation) return;
  if (!pendingMessage || pendingMessage.chatID !== activeConversation) {
    const text = messageText.value.trim();
    if (!text) return notify("请输入消息内容。");
    pendingMessage = { id: uuidV7(), text, chatID: activeConversation };
  }
  const submitted = pendingMessage;
  const selectedMembership = actingMembership;
  sendButton.disabled = true;
  messageText.disabled = true;
  element("send-hint").textContent = "正在保存…";
  try {
    await request(`/api/v1/conversations/${encodeURIComponent(activeConversation)}/messages`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ client_msg_id: submitted.id, text: submitted.text }),
    });
    if (actingMembership !== selectedMembership || activeConversation !== submitted.chatID || pendingMessage !== submitted) return;
    messageText.value = "";
    messageText.readOnly = false;
    messageText.disabled = false;
    discardPendingButton.classList.add("hidden");
    pendingMessage = null;
    element("send-hint").textContent = "已保存到服务器；对方送达和已读状态尚不可用。";
    refreshInbox().catch(report);
    try { await syncMessages(); } catch (error) { report(error); }
  } catch (error) {
    if (error.stale || actingMembership !== selectedMembership || activeConversation !== submitted.chatID) return;
    messageText.disabled = false;
    messageText.readOnly = true;
    discardPendingButton.classList.remove("hidden");
    element("send-hint").textContent = "发送结果未确认；点击发送将用同一编号重试。";
    report(error);
  } finally {
    if (actingMembership === selectedMembership && activeConversation === submitted.chatID) {
      messageText.disabled = false;
      sendButton.disabled = false;
    }
  }
}

function discardPending() {
  if (!pendingMessage) return;
  pendingMessage = null;
  messageText.readOnly = false;
  discardPendingButton.classList.add("hidden");
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
      let frame;
      try { frame = JSON.parse(event.data); } catch (_) { return; }
      if (frame.type === "ready" || frame.type === "sync_required") {
        refreshInbox().catch(report);
        syncMessages().catch(report);
      }
    };
    socket.onclose = () => {
      if (realtimeSocket !== socket) return;
      realtimeSocket = null;
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
