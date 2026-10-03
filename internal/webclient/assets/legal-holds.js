"use strict";

// Legal hold records and current-page idempotent administration.
window.LegalHoldRecords = class {
  constructor(request, context, canOpen = () => true) {
    this.canOpen = canOpen;
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
    this.actionForm = document.getElementById("legal-hold-action-form");
    this.actionTitle = document.getElementById("legal-hold-action-title");
    this.actionTarget = document.getElementById("legal-hold-action-target");
    this.reference = document.getElementById("legal-hold-reference");
    this.confirm = document.getElementById("legal-hold-confirm");
    this.confirmLabel = document.getElementById("legal-hold-confirm-label");
    this.submitButton = document.getElementById("legal-hold-submit");
    this.retryButton = document.getElementById("legal-hold-retry");
    this.abandonButton = document.getElementById("legal-hold-abandon");
    this.actionHint = document.getElementById("legal-hold-action-hint");
    this.draft = null;
    this.pending = null;
    this.actionForm.addEventListener("submit", event => { event.preventDefault(); this.submitAction(); });
    this.retryButton.addEventListener("click", () => this.submitAction());
    this.abandonButton.addEventListener("click", () => this.abandon());
    document.getElementById("legal-hold-cancel").addEventListener("click", () => { if (!this.pending) this.resetDraft(); });
    window.addEventListener("beforeunload", event => {
      if (this.pending) { event.preventDefault(); event.returnValue = ""; }
    });
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
    if (!this.pending) this.resetDraft();
    this.updateButtons();
  }

  close() {
    this.clear();
    if (this.dialog.open) this.dialog.close();
  }

  contextChanged() {
    const current = this.context();
    if (this.pending && !this.sameContext(this.pending, current)) this.pending = null;
    this.close();
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
    this.refreshButton.disabled = !available || this.loading || !!this.pending;
    this.moreButton.classList.toggle("hidden", !available || !this.cursor);
    this.moreButton.disabled = this.loading || !!this.pending;
    this.list.setAttribute("aria-busy", String(this.loading));
    this.updateAction();
  }

  open() {
    if (!this.canOpen()) return;
    if (!this.allowed || !this.context().conversation) return;
    this.dialog.showModal();
    if (!this.pending) this.load(true);
    else this.showPending();
  }

  isCurrent(generation, current) {
    const latest = this.context();
    return this.dialog.open && this.generation === generation &&
      current.identityKey === latest.identityKey && current.conversation === latest.conversation &&
      current.conversationEpoch === latest.conversationEpoch;
  }

  async load(firstPage) {
    const current = this.context();
    if (!this.dialog.open || !this.allowed || this.pending || !current.conversation || (!firstPage && (this.loading || !this.cursor))) return;
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

  validatePage(page, conversation, cursor, knownRecords = this.records) {
    const uuid = value => typeof value === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
    const time = value => typeof value === "string" && Number.isFinite(Date.parse(value));
    const reference = value => typeof value === "string" && Array.from(value).length >= 1 &&
      Array.from(value).length <= 128 && value.trim() === value && !/[\u0000-\u001f\u007f-\u009f]/u.test(value);
    if (!page || !Array.isArray(page.holds) || page.holds.length > 20 ||
        typeof page.next_cursor !== "string" || page.next_cursor.length > 1024 ||
        (page.next_cursor && (page.next_cursor === cursor || !page.holds.length))) throw new Error("invalid hold page");
    const seen = new Set(knownRecords.map(record => record.id.toLowerCase()));
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
      card.append(id, details);
      if (!record.released_at) {
        const release = document.createElement("button");
        release.type = "button"; release.className = "secondary-button legal-hold-release";
        release.textContent = "解除此项保全";
        release.disabled = this.loading || !!this.pending || !this.allowed;
        release.addEventListener("click", () => this.prepareRelease(record));
        card.append(release);
      }
      this.list.append(card);
    }
  }

  sameContext(operation, current = this.context()) {
    return operation.identityKey === current.identityKey && operation.conversation === current.conversation &&
      operation.conversationEpoch === current.conversationEpoch;
  }

  resetDraft() {
    this.draft = null;
    this.reference.value = "";
    this.confirm.checked = false;
    this.actionHint.textContent = "";
    this.updateAction();
  }

  updateAction() {
    const ready = this.allowed && !!this.context().conversation && !this.loading;
    const operation = this.pending || this.draft;
    const release = operation?.type === "release";
    this.actionTitle.textContent = release ? "解除一项保全" : "登记保全";
    this.actionTarget.textContent = release ? `${operation.caseReference} · ${operation.holdID}` : "请输入调查或诉讼的案件引用。";
    document.getElementById("legal-hold-reference-label").textContent = release ? "已取得的解除审批引用（1～128 个字符）" : "案件引用（1～128 个字符）";
    this.confirmLabel.classList.toggle("hidden", !release || !!this.pending);
    this.reference.disabled = !ready || !!this.pending;
    this.confirm.disabled = !ready || !!this.pending;
    this.submitButton.textContent = release ? "确认解除" : "登记保全";
    this.submitButton.disabled = !ready || !!this.pending;
    document.getElementById("legal-hold-cancel").disabled = !!this.pending;
    this.retryButton.classList.toggle("hidden", !this.pending);
    this.abandonButton.classList.toggle("hidden", !this.pending);
    this.retryButton.disabled = !ready || !!this.pending?.sending;
    this.abandonButton.disabled = !!this.pending?.sending;
    for (const button of this.list.querySelectorAll(".legal-hold-release")) button.disabled = !ready || !!this.pending;
  }

  prepareRelease(record) {
    if (!this.allowed || this.loading || this.pending || record.released_at) return;
    this.resetDraft();
    this.draft = { type: "release", holdID: record.id, caseReference: record.case_reference };
    this.updateAction();
    this.actionForm.scrollIntoView({ block: "nearest" });
    this.reference.focus();
  }

  showPending() {
    if (!this.pending) return;
    this.title.textContent = `${this.pending.title} · ${this.pending.conversation}`;
    if (!this.dialog.open) this.dialog.showModal();
    this.reference.value = this.pending.reference;
    this.actionHint.textContent = `结果待确认，请使用原编号重试：${this.pending.requestID}。放弃重试不代表撤销服务器操作。`;
    this.updateAction();
  }

  canSwitchContext() {
    if (!this.pending) return true;
    this.showPending();
    return false;
  }

  abandon() {
    if (!this.pending || this.pending.sending || !window.confirm("放弃本次重试不代表撤销服务器操作。请核对服务器保全记录后再操作。确认放弃？")) return;
    this.pending = null;
    this.resetDraft();
    this.actionHint.textContent = "已放弃重试，不代表撤销服务器操作，请刷新核对保全记录。";
    this.updateButtons();
  }

  async submitAction() {
    if (!this.allowed || this.loading || this.pending?.sending) return;
    const current = this.context();
    if (!current.identityKey || !current.conversation) return;
    if (!this.pending) {
      const reference = this.reference.value.trim();
      if (!reference || Array.from(reference).length > 128 || /[\u0000-\u001f\u007f-\u009f]/u.test(reference) ||
          /[\uD800-\uDFFF]/u.test(reference)) {
        this.actionHint.textContent = "引用需为 1～128 个字符，不得包含控制字符。";
        return;
      }
      if (this.draft?.type === "release" && !this.confirm.checked) {
        this.actionHint.textContent = "请先确认已取得审批，并核对解除的是这一项保全。";
        return;
      }
      this.pending = { ...current, type: this.draft?.type || "place", holdID: this.draft?.holdID,
        caseReference: this.draft?.caseReference, reference, requestID: crypto.randomUUID(), sending: false };
    }
    const operation = this.pending;
    if (!this.sameContext(operation, current)) return;
    operation.sending = true;
    this.actionHint.textContent = "正在提交，请等待服务器确认…";
    this.updateButtons();
    try {
      const releasing = operation.type === "release";
      const path = `/api/v1/admin/conversations/${encodeURIComponent(operation.conversation)}/legal-holds` +
        (releasing ? `/${encodeURIComponent(operation.holdID)}/release` : "");
      const result = await this.request(path, { method: "POST", signal: AbortSignal.timeout(15000), headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ request_id: operation.requestID,
          [releasing ? "approval_reference" : "case_reference"]: operation.reference }) });
      if (this.pending !== operation || !this.sameContext(operation)) return;
      this.validatePage({ holds: [result], next_cursor: "" }, operation.conversation, "", []);
      if (releasing ? result.id !== operation.holdID || result.case_reference !== operation.caseReference ||
          result.release_approval_reference !== operation.reference || result.released_by_user_id !== operation.userId ||
          result.released_by_membership_id !== operation.membershipId : result.case_reference !== operation.reference ||
          result.placed_by_user_id !== operation.userId || result.placed_by_membership_id !== operation.membershipId) {
        throw new Error("write result mismatch");
      }
      this.pending = null;
      this.resetDraft();
      this.actionHint.textContent = "服务器已确认保全记录，请以刷新后的状态为准。";
      this.updateButtons();
      if (this.dialog.open) await this.load(true);
    } catch (error) {
      if (this.pending !== operation || !this.sameContext(operation) || error.stale) return;
      if ([400, 403, 404, 409].includes(error.status)) {
        this.pending = null;
        this.resetDraft();
        if (error.status === 403 || error.status === 404) {
          this.allowed = false; this.records = []; this.cursor = ""; this.list.replaceChildren();
          this.actionHint.textContent = "权限已失效或会话不可用，请核查服务器保全记录。";
        } else this.actionHint.textContent = error.status === 409 ? "操作冲突，请刷新核对保全记录后再操作。" : "请求被拒绝，请检查引用后再操作。";
      } else {
        this.actionHint.textContent = `结果待确认，请使用原编号重试：${operation.requestID}。`;
      }
    } finally {
      operation.sending = false;
      if (this.sameContext(operation)) this.updateButtons();
    }
  }

};
