# P3-01 Group Membership Schema Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为群会话与成员历史建立可回滚、受租户和序号约束的 PostgreSQL 数据基础。

**Architecture:** 复用 `conversations` 的会话 ID、状态和 `last_seq`，添加与单聊字段互斥的群元数据和创建人任职外键；新建群成员区间表，用复合外键、活跃唯一索引、GiST 排斥约束和历史不可改写触发器保留授权边界。

**Tech Stack:** PostgreSQL 16、现有 Go 迁移集成测试。

**Spec:** `docs/superpowers/specs/2026-09-29-p3-01-group-membership-schema-design.md`

## Global Constraints

- 不更改现有单聊 API、消息写入与补拉语义。
- 所有群关系以 `tenant_id`、`conversation_id` 和用户任职为边界。
- 区间包含 `join_seq` 与 `leave_seq`，退群前无新消息时允许空区间。
- Down 遇到群会话时必须失败，不删除群历史。

## Review Focus

- 群行同时写入单聊字段或单聊行写入群字段时，数据库必须拒绝。
- 创建人的任职与群创建组织、法人不一致时，数据库必须拒绝。
- 跨租户、任职不属于用户、组织/法人快照不一致时，数据库必须拒绝。
- 先退群再入群允许两个不重叠区间，历史中间缺口不能被覆盖。
- `leave_seq=0` 的空区间可保存，但第二条活跃记录仍受唯一约束。
- 已退出区间不可重开、延长或删除，否则会填平历史访问缺口。
- 迁移回滚存在群数据时不得丢数据，清空群数据后能恢复原结构。

---

### Task 1: 群会话与成员区间迁移

**Files:** `db/migrations/000009_group_membership.up.sql`、`.down.sql`、`internal/policystore/group_schema_test.go`。

**Interfaces:** `conversations.kind='group'` 搭配 `group_name`、`group_creator_membership_id`、`group_creator_organization_id`、`group_creator_legal_entity_id`；`conversation_membership_intervals` 包含 `join_seq`、`leave_seq`、`role`、`status` 和任职快照。

- [x] 写 PostgreSQL 失败用例：群字段互斥、复合关系、活跃唯一、历史不重叠、空区间再入群、Down 保护。
- [x] 先运行测试，确认缺少 `000009` 结构而失败。
- [x] 实现 Up/Down 和索引，让目标测试通过。

### Task 2: 最新迁移链与单聊回归

**Files:** `internal/policystore/migration_test.go`、`conversations_test.go`、`internal/oidcauth/store_test.go`、`README.md`。

- [x] 将全链测试辅助迁移到 `000009`，涉及旧 `000005` 回滚的测试先按逆序回滚 `000009` 和 `000008`，再按顺序恢复。
- [x] 更新迁移说明并运行单聊写入、补拉、会话列表与全量 PostgreSQL 测试，确保旧功能不变。

### Task 3: 验证与评审

- [x] 运行全量 Go 测试、关键路径 `-race`、`go vet`、`go build` 与 `git diff --check`。
- [x] 完成独立评审，修正重要问题，提交基于 P2-10 的草稿 PR。
