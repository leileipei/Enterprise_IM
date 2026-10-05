# P2-06 Stream Fanout Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把已发布的单聊消息事件转为每个在线设备的安全补拉通知。

**Architecture:** 每个 API 实例从 Redis Stream 独立读新事件，用 PostgreSQL 验证 Outbox 与消息并解析历史双方用户，再由本机 Hub 合并通知并写入已认证 WebSocket。任何读流或数据库故障关闭通知能力，客户端重连时依据 `ready` 进行 HTTP 补拉。

**Tech Stack:** Go 1.27、PostgreSQL 16、Redis 7、go-redis/v9、coder/websocket v1.8.15。

**Spec:** `docs/superpowers/specs/2026-09-29-p2-06-stream-fanout-design.md`

## Global Constraints

- 只发 `{"type":"sync_required"}`，不发正文、会话 ID、序号或送达 ACK。
- 多实例各自读取 Stream，绝不使用共享消费组来分配在线通知。
- 启动从 Stream 尾部开始；故障关闭连接并要求重新连接补拉。
- Worker 在健康循环刷新 10 秒存活标记；通知 API 需先确认同一 Redis/Stream 的标记。
- Stream 不自动裁剪；生产保留策略仍需容量与缺口检测设计。

## Review Focus

- 两个实例上的同一用户均应收到新事件的补拉信号。
- 伪造或跨租户的 Redis 字段不得路由到其他用户。
- 同事件重复发布和突发事件不得导致无界内存或重复帧风暴。
- Redis/数据库中断后，已有连接不得静默保持在线。
- 错配 Redis/Stream 或数据库核验卡住时，就绪状态必须失效。
- 授权在事件入队后被撤销时，写入前必须重新检查。

---

### Task 1: 本机订阅与 Stream 读取

**Files:** `internal/realtime/stream.go`、`internal/realtime/stream_test.go`

**Interfaces:** `RecipientResolver.ResolveMessageEvent(ctx, tenantID, eventID, conversationID, messageID string, seq int64) ([]string,error)`；`StartStreamFanout(ctx, redisClient, stream, resolver) (*Fanout,error)`；`Fanout.Subscribe(identity) (<-chan struct{},func())`；`Fanout.Done() <-chan struct{}`。

- [x] 写真实 Redis 测试：两个读者都接收、租户/用户隔离、事件去重与合并、读流失败后 Done 关闭。
- [x] 实现启动尾游标、逐实例 `XREAD`、有界事件 ID 去重及本地通知 Hub。
- [x] 运行 Stream 针对性测试。

### Task 2: 数据库事件接收人解析

**Files:** `internal/policystore/realtime_recipients.go`、`internal/policystore/realtime_recipients_test.go`

**Interfaces:** `Service.ResolveMessageEvent(ctx, tenantID, eventID, conversationID, messageID string, seq int64) ([]string,error)`。

- [x] 写 PostgreSQL 测试：真实 Outbox、跨租户/错 ID/错序号、旧消息无接收人和数据库故障。
- [x] 查询 Outbox 与消息交叉验证，只返回历史发送与接收用户 ID。
- [x] 运行数据库针对性测试。

### Task 3: WebSocket 通知与失效

**Files:** `internal/httpserver/realtime.go`、`internal/httpserver/realtime_test.go`

**Interfaces:** 保留 `HandlerWithRealtime`；新增 `HandlerWithRealtimeNotifications(..., source RealtimeNotificationSource)`。

- [x] 写 WebSocket 测试：ready 后通知、通知前撤权、源故障关闭、票据拒绝和 `/health/ready` 失效。
- [x] 连接成功即订阅，通知前复核身份；对源故障执行故障关闭。
- [x] 运行 HTTP/WebSocket 针对性测试。

### Task 4: 服务接线与说明

**Files:** `cmd/im-api/main.go`、`cmd/im-api/main_test.go`、`README.md`

- [x] 写 Stream 配置测试：默认名称、非法名称、与 Worker 一致的可配置名称。
- [x] 在监听 HTTP 前启动 Stream 读者并接入通知 Handler，更新客户端行为和部署边界。
- [x] 运行完整 PostgreSQL/Redis 测试、关键路径 `-race`、`go vet ./...`、`go build ./...`，完成独立评审。

### Task 5: 评审后可靠性修正

**Files:** `internal/outbox/presence.go`、`cmd/im-outbox-worker/main.go`、`internal/realtime/stream.go` 和对应测试。

- [x] 给每次数据库接收人核验设置 3 秒期限，超时使读者和就绪状态失效。
- [x] 未验证出接收人的事件不占用去重 ID；测试错误事件先于真实事件。
- [x] Worker 刷新带 10 秒 TTL 的 Stream 存活标记；API 启动和运行时检查，测试错配与过期。
- [x] 在慢速事件批次中约每秒复核存活标记，防止 100 条事件将故障检测拖延至数分钟。
