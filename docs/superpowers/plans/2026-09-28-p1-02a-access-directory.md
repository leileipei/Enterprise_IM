# P1-02a 管理授权与通讯录实施计划

**基线：** `dev/group-foundation` 的 P1-01。**规格：** `docs/superpowers/specs/2026-09-28-p1-02a-access-directory-design.md`。

**目标：**完成可在 PostgreSQL 16 上验收的管理员 Scope、受限人员查询和单组织离职服务；保持公共 HTTP 服务仅有健康探针。

## 全局约束

- 一切业务查询携带 tenant_id；管理员角色不跨租户。
- `acting_membership_id` 必须显式提供，并从数据库校验归属、账号/组织/租户状态及有效期。
- 授权绑定选定任职；组织管理员仅管理明确授权组织，不隐式继承子组织。
- 管理查询和变更均写审计；审计失败不返回业务数据，也不提交变更。
- 用服务端时钟判定有效期。未接入 SSO 前，不开放管理 HTTP API。

## Task 1：管理员授权与审计迁移

**文件：** `db/migrations/000002_admin_access.up.sql`、`.down.sql`、`internal/access/migration_test.go`。

**产出：** `admin_grants` 绑定同租户任职及目标组织；`audit_events` 记录管理结果。迁移显式事务，Down 仅撤销本次对象。

- [ ] 先写迁移集成测试：合法集团/组织授权，跨租户 Scope、错误任职组织、角色和 Scope 组合无效均被数据库拒绝；回滚重建可用。
- [ ] 在 PostgreSQL 16 运行红灯，确认缺失迁移是预期失败。
- [ ] 实现迁移和必要索引；运行绿灯与完整测试。
- [ ] 提交迁移和测试。

## Task 2：选定任职与管理人员查询

**文件：** `internal/access/service.go`、`internal/access/service_test.go`。

**接口：** `Service.GetManagedPerson(ctx, TrustedIdentity, targetUserID) (Person, error)`；`TrustedIdentity` 仅供未来已验证的认证入口构造。

- [ ] 先写 G01/G02 测试：集团管理员看到双任职，组织管理员仅看到自己获授权组织，访问其他组织人员无详情且记拒绝审计；失效任职和跨租户拒绝。
- [ ] 运行红灯后实现 actor 解析、授予 Scope 查询、可见任职与部门查询、事务审计；所有 SQL 按租户过滤。
- [ ] 运行绿灯与完整测试；提交。

## Task 3：单组织离职

**文件：** `internal/access/service.go`、`internal/access/service_test.go`。

**接口：** `Service.EndMembership(ctx, TrustedIdentity, membershipID) error`。

- [ ] 先写 G05 测试：结束目标组织和部门任职，其他任职和集团账号仍有效；越权、重复结束及未来部门任职均拒绝并审计。
- [ ] 运行红灯后实现单事务、目标行锁、部门和组织区间关闭、审计；不自动转移主任职。
- [ ] 运行绿灯、完整测试、静态检查和构建；提交。

## Task 4：验收与评审

**文件：** `README.md`、`docs/开发增量-P1-02a-验收记录.md`。

- [ ] 重跑全部 PostgreSQL 集成测试、`go vet ./...`、`go build ./...` 和 P1-01 回归测试。
- [ ] 对照规格复核公开接口、跨租户与信息泄漏风险；做独立代码评审并修复重要问题。
- [ ] 记录本地验证、未完成的认证/策略持久化/API 范围，提交后上传分支，创建以 `dev/group-foundation` 为基线的草稿 PR。
