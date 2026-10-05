# P2-04 Direct Message Pull Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 增加单聊文本消息按序号补拉，依据发送时双方任职和当前强制拒绝过滤正文。

**Architecture:** 发送事务保存接收任职和双方组织快照，读取服务按 tenant/conversation/seq 分页；HTTP 只解析身份和游标。旧消息无可靠接收任职时占位。

**Tech Stack:** Go、pgx、PostgreSQL、现有 OIDC HTTP 认证。

**Spec:** `docs/superpowers/specs/2026-09-28-p2-04-direct-message-pull-design.md`

## Global Constraints

- 只覆盖单聊文本，结果不可宣称已实时投递。
- 最大页大小 500；所有错误响应不得暴露 SQL 细节或消息正文。
- 旧消息缺少接收任职时必须隐藏正文。

## Review Focus

- 会话重选后旧内容是否仍由写入时任职授权。
- 普通策略隔离与 hard deny 的历史处理是否有别。
- 第三方与跨租户请求是否无法探测内容。
- 审计失败是否阻止正文返回。
- 游标边界和空页能否稳定推进。

---

### Task 1: 保存消息历史接收任职

**Files:** `db/migrations/000007_message_recipient.up.sql`、`.down.sql`、`internal/policystore/messages.go`、`internal/policystore/messages_test.go`、`internal/policystore/message_send_test.go`、`internal/policystore/migration_test.go`

- [ ] 写迁移和发送测试，确认新消息的接收用户/任职及双方组织快照与会话选择一致，旧消息新列为空且数据库拒绝跨用户任职。
- [ ] 运行对应测试确认先失败。
- [ ] 新增迁移、在发送事务写入接收用户及任职；更新测试迁移装载。
- [ ] 运行对应测试并提交。

### Task 2: 服务端安全分页与撤权

**Files:** `internal/policy/evaluator.go`、`internal/policy/evaluator_test.go`、`internal/policystore/message_pull.go`、`internal/policystore/message_pull_test.go`

- [ ] 写测试覆盖分页、重选、普通策略变化、强制拒绝、冻结、旧数据、跨租户及审计失败，确认先失败。
- [ ] 实现 `Service.PullTextMessages(ctx, identity, conversationID, afterSeq, limit)`，一次事务完成验证、查询、过滤和审计。
- [ ] 运行 PostgreSQL 与策略单元测试并提交。

### Task 3: HTTP 补拉接口和说明

**Files:** `internal/httpserver/conversations.go`、`internal/httpserver/conversations_test.go`、`README.md`

- [ ] 写 HTTP 测试覆盖输入、认证、分页响应和错误映射，确认先失败。
- [ ] 加入 GET 路由及严格参数解析，更新 README 的迁移和使用说明。
- [ ] 运行全部测试、`go vet ./...`、`go build ./...`、`git diff --check`，独立评审并修正问题。
