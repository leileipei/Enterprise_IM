# P3-04 群成员邀请 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 安全邀请一名群成员，按策略逐对检查并保证重试不会重复入群。

**Architecture:** 群行锁串行化成员变化；当前群成员与目标任职按同一策略快照双向检查；邀请账本保持请求结果。HTTP 只接受经过身份验证的严格请求。

**Tech Stack:** Go、pgx、PostgreSQL、`net/http`。

**Spec:** `docs/superpowers/specs/2026-09-30-p3-04-group-invite-design.md`

## Global Constraints

- 租户和用户主体从已验证令牌取得；当前任职不得由请求体覆盖。
- 活跃群仅允许活跃群主或群管理员邀请；`policy_blocked` 不允许邀请。
- 策略动作固定为 `invite_group`，每名现有成员与新成员双向判定。
- 不开放群消息、批量邀请或成员移除。

## Review Focus

- 旧邀请重试发生在目标退群/重新入群之后：只返回原区间。
- 已有成员用另一任职作为目标：不得创建第二个活跃区间。
- 非邀请人现有成员与目标的策略拒绝：不得创建区间。
- 处理中策略或任职自然到期：拒绝，留下完整审计。
- 审计或账本写入失败：没有半提交成员。

---

### Task 1: 持久重试账本

**Files:** Create `db/migrations/000011_group_invitation.up.sql`, `.down.sql`; Modify `internal/policystore/migration_test.go`; Test `internal/policystore/group_invite_test.go`.

**Interfaces:** 账本键为 `(tenant_id,group_id,inviter_user_id,request_id)`，保存请求摘要和原区间 ID。

- [x] 写失败测试验证迁移结构和拒绝有数据时回滚。
- [x] 实现迁移并加入测试数据库加载路径。
- [x] 运行迁移定向测试确认通过。

### Task 2: 邀请服务

**Files:** Create `internal/policystore/group_invite.go`; Test `internal/policystore/group_invite_test.go`.

**Interfaces:** `InviteGroupMember(context.Context, access.TrustedIdentity, string, InviteGroupRequest) (GroupInvitation,error)`。

- [x] 写正常、拒绝、审计失败、重试和并发回归测试并确认失败。
- [x] 实现成员权限、锁序、双向策略、最终有效期复核和单事务写入。
- [x] 运行服务定向测试确认通过。

### Task 3: HTTP、文档和最终验证

**Files:** Modify `internal/httpserver/conversations.go`, `internal/httpserver/conversations_test.go`, `internal/httpserver/groups.go`, `README.md`; Create `internal/httpserver/group_invite.go`; Test `internal/httpserver/group_invite_test.go`.

- [x] 写 HTTP 输入输出和错误映射测试并确认失败。
- [x] 实现邀请路由，更新 README。
- [x] 完整 Go 测试、定向竞态测试、vet、build、diff 检查。
- [x] 独立审阅、提交并创建基于 P3-03 的草稿 PR。
