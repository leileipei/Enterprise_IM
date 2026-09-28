# P2-03 Outbox 发布 Worker 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把已持久化的消息 Outbox 事件以至少一次语义发布到 Redis Stream，并在失败时安全重试。

**Architecture:** 独立 Go Worker 在 PostgreSQL 事务中以 `FOR UPDATE SKIP LOCKED` 领取一条到期事件，调用 Redis `XADD`，再提交发布状态或退避时间。后续 Gateway 使用稳定事件 ID 去重，PostgreSQL 保留消息事实。

**Tech Stack:** Go 1.27.1、PostgreSQL 16、pgx/v5、Redis Streams、go-redis/v9。

**Spec:** `docs/superpowers/specs/2026-09-28-p2-03-outbox-worker-design.md`

## Global Constraints

- 不更改 P2-02 消息写入事务与 ACK 语义。
- Stream 载荷只有事件/租户/会话/消息 ID、seq 和类型，不含正文。
- 多 Worker 通过数据库行锁互斥；Redis 与 PostgreSQL 之间只承诺至少一次。
- 失败退避为 1、2、4 秒递增，最多 5 分钟；发布超时 5 秒。
- 本增量不实现 Gateway、推送、设备 ACK 和补拉。

## Review Focus

- 两个 Worker 同时领取同一行时只能有一个发布；另一个能处理别的行或返回空。
- Redis `XADD` 成功但数据库提交失败时，事件可重试，消费者有稳定 `event_id` 去重。
- Redis 失败后事件保持 `pending`，退避时间和次数原子更新；数据库失败则原状态不变。
- 进程退出时不把未成功提交的事件误标为 `published`。
- Redis 载荷不得泄漏消息正文。

---

### Task 1: 数据库领取与失败退避

**Files:** `internal/outbox/worker.go`、`internal/outbox/worker_test.go`。

**Interfaces:** `Event` 包含 `ID/TenantID/ConversationID/MessageID/EventType/Seq`；`Publisher.Publish(context.Context, Event) error`；`Worker.ProcessOne(context.Context) (bool, error)`。

- [x] 写失败测试：无到期事件、成功发布标记、失败退避、多连接 `SKIP LOCKED`、状态更新故障回滚。
- [x] 运行定向测试确认缺少 Worker。
- [x] 实现单条事务领取、5 秒发布超时、原子状态更新和指数退避。
- [x] 运行 PostgreSQL 定向测试并提交。

### Task 2: Redis Stream 适配器

**Files:** `internal/outbox/redis.go`、`internal/outbox/redis_test.go`、`go.mod`、`go.sum`。

**Interfaces:** `RedisPublisher` 实现 `Publisher`，将稳定事件字段写入配置的 Stream。

- [x] 写失败测试：真实 Redis Stream 收到完整标识字段且没有正文；Redis 不可用返回错误。
- [x] 运行定向测试确认适配器缺失。
- [x] 引入 go-redis/v9 并实现 `XADD`；运行 Redis 与 PostgreSQL 集成测试并提交。

### Task 3: 独立进程与交付文档

**Files:** `cmd/im-outbox-worker/main.go`、`cmd/im-outbox-worker/main_test.go`、`README.md`。

**Interfaces:** `IM_DATABASE_URL`、`IM_OUTBOX_REDIS_URL` 必需，`IM_OUTBOX_STREAM` 可选。

- [x] 写失败测试：缺失/无效配置拒绝启动，默认与自定义 Stream 名正确。
- [x] 运行定向测试确认配置解析缺失。
- [x] 实现启动检查、轮询循环、结构化日志、信号退出；更新运行说明与边界。
- [x] 全量 PostgreSQL/Redis 测试、`go vet ./...`、`go build ./...`、差异检查。
- [x] 独立审阅并修正；提交、推送，基于 `dev/p2-02-message-transaction` 创建草稿 PR。
