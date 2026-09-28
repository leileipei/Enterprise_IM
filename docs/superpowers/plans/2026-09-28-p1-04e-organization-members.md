# P1-04e 按组织浏览可见成员实施计划

**目标：**按组织实时过滤可见任职，并用已验证的可见任职作为分页锚点。

**设计：**`docs/superpowers/specs/2026-09-28-p1-04e-organization-members-design.md`

## Task 1：事务化成员分页

**文件：**新增 `internal/policystore/organization_members.go`、`organization_members_test.go`。

- [x] 先写 PostgreSQL 失败测试：同组织、跨组织策略、隐藏候选分页、兼岗字段隔离、可见与隐藏锚点、跨租户、身份失效和审计失败。
- [x] 运行定向测试，确认服务方法缺失导致失败。
- [x] 实现 `ListVisibleOrganizationMembers(ctx,id,orgID,afterMembershipID,limit) (DirectoryMemberPage,error)`；按键集批量扫描、逐任职锁定与判定，在一个事务中写全部审计。
- [x] 运行定向测试，确认领域层增量通过。

## Task 2：受保护 HTTP 接口

**文件：**修改 `internal/httpserver/directory.go`、`directory_test.go`、`internal/oidcauth/store_test.go`、`README.md`。

- [x] 先写 HTTP 失败测试：身份、路径 UUID、方法、`limit` 和 `after` 校验、响应与错误映射。
- [x] 运行定向测试，确认路由缺失导致失败。
- [x] 接入 `GET /api/v1/directory/organizations/{id}/members`，补签名令牌到数据库的完整链路。
- [x] 运行 PostgreSQL 完整测试、静态检查、构建及差异检查；独立审阅后提交并建立草稿 PR。

## Review Focus

- 锚点必须重新验证为本组织当前可见任职，不能用隐藏 ID 跳转分页。
- `has_more` 与 `next_after` 只能取决于可见人员，不能由隐藏候选推导。
- 同一用户在其他组织的任职和部门不能进入当前组织响应。
- 人员调动、冻结、规则发布与锁冲突时不得返回拼接或未审计资料。
- 大组织全隐藏场景的查询成本须列为容量验证事项。
