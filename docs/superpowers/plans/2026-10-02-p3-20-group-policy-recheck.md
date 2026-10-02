# P3-20 群策略复核实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 群主或管理员能显式复核 `policy_blocked` 群，并仅在全员策略允许时恢复。

**Architecture:** 后端新增独立复核服务和 POST 路由。沿用群行串行化、来源任职加载和全员双向策略判断；结果与审计原子提交。

**Tech Stack:** Go、pgx、PostgreSQL、net/http。

**Spec:** `docs/superpowers/specs/2026-10-02-p3-20-group-policy-recheck-design.md`

## 文件职责

- `internal/policystore/group_policy_recheck.go`：授权、全员复核、状态和审计事务。
- `internal/policystore/group_policy_recheck_test.go`：真实 PostgreSQL 状态、权限、时间和回滚测试。
- `internal/httpserver/group_policy_recheck.go` 及测试：POST 校验、调用和错误映射。
- `internal/httpserver/groups.go`、`conversations.go`：路由及服务接口。
- `README.md`：接口与恢复语义。

## 步骤

1. [x] 写 PostgreSQL 集成测试，确认当前缺少复核能力；实现角色检查、全员复核和成功恢复。
2. [x] 写冲突、失效、时间、审计回滚测试；补齐原子状态和拒绝审计。
3. [x] 写 HTTP 失败测试；实现严格空请求、错误映射和响应。
4. [ ] 更新 README；运行测试、静态检查与代码审阅，提交并创建草稿 PR。

## 复审重点

- 普通成员或其他租户不能获知策略冲突细节。
- 同一个群仅在全部有向成员对允许时恢复。
- 来源任职到期或冻结不能因旧策略决策而恢复。
- 审计失败不得留下已恢复状态。
- 重复复核不得生成消息、区间或重复的状态转换。
