# P2-02 文本消息可靠写入实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 已授权文本消息在一个事务中生成连续 seq、幂等记录和待发布 Outbox，并在提交后返回 ACK。

**Architecture:** PostgreSQL 行锁串行化单聊写入；`policystore.Service` 复用任职与策略加载，新增消息事务服务。现有会话 HTTP 包装器扩展消息写入路径。

**Tech Stack:** Go 1.27.1、PostgreSQL 16、pgx/v5、现有 JWT 认证器。

**Spec:** `docs/superpowers/specs/2026-09-28-p2-02-durable-message-write-design.md`

## Global Constraints

- 只接收文本消息；正文最大 16 KiB；客户端 ID 为时间窗口内 UUIDv7。
- 同键同摘要返回原 ACK；同键异摘要返回 409，不写消息或新 Outbox。
- 新消息实时复核当前任职和 `send_message` 策略；所有业务 SQL 带 `tenant_id`。
- 消息、幂等记录、Outbox、seq、限流与审计同事务，提交后才 ACK。
- Outbox Worker、WebSocket 推送与补拉不属于本增量。

## Review Focus

- 两个连接对同一会话并发发送，seq 必须唯一连续。
- 同键并发重试与不同正文冲突，不能各自分配 seq。
- 审计或 Outbox 插入失败，不能留下消息、限流占用或 seq 缺口。
- 冻结账号的重复请求不能得到旧 ACK；策略撤权后新消息拒绝。
- 会话两端任职在写入期间重选时，不得用旧授权继续写入。

---

### Task 1: 消息与 Outbox 数据约束

**Files:** `db/migrations/000006_message_write.up.sql`、`000006_message_write.down.sql`、`internal/policystore/migration_test.go`、`internal/policystore/messages_test.go`

**Interfaces:** `messages`、`message_idempotency`、`outbox_events`、`message_rate_windows`。

- [ ] 写失败测试：租户、会话、发送任职复合外键，seq 与幂等唯一性，Outbox 消息归属，迁移回滚。
- [ ] 运行定向测试确认表缺失。
- [ ] 添加前后向迁移和测试装载，验证数据库约束。
- [ ] 提交迁移增量。

### Task 2: UUIDv7 与事务写入

**Files:** `internal/policystore/messages.go`、`messages_test.go`、`internal/policystore/message_uuid.go`、`message_uuid_test.go`

**Interfaces:** `Service.SendTextMessage(ctx,id,conversationID,clientMessageID,text) (MessageACK,error)`；`Service.MessageRatePerSecond` 可选配置，默认 10。

- [ ] 写失败测试：UUIDv7 7 天/未来 5 分钟、正文限制、同键重试和异正文冲突。
- [ ] 运行定向测试确认服务缺失；实现验证与原 ACK 查询。
- [ ] 写失败测试：新消息授权、限流、审计/Outbox 回滚、并发 seq 和任职重选。
- [ ] 实现单事务写入及错误类型；运行 PostgreSQL 全量测试并提交。

### Task 3: 受保护消息 API

**Files:** `internal/httpserver/conversations.go`、`conversations_test.go`、`internal/oidcauth/store_test.go`、`cmd/im-api/main.go`、`README.md`

**Interfaces:** `POST /api/v1/conversations/{id}/messages`，成功返回 ACK JSON。

- [ ] 写失败测试：签名身份、严格 JSON、正文边界、UUIDv7、路径、方法及错误映射。
- [ ] 运行定向测试确认路由缺失；接入服务并补真实签名令牌到数据库用例。
- [ ] 运行全量 PostgreSQL 测试、`go vet ./...`、`go build ./...`、差异检查。
- [ ] 独立审阅、修正、提交、推送并创建草稿 PR。
