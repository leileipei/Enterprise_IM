# P1-02b 通信策略版本实施计划

**基线：** `dev/p1-02-identity-policy`。**规格：** `docs/superpowers/specs/2026-09-28-p1-02b-policy-store-design.md`。

## Task 1：策略版本迁移

**文件：** `db/migrations/000003_policy_store.up.sql`、`.down.sql`、`internal/policystore/migration_test.go`。

- [ ] 先写 PostgreSQL 集成测试：同租户版本/规则、跨租户引用拒绝、已发布版本和规则不可变、Down 后重建。
- [ ] 确认缺失迁移时红灯；实现显式事务、复合外键与必要触发器。
- [ ] 在 PostgreSQL 16 运行绿灯和完整回归；提交。

## Task 2：完整快照发布

**文件：** `internal/policystore/publish.go`、`internal/policystore/publish_test.go`；按需扩展 `internal/policy/evaluator.go` 的规则元数据。

- [ ] 先写合法发布、非集团管理员拒绝、跨租户与无审批白名单拒绝、例外必须引用本版本隔离、版本冲突和并发发布测试。
- [ ] 红灯后实现管理员任职重验、事务锁、规则校验、完整快照写入与管理审计。
- [ ] 绿灯、完整回归；提交。

## Task 3：当前版本判定与审计

**文件：** `internal/policystore/evaluate.go`、`internal/policystore/evaluate_test.go`。

- [ ] 先写无策略默认、跨组织白名单、hard_deny、过期例外、跨租户目标拒绝及审计失败不允许测试。
- [ ] 红灯后实现数据库身份重验、当前版本规则读取、纯函数判定和同步审计。
- [ ] 绿灯、完整回归、静态检查和构建；提交。

## Task 4：验收与独立评审

**文件：** `README.md`、`docs/开发增量-P1-02b-验收记录.md`。

- [ ] 重跑完整 PostgreSQL 集成测试和构建检查，对照规格确认未开放无认证 HTTP 接口。
- [ ] 独立评审跨租户、版本竞态、规则不可变与审计，修复重要问题。
- [ ] 记录证据和限制，上传分支，创建以 P1-02a 分支为基线的草稿 PR。
