"use strict";

// Read-only legal hold viewer. Records live only in the current page and context.
window.LegalHoldRecords = class {
  constructor(request, context) {
    this.request = request;
    this.context = context;
    this.openButton = document.getElementById("legal-holds-open");
    this.accessRetry = document.getElementById("legal-holds-access-retry");
    this.dialog = document.getElementById("legal-holds-dialog");
    this.title = document.getElementById("legal-holds-conversation");
    this.list = document.getElementById("legal-holds-list");
    this.hint = document.getElementById("legal-holds-hint");
    this.refreshButton = document.getElementById("legal-holds-refresh");
    this.moreButton = document.getElementById("legal-holds-more");
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
    document.getElementById("legal-holds-close").addEventListener("click", () => this.close());
    this.dialog.addEventListener("cancel", event => { event.preventDefault(); this.close(); });
    this.dialog.addEventListener("close", () => { if (!this.dialog.open) this.clear(); });
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
    this.updateButtons();
  }

  updateButtons() {
    const available = this.allowed && !!this.context().conversation;
    this.openButton.classList.toggle("hidden", !available);
    this.accessRetry.classList.toggle("hidden", !this.accessFailed || !this.context().identityKey);
    this.accessRetry.disabled = this.accessPending;
    this.refreshButton.disabled = !available || this.loading;
    this.moreButton.classList.toggle("hidden", !available || !this.cursor);
    this.moreButton.disabled = this.loading;
    this.list.setAttribute("aria-busy", String(this.loading));
  }

  open() {
    if (!this.allowed || !this.context().conversation) return;
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
    const cursor = firstPage ? "" : this.cursor;
    if (firstPage) {
      this.records = [];
      this.cursor = "";
      this.list.replaceChildren();
    }
    this.title.textContent = current.title;
    this.loading = true;
    this.hint.textContent = "正在加载保全记录…";
    this.updateButtons();
    try {
      const page = await this.request(`/api/v1/admin/conversations/${encodeURIComponent(current.conversation)}/legal-holds?limit=20` +
        (cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""));
      if (!this.isCurrent(generation, current)) return;
      this.validatePage(page, current.conversation, cursor);
      this.records.push(...page.holds.map(h => ({
        id: h.id, case_reference: h.case_reference, placed_at: h.placed_at,
        placed_by_user_id: h.placed_by_user_id, placed_by_membership_id: h.placed_by_membership_id,
        ...(h.released_at ? { released_at: h.released_at, release_approval_reference: h.release_approval_reference,
          released_by_user_id: h.released_by_user_id, released_by_membership_id: h.released_by_membership_id } : {}),
      })));
      this.cursor = page.next_cursor;
      this.render();
      this.hint.textContent = this.records.length ?
        `已显示 ${this.records.length} 项保全` + (this.cursor ? "，可继续加载。" : "，已到末页。") : "暂无保全记录。";
    } catch (error) {
      if (!this.isCurrent(generation, current) || error.stale) return;
      this.records = [];
      this.cursor = "";
      this.list.replaceChildren();
      if (error.status === 403 || error.status === 404) {
        this.allowed = false;
        this.hint.textContent = "权限已失效或会话不可用，已清空保全记录。";
      } else {
        this.hint.textContent = "保全记录加载失败，请刷新重试。";
      }
    } finally {
      if (this.isCurrent(generation, current)) {
        this.loading = false;
        this.updateButtons();
      }
    }
  }

  validatePage(page, conversation, cursor) {
    const uuid = value => typeof value === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
    const time = value => typeof value === "string" && Number.isFinite(Date.parse(value));
    const reference = value => typeof value === "string" && Array.from(value).length >= 1 &&
      Array.from(value).length <= 128 && value.trim() === value && !/[\u0000-\u001f\u007f-\u009f]/u.test(value);
    if (!page || !Array.isArray(page.holds) || page.holds.length > 20 ||
        typeof page.next_cursor !== "string" || page.next_cursor.length > 1024 ||
        (page.next_cursor && (page.next_cursor === cursor || !page.holds.length))) throw new Error("invalid hold page");
    const seen = new Set(this.records.map(record => record.id.toLowerCase()));
    for (const h of page.holds) {
      if (!h || !uuid(h.id) || h.conversation_id !== conversation || !reference(h.case_reference) ||
          !time(h.placed_at) || !uuid(h.placed_by_user_id) || !uuid(h.placed_by_membership_id) ||
          seen.has(h.id.toLowerCase())) throw new Error("invalid hold");
      const releaseKeys = ["released_at", "release_approval_reference", "released_by_user_id", "released_by_membership_id"];
      if (releaseKeys.some(key => Object.prototype.hasOwnProperty.call(h, key))) {
        if (!time(h.released_at) || Date.parse(h.released_at) < Date.parse(h.placed_at) ||
            !reference(h.release_approval_reference) || !uuid(h.released_by_user_id) ||
            !uuid(h.released_by_membership_id)) throw new Error("invalid release");
      }
      seen.add(h.id.toLowerCase());
    }
  }

  render() {
    this.list.replaceChildren();
    for (const record of this.records) {
      const card = document.createElement("li");
      card.className = "legal-holds-card retention-record-card";
      const id = document.createElement("strong");
      id.className = "retention-record-id";
      id.textContent = record.id;
      const details = document.createElement("dl");
      const field = (label, value) => {
        const term = document.createElement("dt"), description = document.createElement("dd");
        term.textContent = label; description.textContent = value; details.append(term, description);
      };
      const date = value => new Date(value).toLocaleString("zh-CN", { hour12: false });
      field("状态", record.released_at ? "已解除" : "保全中");
      field("案件引用", record.case_reference);
      field("登记时间", date(record.placed_at));
      field("登记人 ID", record.placed_by_user_id);
      field("登记任职 ID", record.placed_by_membership_id);
      if (record.released_at) {
        field("解除时间", date(record.released_at));
        field("解除审批引用", record.release_approval_reference);
        field("解除人 ID", record.released_by_user_id);
        field("解除任职 ID", record.released_by_membership_id);
      }
      card.append(id, details); this.list.append(card);
    }
  }
};
