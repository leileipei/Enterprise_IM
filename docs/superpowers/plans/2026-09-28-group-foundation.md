# Group Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付集团多组织基础数据约束、通信边界判定核心和可运行的健康服务。

**Architecture:** PostgreSQL 保存带租户键和有效期的事实数据，并用外键、排斥约束和触发器保护任职不变量。Go 领域包执行通信策略判定；HTTP 进程只暴露健康接口，业务接口等身份认证后再开放。

**Tech Stack:** Go 1.27、PostgreSQL 16、pgx/v5、Go 标准库 HTTP 与测试工具。

**Spec:** `docs/superpowers/specs/2026-09-28-group-foundation-design.md`；完整产品基线：`enterprise_im_group_v2_2.docx`。

## Global Constraints

- 所有业务实体携带 tenant_id，跨租户访问一律拒绝。
- 任职区间为 `[effective_from, effective_to)`，结束可空；重叠任职和同时双主任职由数据库拒绝。
- 运营组织必填同租户 legal_entity_id；虚拟节点无任职资格。
- hard_deny 不可被例外覆盖；exception_allow 只能覆盖明确引用的普通隔离规则。
- 本增量不开放缺少认证的业务 API。

## Review Focus

- 空结束时间与边界相接的任职区间：相接应允许，重叠应拒绝；Task 1 集成测试覆盖。
- 更新组织任职有效期后使部门任职越界：数据库应拒绝；Task 1 集成测试覆盖。
- 错误覆盖规则 ID 和过期白名单：仍拒绝；Task 2 单元测试覆盖。
- hard_deny 与例外同时命中：仍拒绝；Task 2 单元测试覆盖。
- 数据库不可用：ready 返回 503；Task 3 HTTP 测试覆盖。

---

### Task 1: 集团基础表与任职约束

**Files:**
- Create: `go.mod`
- Create: `db/migrations/000001_group_foundation.up.sql`
- Create: `db/migrations/000001_group_foundation.down.sql`
- Create: `internal/groupdb/migration_test.go`
- Create: `internal/groupdb/testdata/` only if required by tests

**Interfaces:**
- Produces: PostgreSQL 表 `tenants`, `legal_entities`, `organizations`, `departments`, `users`, `user_organizations`, `user_departments`，供后续领域存储和身份服务使用。

- [ ] 写 PostgreSQL 集成测试，覆盖双任职、相接区间、同组织重叠、双主任职、跨租户法人、虚拟组织任职、跨组织部门和有效期包含关系。
- [ ] 运行测试，确认迁移缺失导致预期失败。
- [ ] 写迁移 SQL，使用复合外键、GiST 排斥约束与必要的约束触发器；Down 迁移只清理由本迁移创建的对象。
- [ ] 在 PostgreSQL 16 容器执行迁移与集成测试，确认全部通过；再运行迁移回滚与重建。
- [ ] 记录迁移和测试结果；提交 Task 1 变更。

### Task 2: 通信策略判定

**Files:**
- Create: `internal/policy/evaluator_test.go`
- Create: `internal/policy/evaluator.go`

**Interfaces:**
- Consumes: 已鉴权的操作者与目标任职，包含 tenant、organization、legal entity、有效状态；不直接查询数据库。
- Produces: `Evaluate(Input) Decision`，Decision 包含 allow/deny、reason、命中和覆盖的规则 ID，后续 API 与审计复用。

- [ ] 先写同组织、跨组织、隔离、精准例外、过期例外、hard_deny、跨租户和失效任职测试。
- [ ] 运行测试，确认缺失实现导致预期失败。
- [ ] 实现最小纯函数判定，明确规则匹配的操作、方向、任职和有效期。
- [ ] 运行包测试与全量测试，确认全部通过。
- [ ] 记录测试结果；提交 Task 2 变更。

### Task 3: 服务入口与就绪探针

**Files:**
- Create: `cmd/im-api/main.go`
- Create: `internal/httpserver/server_test.go`
- Create: `internal/httpserver/server.go`
- Create: `README.md`
- Create: `.gitignore`

**Interfaces:**
- Consumes: PostgreSQL DSN；就绪探针调用数据库 Ping。
- Produces: `/health/live` 和 `/health/ready`，供开发环境与后续部署使用。

- [ ] 先写 HTTP 测试，验证 live 成功、数据库正常时 ready 成功、数据库故障时 ready 返回 503。
- [ ] 运行测试，确认缺失实现导致预期失败。
- [ ] 实现 HTTP 服务与优雅退出，禁止在启动日志打印 DSN；README 写本地启动、迁移、测试与当前范围。
- [ ] 运行包测试、全量测试、`go vet ./...`、启动与探针检查。
- [ ] 记录验证结果；提交 Task 3 变更。

### Task 4: 独立验收与交接

**Files:**
- Create: `docs/开发增量-P1-01-验收记录.md`

**Interfaces:**
- Consumes: Tasks 1～3 的迁移、策略与服务。
- Produces: 有证据的范围与限制说明，为下一增量的身份认证、管理 API 和消息核心排期提供基线。

- [ ] 重新运行完整 Go 测试、PostgreSQL 集成测试、迁移回滚重建、静态检查和 HTTP 探针。
- [ ] 对照本计划和 v2.2 第 3、5、6、11 章复核已实现与未实现项。
- [ ] 在验收记录写测试命令、结果、环境、限制和下一增量建议。
- [ ] 评审全部变更，修复阻断问题；提交验收记录。
