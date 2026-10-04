"use strict";
(() => {
  const MAX_BYTES = 26214400;
  const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
  const contextFields = ["identityKey", "membershipId", "identityEpoch", "conversation", "conversationEpoch", "kind"];
  const sameContext = (a, b) => !!a && !!b && contextFields.every(key => a[key] === b[key]);
  const validFilename = name => typeof name === "string" && name === name.trim() && name !== "." && name !== ".." &&
    new TextEncoder().encode(name).length >= 1 && new TextEncoder().encode(name).length <= 255 &&
    !/[\/\\:\p{Cc}]/u.test(name) && new TextDecoder("utf-8", {fatal: true}).decode(new TextEncoder().encode(name)) === name;
  function dispositionFilename(raw) {
    if (typeof raw !== "string" || /[\r\n\0]/.test(raw)) throw new Error("文件名响应无效");
    const match = /^attachment\s*;\s*(filename\*|filename)\s*=\s*(.+)$/i.exec(raw);
    if (!match) throw new Error("文件名响应无效");
    let name;
    if (match[1].toLowerCase() === "filename*") {
      if (!/^UTF-8''(?:[a-z0-9!#$&+.^_`|~\-]|%[0-9a-f]{2})+$/i.test(match[2])) throw new Error("文件名响应无效");
      try { name = decodeURIComponent(match[2].slice(7)); } catch (_) { throw new Error("文件名响应无效"); }
    } else if (/^"(?:[^"\\\r\n]|\\["\\])*"$/.test(match[2])) {
      name = match[2].slice(1, -1).replace(/\\(["\\])/g, "$1");
      if (/[^\x20-\x7e]/.test(name)) throw new Error("文件名响应无效");
    } else if (/^[a-z0-9!#$%&'*+.^_`|~\-]+$/i.test(match[2])) {
      name = match[2];
    } else throw new Error("文件名响应无效");
    if (!validFilename(name)) throw new Error("文件名响应无效");
    return name;
  }
  class FileTransport {
    #snapshot; #onHTTPError; #controllers = new Set();
    constructor({snapshot, onHTTPError}) { this.#snapshot = snapshot; this.#onHTTPError = onHTTPError; }
    static sameContext(a, b) { return sameContext(a, b); }
    static validFilename(name) { return validFilename(name); }
    static validUUID(value) { return typeof value === "string" && UUID.test(value); }
    static dispositionFilename(raw) { return dispositionFilename(raw); }
    contextChanged() { for (const controller of this.#controllers) controller.abort(); this.#controllers.clear(); }
    #check(original, controller) {
      const current = this.#snapshot();
      if (controller.signal.aborted || !sameContext(original, current) || original.token !== current.token ||
          original.tokenExpiresAt !== current.tokenExpiresAt || Date.now() >= original.tokenExpiresAt) {
        const error = new DOMException("请求已取消或上下文已变化", "AbortError");
        error.stale = !sameContext(original, current) || original.token !== current.token;
        throw error;
      }
    }
    async #run(fileID, signal, operation) {
      if (!FileTransport.validUUID(fileID)) throw new Error("文件 ID 无效");
      const original = Object.freeze({...this.#snapshot()});
      if (!original.identityKey || !FileTransport.validUUID(original.membershipId) || !original.token ||
          !Number.isFinite(original.tokenExpiresAt) || Date.now() >= original.tokenExpiresAt) {
        if (original.token && Date.now() >= original.tokenExpiresAt) this.#onHTTPError?.(401, "unauthorized");
        throw new Error("请先选择有效任职并登录");
      }
      const controller = new AbortController();
      const abort = () => controller.abort();
      if (signal?.aborted) abort(); else signal?.addEventListener("abort", abort, {once: true});
      this.#controllers.add(controller);
      const totalTimer = setTimeout(abort, 65000);
      const expiryTimer = setTimeout(abort, Math.min(2147483647, Math.max(0, original.tokenExpiresAt - Date.now())));
      let rejectAbort;
      const interrupted = new Promise((_, reject) => { rejectAbort = () => reject(new DOMException("请求已取消", "AbortError")); controller.signal.addEventListener("abort", rejectAbort, {once: true}); });
      const guarded = promise => Promise.race([promise, interrupted]);
      try {
        this.#check(original, controller);
        const headers = new Headers({Authorization: `Bearer ${original.token}`, "X-Acting-Membership-ID": original.membershipId});
        return await operation({original, controller, headers, guarded, check: () => this.#check(original, controller), path: `/api/v1/files/${fileID.toLowerCase()}/content`});
      } finally {
        clearTimeout(totalTimer); clearTimeout(expiryTimer);
        signal?.removeEventListener("abort", abort);
        controller.signal.removeEventListener("abort", rejectAbort);
        this.#controllers.delete(controller);
      }
    }
    async #httpError(response, run) {
      let code = "";
      try { const body = await run.guarded(response.json()); if (typeof body.error_code === "string" && /^[a-z_]{1,64}$/.test(body.error_code)) code = body.error_code; } catch (_) { /* Never echo a response body. */ }
      run.check(); this.#onHTTPError?.(response.status, code);
      const error = new Error("文件请求未完成，请核对服务器状态"); error.status = response.status; error.code = code; throw error;
    }
    async putFile(fileID, file, signal) {
      if (!file || !Number.isSafeInteger(file.size) || file.size < 1 || file.size > MAX_BYTES) throw new Error("文件大小无效");
      return this.#run(fileID, signal, async run => {
        if (!FileTransport.validUUID(run.original.conversation) || !["direct", "group"].includes(run.original.kind)) throw new Error("请选择有效会话");
        run.headers.set("Content-Type", "application/octet-stream");
        const response = await run.guarded(fetch(run.path, {method: "PUT", headers: run.headers, body: file,
          signal: run.controller.signal, credentials: "omit", cache: "no-store", redirect: "error"}));
        run.check(); if (!response.ok) return this.#httpError(response, run);
        if (response.redirected || response.status !== 200) throw new Error("上传响应无效，请核对服务器状态");
        const status = await run.guarded(response.json()); run.check(); return status;
      });
    }
    async readDownload(fileID, signal) {
      return this.#run(fileID, signal, async run => {
        const response = await run.guarded(fetch(run.path, {method: "GET", headers: run.headers, signal: run.controller.signal,
          credentials: "omit", cache: "no-store", redirect: "error"}));
        run.check(); if (!response.ok) return this.#httpError(response, run);
        const h = response.headers, length = h.get("Content-Length");
        if (response.status !== 200 || response.redirected || h.has("Location") || h.has("Content-Encoding") ||
            h.get("Content-Type")?.toLowerCase() !== "application/octet-stream" || h.get("X-Content-Type-Options")?.toLowerCase() !== "nosniff" ||
            !h.get("Cache-Control")?.toLowerCase().split(",").map(v => v.trim()).includes("no-store") ||
            !/^[1-9][0-9]*$/.test(length || "") || BigInt(length) > BigInt(MAX_BYTES) || !response.body?.getReader) throw new Error("下载响应无效");
        const filename = dispositionFilename(h.get("Content-Disposition"));
        const expected = Number(length), chunks = [], reader = response.body.getReader(); let received = 0, complete = false;
        try {
          for (;;) {
            const part = await run.guarded(reader.read()); run.check();
            if (part.done) break;
            if (!(part.value instanceof Uint8Array)) throw new Error("下载流无效");
            received += part.value.byteLength;
            if (received > expected || received > MAX_BYTES) throw new Error("下载长度无效");
            chunks.push(part.value.slice());
          }
          if (received !== expected) throw new Error("下载未完整，请重新请求");
          run.check(); const blob = new Blob(chunks, {type: "application/octet-stream"}); complete = true;
          return {filename, blob};
        } finally {
          if (!complete) { run.controller.abort(); try { await run.guarded(reader.cancel()); } catch (_) { /* Release partial bytes. */ } }
          reader.releaseLock?.(); for (const chunk of chunks) chunk.fill(0); chunks.length = 0;
        }
      });
    }
  }
  window.FileTransport = FileTransport;
})();
