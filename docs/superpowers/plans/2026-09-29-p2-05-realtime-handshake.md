# P2-05 Realtime Handshake Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在现有 OIDC API 上提供可审计、短时、一次性的浏览器 WebSocket 握手。

**Architecture:** HTTP 令牌请求经数据库验证后写 Redis 票据；WebSocket 请求以子协议携票，经 Redis 原子消费和数据库复核后升级。连接只发送 ready 并提示 HTTP 补拉。

**Tech Stack:** Go 1.27、PostgreSQL 16、Redis 7、go-redis v9、coder/websocket v1.8.15。

**Spec:** `docs/superpowers/specs/2026-09-29-p2-05-realtime-handshake-design.md`

## Global Constraints

- 不推送消息正文或 Redis Stream 事件；WebSocket 只建立经验证的连接。
- 票据 30 秒且单次使用，Redis 故障关闭入口。
- 默认只允许同源；凭据不得出现在 URL、日志和回显子协议。

## Review Focus

- 并发使用同一票据能否建立多条连接。
- 冻结、任职结束及审计故障是否能阻止升级。
- 跨源请求和异常子协议是否可能绕过检查。
- Redis 丢失时是否有不安全的本机回退。
- 长连接停机、断开和限额是否正确释放。

---

### Task 1: 票据存储

**Files:** `internal/realtime/tickets.go`、`internal/realtime/tickets_test.go`、`go.mod`、`go.sum`

- [x] 写 Redis 真实集成测试：一次性、30 秒 TTL、过期、畸形票据、并发消费和故障，先确认失败。
- [x] 实现 `RedisTickets.Issue` 与 `Consume`，随机原票据只返回给调用方，Redis 使用摘要键和 `GETDEL`。
- [x] 运行针对性测试。

### Task 2: 任职验证与审计

**Files:** `internal/policystore/realtime_identity.go`、`internal/policystore/realtime_identity_test.go`

- [x] 写数据库测试：有效、冻结、结束任职、跨租户、审计失败、时间变化，先确认失败。
- [x] 实现票据和连接阶段审计验证，以及连接期间轻量复核。
- [x] 运行 PostgreSQL 测试。

### Task 3: HTTP 与 WebSocket 路由

**Files:** `internal/httpserver/realtime.go`、`internal/httpserver/realtime_test.go`

- [x] 写 HTTP/WebSocket 测试：严格输入、Origin、子协议、ready、重放、撤权与关闭，先确认失败。
- [x] 实现路由、同源握手、连接上限与定时复核。
- [x] 运行针对性测试。

### Task 4: 进程配置和文档

**Files:** `cmd/im-api/main.go`、`cmd/im-api/main_test.go`、`README.md`

- [x] 写配置测试：默认关闭、Redis URL 无效、OIDC 未开启时拒绝。
- [x] 按显式配置启用 Redis 与实时路由，启动时探测 Redis，记录正确使用方式和本增量边界。
- [x] 运行完整真实服务测试、`go vet ./...`、`go build ./...`、`git diff --check`，并完成独立评审。
