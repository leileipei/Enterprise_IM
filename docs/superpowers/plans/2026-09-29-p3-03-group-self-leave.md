# P3-03 群成员资格与主动退群 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让本人查询群成员区间并安全退群，旧请求重试不会关闭重新入群的新区间。

**Architecture:** 服务层以群会话行锁串行化状态改变，以明确区间 ID 定位退群目标，审计与区间关闭同事务。HTTP 层严格验证路径、JSON、身份并映射稳定错误码。

**Tech Stack:** Go、pgx、PostgreSQL、`net/http`。

**Spec:** `docs/superpowers/specs/2026-09-29-p3-03-group-self-leave-design.md`

## Global Constraints

- 维持租户隔离与统一用户主体；仅已验证且当前任职有效的用户可调用。
- 已关闭区间不可修改；重试仅针对指定旧区间。
- 群主转让接口尚未提供，活跃群主不能自退。
- 不开放群消息与邀请接口。

## Review Focus

- 重新入群后旧退群请求重试：返回旧结果，新区间仍活跃。
- 同一用户切换有效组织任职：仍能退本人区间。
- 群处于 `policy_blocked`：允许退群。
- 审计写入失败：成员状态回滚。
- 他人、跨租户或非群 ID：不泄露成员详情。

---

### Task 1: 服务层查询与退群

**Files:** Create `internal/policystore/group_membership.go`; Test `internal/policystore/group_membership_test.go`.

**Interfaces:** `GetOwnGroupMembership(context.Context, access.TrustedIdentity, string) (GroupMembership, error)`；`LeaveGroup(context.Context, access.TrustedIdentity, string, string) (GroupLeaveResult, error)`。

- [x] 写测试覆盖正常查询/退群、序号截断、重试及重新入群、群主拒绝、非成员和跨租户、有效任职切换、策略封锁、审计失败。
- [x] 运行 `go test ./internal/policystore -run 'Test(GroupMembership|LeaveGroup)'`，确认新行为失败。
- [x] 实现最小服务行为与稳定错误值。
- [x] 重跑定向测试，确认通过。

### Task 2: HTTP 路由

**Files:** Modify `internal/httpserver/conversations.go`, `internal/httpserver/conversations_test.go`, `internal/httpserver/groups.go`; Create `internal/httpserver/group_membership_test.go`.

**Interfaces:** 依赖 Task 1 的两个服务方法与结果类型。

- [x] 写测试覆盖身份绑定、请求格式、错误映射、成功与重试响应、非群路径。
- [x] 运行 `go test ./internal/httpserver -run 'TestGroup(Membership|Leave)'`，确认失败。
- [x] 实现路由与输入输出。
- [x] 重跑定向测试，确认通过。

### Task 3: 文档与全量验证

**Files:** Modify `README.md`.

- [x] 描述接口使用方式及群主转让前不能自退的限制。
- [x] 运行 `go test ./... -count=1`、`go test -race` 定向用例、`go vet ./...`、`go build ./...`、`git diff --check`。
- [x] 独立审阅变更并修复发现的问题，然后提交和创建草稿 PR。
