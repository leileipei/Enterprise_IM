# P1-04d 可见组织索引实施计划

**目标：**只把至少有一个可见任职的组织及必要的有效祖先返回给普通员工。

**设计：**`docs/superpowers/specs/2026-09-28-p1-04d-visible-organization-index-design.md`

## Task 1：组织可见性与祖先投影

**文件：**新增 `internal/policystore/organizations.go`、`organizations_test.go`。

- [ ] 先写 PostgreSQL 失败测试：同组织与隐藏分支、虚拟祖先、发布放行/强制拒绝、停用祖先、跨租户、身份失效与审计失败。
- [ ] 运行定向测试，确认新服务方法缺失导致失败。
- [ ] 实现 `ListVisibleOrganizations(ctx,id) ([]DirectoryOrganization,error)`；同事务锁定身份，按当前策略逐任职判断，审计并投影有效祖先。
- [ ] 运行定向测试并提交领域层增量。

## Task 2：受保护接口与完整链路

**文件：**修改 `internal/httpserver/directory.go`、`directory_test.go`、`internal/oidcauth/store_test.go`、`README.md`。

- [ ] 先写 HTTP 失败测试：验证身份、无查询参数、GET 限制、响应顺序与错误映射。
- [ ] 运行定向测试，确认新路由缺失导致失败。
- [ ] 接入 `GET /api/v1/directory/organizations`，更新用户文档与签名令牌集成测试。
- [ ] 运行完整 PostgreSQL 测试、静态检查、构建及差异检查；独立审阅后提交并建立草稿 PR。

## Review Focus

- 候选组织缩小范围不能漏掉有效的跨组织放行或双向规则。
- 祖先节点只因可见后代而出现，隐藏分支与停用祖先不泄露。
- 组织成员调动和树结构并发更新不能导致跨租户或错误归属。
- 决策审计或请求审计失败时不能返回部分组织树。
- 大集团全拒绝场景的查询成本要清楚标为尚未容量验证。
