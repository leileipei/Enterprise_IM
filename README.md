# Enterprise IM

集团型多组织企业即时通信项目。产品基线见 [v2.2 需求文档](enterprise_im_group_v2_2.docx)，实施路径见 [开发计划](docs/企业IM-开发计划与实施路径-v0.1.md)。

## 当前开发增量

本分支实现 P1-01 集团模型与通信策略核心、P1-02a 管理授权与单组织离职、P1-02b 策略快照及决策审计、P1-03a 受保护管理 API、P1-03b OIDC JWT 访问令牌验证与本地身份映射、P1-03c 管理端人员检索、P1-04a 普通员工通讯录任职详情、P1-04b 按集团工号精确查找可见同事、P1-04c 按姓名片段搜索可见同事、P1-04d 可见组织索引，以及 P1-04e 按组织浏览可见成员。配置有效的身份提供方后，可显式启用这些受保护接口。**尚无浏览器登录流程、聊天界面或消息收发功能；客户身份提供方尚未联调。**

受保护管理 API 合约：

| 方法与路径 | 用途 | 成功响应 |
| --- | --- | --- |
| `GET /api/v1/admin/users?q={关键词}&limit={1..50}` | 在管理员授权组织内按姓名或集团工号检索在职人员 | 200，`people` 与 `has_more` |
| `GET /api/v1/admin/users/{id}` | 查询授权范围内的人员任职 | 200，snake_case JSON |
| `POST /api/v1/admin/memberships/{id}:end` | 结束一个组织任职 | 204，无响应体 |

访问令牌须为签给本 API 的 RFC 9068 JWT，具有 RS256 签名、`at+jwt` 类型、正确发行方及受众，并包含允许的 `client_id`。仅通过 `(issuer, sub)` 查找受控导入的本地身份绑定；选定任职由数据库二次校验。ID Token、邮件地址和请求中的租户 ID 不用于映射。401 表示未认证，400 表示 ID 或请求格式错误，403 表示身份失效，404 隐藏无权限资源，409 表示状态冲突，503 表示认证、审计或数据库不可用。

三个管理接口均要求 `Authorization: Bearer <access-token>` 和 `X-Acting-Membership-ID: <uuid>`。检索关键词不能为空，最多 100 个字符；`limit` 默认 20，最大 50。结果只包含 ID、集团工号和姓名；`has_more=true` 时需缩小关键词继续查找，不提供翻页。检索会写入管理审计，且只返回当前有效任职落在管理授权范围内的在职人员。它不代表普通员工的通讯录可见性。

普通员工可用 `GET /api/v1/directory/memberships/{id}` 查看一个目标任职，使用同样的身份头。服务端按当前 `directory_view` 策略实时判断，并只返回该任职的用户 ID、姓名、集团工号、组织、职位、主任职和有效部门。不可见或不存在返回 404；本人选定任职失效返回 403；审计或数据库故障返回 503。该接口不接受查询参数中的权限或目标用户声明，允许查看不代表允许发起单聊。

普通员工还可用 `GET /api/v1/directory/users?employee_no={集团工号}` 精确查找一个人员，身份头与目录详情相同。工号去除前后空格后必须非空，最多 128 个字符；`employee_no` 模式不做模糊匹配。响应只列出此人在当前选定任职下可见的组织任职；不存在与全部不可见都返回 404。账号冻结或令牌无效由认证层返回 401；已认证但选定任职失效返回 403。每个候选任职写策略决策审计，整次查找另写一条不含工号原文的审计记录，审计失败不返回结果。

普通员工可用 `GET /api/v1/directory/users?q={姓名片段}&limit={1..20}` 搜索可见人员。姓名片段去除前后空格后须为 2～100 个字符，`limit` 默认 20；`%`、`_` 和反斜杠按字面匹配。结果包含人员及其可见任职，`has_more` 只按可见人员计算。若姓名片段命中超过 500 名候选人员，接口返回 `400 refine_search`，需缩小关键词；不返回部分结果。搜索、逐任职策略决策和请求审计同事务完成，审计失败不返回结果。此接口不搜索职位、部门或工号片段。

普通员工可用 `GET /api/v1/directory/organizations` 获取可见组织索引。只有至少一个当前任职通过 `directory_view` 判定的组织才作为可选择节点；为拼接树结构会补上仍有效的祖先节点，后者用 `has_visible_members=false` 标记。停用祖先不返回，其下可见节点会成为根节点。接口不返回组织人数；逐任职策略判定和请求审计同事务完成。

普通员工可用 `GET /api/v1/directory/organizations/{organization_id}/members?limit={1..20}&after={membership_id}` 分页查看某组织的可见成员。`limit` 默认 20；`after` 省略时从第一页开始，后续传入上一页的 `next_after`。响应包含 `people`、`has_more` 和 `next_after`；末页的 `next_after` 为 `null`。人员只带该组织下可见的当前任职与部门；分页锚点每次都会重新校验。组织、锚点不存在或不可见统一返回 404；无效 UUID 或分页参数返回 400。策略决策与请求审计在同一事务内完成。大组织且大量成员不可见时，接口可能扫描较多候选；上线前需按目标规模验证查询耗时及容量。

## 本地运行

需要 Go 1.27 和 PostgreSQL 16。数据库应启用 `btree_gist` 扩展；首次迁移需要具备创建扩展和表的权限。可用现有 PostgreSQL 实例，也可使用隔离的本地测试容器。

本地没有 `psql` 时，可用 Docker 启动仅监听本机的开发数据库，并在容器内执行迁移：

```sh
docker run --rm -d --name enterprise-im-dev-db -e POSTGRES_PASSWORD=local_only_password -e POSTGRES_DB=enterprise_im -p 127.0.0.1:55432:5432 postgres:16-alpine
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000001_group_foundation.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000002_admin_access.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000003_policy_store.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000004_external_identities.up.sql
```

迁移脚本包含显式事务；执行中途出错时，已创建的表会回滚。

然后启动服务：

```sh
export IM_DATABASE_URL='postgres://postgres:local_only_password@127.0.0.1:55432/enterprise_im?sslmode=disable'
go run ./cmd/im-api
```

服务默认监听 `:8080`，可用 `IM_HTTP_ADDR` 修改。探针为 `GET /health/live` 与 `GET /health/ready`；数据库不可用时 ready 返回 503。不要在共享环境使用示例密码，服务不会在日志中打印 DSN。

默认 `IM_OIDC_ENABLED` 为空，服务只暴露健康检查。启用受保护管理 API 前，先核对 IdP 能签发上述 JWT 访问令牌，迁移数据库，并导入与本地用户一一核对的 `external_identities` 绑定及管理员授权。然后配置：

```sh
export IM_OIDC_ENABLED=true
export IM_OIDC_ISSUER='https://sso.example.com/group'
export IM_OIDC_AUDIENCE='enterprise-im-api'
export IM_OIDC_JWKS_URL='https://sso.example.com/group/keys'
export IM_OIDC_ALLOWED_CLIENT_IDS='enterprise-im-web,enterprise-im-desktop'
```

这些地址和客户端 ID 必须替换为身份提供方实际配置；JWKS 地址必须经 HTTPS 直接访问，重定向会被拒绝，密钥须声明 `use=sig`，访问令牌须携带 `kid`。启用时配置不完整或初次获取验签密钥失败，服务启动失败。身份绑定不自动按姓名或邮箱创建。当前仅验证外部签发的访问令牌，不提供授权码回调或客户端登录页面；非标准或不透明令牌需另建适配器。

## 测试

```sh
go test ./...
IM_TEST_DATABASE_URL='postgres://postgres:local_only_password@127.0.0.1:55432/enterprise_im?sslmode=disable' go test ./... -count=1
go vet ./...
```

集成测试为每个用例创建独立 schema 并清理；未提供 `IM_TEST_DATABASE_URL` 时跳过 PostgreSQL 集成测试。测试开始前可先在临时库创建 `btree_gist` 扩展，避免并行用例同时创建它。回滚时按 `000004`、`000003`、`000002`、`000001` 的逆序执行 Down 脚本，只对可丢弃的开发或测试数据库执行回滚。
