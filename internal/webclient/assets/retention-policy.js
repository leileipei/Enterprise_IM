"use strict";

// Tenant policy editor. A versioned write is reconciled by GET before retry.
window.RetentionPolicyEditor = class {
  constructor(request, context, canOpen = () => true) {
    this.canOpen = canOpen;
    this.request = request; this.context = context;
    const el = id => document.getElementById(`retention-policy-${id}`);
    this.openButton = el("open"); this.dialog = el("dialog"); this.current = el("current");
    this.contextLabel = el("context"); this.days = el("days"); this.reference = el("reference");
    this.confirm = el("confirm"); this.submitButton = el("submit"); this.refreshButton = el("refresh");
    this.retryButton = el("retry"); this.abandonButton = el("abandon"); this.hint = el("hint");
    this.identityKey = ""; this.allowed = false; this.policy = null; this.pending = null;
    this.loading = false; this.generation = 0;
    this.openButton.addEventListener("click", () => this.open());
    el("close").addEventListener("click", () => this.close());
    this.dialog.addEventListener("cancel", event => { event.preventDefault(); this.close(); });
    this.dialog.addEventListener("close", () => { if (!this.dialog.open) this.clearView(); });
    el("form").addEventListener("submit", event => { event.preventDefault(); this.submit(); });
    this.refreshButton.addEventListener("click", () => this.load());
    this.retryButton.addEventListener("click", () => this.submit());
    this.abandonButton.addEventListener("click", () => this.abandon());
    window.addEventListener("beforeunload", event => { if (this.pending) { event.preventDefault(); event.returnValue = ""; } });
    this.update();
  }

  sameIdentity(snapshot) { return snapshot.identityKey === this.context().identityKey && !!snapshot.identityKey; }

  contextChanged() {
    const identity = this.context().identityKey;
    if (identity === this.identityKey) return;
    this.identityKey = identity; this.allowed = false; this.pending = null;
    this.close();
  }

  setAccess(allowed) { this.allowed = allowed && !!this.identityKey; this.update(); }

  clearView() {
    this.generation++; this.loading = false; this.policy = null;
    this.current.replaceChildren(); this.days.value = ""; this.reference.value = "";
    this.confirm.checked = false; this.hint.textContent = ""; this.contextLabel.textContent = "";
    this.update();
  }

  close() { this.clearView(); if (this.dialog.open) this.dialog.close(); }

  open() {
    if (!this.canOpen()) return;
    if (!this.allowed) return;
    if (!this.dialog.open) this.dialog.showModal();
    this.showContext();
    if (this.pending) this.showPending(); else this.load();
  }

  showContext() {
    const snapshot = this.pending || this.context();
    this.contextLabel.textContent = `当前集团（租户）：${snapshot.tenantId} · 操作任职：${snapshot.membershipId}`;
  }

  showPending() {
    if (!this.pending) return;
    if (!this.dialog.open) this.dialog.showModal();
    this.showContext();
    this.days.value = String(this.pending.days); this.reference.value = this.pending.reference;
    this.hint.textContent = this.pending.sending ? "正在提交，请等待服务器确认…" :
      `结果待确认（原版本 ${this.pending.version}），请先查询服务器状态。放弃核对不代表撤销服务器操作。`;
    this.update();
  }

  canSwitchContext() { if (!this.pending) return true; this.showPending(); return false; }

  update() {
    this.openButton.classList.toggle("hidden", !this.allowed);
    const busy = this.loading || !!this.pending?.sending;
    const ready = this.allowed && !!this.policy && !busy && !this.pending;
    this.days.disabled = !ready; this.reference.disabled = !ready; this.confirm.disabled = !ready;
    this.submitButton.disabled = !ready;
    this.refreshButton.disabled = !this.allowed || busy;
    this.refreshButton.textContent = this.pending ? "查询服务器状态" : "刷新当前配置";
    this.retryButton.classList.toggle("hidden", !this.pending?.retryReady);
    this.retryButton.disabled = !this.allowed || busy;
    this.abandonButton.classList.toggle("hidden", !this.pending);
    this.abandonButton.disabled = busy;
    this.current.setAttribute("aria-busy", String(this.loading));
  }

  validReference(value) {
    return typeof value === "string" && !!value && value.trim() === value && Array.from(value).length <= 128 &&
      !/[\u0000-\u001f\u007f-\u009f\uFFFD]/u.test(value) && !/[\uD800-\uDFFF]/u.test(value);
  }

  validTime(value) {
    if (typeof value !== "string") return false;
    const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d{1,9})?(?:Z|([+-])(\d{2}):(\d{2}))$/.exec(value);
    if (!match) return false;
    const [, year, month, day, hour, minute, second, , zoneHour = "0", zoneMinute = "0"] = match;
    if (+year < 1 || +month < 1 || +month > 12 || +day < 1 || +day > 31 || +hour > 23 ||
        +minute > 59 || +second > 59 || +zoneHour > 23 || +zoneMinute > 59) return false;
    const calendar = new Date(0);
    calendar.setUTCFullYear(+year, +month - 1, +day);
    return calendar.getUTCFullYear() === +year && calendar.getUTCMonth() === +month - 1 &&
      calendar.getUTCDate() === +day && Number.isFinite(Date.parse(value));
  }

  validate(policy) {
    if (!policy || !Number.isInteger(policy.message_body_days) || policy.message_body_days < 1 ||
        policy.message_body_days > 3650 || !Number.isSafeInteger(policy.version) || policy.version < 0) throw new Error("invalid policy");
    if (policy.version === 0) {
      if (policy.message_body_days !== 365 || policy.approval_reference !== "" || policy.approved_by_user_id !== "" || policy.approved_at !== null) throw new Error("invalid default policy");
    } else if (!this.validReference(policy.approval_reference) || typeof policy.approved_by_user_id !== "string" ||
        !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(policy.approved_by_user_id) ||
        !this.validTime(policy.approved_at)) throw new Error("invalid approval metadata");
  }

  accept(policy) {
    // Keep only policy fields, including approval metadata, in page memory.
    this.policy = { message_body_days: policy.message_body_days, version: policy.version,
      approval_reference: policy.approval_reference, approved_by_user_id: policy.approved_by_user_id, approved_at: policy.approved_at };
    this.current.replaceChildren();
    const details = document.createElement("dl");
    const field = (name, value) => { const dt = document.createElement("dt"), dd = document.createElement("dd"); dt.textContent = name; dd.textContent = value; details.append(dt, dd); };
    field("正文保留期限", `${policy.message_body_days} 天`); field("版本", String(policy.version));
    if (policy.version === 0) field("审批记录", "初始默认配置，尚无变更审批记录");
    else { field("审批引用", policy.approval_reference); field("审批登记人 ID", policy.approved_by_user_id); field("登记时间", new Date(policy.approved_at).toLocaleString("zh-CN", { hour12: false })); }
    this.current.append(details); this.days.value = String(policy.message_body_days);
    this.reference.value = ""; this.confirm.checked = false;
  }

  async load() {
    if (!this.allowed || !this.dialog.open || this.loading || this.pending?.sending) return;
    const snapshot = this.context(), generation = ++this.generation, operation = this.pending;
    const isCurrent = () => this.dialog.open && generation === this.generation && this.sameIdentity(snapshot);
    this.policy = null; this.current.replaceChildren(); this.confirm.checked = false;
    if (operation) operation.retryReady = false;
    else { this.days.value = ""; this.reference.value = ""; }
    this.loading = true; this.showContext(); this.hint.textContent = "正在查询当前配置…"; this.update();
    try {
      const policy = await this.request("/api/v1/admin/retention-policy", { signal: AbortSignal.timeout(15000) });
      if (!isCurrent()) return;
      this.validate(policy); this.accept(policy);
      if (operation) {
        if (policy.version === operation.version) {
          // A stale read can still race with commit. Retry uses the original CAS.
          operation.retryReady = true; this.days.value = String(operation.days); this.reference.value = operation.reference;
          this.hint.textContent = `服务器仍为原版本 ${operation.version}，可按原版本重试；不会自动使用新版本提交。`;
        } else if (policy.version === operation.version + 1 && policy.message_body_days === operation.days &&
            policy.approval_reference === operation.reference && policy.approved_by_user_id === operation.userId) {
          this.pending = null;
          this.hint.textContent = "当前配置与本次提交一致，已结束待确认状态。仅凭当前配置无法独立证明由哪次请求提交。";
        } else {
          this.pending = null;
          this.hint.textContent = "服务器配置已变化，无法仅凭当前配置确认原提交结果。请核查审批及审计记录，重新填写并确认后再操作。";
        }
      } else this.hint.textContent = "已加载当前配置。";
    } catch (error) {
      if (!isCurrent() || error.stale) return;
      this.policy = null; this.current.replaceChildren();
      if (error.status === 403 || error.status === 404) {
        this.allowed = false; this.pending = null; this.days.value = ""; this.reference.value = "";
        this.hint.textContent = "权限已失效，已清空配置。请联系集团管理员核查服务器状态。";
      } else this.hint.textContent = operation ? "配置加载失败，结果仍待确认；请再次查询服务器状态。" : "配置加载失败，请刷新重试。";
    } finally { if (isCurrent()) { this.loading = false; this.update(); } }
  }

  abandon() {
    if (!this.pending || this.pending.sending || this.loading || !window.confirm("放弃核对不代表撤销服务器操作。请核查服务器配置与审计记录。确认放弃？")) return;
    this.pending = null; this.clearView(); this.showContext();
    this.hint.textContent = "已放弃核对，不代表撤销服务器操作。请刷新当前配置后再操作。"; this.update();
  }

  async submit() {
    if (!this.allowed || this.loading || this.pending?.sending) return;
    if (!this.pending) {
      if (!this.policy || this.policy.version >= Number.MAX_SAFE_INTEGER) return;
      const value = this.days.value.trim(), days = Number(value), reference = this.reference.value.trim();
      if (!/^\d{1,4}$/.test(value) || days < 1 || days > 3650) { this.hint.textContent = "保留期限须为 1～3650 天的整数。"; return; }
      if (!this.validReference(reference)) { this.hint.textContent = "审批引用须为 1～128 个字符，不得包含控制字符或无效字符。"; return; }
      if (!this.confirm.checked) { this.hint.textContent = "请确认已取得审批，并了解更改作用于当前集团的全部消息。"; return; }
      this.pending = { ...this.context(), version: this.policy.version, days, reference, sending: false, retryReady: false };
    } else if (!this.pending.retryReady) return;
    const operation = this.pending;
    if (!this.sameIdentity(operation)) return;
    operation.sending = true; operation.retryReady = false;
    this.hint.textContent = "正在提交，请等待服务器确认…"; this.update();
    try {
      const policy = await this.request("/api/v1/admin/retention-policy", { method: "PUT", signal: AbortSignal.timeout(15000),
        headers: { "Content-Type": "application/json" }, body: JSON.stringify({ message_body_days: operation.days,
          expected_version: operation.version, approval_reference: operation.reference }) });
      if (this.pending !== operation || !this.sameIdentity(operation)) return;
      this.validate(policy);
      if (policy.version !== operation.version + 1 || policy.message_body_days !== operation.days ||
          policy.approval_reference !== operation.reference || policy.approved_by_user_id !== operation.userId) throw new Error("write result mismatch");
      this.pending = null;
      if (this.dialog.open) { this.accept(policy); this.hint.textContent = "服务器已确认保留期配置。"; }
    } catch (error) {
      if (this.pending !== operation || !this.sameIdentity(operation) || error.stale) return;
      if ([400, 403, 404, 409].includes(error.status)) {
        this.pending = null; this.policy = null; this.current.replaceChildren(); this.confirm.checked = false;
        if (error.status === 403 || error.status === 404) {
          this.allowed = false; this.days.value = ""; this.reference.value = "";
          if (this.dialog.open) this.hint.textContent = "权限已失效，已清空配置。请联系集团管理员核查服务器状态。";
        } else if (this.dialog.open) this.hint.textContent = error.status === 409 ?
          "操作冲突：版本已变化，或已有消息而不允许延长期限。请刷新后重新填写并确认。" : "请求被拒绝，请刷新当前配置，检查期限和审批引用。";
      } else if (this.dialog.open) this.hint.textContent = `结果待确认（原版本 ${operation.version}），请先查询服务器状态。`;
    } finally { operation.sending = false; if (this.sameIdentity(operation)) this.update(); }
  }
};
