# P2-09 Conversation Inbox Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让集团用户在当前任职下重新找到、打开并分页浏览已有单聊会话。

**Architecture:** PostgreSQL 服务按租户、参与者和任职筛选单聊，以更新时间和 ID 分页；现有目录策略控制对方资料显示。HTTP 增加只读列表接口，Web 页面在身份切换、实时信号和降级轮询时刷新。

**Tech Stack:** Go 1.27、PostgreSQL、现有策略引擎、原生 JavaScript。

**Spec:** `docs/superpowers/specs/2026-09-29-p2-09-conversation-inbox-design.md`

## Global Constraints

- 所有查询由已验证的租户、用户和任职限定；游标和请求参数不得成为授权依据。
- 对方资料仅在当前 `directory_view` 策略允许时返回；拒绝时不返回姓名或组织。
- 不返回消息正文、预览、未读数或送达状态。
- 策略决策及列表操作审计失败时请求失败。
- 页面旧身份请求不能回填新身份列表；仍用现有消息补拉接口复核正文权限。

## Review Focus

- 伪造游标或换租户/任职使用旧游标时仍只返回当前身份可见会话。
- 对方离职、隐藏或策略变更后不得泄露姓名与组织。
- 更新时间相同的两条会话必须稳定分页，不重不漏。
- 列表加载慢时切换任职，旧响应不得回填。
- 实时连接可用但当前没有打开会话时，新会话仍能出现在列表。

---

### Task 1: 受限会话列表服务

**Files:** `internal/policystore/conversation_list.go`、`conversation_list_test.go`、`db/migrations/000008_conversation_inbox.up.sql`、`.down.sql`。

**Interfaces:** `ListDirectConversations(context.Context, access.TrustedIdentity, string, int) (policystore.ConversationListPage,error)`；游标作为不透明字符串输入。

- [x] 测试本人、其他用户、其他任职、其他租户、失效身份、拒绝目录、相同时间分页、审计失败；先观察失败。
- [x] 实现严格游标解码、受限查询、资料策略判定及事务审计；针对性 PostgreSQL 测试通过。

### Task 2: HTTP 列表契约

**Files:** `internal/httpserver/conversations.go`、`conversations_test.go`。

- [x] 测试验证身份透传、参数错误、错误码映射和不会输出隐藏资料；先观察失败。
- [x] 接入 `GET /api/v1/conversations`，返回稳定 JSON；HTTP 测试通过。

### Task 3: Web 收件列表

**Files:** `internal/webclient/assets/index.html`、`app.js`、`style.css`、`README.md`。

- [x] 用本地浏览器模拟先复现已有会话缺失，再覆盖加载更多、实时通知和切换任职旧响应。
- [x] 实现列表加载、渲染、分页与刷新；浏览器用例通过，保留现有聊天发送行为。

### Task 4: 全量验证与评审

- [x] 运行全量 Go 测试、关键路径 `-race`、静态检查、构建、浏览器回归、`git diff --check`。
- [x] 独立代码评审并修正策略生效时刻和高频通知导致的列表刷新问题。
- [x] 提交草稿 PR。
