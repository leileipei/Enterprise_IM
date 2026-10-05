"use strict";

// Read-only evidence viewer. Records live only in the current page and context.
window.RetentionRecords = class {
  constructor(request, context, onAccess = () => {}) {
    this.onAccess = onAccess;
    this.request = request;
    this.context = context;
    this.openButton = document.getElementById("retention-records-open");
    this.accessRetry = document.getElementById("retention-access-retry");
    this.dialog = document.getElementById("retention-records-dialog");
    this.title = document.getElementById("retention-records-conversation");
    this.kind = document.getElementById("retention-records-kind");
    this.list = document.getElementById("retention-records-list");
    this.hint = document.getElementById("retention-records-hint");
    this.refreshButton = document.getElementById("retention-records-refresh");
    this.moreButton = document.getElementById("retention-records-more");
    this.identityKey = "";
    this.allowed = false;
    this.accessFailed = false;
    this.accessPending = false;
    this.generation = 0;
    this.cursor = "";
    this.records = [];
    this.loading = false;
    this.openButton.addEventListener("click", () => this.open());
    this.accessRetry.addEventListener("click", () => this.checkAccess(this.identityKey));
    document.getElementById("retention-records-close").addEventListener("click", () => this.close());
    this.dialog.addEventListener("cancel", event => { event.preventDefault(); this.close(); });
    this.dialog.addEventListener("close", () => { if (!this.dialog.open) this.clear(); });
    this.kind.addEventListener("change", () => this.load(true));
    this.refreshButton.addEventListener("click", () => this.load(true));
    this.moreButton.addEventListener("click", () => this.load(false));
  }

  clear() {
    this.generation++;
    this.cursor = "";
    this.records = [];
    this.loading = false;
    this.list.replaceChildren();
    this.hint.textContent = "";
    this.title.textContent = "";
    this.updateButtons();
  }

  close() {
    this.clear();
    if (this.dialog.open) this.dialog.close();
  }

  contextChanged() {
    this.close();
    const current = this.context();
    if (this.identityKey !== current.identityKey) {
      this.identityKey = current.identityKey;
      this.allowed = false;
      this.accessFailed = false;
      this.accessPending = false;
      this.onAccess(false);
      if (this.identityKey) this.checkAccess(this.identityKey);
    }
    this.updateButtons();
  }

  async checkAccess(identityKey) {
    if (!identityKey || this.accessPending) return;
    this.accessPending = true;
    this.updateButtons();
    try {
      // This existing tenant policy endpoint only permits a valid group administrator.
      const policy = await this.request("/api/v1/admin/retention-policy");
      if (identityKey !== this.context().identityKey || identityKey !== this.identityKey) return;
      this.allowed = Number.isInteger(policy.message_body_days) && policy.message_body_days >= 1 &&
        policy.message_body_days <= 3650 && Number.isSafeInteger(policy.version) && policy.version >= 0;
      this.accessFailed = !this.allowed;
    } catch (error) {
      if (identityKey !== this.context().identityKey || identityKey !== this.identityKey) return;
      this.allowed = false;
      this.accessFailed = !error.status || error.status >= 500;
    }
    this.accessPending = false;
    this.onAccess(this.allowed);
    this.updateButtons();
  }

  updateButtons() {
    const available = this.allowed && !!this.context().conversation;
    this.openButton.classList.toggle("hidden", !available);
    this.accessRetry.classList.toggle("hidden", !this.accessFailed || !this.context().identityKey);
    this.accessRetry.disabled = this.accessPending;
    this.refreshButton.disabled = !available || this.loading;
    this.kind.disabled = !available;
    this.moreButton.classList.toggle("hidden", !available || !this.cursor);
    this.moreButton.disabled = this.loading;
    this.list.setAttribute("aria-busy", String(this.loading));
  }

  open() {
    if (!this.allowed || !this.context().conversation) return;
    this.kind.value = "body";
    this.dialog.showModal();
    this.load(true);
  }

  isCurrent(generation, current) {
    const latest = this.context();
    return this.dialog.open && this.generation === generation &&
      current.identityKey === latest.identityKey && current.conversation === latest.conversation &&
      current.conversationEpoch === latest.conversationEpoch;
  }

  async load(firstPage) {
    const current = this.context();
    if (!this.dialog.open || !this.allowed || !current.conversation || (!firstPage && (this.loading || !this.cursor))) return;
    const generation = ++this.generation;
    const kind = this.kind.value;
    const cursor = firstPage ? "" : this.cursor;
    if (firstPage) {
      this.records = [];
      this.cursor = "";
      this.list.replaceChildren();
    }
    this.title.textContent = current.title;
    this.loading = true;
    this.hint.textContent = "正在加载清理记录…";
    this.updateButtons();
    try {
      const page = await this.request(`/api/v1/admin/conversations/${encodeURIComponent(current.conversation)}/retention-batches?kind=${kind}&limit=20` +
        (cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""));
      if (!this.isCurrent(generation, current)) return;
      this.validatePage(page, current.conversation, kind, cursor);
      this.records.push(...page.batches.map(b => ({
        id: b.id, processed_at: b.processed_at, processed_count: b.processed_count,
        first_seq: b.first_seq, last_seq: b.last_seq, kind: b.kind,
        ...(kind === "body" ? { retention_days: b.retention_days, cutoff_at: b.cutoff_at } :
          { min_expires_at: b.min_expires_at, max_expires_at: b.max_expires_at }),
      })));
      this.cursor = page.next_cursor;
      this.render();
      this.hint.textContent = this.records.length ?
        `已显示 ${this.records.length} 个批次` + (this.cursor ? "，可继续加载。" : "，已到末页。") : "暂无此类清理记录。";
    } catch (error) {
      if (!this.isCurrent(generation, current) || error.stale) return;
      this.records = [];
      this.cursor = "";
      this.list.replaceChildren();
      if (error.status === 403 || error.status === 404) {
        this.allowed = false;
        this.hint.textContent = "权限已失效或会话不可用，已清空清理记录。";
      } else {
        this.hint.textContent = "清理记录加载失败，请刷新重试。";
      }
    } finally {
      if (this.isCurrent(generation, current)) {
        this.loading = false;
        this.updateButtons();
      }
    }
  }

  validatePage(page, conversation, kind, cursor) {
    const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
    const time = value => typeof value === "string" && Number.isFinite(Date.parse(value));
    if (!page || !Array.isArray(page.batches) || page.batches.length > 20 ||
        typeof page.next_cursor !== "string" || page.next_cursor.length > 1024 ||
        (page.next_cursor && (page.next_cursor === cursor || !page.batches.length))) throw new Error("invalid evidence page");
    const seen = new Set(this.records.map(record => record.id.toLowerCase()));
    for (const b of page.batches) {
      if (!b || typeof b.id !== "string" || !uuid.test(b.id) || b.conversation_id !== conversation || b.kind !== kind ||
          !time(b.processed_at) || !Number.isInteger(b.processed_count) || b.processed_count < 1 || b.processed_count > 1000 ||
          !Number.isSafeInteger(b.first_seq) || b.first_seq < 1 || !Number.isSafeInteger(b.last_seq) || b.last_seq < b.first_seq ||
          seen.has(b.id.toLowerCase())) throw new Error("invalid evidence batch");
      if (kind === "body" ? !Number.isInteger(b.retention_days) || b.retention_days < 1 || b.retention_days > 3650 || !time(b.cutoff_at) :
          !time(b.min_expires_at) || !time(b.max_expires_at)) throw new Error("invalid evidence boundary");
      seen.add(b.id.toLowerCase());
    }
  }

  render() {
    this.list.replaceChildren();
    for (const record of this.records) {
      const card = document.createElement("li");
      card.className = "retention-record-card";
      const id = document.createElement("strong");
      id.className = "retention-record-id";
      id.textContent = record.id;
      const details = document.createElement("dl");
      const field = (label, value) => {
        const term = document.createElement("dt"), description = document.createElement("dd");
        term.textContent = label; description.textContent = value; details.append(term, description);
      };
      const date = value => new Date(value).toLocaleString("zh-CN", { hour12: false });
      field("清理时间", date(record.processed_at));
      field("实际数量", `${record.processed_count} 条`);
      field("序号边界", `${record.first_seq} ～ ${record.last_seq}`);
      if (record.kind === "body") {
        field("正文保留期", `${record.retention_days} 天`);
        field("清理截止时间", date(record.cutoff_at));
      } else {
        field("最早到期时间", date(record.min_expires_at));
        field("最晚到期时间", date(record.max_expires_at));
      }
      card.append(id, details); this.list.append(card);
    }
  }
};
