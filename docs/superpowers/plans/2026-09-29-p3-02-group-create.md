# P3-02 建群接口实施计划

**目标：** 交付逐对策略检查、原子写入与请求幂等的建群接口。

**设计：** `docs/superpowers/specs/2026-09-29-p3-02-group-create-design.md`。

## 文件分工

- `db/migrations/000010_group_create_request.{up,down}.sql`：建群请求幂等字段、索引和回滚保护。
- `internal/policystore/group_create.go`：输入验证、锁、逐对策略、群/区间写入与审计。
- `internal/policystore/group_create_test.go`：PostgreSQL 集成与并发验收。
- `internal/httpserver/groups.go`：严格 JSON 和 HTTP 错误映射。
- `internal/httpserver/groups_test.go`：接口验收。
- 现有迁移辅助、会话路由、README：接入新迁移和接口，保持单聊回归。

## 实施顺序

1. [x] 写迁移和服务失败用例，确认当前缺少请求幂等结构与建群方法。
2. [x] 实现 `000010` Up/Down，更新全链迁移测试和 README。
3. [x] 实现 `CreateGroup`，验证同租户、实时任职、全部有向成员组合、审计及原子回滚。
4. [x] 写 HTTP 失败用例，实现 `POST /api/v1/groups` 严格输入和响应。
5. [x] 全量测试、`-race`、`go vet`、构建与格式检查；独立评审并修复问题。
6. [x] 提交、推送，创建以 P3-01 为基线的草稿 PR。
