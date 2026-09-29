# Enterprise IM

集团型多组织企业即时通信项目。产品基线见 [v2.2 需求文档](enterprise_im_group_v2_2.docx)，实施路径见 [开发计划](docs/企业IM-开发计划与实施路径-v0.1.md)。

## 当前开发增量

本分支实现 P1 集团模型、管理授权、身份认证、通讯录与通信策略基础，以及 P2-01 单聊会话、P2-02 文本消息可靠写入、P2-03 Outbox 发布 Worker、P2-04 单聊文本补拉、P2-05 WebSocket 身份握手、P2-06 在线补拉通知、P2-07 本人身份上下文、P2-08 基础 Web 单聊和 P2-09 当前任职会话列表。配置有效的身份提供方后，可显式启用受保护 API 与 Web 页面。**尚无消息正文直推、设备送达确认、群聊补拉或完整断线恢复验收；客户身份提供方尚未联调。**

受保护管理 API 合约：

| 方法与路径 | 用途 | 成功响应 |
| --- | --- | --- |
| `GET /api/v1/admin/users?q={关键词}&limit={1..50}` | 在管理员授权组织内按姓名或集团工号检索在职人员 | 200，`people` 与 `has_more` |
| `GET /api/v1/admin/users/{id}` | 查询授权范围内的人员任职 | 200，snake_case JSON |
| `POST /api/v1/admin/memberships/{id}:end` | 结束一个组织任职 | 204，无响应体 |

访问令牌须为签给本 API 的 RFC 9068 JWT，具有 RS256 签名、`at+jwt` 类型、正确发行方及受众，并包含允许的 `client_id`。仅通过 `(issuer, sub)` 查找受控导入的本地身份绑定；选定任职由数据库二次校验。ID Token、邮件地址和请求中的租户 ID 不用于映射。401 表示未认证，400 表示 ID 或请求格式错误，403 表示身份失效，404 隐藏无权限资源，409 表示状态冲突，503 表示认证、审计或数据库不可用。

浏览器取得访问令牌后，先调用 `GET /api/v1/me` 获取本人身份与当前可选任职。该接口只需 `Authorization: Bearer <access-token>`，不接受 `X-Acting-Membership-ID`、查询参数或请求正文；响应包含 `tenant_id`、`user_id`、`display_name`、`global_employee_no` 和 `memberships` 数组。每项有 `id`、组织与法人 ID/名称、`title`、`is_primary`。有效账号暂无任职时数组为空。仅返回当前有效的任职、组织和法人，主任职排在前面；响应禁止缓存。客户端随后选择一个任职 ID 作为其他业务请求的 `X-Acting-Membership-ID`，服务端仍逐次核验，不以此列表授予权限。当前审计表要求记录选定任职，故选择前的 `/me` 查询暂不写任职审计，后续登录审计需单独设计。

三个管理接口均要求 `Authorization: Bearer <access-token>` 和 `X-Acting-Membership-ID: <uuid>`。检索关键词不能为空，最多 100 个字符；`limit` 默认 20，最大 50。结果只包含 ID、集团工号和姓名；`has_more=true` 时需缩小关键词继续查找，不提供翻页。检索会写入管理审计，且只返回当前有效任职落在管理授权范围内的在职人员。它不代表普通员工的通讯录可见性。

普通员工可用 `GET /api/v1/directory/memberships/{id}` 查看一个目标任职，使用同样的身份头。服务端按当前 `directory_view` 策略实时判断，并只返回该任职的用户 ID、姓名、集团工号、组织、职位、主任职和有效部门。不可见或不存在返回 404；本人选定任职失效返回 403；审计或数据库故障返回 503。该接口不接受查询参数中的权限或目标用户声明，允许查看不代表允许发起单聊。

普通员工还可用 `GET /api/v1/directory/users?employee_no={集团工号}` 精确查找一个人员，身份头与目录详情相同。工号去除前后空格后必须非空，最多 128 个字符；`employee_no` 模式不做模糊匹配。响应只列出此人在当前选定任职下可见的组织任职；不存在与全部不可见都返回 404。账号冻结或令牌无效由认证层返回 401；已认证但选定任职失效返回 403。每个候选任职写策略决策审计，整次查找另写一条不含工号原文的审计记录，审计失败不返回结果。

普通员工可用 `GET /api/v1/directory/users?q={姓名片段}&limit={1..20}` 搜索可见人员。姓名片段去除前后空格后须为 2～100 个字符，`limit` 默认 20；`%`、`_` 和反斜杠按字面匹配。结果包含人员及其可见任职，`has_more` 只按可见人员计算。若姓名片段命中超过 500 名候选人员，接口返回 `400 refine_search`，需缩小关键词；不返回部分结果。搜索、逐任职策略决策和请求审计同事务完成，审计失败不返回结果。此接口不搜索职位、部门或工号片段。

普通员工可用 `GET /api/v1/directory/organizations` 获取可见组织索引。只有至少一个当前任职通过 `directory_view` 判定的组织才作为可选择节点；为拼接树结构会补上仍有效的祖先节点，后者用 `has_visible_members=false` 标记。停用祖先不返回，其下可见节点会成为根节点。接口不返回组织人数；逐任职策略判定和请求审计同事务完成。

普通员工可用 `GET /api/v1/directory/organizations/{organization_id}/members?limit={1..20}&after={membership_id}` 分页查看某组织的可见成员。`limit` 默认 20；`after` 省略时从第一页开始，后续传入上一页的 `next_after`。响应包含 `people`、`has_more` 和 `next_after`；末页的 `next_after` 为 `null`。人员只带该组织下可见的当前任职与部门；分页锚点每次都会重新校验。组织、锚点不存在或不可见统一返回 404；无效 UUID 或分页参数返回 400。策略决策与请求审计在同一事务内完成。大组织且大量成员不可见时，接口可能扫描较多候选；上线前需按目标规模验证查询耗时及容量。

普通员工可用 `POST /api/v1/conversations` 发起或复用单聊。请求须有相同的 Bearer 令牌和 `X-Acting-Membership-ID`，`Content-Type: application/json`，请求体为 `{ "target_membership_id": "<uuid>" }`。服务端只按当前 `start_chat` 策略授权，集团内同一对用户只保留一个单聊会话；换组织任职后经授权仍复用原会话。成功返回 200，包含 `id`、`type`、`last_seq`、`policy_version`、`cross_legal` 和 `decision_reason`。目标任职不存在、跨租户、自聊或当前不允许通信都返回 404；本人任职失效返回 403。已有会话不会绕过新发布的拒绝规则。会话创建、策略决策和请求审计同事务提交。

普通员工可用 `GET /api/v1/conversations?limit=20&cursor=<opaque>` 浏览当前任职对应的已有单聊。`limit` 默认 20、最大 50，`cursor` 由上一页的 `next_cursor` 原样传回；响应包含 `conversations` 和 `has_more`。列表按更新时间倒序，跨租户、非参与者或其他任职的会话不返回。对方个人资料受当前 `directory_view` 策略控制：不可见时仍可显示通用会话项，但不会返回对方姓名或组织。列表不含消息正文、未读数和发送授权；打开会话后仍由补拉接口逐条复核正文，发送仍由原接口重新判定。

普通员工可用 `POST /api/v1/conversations/{id}/messages` 写入单聊文本消息。沿用上述身份头，请求体仅含 `{ "client_msg_id": "<uuidv7>", "text": "消息正文" }`。UUIDv7 的时间须在服务端当前时间之前 7 天至之后 5 分钟内；正文须为有效 UTF-8、非空白，最多 16 KiB。成功返回 200，包含 `message_id`、`conversation_id`、`seq` 与 `server_time`。同一租户、会话、发送用户和客户端消息 ID 重试，若正文相同则返回原 ACK，不重复占用序号、限流额度或 Outbox；正文不同返回 409。服务端在一个事务中复核双方任职和当前 `send_message` 策略，分配连续序号，保存消息、幂等记录、待发布 Outbox 和审计，提交后才返回 ACK。本人身份失效返回 403；会话、目标或通信边界不可用返回 404；会话任职上下文变化返回 409；客户端消息 ID 过期返回 410；超出每秒发送上限返回 429；数据库或审计故障返回 503。默认每用户每秒 10 条，可用 `IM_MESSAGE_RATE_PER_SECOND` 配置 1～10000 的正整数。当前 ACK 只表示服务端持久化接收，不表示收件人已送达或已读；Outbox 由独立 Worker 异步发布。

Outbox Worker 从 PostgreSQL 领取到期事件并写入 Redis Stream，成功后标记 `published`；失败会按最长 5 分钟的指数退避重试。Redis 事件只含 `event_id`、`tenant_id`、`conversation_id`、`message_id`、`seq` 和 `event_type`，不含正文。Redis 发布与数据库标记之间可能发生重复；每个 API 实例独立读取新事件，按稳定的 `event_id` 在本机有界窗口内去重，并让客户端按 PostgreSQL `seq` 补拉、处理乱序与缺口。`published` 只表示 Redis 接受了事件，**不表示消息已送达设备**。生产者暂不裁剪 Stream；上线前必须监控积压容量并制定可检测缺口的保留策略。PostgreSQL 仍为消息事实来源。

单聊文本可通过 `GET /api/v1/conversations/{id}/messages?after_seq=0&limit=100` 按会话序号升序补拉。`after_seq` 必填，`limit` 可选（默认 100，最大 500）。响应中的 `next_after_seq` 用于下一页；`has_more` 表示是否还有后续序号。不可见消息只返回 `seq` 和 `redacted:true`，客户端仍须推进游标。补拉依据消息发送时保存的双方任职及组织快照；迁移前无法证明接收任职的旧消息只能返回不可见占位，不能用当前会话任职自动回填。冻结账户或失效任职不能补拉，现行 `send_message` 强制拒绝会撤销匹配内容的读取，普通通信隔离不追溯删除已授权历史。该接口不提供实时推送或设备送达确认。

显式配置 `IM_REALTIME_REDIS_URL` 后，浏览器可用相同身份头向 `POST /api/v1/realtime/tickets` 申请 30 秒一次性票据，再以子协议 `enterprise-im.v1`、`ticket.<票据>` 连接同源 `GET /api/v1/realtime` WebSocket。票据不放在 URL；服务器只回显 `enterprise-im.v1`。连接成功首先收到 `{"type":"ready","resync_required":true}`，客户端应立即用上次连续确认的 `seq` 调用上述 HTTP 补拉。新消息发布到 Redis Stream 后，本机在线的单聊双方会收到 `{"type":"sync_required"}`，再次通过 HTTP 补拉；该信号不包含正文、会话 ID 或序号，也不是送达确认。票据不可重复使用，任职或账号失效时连接会关闭。Redis 或通知读者不可用时票据入口和就绪探针返回 503，现有连接关闭并等待重连补拉。

## 本地运行

需要 Go 1.27 和 PostgreSQL 16。数据库应启用 `btree_gist` 扩展；首次迁移需要具备创建扩展和表的权限。可用现有 PostgreSQL 实例，也可使用隔离的本地测试容器。

本地没有 `psql` 时，可用 Docker 启动仅监听本机的开发数据库，并在容器内执行迁移：

```sh
docker run --rm -d --name enterprise-im-dev-db -e POSTGRES_PASSWORD=local_only_password -e POSTGRES_DB=enterprise_im -p 127.0.0.1:55432:5432 postgres:16-alpine
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000001_group_foundation.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000002_admin_access.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000003_policy_store.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000004_external_identities.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000005_direct_conversations.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000006_message_write.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000007_message_recipient.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000008_conversation_inbox.up.sql
```

迁移脚本包含显式事务；执行中途出错时，已创建的表会回滚。

然后启动服务：

```sh
export IM_DATABASE_URL='postgres://postgres:local_only_password@127.0.0.1:55432/enterprise_im?sslmode=disable'
go run ./cmd/im-api
```

服务默认监听 `:8080`，可用 `IM_HTTP_ADDR` 修改。探针为 `GET /health/live` 与 `GET /health/ready`；数据库不可用时 ready 返回 503。不要在共享环境使用示例密码，服务不会在日志中打印 DSN。

要运行独立的 Outbox Worker，先准备 Redis，再在另一个终端使用同一 PostgreSQL 数据库启动它：

```sh
docker run --rm -d --name enterprise-im-dev-redis -p 127.0.0.1:16379:6379 redis:7-alpine
export IM_DATABASE_URL='postgres://postgres:local_only_password@127.0.0.1:55432/enterprise_im?sslmode=disable'
export IM_OUTBOX_REDIS_URL='redis://127.0.0.1:16379/0'
go run ./cmd/im-outbox-worker
```

`IM_OUTBOX_REDIS_URL` 必须使用 `redis://` 或 `rediss://`；可用 `IM_OUTBOX_STREAM` 覆盖默认 Stream `enterprise-im:message-created:v1`。Worker 启动时检查数据库和 Redis，运行中按 250 毫秒空闲间隔轮询，收到终止信号后停止领取新事件。Redis Stream 不能代替客户端补拉；Redis 数据丢失时，已 ACK 的消息仍保存在 PostgreSQL。

默认 `IM_OIDC_ENABLED` 为空，服务只暴露健康检查。启用受保护管理 API 前，先核对 IdP 能签发上述 JWT 访问令牌，迁移数据库，并导入与本地用户一一核对的 `external_identities` 绑定及管理员授权。然后配置：

```sh
export IM_OIDC_ENABLED=true
export IM_OIDC_ISSUER='https://sso.example.com/group'
export IM_OIDC_AUDIENCE='enterprise-im-api'
export IM_OIDC_JWKS_URL='https://sso.example.com/group/keys'
export IM_OIDC_ALLOWED_CLIENT_IDS='enterprise-im-web,enterprise-im-desktop'
export IM_MESSAGE_RATE_PER_SECOND=10
export IM_REALTIME_REDIS_URL='redis://127.0.0.1:16379/0'
export IM_REALTIME_STREAM='enterprise-im:message-created:v1'
```

`IM_REALTIME_REDIS_URL` 可省略；省略时不启用实时握手与通知路由。启用时必须同时启用 OIDC，且启动时 Redis 必须可连接。`IM_REALTIME_STREAM` 默认与 Worker 的 `IM_OUTBOX_STREAM` 相同；自定义时两者必须设为同一名称，并先启动 Worker。Worker 在健康处理循环刷新 10 秒存活标记；API 要求同一 Redis 和 Stream 上有该标记。Worker 停止、配置错配、读流或数据库核验故障会让通知入口失效，就绪探针返回 503，现有连接关闭。每个 API 实例独立从 Stream 尾部读取新事件并只通知本机连接，客户端重连收到 `ready` 后从 PostgreSQL 补拉。浏览器 WebSocket 默认只允许同源；生产环境应通过 HTTPS/WSS 提供入口。单进程最多保持 5000 条连接、同一用户最多 5 条，连接每 5 秒复核任职并每 30 秒发送 Ping。Stream 目前不自动裁剪，投入生产前仍需容量与保留策略验证。

基础 Web 页面默认关闭。先在身份源注册支持授权码和 PKCE S256 的**公共客户端**，将其客户端 ID 同时加入 `IM_OIDC_ALLOWED_CLIENT_IDS`，并把精确回调 URL 注册为公开 HTTPS 地址，例如 `https://im.example.com/web/`。身份源须为此客户端签发满足上文约束的 API 访问令牌。经 HTTPS 反向代理提供同源页面和 API 后，额外配置：

```sh
export IM_WEB_ENABLED=true
export IM_WEB_AUTHORIZATION_URL='https://sso.example.com/group/authorize'
export IM_WEB_TOKEN_URL='https://sso.example.com/group/token'
export IM_WEB_CLIENT_ID='enterprise-im-web'
export IM_WEB_REDIRECT_URL='https://im.example.com/web/'
export IM_WEB_SCOPE='openid profile'
```

然后访问 `https://im.example.com/web/`。页面完成授权码登录和当前有效任职选择，可浏览当前任职下的已有单聊、按姓名查找可见同事、发起单聊、发送文本并补拉消息。令牌只保存在页面内存，刷新或退出后需重新登录；重新登录并选择任职后，会话列表会从服务端重新加载。可配置 Redis 实时通知，未配置或暂不可用时页面以定时补拉继续工作。当前 ACK 文案仅表示服务器持久化接收。`IM_WEB_SCOPE` 默认 `openid profile`；令牌兑换只访问服务器配置的 HTTPS 地址，不使用浏览器请求中的目标地址。部署前仍需用客户 IdP、HTTPS 入口和真实组织数据做联调；本地模拟测试不等于已完成联调。

这些地址和客户端 ID 必须替换为身份提供方实际配置；JWKS 地址必须经 HTTPS 直接访问，重定向会被拒绝，密钥须声明 `use=sig`，访问令牌须携带 `kid`。启用时配置不完整或初次获取验签密钥失败，服务启动失败。身份绑定不自动按姓名或邮箱创建。非标准或不透明令牌需另建适配器。

## 测试

```sh
go test ./...
IM_TEST_DATABASE_URL='postgres://postgres:local_only_password@127.0.0.1:55432/enterprise_im?sslmode=disable' go test ./... -count=1
IM_TEST_DATABASE_URL='postgres://postgres:local_only_password@127.0.0.1:55432/enterprise_im?sslmode=disable' IM_TEST_REDIS_URL='redis://127.0.0.1:16379/0' go test ./... -count=1
go vet ./...
```

集成测试为每个用例创建独立 schema 并清理；未提供 `IM_TEST_DATABASE_URL` 或 `IM_TEST_REDIS_URL` 时分别跳过 PostgreSQL 或 Redis 集成测试。测试开始前可先在临时库创建 `btree_gist` 扩展，避免并行用例同时创建它。回滚时按 `000006`、`000005`、`000004`、`000003`、`000002`、`000001` 的逆序执行 Down 脚本，只对可丢弃的开发或测试数据库执行回滚。
