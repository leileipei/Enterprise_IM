"use strict";

// Search results are separate from the synchronization timeline and stay in memory.
window.MessageSearch = class {
  constructor(request, context, validTime, canOpen) {
    this.mode="text";this.fileSearch=null;
    this.request = request; this.context = context; this.validTime = validTime; this.canOpen = canOpen;
    const el = id => document.getElementById(`message-search-${id}`);
    this.entry = el("open"); this.dialog = el("dialog"); this.query = el("query"); this.apply = el("apply");
    this.refresh = el("refresh"); this.more = el("more"); this.list = el("list"); this.hint = el("hint"); this.label = el("context");
    this.modeSelect=el("mode");this.modeSelect?.addEventListener("change",()=>this.setMode(this.modeSelect.value));
    this.generation = 0; this.loading = false; this.records = []; this.cursor = ""; this.cursors = new Set(); this.controller = null;
    this.entry.addEventListener("click", () => this.open());
    el("close").addEventListener("click", () => this.close());
    this.dialog.addEventListener("cancel", e => { e.preventDefault(); this.close(); });
    this.dialog.addEventListener("close", () => { if (!this.dialog.open) this.clear(true); });
    el("form").addEventListener("submit", e => { e.preventDefault(); this.load(true); });
    this.refresh.addEventListener("click", () => this.load(true));
    this.more.addEventListener("click", () => this.load(false));
    this.query.addEventListener("input", () => { this.clear(); this.hint.textContent = "关键词已变化，请重新搜索。"; });
    this.update();
  }

  available() {
    const c = this.context();
    return !!c.identityKey && !!c.conversation && ["direct", "group"].includes(c.kind);
  }
  setMode(mode){if(!["text","file"].includes(mode) || (mode==="file" && !this.context().filenameSearchEnabled))mode="text";this.mode=mode;if(this.modeSelect)this.modeSelect.value=mode;this.clear();this.hint.textContent=mode==="file"?"仅搜索文件名称，不搜索正文或附件说明。":"请输入正文关键词。";}
  clear(reset = false) {
    this.fileSearch?.clear();
    this.generation++; this.controller?.abort(); this.controller = null; this.loading = false;
    this.records = []; this.cursor = ""; this.cursors = new Set(); this.list.replaceChildren(); this.hint.textContent = "";
    if (reset) { this.query.value = ""; this.label.textContent = ""; }
    this.update();
  }
  close() { this.clear(true); if (this.dialog.open) this.dialog.close(); }
  contextChanged() { this.setMode("text");this.close(); }
  update() {
    this.modeSelect?.classList.toggle("hidden",!this.context().filenameSearchEnabled);
    if(this.mode==="file"){this.fileSearch?.update();return;}
    const available = this.available();
    this.entry.classList.toggle("hidden", !available);
    this.query.disabled = !available; this.apply.disabled = !available || this.loading; this.refresh.disabled = !available || this.loading;
    this.more.classList.toggle("hidden", !available || !this.cursor); this.more.disabled = this.loading;
    this.list.setAttribute("aria-busy", String(this.loading));
  }
  open() {
    if (!this.available() || !this.canOpen()) return;
    this.clear(true);
    const c = this.context();
    this.label.textContent = `${c.kind === "group" ? "群聊" : "单聊"}：${c.title} · 查询任职：${c.membershipId}`;
    this.dialog.showModal(); this.hint.textContent = "输入 2～100 字的关键词，查询当前可见正文。"; this.query.focus();
  }
  unicode(value) { return typeof value === "string" && ![...value].some(c => { const n = c.codePointAt(0); return n >= 0xd800 && n <= 0xdfff; }); }
  trim(value) {
    // Match Go unicode.IsSpace: include U+0085 and preserve non-space U+FEFF.
    return value.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, "");
  }
  lower(value) {
    // Go uses simple per-code-point lowercase, unlike JS contextual/full casing.
    return [...value].map(c => c === "İ" ? "i" : c.toLowerCase()).join("");
  }
  validate(page, current, cursor, query) {
    const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
    if (!page || typeof page.conversation_id !== "string" || !uuid.test(page.conversation_id) || page.conversation_id.toLowerCase() !== current.conversation.toLowerCase() ||
        !Array.isArray(page.messages) || page.messages.length > 20 || typeof page.has_more !== "boolean" || typeof page.next_cursor !== "string" ||
        page.next_cursor.length > 2048 || page.has_more !== !!page.next_cursor ||
        (page.next_cursor && (page.next_cursor === cursor || this.cursors.has(page.next_cursor)))) throw new Error("invalid search page");
    const ids = new Set(cursor ? this.records.map(m => m.id.toLowerCase()) : []);
    let after = cursor && this.records.length ? BigInt(this.records.at(-1).seq) : 0n;
    for (const m of page.messages) {
      if (!m || typeof m.id !== "string" || !uuid.test(m.id) || ids.has(m.id.toLowerCase()) || typeof m.seq !== "string" || !/^[1-9][0-9]{0,18}$/.test(m.seq) ||
          BigInt(m.seq) > 9223372036854775807n || BigInt(m.seq) <= after || typeof m.sender_user_id !== "string" || !uuid.test(m.sender_user_id) ||
          !this.unicode(m.text) || !this.trim(m.text) || new TextEncoder().encode(m.text).length > 16384 || !this.lower(m.text).includes(query) ||
          Object.hasOwn(m, "redacted") || !this.validTime(m.server_time)) throw new Error("invalid search match");
      ids.add(m.id.toLowerCase()); after = BigInt(m.seq);
    }
  }
  async load(first) {
    if(this.mode==="file")return this.fileSearch?.load(first);
    if (!this.dialog.open || !this.available() || this.loading || (!first && !this.cursor)) return;
    const raw = this.query.value, value = this.trim(raw);
    if (!this.unicode(value) || value.includes("\0") || [...value].length < 2 || [...value].length > 100) {
      this.clear(); this.hint.textContent = "关键词须为 2～100 字，且不能包含无效字符。"; return;
    }
    const current = this.context(), query = this.lower(value), cursor = first ? "" : this.cursor;
    if (first) this.clear();
    const generation = ++this.generation;
    const isCurrent = () => {
      const c = this.context();
      return this.dialog.open && this.available() && generation === this.generation && current.identityKey === c.identityKey &&
        current.conversation === c.conversation && current.conversationEpoch === c.conversationEpoch && current.kind === c.kind && this.query.value === raw;
    };
    this.controller = new AbortController();
    const controller = this.controller;
    this.loading = true; this.hint.textContent = "正在搜索当前可见正文…"; this.update();
    const params = new URLSearchParams({ q: value, limit: "20" }); if (cursor) params.set("cursor", cursor);
    const kind = current.kind === "group" ? "groups" : "conversations";
    try {
      const page = await this.request(`/api/v1/${kind}/${current.conversation}/messages/search?${params}`, { signal: AbortSignal.any([controller.signal, AbortSignal.timeout(15000)]) });
      if (!isCurrent()) return;
      this.validate(page, current, cursor, query);
      this.records.push(...page.messages.map(m => ({ id: m.id, seq: m.seq, sender_user_id: m.sender_user_id, text: m.text, server_time: m.server_time })));
      this.cursor = page.next_cursor; if (this.cursor) this.cursors.add(this.cursor); this.render();
      this.hint.textContent = !page.messages.length && this.cursor ?
        `本页未找到匹配正文${this.records.length ? `，已显示 ${this.records.length} 条` : ""}；仍可继续查询。` :
        this.records.length ? `已显示 ${this.records.length} 条匹配消息` + (this.cursor ? "，可继续查询。" : "，已到末页。") : "未找到当前可见正文中的匹配消息，已到末页。";
    } catch (error) {
      if (!isCurrent() || error.stale) return;
      this.records = []; this.cursor = ""; this.cursors = new Set(); this.list.replaceChildren();
      this.hint.textContent = error.status === 403 || error.status === 404 ? "当前会话不可搜索或无权限，已清空结果。" : "搜索失败，已清空结果；请重新搜索或刷新，从首页重试。";
    } finally {
      if (isCurrent()) { this.controller = null; this.loading = false; this.update(); }
    }
  }
  render() {
    this.list.replaceChildren();
    for (const m of this.records) {
      const card = document.createElement("li"); card.className = "message-search-card retention-record-card";
      const title = document.createElement("strong"); title.textContent = `消息序号 ${m.seq}`;
      const body = document.createElement("p"); body.className = "message-search-body"; body.textContent = m.text;
      const details = document.createElement("dl");
      for (const [label, value] of [["发送者 ID", m.sender_user_id], ["服务端时间", m.server_time]]) {
        const dt = document.createElement("dt"), dd = document.createElement("dd"); dt.textContent = label; dd.textContent = value; details.append(dt, dd);
      }
      card.append(title, body, details); this.list.append(card);
    }
  }
};
