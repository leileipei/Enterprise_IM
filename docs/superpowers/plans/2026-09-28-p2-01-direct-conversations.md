# P2-01 单聊会话发起实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 建立经过当前策略授权、同租户用户组合唯一的单聊会话发起接口。

**Architecture:** PostgreSQL 唯一键和复合外键守住会话身份，`policystore.Service` 在一个事务中完成任职锁定、`start_chat` 判定、会话写入和审计。独立 HTTP 包装器仅接收身份令牌与目标任职。

**Tech Stack:** Go 1.27.1、PostgreSQL 16、pgx/v5、现有 OIDC JWT 认证器。

**Spec:** `docs/superpowers/specs/2026-09-28-p2-01-direct-conversations-design.md`

## Global Constraints

- 所有数据访问带 `tenant_id`；目标用户与租户从数据库加载，不能从请求体信任。
- `start_chat` 按当前已发布策略判定，`hard_deny` 优先；失败不返回旧会话 ID。
- 策略决策、会话写入和请求审计同事务，提交后才返回 200。
- 本增量不实现消息、ACK、Outbox 或客户端。

## Review Focus

- 同一用户的不同任职应复用同一会话，跨租户相同用户组合不可复用。
- 并发创建只能产生一个会话，返回的 ID 相同。
- 策略在已有会话后变为拒绝时，重试不能泄漏旧会话 ID。
- 审计写入失败必须回滚新会话与选定任职更新。
- 错误目标、自聊和跨租户目标均不能产生会话或透露目标资料。

---

### Task 1: 会话约束迁移

**Files:** `db/migrations/000005_direct_conversations.up.sql`、`000005_direct_conversations.down.sql`、`internal/policystore/migration_test.go`、`internal/policystore/conversations_test.go`

**Interfaces:** `conversations` 表；用户组合部分唯一键；租户与用户任职复合外键。

- [x] 先写迁移失败测试：合法会话、跨租户错配、自聊、重复组合及回滚重装。
- [x] 运行定向测试，确认表不存在。
- [x] 写迁移与 down 脚本，更新测试数据库装载顺序。
- [x] 运行迁移与现有数据库测试，通过后提交。

### Task 2: 事务化单聊发起服务

**Files:** `internal/policystore/conversations.go`、`conversations_test.go`

**Interfaces:** `Service.StartDirectConversation(ctx context.Context, id access.TrustedIdentity, targetMembershipID string) (DirectConversation, error)`。

- [x] 先写服务失败测试：同组织、复用、多任职重选、策略允许及拒绝、身份失效、自聊、跨租户、审计回滚与并发创建。
- [x] 运行定向测试，确认方法缺失。
- [x] 在同一事务中锁任职、判策略、审计并原子插入或复用；成功后返回会话。
- [x] 运行定向与全量 PostgreSQL 测试，通过后提交。

### Task 3: 受保护 HTTP 与真实令牌链路

**Files:** `internal/httpserver/conversations.go`、`conversations_test.go`、`internal/oidcauth/store_test.go`、`cmd/im-api/main.go`、`README.md`

**Interfaces:** `POST /api/v1/conversations`，请求 `{target_membership_id}`，成功 200 JSON。

- [x] 先写 HTTP 失败测试：认证身份、严格 JSON、长度、方法、查询参数与错误状态。
- [x] 运行定向测试，确认路由缺失。
- [x] 接入服务并补签名令牌到数据库事务的测试。
- [x] 运行 PostgreSQL 全量测试、`go vet ./...`、`go build ./...`、差异检查；独立审阅后提交、推送并建草稿 PR。
