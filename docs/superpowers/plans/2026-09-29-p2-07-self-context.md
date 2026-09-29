# P2-07 Self Context Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让已完成 SSO 的浏览器取得本人当前可选任职，作为安全选择聊天身份的入口。

**Architecture:** HTTP 层从现有认证器取得可信 tenant/user，不读取请求中的身份参数；PostgreSQL 在一个事务中读取本人资料和有效任职。现有需要 acting membership 的业务接口保持原验证方式。

**Tech Stack:** Go 1.27、PostgreSQL 16、pgx/v5、现有 OIDC JWT 认证器。

**Spec:** `docs/superpowers/specs/2026-09-29-p2-07-self-context-design.md`

## Global Constraints

- `/api/v1/me` 只读已验证令牌映射的本人，不接受任职头、查询或正文。
- 仅返回当前有效任职；无任职的有效账号得到空数组。
- 不将返回的任职列表当作后续业务授权；业务接口继续实时复核。
- 选择任职前不伪造 acting membership 审计字段。

## Review Focus

- 伪造 tenant/user/acting membership 参数不能查询他人资料。
- 冻结账号或停用租户在令牌已通过后仍不得返回资料。
- 组织、法人或任职在有效期外不得作为可选项。
- 数据库故障或认证故障不得回退到客户端身份字段。
- 空任职和多任职顺序必须稳定，方便 UI 显示。

---

### Task 1: 本人任职读取

**Files:** `internal/policystore/self_context.go`、`internal/policystore/self_context_test.go`

**Interface:** `Service.GetSelfContext(ctx, tenantID, userID string) (SelfContext,error)`。

- [x] 写 PostgreSQL 测试：多任职、空任职、过期/停用、冻结/跨租户及故障。
- [x] 在事务中复核用户和租户状态，读取当前有效任职并稳定排序。
- [x] 运行数据库针对性测试。

### Task 2: Bearer-only HTTP 入口

**Files:** `internal/httpserver/admin.go`、`internal/httpserver/self_context.go`、对应测试。

**Interface:** `HandlerWithSelfContext(base, authenticator, service) (http.Handler,error)`。

- [x] 写 HTTP 测试：认证、严格输入、无缓存、状态码、原业务任职头不变。
- [x] 抽出仅验证 Bearer 的现有认证逻辑；新增 `/api/v1/me` 包装路由。
- [x] 运行 HTTP 针对性测试。

### Task 3: 服务接线与文档

**Files:** `cmd/im-api/main.go`、`internal/oidcauth/store_test.go`、`README.md`。

- [x] 将自助身份路由接入开启 OIDC 的 API，补签名令牌端到端测试。
- [x] 更新接口说明与浏览器客户端前置条件。
- [x] 运行完整数据库测试、关键路径 `-race`、`go vet ./...`、`go build ./...` 并独立评审。
