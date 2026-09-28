# P1-04c 普通通讯录姓名搜索实施计划

**目标：**按姓名片段返回当前选定任职可见的人员和任职，分页信息只基于可见人员。

**设计：**`docs/superpowers/specs/2026-09-28-p1-04c-directory-name-search-design.md`

## Task 1：事务化搜索与审计

**文件：**新增 `internal/policystore/search.go`、`search_test.go`；必要时仅提取 P1-04b 中的共用校验或审计函数。

- [x] 先写 PostgreSQL 失败测试：可见过滤、同名去重、隐藏候选对 `has_more` 无影响、策略放行/强制拒绝、通配符字面匹配、500 候选边界和审计故障。
- [x] 运行定向测试，确认因新服务方法缺失而失败。
- [x] 实现 `SearchVisiblePeople(ctx,id,q,limit) (DirectorySearchPage,error)`；同事务验证身份、读当前策略、稳定排序、锁定候选并逐任职判定与审计。
- [x] 运行定向测试，提交领域层增量。

## Task 2：受保护的 HTTP 路由

**文件：**修改 `internal/httpserver/directory.go`、`directory_test.go`、`internal/oidcauth/store_test.go`、`README.md`。

- [x] 先写 HTTP 失败测试：签名身份、参数互斥与重复、边界值、响应结构和错误映射。
- [x] 运行定向测试，确认因路由缺失而失败。
- [x] 在 `/api/v1/directory/users` 按 `employee_no` 或 `q` 分流；新增姓名搜索响应，更新文档与令牌到数据库链路测试。
- [x] 运行 PostgreSQL 完整测试、`go vet ./...`、构建与差异检查；独立审阅后提交并建立草稿 PR。

## Review Focus

- 候选用户截断发生在授权前，超宽结果必须整体失败，不能错误返回 `has_more=false`。
- 搜索的 `has_more` 只能由可见人员产生，不能暴露隐藏人员数量。
- 人员与任职并发变更时不得混合身份或返回无权查看的任职。
- 任一决策或请求审计失败时不得返回部分资料。
- 冻结账号的认证层 401 与失效任职的服务层 403 应保持一致。
