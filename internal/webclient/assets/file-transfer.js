"use strict";
(() => {
  const types = ["application/pdf", "image/png", "image/jpeg", "text/plain"];
  const labels = {allocated: "已预约，尚未确认上传完成", uploaded: "已上传，等待扫描", scanning: "安全扫描中", ready: "扫描通过，可发送", rejected: "文件未通过扫描，不能发送", scan_failed: "扫描未完成，不能发送，请核对状态", delete_pending: "文件正在清理，不能发送", deleted: "文件已清理，不能发送"};
  class FileTransfer {
    #request; #transport; #context; #onReady; #onClear; #file = null; #origin = null; #body = null; #reservation = null;
    #phase = "empty"; #busy = false; #generation = 0; #controllers = new Set(); #pollTimer = null; #windowEnd = 0; #delivered = false;
    #select; #upload; #query; #retry; #cancel; #hint;
    constructor({request, transport, context, onReady, onClear}) {
      this.#request = request; this.#transport = transport; this.#context = context; this.#onReady = onReady; this.#onClear = onClear;
      const el = id => document.getElementById(id);
      this.#select = el("file-select"); this.#upload = el("file-upload"); this.#query = el("file-status-query");
      this.#retry = el("file-original-retry"); this.#cancel = el("file-cancel"); this.#hint = el("file-status");
      this.#select?.addEventListener("change", () => { const f = this.#select.files?.[0]; if (f) this.select(f).catch(() => {}); });
      this.#upload?.addEventListener("click", () => this.upload().catch(() => {}));
      this.#query?.addEventListener("click", () => this.queryStatus().catch(() => {}));
      this.#retry?.addEventListener("click", () => this.retryOriginal().catch(() => {}));
      this.#cancel?.addEventListener("click", () => this.contextChanged());
      document.addEventListener("visibilitychange", () => { if (document.hidden) this.#stopPoll(); else this.#schedule(); });
      this.#render();
    }
    #current() { return this.#origin && window.FileTransport.sameContext(this.#origin, this.#context()); }
    #check(generation) { if (generation !== this.#generation || !this.#current()) { const e = new Error("上下文已变化"); e.stale = true; throw e; } }
    #render(message) {
      if (message && this.#hint) this.#hint.textContent = message;
      if (this.#select) this.#select.disabled = this.#busy || !!this.#body;
      if (this.#upload) this.#upload.disabled = this.#busy || this.#phase !== "selected";
      if (this.#query) this.#query.disabled = this.#busy || !this.#reservation;
      if (this.#retry) { this.#retry.hidden = !["reserve_unknown", "put_unknown", "allocated"].includes(this.#phase); this.#retry.disabled = this.#busy; }
      if (this.#cancel) this.#cancel.hidden = this.#phase === "empty";
    }
    #stopPoll() { if (this.#pollTimer !== null) clearTimeout(this.#pollTimer); this.#pollTimer = null; }
    contextChanged() {
      this.#generation++; this.#stopPoll(); for (const c of this.#controllers) c.abort(); this.#controllers.clear();
      this.#file = null; this.#origin = null; this.#body = null; this.#reservation = null; this.#phase = "empty"; this.#busy = false; this.#windowEnd = 0; this.#delivered = false;
      if (this.#select) this.#select.value = ""; this.#onClear?.();
      this.#render("已取消本页操作；服务器可能已完成上传，对象不会因取消而删除。刷新后请补拉消息核对。");
    }
    async #json(path, options, generation) {
      const c = new AbortController(); this.#controllers.add(c); const timeout = setTimeout(() => c.abort(), 10000);
      let aborted; const interrupted = new Promise((_, reject) => { aborted = () => reject(new DOMException("请求已取消", "AbortError")); c.signal.addEventListener("abort", aborted, {once: true}); });
      try { this.#check(generation); const value = await Promise.race([this.#request(path, {...options, signal: c.signal, credentials: "omit", cache: "no-store", redirect: "error"}), interrupted]); this.#check(generation); return value; }
      finally { clearTimeout(timeout); c.signal.removeEventListener("abort", aborted); this.#controllers.delete(c); }
    }
    async select(file) {
      if (this.#busy || this.#body) throw new Error("请先取消当前文件");
      const c = this.#context();
      if (!c.identityKey || !window.FileTransport.validUUID(c.conversation) || !["direct", "group"].includes(c.kind)) throw new Error("请选择可发送的会话");
      let mime = file?.type || ({pdf: "application/pdf", png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", txt: "text/plain"})[file?.name?.split(".").pop().toLowerCase()];
      if (!file || !window.FileTransport.validFilename(file.name) || !Number.isSafeInteger(file.size) || file.size < 1 || file.size > 26214400 || !types.includes(mime)) {
        this.#render("请选择名称有效、1～25 MiB 的 PDF、PNG、JPEG 或 UTF-8 TXT 文件。"); throw new Error("文件声明无效");
      }
      this.#origin = Object.freeze({...c}); const generation = this.#generation; this.#busy = true; this.#render();
      try {
        const p = await this.#json("/api/v1/file-upload-policy", {method: "GET"}, generation);
        if (p.enabled !== true || !/^[1-9][0-9]*$/.test(p.max_size_bytes || "") || BigInt(p.max_size_bytes) > 26214400n || BigInt(file.size) > BigInt(p.max_size_bytes) ||
            !Array.isArray(p.allowed_media_types) || !p.allowed_media_types.includes(mime) || !p.allowed_media_types.every(v => types.includes(v))) throw new Error("租户上传策略不允许此文件");
        this.#check(generation); this.#file = file;
        this.#body = Object.freeze({upload_request_id: crypto.randomUUID(), original_filename: file.name, declared_media_type: mime, declared_size_bytes: String(file.size)});
        this.#phase = "selected"; this.#onClear?.(); this.#render("已选择文件；声明类型须经服务端检测和安全扫描。");
      } catch (e) { if (generation === this.#generation) this.#render("无法选择此文件，请核对上传策略。" ); throw e; }
      finally { if (generation === this.#generation) { this.#busy = false; this.#render(); } }
    }
    #validateStatus(v) {
      if (!v || !window.FileTransport.validUUID(v.file_id) || v.conversation_id !== this.#origin.conversation ||
          (this.#reservation && v.file_id !== this.#reservation.file_id) || v.original_filename !== this.#body.original_filename || v.declared_media_type !== this.#body.declared_media_type ||
          v.declared_size_bytes !== this.#body.declared_size_bytes || typeof v.state !== "string" || !Object.hasOwn(labels, v.state) ||
          typeof v.state_version !== "string" || !/^(0|[1-9][0-9]*)$/.test(v.state_version) || (v.state === "allocated" ? v.state_version !== "0" : v.state_version === "0") || BigInt(v.state_version) > 9223372036854775807n ||
          !Number.isFinite(Date.parse(v.created_at)) || !Number.isFinite(Date.parse(v.upload_expires_at)) || Date.parse(v.upload_expires_at) <= Date.parse(v.created_at)) throw new Error("文件状态响应无效");
      return Object.freeze({...v});
    }
    #accept(v) {
      this.#reservation = this.#validateStatus(v); this.#phase = v.state; this.#render(labels[v.state]);
      if (v.state === "ready") { this.#stopPoll(); this.#file = null; if (!this.#delivered) { this.#delivered = true; this.#onReady?.({fileID: v.file_id, context: this.#origin}); } }
      else if (["rejected", "scan_failed", "delete_pending", "deleted"].includes(v.state)) { this.#stopPoll(); this.#onClear?.(); }
      else if (["uploaded", "scanning"].includes(v.state)) this.#schedule();
    }
    async upload() { if (this.#phase !== "selected") throw new Error("请按原请求核对或重试"); return this.#uploadOriginal(); }
    async retryOriginal() { if (!["reserve_unknown", "put_unknown", "allocated"].includes(this.#phase)) throw new Error("当前状态不能重新上传"); return this.#uploadOriginal(); }
    async #uploadOriginal() {
      if (this.#busy || !this.#file || !this.#body || !this.#current()) throw new Error("当前文件不可上传");
      const generation = this.#generation; this.#busy = true; this.#stopPoll(); this.#render();
      let operation = "reserve";
      try {
        if (!this.#reservation) {
          const v = await this.#json(`/api/v1/conversations/${this.#origin.conversation}/files`, {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify(this.#body)}, generation);
          this.#reservation = this.#validateStatus(v);
        }
        if (this.#reservation.state !== "allocated") { this.#windowEnd = Date.now() + 120000; this.#accept(this.#reservation); return; }
        if (Date.now() >= Date.parse(this.#reservation.upload_expires_at)) { this.#phase = "expired"; throw new Error("上传预约已过期，请核对服务器状态"); }
        operation = "put"; const controller = new AbortController(); this.#controllers.add(controller);
        let v; try { v = await this.#transport.putFile(this.#reservation.file_id, this.#file, controller.signal); } finally { this.#controllers.delete(controller); }
        this.#check(generation); this.#windowEnd = Date.now() + 120000; this.#accept(v);
      } catch (e) {
        if (generation === this.#generation && this.#current()) {
          this.#phase = e.status === 410 || this.#phase === "expired" ? "expired" : operation === "reserve" ? "reserve_unknown" : "put_unknown";
          this.#onClear?.(); this.#render(this.#phase === "expired" ? "预约已过期；请先核对消息，再重新选择文件。" : "结果待确认：不会自动重新上传。请查询状态，或使用同一文件与原请求手动重试。");
        }
        throw e;
      } finally { if (generation === this.#generation) { this.#busy = false; this.#render(); this.#schedule(); } }
    }
    async queryStatus() { this.#windowEnd = Date.now() + 120000; return this.#queryStatus(false); }
    async #queryStatus(automatic) {
      if (this.#busy || !this.#reservation || !this.#current() || (automatic && (document.hidden || Date.now() >= this.#windowEnd))) return;
      const generation = this.#generation; this.#busy = true; this.#stopPoll(); this.#render();
      try { const v = await this.#json(`/api/v1/files/${this.#reservation.file_id}`, {method: "GET"}, generation); this.#accept(v); }
      catch (e) { if (generation === this.#generation) { this.#stopPoll(); this.#windowEnd=0; this.#onClear?.(); if(e.status===404 || e.status===410)this.#phase="unavailable"; this.#render(e.status===404 || e.status===410 ? "附件已不可用或预约已过期，不能发送，请核对消息。" : "状态查询未完成，请手动核对；不会自动重新上传。"); } throw e; }
      finally { if (generation === this.#generation) { this.#busy = false; this.#render(); this.#schedule(); } }
    }
    #schedule() {
      if (this.#pollTimer !== null || this.#busy || !this.#current() || document.hidden || !["uploaded", "scanning"].includes(this.#phase)) return;
      if (Date.now() + 2000 > this.#windowEnd) { this.#render("本轮状态查询已结束，请点击查询状态继续核对。"); return; }
      this.#pollTimer = setTimeout(() => { this.#pollTimer = null; this.#queryStatus(true).catch(() => {}); }, 2000);
    }
  }
  window.FileTransfer = FileTransfer;
})();
