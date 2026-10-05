# P3-05 群成员移除 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让群主和管理员按角色移除指定群成员区间，并保证重试安全。

**Architecture:** 沿用群行锁和不可变成员区间；服务层在事务内完成身份、角色、区间和审计；HTTP 层严格解析请求。

**Tech Stack:** Go、pgx、PostgreSQL、`net/http`。

**Spec:** `docs/superpowers/specs/2026-09-30-p3-05-group-remove-design.md`

## Global Constraints

- 调用人租户、用户和当前任职只取已验证身份。
- 活跃群主可移除他人管理员/普通成员；管理员只可移除他人普通成员。
- `policy_blocked` 允许移除；群状态恢复另行实现。
- 旧区间重试不得更改重新入群的新区间。

## Review Focus

- 目标重入群后旧移除请求：只返回旧结果。
- 管理员尝试移除另一管理员或群主：拒绝且无状态改变。
- 调用人当前任职在等待群锁期间到期：拒绝。
- 审计插入失败：移除回滚。
- 不存在、跨租户或其他群的区间：不泄露成员资料。

---

### Task 1: 服务层移除

**Files:** Create `internal/policystore/group_remove.go`; Test `internal/policystore/group_remove_test.go`.

**Interfaces:** `RemoveGroupMember(context.Context, access.TrustedIdentity, string, string) (GroupRemoveResult,error)`。

- [x] 写正常、拒绝、重试、到期和审计失败测试，确认新行为失败。
- [x] 实现锁序、角色权限、区间关闭和事务审计。
- [x] 重跑服务定向测试并确认通过。

### Task 2: HTTP 路由与文档

**Files:** Modify `internal/httpserver/conversations.go`, `internal/httpserver/conversations_test.go`, `internal/httpserver/groups.go`, `README.md`; Create `internal/httpserver/group_remove.go`; Test `internal/httpserver/group_remove_test.go`.

- [x] 写可信身份、严格请求、结果和错误映射测试，确认失败。
- [x] 实现接口并更新 README。
- [x] 全量测试、定向竞态测试、vet、build、差异检查。
- [x] 独立审阅、提交并创建基于 P3-04 的草稿 PR。
