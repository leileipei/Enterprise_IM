"use strict";

// Read-only tenant audit metadata. Filters and records remain in page memory.
window.AuditRecords = class {
  constructor(request, context, validTime, canOpen) {
    this.request = request; this.context = context; this.validTime = validTime; this.canOpen = canOpen;
    const el = id => document.getElementById(`audit-${id}`);
    this.entry = el("open"); this.dialog = el("dialog"); this.action = el("action"); this.outcome = el("outcome"); this.actor = el("actor"); this.from = el("from"); this.until = el("until");
    this.apply = el("apply"); this.refresh = el("refresh"); this.more = el("more");
    this.list = el("list"); this.hint = el("hint"); this.label = el("context");
    this.allowed = false; this.identityKey = ""; this.generation = 0; this.loading = false; this.cursor = ""; this.records = []; this.cursors = new Set();
    this.entry.addEventListener("click", () => this.open());
    el("close").addEventListener("click", () => this.close());
    this.dialog.addEventListener("cancel", e => { e.preventDefault(); this.close(); });
    this.dialog.addEventListener("close", () => { if (!this.dialog.open) this.clear(true); });
    el("form").addEventListener("submit", e => { e.preventDefault(); this.load(true); });
    this.refresh.addEventListener("click", () => this.load(true));
    this.more.addEventListener("click", () => this.load(false));
    const changed = () => { this.clear(); this.hint.textContent = "筛选已变化，请查询；翻页将从首页开始。"; };
    this.from.addEventListener("input", changed); this.until.addEventListener("input", changed); this.actor.addEventListener("input", changed); this.action.addEventListener("input", changed); this.outcome.addEventListener("change", changed);
    this.update();
  }

  clear(filters = false) {
    this.generation++; this.loading = false; this.cursor = ""; this.records = []; this.cursors = new Set();
    this.list.replaceChildren(); this.hint.textContent = "";
    if (filters) { this.from.value = ""; this.until.value = ""; this.actor.value = ""; this.action.value = ""; this.outcome.value = ""; this.label.textContent = ""; }
    this.update();
  }

  close() { this.clear(true); if (this.dialog.open) this.dialog.close(); }

  contextChanged() {
    const identity = this.context().identityKey;
    if (identity !== this.identityKey) { this.identityKey = identity; this.allowed = false; this.close(); }
  }

  setAccess(allowed) {
    this.allowed = allowed && !!this.identityKey;
    if (!this.allowed) this.close(); else this.update();
  }

  update() {
    this.entry.classList.toggle("hidden", !this.allowed);
    this.from.disabled = !this.allowed; this.until.disabled = !this.allowed; this.actor.disabled = !this.allowed; this.action.disabled = !this.allowed; this.outcome.disabled = !this.allowed;
    this.apply.disabled = !this.allowed || this.loading; this.refresh.disabled = !this.allowed || this.loading;
    this.more.classList.toggle("hidden", !this.allowed || !this.cursor); this.more.disabled = this.loading;
    this.list.setAttribute("aria-busy", String(this.loading));
  }

  open() {
    if (!this.canOpen() || !this.allowed) return;
    if (!this.dialog.open) this.dialog.showModal();
    const current = this.context();
    this.label.textContent = `当前集团（租户）：${current.tenantId} · 查询任职：${current.membershipId}`;
    this.load(true);
  }

  orderKey(e) {
    const fraction = /\.(\d{1,9})/.exec(e.occurred_at)?.[1] || "";
    // Date.parse loses sub-millisecond digits. Compare the original fractional precision.
    const time = BigInt(Date.parse(e.occurred_at.replace(/\.\d+/, ""))) * 1000000n + BigInt(fraction.padEnd(9, "0"));
    return { time, id: BigInt(e.id) };
  }

  filterTime(value) {
    if (!value) return null;
    const year = new Date(value).getUTCFullYear();
    if (!this.validTime(value) || (/\.(\d+)/.exec(value)?.[1].length || 0) > 6 || year < 1 || year > 9999) throw new Error("invalid audit filter time");
    return this.orderKey({ occurred_at: value, id: "1" }).time;
  }

  validate(page, cursor, action, outcome, actorUserID, from, until) {
    if (!page || !Array.isArray(page.events) || page.events.length > 20 || typeof page.next_cursor !== "string" ||
        page.next_cursor.length > 1024 || (page.next_cursor && (page.events.length !== 20 || page.next_cursor === cursor || this.cursors.has(page.next_cursor)))) throw new Error("invalid audit page");
    const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
    const seen = new Set(cursor ? this.records.map(e => e.id) : []);
    let before = cursor && this.records.length ? this.orderKey(this.records.at(-1)) : null;
    if (cursor && !before) throw new Error("missing audit boundary");
    for (const e of page.events) {
      if (!e || typeof e.id !== "string" || !/^[1-9][0-9]{0,18}$/.test(e.id) || BigInt(e.id) > 9223372036854775807n || seen.has(e.id) ||
          typeof e.actor_user_id !== "string" || !uuid.test(e.actor_user_id) || (actorUserID && e.actor_user_id.toLowerCase() !== actorUserID) || typeof e.acting_membership_id !== "string" || !uuid.test(e.acting_membership_id) ||
          typeof e.action !== "string" || !e.action || (action && e.action !== action) || typeof e.resource_type !== "string" || !e.resource_type ||
          (e.resource_id !== null && (typeof e.resource_id !== "string" || !uuid.test(e.resource_id))) ||
          !["allow", "deny"].includes(e.outcome) || (outcome && e.outcome !== outcome) || typeof e.reason !== "string" || !e.reason || !this.validTime(e.occurred_at)) throw new Error("invalid audit event");
      const key = this.orderKey(e);
      if ((from !== null && key.time < from) || (until !== null && key.time >= until)) throw new Error("audit event outside time range");
      if (before && (key.time > before.time || (key.time === before.time && key.id >= before.id))) throw new Error("invalid audit order");
      before = key; seen.add(e.id);
    }
  }

  async load(first) {
    if (!this.dialog.open || !this.allowed || this.loading || (!first && !this.cursor)) return;
    const snapshot = this.context(), action = this.action.value, outcome = this.outcome.value, actorValue = this.actor.value, actorUserID = actorValue.toLowerCase(), fromValue = this.from.value, untilValue = this.until.value;
    let from, until;
    try { from = this.filterTime(fromValue); until = this.filterTime(untilValue); }
    catch { this.clear(); this.hint.textContent = "时间格式应为带时区的 RFC3339 时间，小数秒最多 6 位。"; return; }
    if (from !== null && until !== null && from >= until) { this.clear(); this.hint.textContent = "开始时间须早于结束时间。"; return; }
    if (actorUserID && !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(actorUserID)) { this.clear(); this.hint.textContent = "执行人用户 ID 应为完整 UUID，或留空查询全部。"; return; }
    if (action && !/^[a-z][a-z0-9_]{0,63}$/.test(action)) { this.clear(); this.hint.textContent = "动作格式应为 1～64 个小写字母、数字或下划线，并以字母开头。"; return; }
    if (!["", "allow", "deny"].includes(outcome)) { this.clear(); this.hint.textContent = "请选择允许或拒绝结果。"; return; }
    const cursor = first ? "" : this.cursor, generation = ++this.generation;
    const isCurrent = () => this.dialog.open && this.allowed && generation === this.generation && !!snapshot.identityKey &&
      snapshot.identityKey === this.context().identityKey && this.action.value === action && this.outcome.value === outcome && this.actor.value === actorValue && this.from.value === fromValue && this.until.value === untilValue;
    if (first) { this.records = []; this.cursor = ""; this.cursors = new Set(); this.list.replaceChildren(); }
    this.loading = true; this.hint.textContent = "正在加载审计记录…"; this.update();
    const params = new URLSearchParams({ limit: "20" });
    if (fromValue) params.set("from", fromValue); if (untilValue) params.set("until", untilValue);
    if (actorUserID) params.set("actor_user_id", actorUserID);
    if (action) params.set("action", action); if (outcome) params.set("outcome", outcome); if (cursor) params.set("cursor", cursor);
    try {
      const page = await this.request(`/api/v1/admin/audit-events?${params}`, { signal: AbortSignal.timeout(15000) });
      if (!isCurrent()) return;
      this.validate(page, cursor, action, outcome, actorUserID, from, until);
      this.records.push(...page.events.map(e => ({ id: e.id, actor_user_id: e.actor_user_id, acting_membership_id: e.acting_membership_id,
        action: e.action, resource_type: e.resource_type, resource_id: e.resource_id, outcome: e.outcome, reason: e.reason, occurred_at: e.occurred_at })));
      this.cursor = page.next_cursor; if (this.cursor) this.cursors.add(this.cursor); this.render();
      this.hint.textContent = this.records.length ? `已显示 ${this.records.length} 条审计记录` + (this.cursor ? "，可继续加载。" : "，已到末页。") : "暂无审计记录。";
    } catch (error) {
      if (!isCurrent() || error.stale) return;
      this.records = []; this.cursor = ""; this.cursors = new Set(); this.list.replaceChildren();
      if (error.status === 403 || error.status === 404) { this.allowed = false; this.loading = false; this.hint.textContent = "权限已失效，已清空审计记录；请联系集团管理员核查。"; this.update(); }
      else this.hint.textContent = "审计记录加载失败，请刷新从首页重新查询。";
    } finally { if (isCurrent()) { this.loading = false; this.update(); } }
  }

  render() {
    this.list.replaceChildren();
    for (const e of this.records) {
      const card = document.createElement("li"); card.className = "audit-record-card retention-record-card";
      const title = document.createElement("strong"); title.textContent = `事件 ${e.id} · ${e.outcome === "allow" ? "允许" : "拒绝"}`;
      const details = document.createElement("dl");
      const field = (label, value) => { const dt = document.createElement("dt"), dd = document.createElement("dd"); dt.textContent = label; dd.textContent = value; details.append(dt, dd); };
      field("动作", e.action); field("执行人 ID", e.actor_user_id); field("执行任职 ID", e.acting_membership_id);
      field("资源类型", e.resource_type); field("资源 ID", e.resource_id ?? "未指定"); field("原因代码", e.reason);
      field("发生时间", new Date(e.occurred_at).toLocaleString("zh-CN", { hour12: false }) + `（${e.occurred_at}）`);
      card.append(title, details); this.list.append(card);
    }
  }
};
