# P4-03 会话级法务保全 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为单聊和群聊提供可审计、可重试的会话级法务保全管理，给后续正文清理建立可靠的暂停条件。

**Architecture:** `conversation_legal_holds` 保存每个案件的当前状态，`conversation_legal_hold_events` 追加不可修改的登记与解除证据。`access.Service` 在可信身份与集团管理员授权下操作两表和审计，HTTP 层只解析严格请求。管理操作锁定会话行；后续 Worker 使用同一行锁和有效保全查询。

**Tech Stack:** Go、pgx、PostgreSQL、现有 OIDC 管理路由。

**Spec:** `docs/superpowers/specs/2026-10-02-p4-03-conversation-legal-hold-design.md`

## Global Constraints

- 本增量不物理删除正文，不修改 P4-01/P4-02 的到期读取遮蔽、消息 `seq`、幂等及 Outbox。
- 同一会话可有多项有效保全；只要任一项有效，未来 Worker 必须暂停该会话正文清理。
- 只有当前有效集团管理员可管理本租户会话；案件与解除审批引用只记录，不核验外部审批系统。
- `request_id` 在租户内跨登记和解除唯一；完全相同重试返回已有状态，不再执行状态转移。
- 保全状态、不可修改事件和审计同事务提交；Down 遇到历史记录必须拒绝。

## Review Focus

- 同一 `request_id` 用于另一会话、动作、引用或操作者：返回 409，不能泄露原保全内容（Task 2/3 测试）。
- 登记请求在保全已解除后重试：返回该保全当前已解除状态，不新建有效保全（Task 2 测试）。
- 某一案件解除但另一案件仍有效：有效保全查询仍为真（Task 3 测试）。
- 管理请求等待会话行锁期间授权失效：写入前重查并拒绝，不能留下事件（Task 2/3 测试）。
- 列表游标被换到另一租户或会话、损坏或超长：拒绝且不返回案件信息（Task 4 测试）。

---

### Task 1: 迁移与数据库不变量

**Files:** Create `db/migrations/000014_conversation_legal_hold.up.sql`、`.down.sql`、`internal/access/legal_hold_migration_test.go`; modify `internal/access/migration_test.go`、`internal/policystore/migration_test.go`、`internal/oidcauth/store_test.go`、`internal/policystore/conversations_test.go`。

**Interfaces:** Produces `conversation_legal_holds` with `id`, `tenant_id`, `conversation_id`, `case_reference`, `create_request_id`, `placed_by_user_id`, `placed_by_membership_id`, `placed_at`, nullable release fields; `conversation_legal_hold_events` with `hold_id`, `tenant_id`, `conversation_id`, `event_type`, `request_id`, `reference`, actor IDs and `occurred_at`. Active predicate is `released_at IS NULL`; partial unique index covers `(tenant_id, conversation_id, case_reference)` for active rows.

- [x] Write `TestLegalHoldMigrationConstraintsAndRollback`: seed two tenants/conversations; assert cross-tenant conversation and actor references fail, partial release fields fail, duplicate active case fails, second case succeeds, event UPDATE/DELETE fails, second release rewrite fails, Down with any history fails; empty Down/Up succeeds.
- [x] Run `test -n "$IM_TEST_DATABASE_URL" && go test ./internal/access -run '^TestLegalHoldMigrationConstraintsAndRollback$' -count=1 -v`; expect RED because migration/table is absent.
- [x] Implement migration with composite foreign keys, unique tenant request IDs across both event types, one `placed` and one `released` event per hold, immutable event trigger, one-way release trigger, and protective Down; add `000014` to all four migration-loading fixtures, with reverse Down order in conversation rollback test.
- [x] Run the target test and `test -n "$IM_TEST_DATABASE_URL" && go test ./internal/access ./internal/policystore ./internal/oidcauth -count=1`; expect PASS, then commit migration and fixture changes.

### Task 2: 登记、重试与权限

**Files:** Create `internal/access/legal_hold.go`、`internal/access/legal_hold_place.go`、`internal/access/legal_hold_place_test.go`.

**Interfaces:** `LegalHold` contains ID, conversation ID, case reference, placement actor/time and nullable release metadata. `PlaceLegalHold(ctx context.Context, id TrustedIdentity, conversationID, requestID, caseReference string) (LegalHold, bool, error)` returns `created=true` only for a new hold. Define `ErrInvalidLegalHold` for malformed IDs/references and use existing `ErrInvalidIdentity`、`ErrNotFound`、`ErrConflict`、`ErrAuditUnavailable`.

- [x] Write `TestPlaceLegalHoldAuthorizationAndReplay`: group admin succeeds; org admin and cross-tenant/nonexistent conversation see 404; invalid identity sees 403; same actor/request/payload replays without a second hold or placed event; changed actor, conversation or reference with the same request ID returns 409; second active hold for the same case returns 409.
- [x] Run `test -n "$IM_TEST_DATABASE_URL" && go test ./internal/access -run '^TestPlaceLegalHoldAuthorizationAndReplay$' -count=1 -v`; expect RED because service method is absent.
- [x] Implement validation (UUIDs, 1～128 字符引用、首尾无空白、无控制字符), actor/grant check, conversation lock, final-time recheck, status/event/audit atomic insert and exact replay comparison. Keep lock order: actor/grant and tenant before conversation; no request-supplied tenant/actor.
- [x] Add `TestPlaceLegalHoldRechecksExpiredGrantAfterConversationLockWait` and `TestPlaceLegalHoldAuditFailureRollsBack`, using two PostgreSQL connections and a failing audit trigger; run targeted tests until PASS.
- [x] Add `TestPlaceLegalHoldConcurrentCaseAndMembershipEnd`: distinct concurrent requests for one case create only one active hold, and concurrent `EndMembership` cannot create a lock cycle or commit a hold after authorization is lost; run until PASS and commit.

### Task 3: 解除与多案件保全

**Files:** Create `internal/access/legal_hold_release.go`、`internal/access/legal_hold_release_test.go`; modify `internal/access/legal_hold.go` if shared result metadata needs extension.

**Interfaces:** `ReleaseLegalHold(ctx context.Context, id TrustedIdentity, conversationID, holdID, requestID, approvalReference string) (LegalHold, error)`. The same `LegalHold` type from Task 2 reports release metadata; SQL active predicate remains `released_at IS NULL`.

- [x] Write `TestReleaseLegalHoldKeepsOtherCasesActive`: place two cases on one conversation; release one; assert one active remains, both events remain, first case can be newly placed again after release, the original placement request replays the released state, and using a placement request ID for release returns 409.
- [x] Run `test -n "$IM_TEST_DATABASE_URL" && go test ./internal/access -run '^TestReleaseLegalHold' -count=1 -v`; expect RED because release method is absent.
- [x] Implement release with the same actor/grant → conversation → hold lock order, exact request replay, one-time active-to-released transition, event and audit in one transaction; second distinct request on released hold returns 409.
- [x] Add `TestReleaseLegalHoldConcurrentAndAuditRollback`: two distinct concurrent release requests yield one transition, audit/event trigger failure leaves the hold active, and a conversation-lock wait followed by grant expiry leaves no release event; run targeted tests until PASS and commit.

### Task 4: 分页查询与受保护 HTTP 路由

**Files:** Create `internal/access/legal_hold_list.go`、`internal/access/legal_hold_list_test.go`、`internal/httpserver/legal_hold_admin.go`、`internal/httpserver/legal_hold_admin_test.go`; modify `cmd/im-api/main.go`、`README.md`.

**Interfaces:** `LegalHoldPage{Holds []LegalHold, NextCursor string}` and `ListLegalHolds(ctx context.Context, id TrustedIdentity, conversationID, cursor string, limit int) (LegalHoldPage, error)`. HTTP wrapper `HandlerWithLegalHolds(next http.Handler, authenticator Authenticator, service LegalHoldService) (http.Handler, error)` consumes all three service methods from Tasks 2～4.

- [x] Write `TestListLegalHoldsTenantScopeAndCursor`: stable `(placed_at,id)` ordering including tied timestamps, no omitted or duplicated records across pages, invalid/foreign cursor rejected, group admin required, audit failure returns no details; run target test and observe RED.
- [x] Implement cursor with URL-safe base64 encoded tenant/conversation/time/ID tuple, validate binding and length, query `limit+1`; run target test until PASS.
- [x] Write `TestLegalHoldAdminRoutesAndStrictBodies`: verified identity and acting membership forwarded; GET defaults to 100/max 500 and rejects bad query params; POST/release paths, 201 vs 200 replay, 400/403/404/409/503 mapping, `no-store`; duplicate/unknown JSON fields, malformed UTF-8, `null`, oversized/trailing body and spoofed tenant rejected before service; run and observe RED.
- [x] Implement route parser and bounded strict JSON decoding following `retention_admin.go`, wire into `cmd/im-api/main.go`, document `000014` migration、API and “保全应在清理前登记” in README; run `go test ./internal/httpserver ./cmd/im-api -count=1` until PASS and commit.

### Task 5: 全量验证与交付

**Files:** Review all files from Tasks 1～4; no additional product file unless verification finds a defect.

**Interfaces:** No new API. Produce a stacked draft PR based on `codex/p4-02-tenant-retention-policy` (PR #49).

- [x] Add a PostgreSQL pull regression showing an expired message remains redacted while its conversation is under legal hold; start dedicated PostgreSQL/Redis test services, set `IM_TEST_DATABASE_URL`、`IM_TEST_REDIS_URL` and real browser variables; run `go test ./... -count=1`, `go vet ./...`, `git diff --check`; explicitly run and inspect the browser acceptance test to exclude a skip.
- [x] Review migration rollback, tenant isolation, exact retry, row-lock order and read-side redaction; request independent code review and fix confirmed findings, then rerun affected tests.
- [x] Confirm clean status, push branch, create and attach stacked draft PR; stop dedicated test services. Report that physical clearing, backup expiry and external approval verification remain deferred.
