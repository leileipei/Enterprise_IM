# P4-02 租户消息正文保留期配置 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让租户持久化配置消息正文读取期限，并在单聊、群聊补拉中一致生效。

**Architecture:** 当前期限与最近一次审批保存在 `tenants` 行，每个批准版本追加到不可修改的 `tenant_retention_policy_history`。读事务的租户行锁与管理更新串行化；管理更新先锁操作者任职、再锁租户行，避免与结束任职形成锁环。受保护管理 API 调用 `access.Service`，按集团管理员授权和乐观版本更新；补拉从同一行读取期限，在最终时间判定正文可见性。

**Tech Stack:** Go、pgx、PostgreSQL、现有 OIDC 与 HTTP 路由。

**Spec:** `docs/superpowers/specs/2026-10-02-p4-02-tenant-retention-policy-design.md`

## Global Constraints

- 默认期限 365 个 24 小时天；配置范围 1～3650 天。
- 只修改正文读取可见性；序号占位、ACK 和 Outbox 契约不变。
- 审批引用只作为可追溯凭据，不代表服务端验证了外部审批。

## Review Focus

- 管理员写入等待租户读锁后授权过期：拒绝写入且不改版本。
- 两个管理员基于同一版本写入：只能一个成功，另一个 409。
- 审计插入失败：事务回滚，配置与版本不变。
- 另一个租户的期限不同：读取和管理均不得串租户。
- 到期边界恰等于期限：必须返回只含序号的占位。
- 已有消息后延长期限：拒绝，避免重新暴露此前遮蔽的正文。
- 连续两次审批：两个版本均可追溯且历史不能修改或删除；审计失败时历史也回滚。
- 更新保留期与结束任职并发：无死锁；HTTP 拒绝重复 JSON 字段和非法 UTF-8。

## Task 1: 数据迁移与审批约束

**Files:** `db/migrations/000013_tenant_retention.up.sql`、`.down.sql`；`internal/policystore/migration_test.go`；`internal/policystore/retention_migration_test.go`；迁移加载处。

- [x] 先写迁移集成测试：默认 365/版本 0、非法期限及不完整审批被拒、修改后 Down 拒绝。
- [x] 运行目标测试，确认因缺少迁移失败。
- [x] 实现 Up/Down 并加入全部测试迁移加载处。
- [x] 运行目标测试及 `go test ./internal/policystore -count=1`。

## Task 2: 单聊和群聊读取期限

**Files:** `internal/policystore/message_pull.go`、`group_history.go`、`retention.go`、相关测试。

- [x] 先写真实 PostgreSQL 测试：同一消息在 365 天默认遮蔽、自定义期限内可见；另一租户保持默认；单聊和群聊都覆盖到期边界。
- [x] 运行目标测试，确认默认常量导致自定义期限用例失败。
- [x] 从事务内租户行读取期限，并以最终时间判定正文；保留占位与游标。
- [x] 运行目标与包级测试。

## Task 3: 受保护配置管理 API

**Files:** `internal/access/retention.go`、相关集成测试；`internal/httpserver/retention_admin.go`、相关 HTTP 测试；`cmd/im-api/main.go`；`README.md`。

- [x] 先写服务测试：集团管理员读写、组织管理员拒绝、过期授权、版本冲突、审计失败回滚、租户隔离、已有消息后延长被拒；运行并观察失败。
- [x] 实现租户行锁、重新校验、版本更新与同事务审计；运行目标测试。
- [x] 先写 HTTP 测试：可信身份、严格 JSON、状态码、no-store；运行并观察失败。
- [x] 接入 GET/PUT 路由和生产 API，更新 README；运行目标与包级测试。
- [x] 执行真实 PostgreSQL/Redis/浏览器全量 `go test ./... -count=1`、`go vet ./...`、`git diff --check`，请独立评审后提交并创建草稿 PR。
