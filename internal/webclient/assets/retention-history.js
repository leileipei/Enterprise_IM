"use strict";

// Read-only immutable approvals. Each page uses the original tenant/identity.
window.RetentionPolicyHistory = class {
  constructor(request, context, validatePolicy, onDenied) {
    this.request = request; this.context = context; this.validatePolicy = validatePolicy; this.onDenied = onDenied;
    const el = id => document.getElementById(`retention-history-${id}`);
    this.toggle = el("toggle"); this.section = el("section"); this.list = el("list"); this.hint = el("hint");
    this.refresh = el("refresh"); this.more = el("more");
    this.available = false; this.visible = false; this.loading = false; this.generation = 0; this.cursor = ""; this.records = [];
    this.toggle.addEventListener("click", () => {
      if (!this.available) return;
      if (this.visible) this.clear(); else { this.visible = true; this.update(); this.load(true); }
    });
    this.refresh.addEventListener("click", () => this.load(true));
    this.more.addEventListener("click", () => this.load(false));
    this.update();
  }

  setAvailable(available) {
    this.available = available;
    if (!available) this.clear(); else this.update();
  }

  clear() {
    this.generation++; this.visible = false; this.loading = false; this.cursor = ""; this.records = [];
    this.list.replaceChildren(); this.hint.textContent = ""; this.update();
  }

  update() {
    this.toggle.disabled = !this.available;
    this.toggle.textContent = this.visible ? "收起审批历史" : "查看审批历史";
    this.toggle.setAttribute("aria-expanded", String(this.visible));
    this.section.classList.toggle("hidden", !this.visible);
    this.refresh.disabled = !this.available || this.loading;
    this.more.classList.toggle("hidden", !this.cursor);
    this.more.disabled = !this.available || this.loading;
    this.list.setAttribute("aria-busy", String(this.loading));
  }

  validate(page, cursor) {
    if (!page || !Array.isArray(page.history) || page.history.length > 20 || typeof page.next_cursor !== "string" ||
        page.next_cursor.length > 1024 || (page.next_cursor && (page.history.length !== 20 || page.next_cursor === cursor))) throw new Error("invalid history page");
    let before = cursor ? this.records.at(-1)?.version : Infinity;
    if (cursor && !Number.isSafeInteger(before)) throw new Error("missing history boundary");
    for (const policy of page.history) {
      this.validatePolicy(policy);
      if (policy.version < 1 || policy.version >= before) throw new Error("invalid history order");
      before = policy.version;
    }
  }

  async load(first) {
    if (!this.available || !this.visible || this.loading || (!first && !this.cursor)) return;
    const snapshot = this.context(), generation = ++this.generation, cursor = first ? "" : this.cursor;
    const isCurrent = () => this.available && this.visible && generation === this.generation &&
      snapshot.identityKey === this.context().identityKey && !!snapshot.identityKey;
    if (first) { this.records = []; this.cursor = ""; this.list.replaceChildren(); }
    this.loading = true; this.hint.textContent = "正在加载审批历史…"; this.update();
    try {
      const page = await this.request("/api/v1/admin/retention-policy/history?limit=20" +
        (cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""), { signal: AbortSignal.timeout(15000) });
      if (!isCurrent()) return;
      this.validate(page, cursor);
      this.records.push(...page.history.map(p => ({ version: p.version, message_body_days: p.message_body_days,
        approval_reference: p.approval_reference, approved_by_user_id: p.approved_by_user_id, approved_at: p.approved_at })));
      this.cursor = page.next_cursor; this.render();
      this.hint.textContent = this.records.length ? `已显示 ${this.records.length} 个版本` +
        (this.cursor ? "，可继续加载。" : "，已到末页。") : "暂无变更审批记录；初始默认配置不属于变更审批。";
    } catch (error) {
      if (!isCurrent() || error.stale) return;
      this.records = []; this.cursor = ""; this.list.replaceChildren();
      if (error.status === 403 || error.status === 404) this.onDenied();
      else this.hint.textContent = "审批历史加载失败，请刷新从最新版本重新查询。";
    } finally { if (isCurrent()) { this.loading = false; this.update(); } }
  }

  render() {
    this.list.replaceChildren();
    for (const p of this.records) {
      const card = document.createElement("li"); card.className = "retention-history-card retention-record-card";
      const title = document.createElement("strong"); title.textContent = `版本 ${p.version}`;
      const details = document.createElement("dl");
      const field = (label, value) => { const dt = document.createElement("dt"), dd = document.createElement("dd"); dt.textContent = label; dd.textContent = value; details.append(dt, dd); };
      field("正文保留期限", `${p.message_body_days} 天`); field("审批引用", p.approval_reference);
      field("登记人 ID", p.approved_by_user_id);
      field("登记时间", new Date(p.approved_at).toLocaleString("zh-CN", { hour12: false }));
      card.append(title, details); this.list.append(card);
    }
  }
};
