# Enterprise IM

集团型多组织企业即时通信项目。产品基线见 [v2.2 需求文档](enterprise_im_group_v2_2.docx)，实施路径见 [开发计划](docs/企业IM-开发计划与实施路径-v0.1.md)。

## 当前开发增量

本分支实现 P1 集团模型、管理授权、身份认证、通讯录与通信策略基础，P2 单聊与浏览器基础能力，P3 群成员管理、群文本消息写入、区间补拉与群列表 API，并实现 P4 消息正文保留期、租户配置、会话级法务保全及默认关闭的正文／摘要清理。配置有效的身份提供方后，可显式启用受保护 API 与 Web 页面。**尚无消息正文直推、设备送达确认及客户环境的完整断线恢复验收；客户身份提供方尚未联调。**

受保护管理 API 合约：

| 方法与路径 | 用途 | 成功响应 |
| --- | --- | --- |
| `GET /api/v1/admin/users?q={关键词}&limit={1..50}` | 在管理员授权组织内按姓名或集团工号检索在职人员 | 200，`people` 与 `has_more` |
| `GET /api/v1/admin/users/{id}` | 查询授权范围内的人员任职 | 200，snake_case JSON |
| `POST /api/v1/admin/memberships/{id}:end` | 结束一个组织任职 | 204，无响应体 |
| `GET /api/v1/admin/audit-events` | 集团管理员按执行人 ID、动作、允许／拒绝结果分页查询本租户审计 | 200，`events` 与 `next_cursor` |
| `GET /api/v1/admin/retention-policy` | 集团管理员查询本租户消息正文保留期与审批记录 | 200，期限、版本与审批元数据 |
| `GET /api/v1/admin/retention-policy/history?limit={1..100}&cursor={游标}` | 集团管理员分页查询本租户已提交的保留期审批历史 | 200，`history` 与 `next_cursor` |
| `PUT /api/v1/admin/retention-policy` | 集团管理员按版本更新本租户消息正文保留期 | 200，更新后的配置 |
| `GET /api/v1/admin/conversations/{id}/legal-holds?limit={1..500}&cursor={游标}` | 集团管理员分页查询本租户会话保全 | 200，`holds` 与 `next_cursor` |
| `POST /api/v1/admin/conversations/{id}/legal-holds` | 集团管理员登记案件保全 | 新建 201，完全相同重试 200 |
| `POST /api/v1/admin/conversations/{id}/legal-holds/{hold_id}/release` | 集团管理员凭审批引用解除一项保全 | 200 |
| `GET /api/v1/admin/conversations/{id}/retention-batches?kind={body\|digest}&limit={1..500}&cursor={游标}` | 集团管理员查询本租户会话的已提交清理批次 | 200，`batches` 与 `next_cursor` |

保留期默认 365 个 24 小时天，可设 1～3650 天。PUT 请求体为 `{ "message_body_days": 730, "expected_version": 0, "approval_reference": "CAB-2026-01" }`，审批引用为已取得的外部审批单号，服务端只记录该引用、执行人和时间，不核验外部审批结果。接口要求与管理 API 相同的可信身份头；组织管理员不能读取或修改租户级期限。版本不一致返回 409；租户已有消息时延长期限也返回 409，避免重新显示曾被遮蔽的旧正文，需在首条消息前设定更长期限。每次成功修改都会在 `tenant_retention_policy_history` 留下不可修改的版本记录；当前配置、版本历史与审计同事务提交，审计失败时全部回滚。配置生效后，单聊与群聊补拉都在读取时使用当前租户期限；自动清理由独立进程显式启用，默认关闭。

管理审计查询支持可选 `actor_user_id`（完整 UUID，按用户 ID 精确匹配，大小写等价；省略查询全部，显式空值／重复／非法值返回 400）、`action`（1～64 个小写字母、数字或下划线，以字母开头）、`outcome`（`allow` 或 `deny`）、`limit`（默认 20，最多 100）和非空 `cursor`。例如 `GET /api/v1/admin/audit-events?actor_user_id=abcdefab-cdef-4abc-8abc-abcdefabcdef&action=retention_policy_update&outcome=deny&limit=20`。只查询当前租户 `audit_events`；按发生时间和数值 ID 倒序，返回 ID 字符串、执行人／任职、动作、资源类型／可空 ID、结果、原因及时间。游标绑定租户、执行人、动作和结果，改筛选须从首页查询；旧版不含执行人筛选的游标仍可续页。其他租户或不存在的执行人 ID 返回空记录，不查询人员存在性。每页独立复核有效集团权限，查询审计提交成功后才返回；未提供跨页共享快照，新事件需刷新查看。无筛选时可见此前查询审计，本页不含自身尚未提交的审计事件。Web 管理员侧栏可打开“管理审计查询”，填写执行人完整用户 ID、动作代码并选择允许／拒绝结果；每页 20 条，支持刷新首页及加载更多。改变筛选、关闭或切换任职会清空旧记录；权限失效禁用入口。未知结果的保留期／保全写入须先核对后再打开查询。详见 [P4-14 验收记录](docs/开发增量-P4-14-验收记录.md) 和 [P4-13 验收记录](docs/开发增量-P4-13-验收记录.md)。接口不包含 `policy_decision_events` 的规则命中细节、审计导出、审计过期清理或防篡改存储。详见 [P4-12 验收记录](docs/开发增量-P4-12-验收记录.md)。

审批历史按版本从新到旧分页，默认每页 20 条、最多 100 条；游标仅适用于原租户，版本 0 默认配置不生成历史记录。每次查询重新检查有效集团管理员权限并登记审计，审计失败不返回历史。分页不共享跨请求快照，新增版本请刷新首页。

法务保全应在正文清理前登记。登记请求体为 `{ "request_id": "<uuid>", "case_reference": "CASE-2026-01" }`；解除请求体为 `{ "request_id": "<uuid>", "approval_reference": "CAB-2026-02" }`。`request_id` 在租户内跨登记和解除唯一，完全相同请求可重试；重复用于不同动作、会话、案件或执行人返回 409。同一会话可有多项有效保全，解除一项不影响其他案件。GET 默认每页 100 项、最多 500 项，使用返回的游标继续读取；游标仅适用于原租户和会话。只有当前有效集团管理员可操作，案件与审批引用仅记录，不核验外部审批系统。状态、不可修改事件和审计同事务提交。保全暂停独立 Worker 的正文及摘要清理；当前读取仍按保留期遮蔽到期正文。备份到期和外部审批核验尚未实现。

清理批次查询要求同样的可信身份头，只允许当前有效集团管理员；每页重新验证身份、授权及本租户会话归属。`kind` 必填且仅允许 `body` 或 `digest`；`limit` 默认 100、最大 500。按 `(processed_at, id)` 倒序返回，游标绑定租户、会话、类型，末页 `next_cursor` 为空字符串；空列表为 `[]`。非法参数或游标为 400，身份失效为 403，非集团管理员、跨租户或不存在会话统一为 404，数据库或审计失败为 503；响应禁止缓存。每次成功查询的审计记录区分 `listed_body` 与 `listed_digest`，审计提交成功后才返回证据。

批次公共字段为 `id`、`conversation_id`、`kind`、`processed_at`、`processed_count`、`first_seq`、`last_seq`；正文批次另含 `retention_days`、`cutoff_at`，摘要批次另含 `min_expires_at`、`max_expires_at`。不返回正文、摘要、发送者或客户端重试键。序号边界可能有间隔，不能用 `last_seq-first_seq+1` 代替实际数量。分页不提供跨请求共享快照；新提交批次应刷新首页核查。此入口查询在线库已提交批次，不能证明备份、WAL 或外部副本已擦除。

Web 会话标题区提供“清理记录”只读弹窗。选定任职通过集团管理员检查后显示入口；检查遇到网络／5xx 故障时可点击“重试清理记录权限检查”。弹窗查询当前单聊或群聊，正文／摘要切换均从首页开始，每页 20 个批次，支持加载更多与刷新。时间按浏览器本机时区显示，数量取实际处理数量。切换任职、会话、关闭或退出会清空当前记录，延迟响应不会恢复旧数据。403／404 清空并禁用入口，401 按现有登录流程退出；503 或不符合合约的响应清空后可刷新重试。记录只保存在当前页面内存，未知字段不保留。此入口仅覆盖用户已选中的会话；按会话 ID 查询本租户其他会话仍可使用上述管理员 API。

Web 会话另提供“法务保全”弹窗，沿用当前任职的集团管理员检查及重试。每页 20 项，按登记时间与 ID 升序分页，可查看案件引用、登记人员／任职 ID、时间及已解除项的审批引用、解除人员／任职 ID 和时间。状态按每项完整解除记录判断，不根据第一页推断整个会话无保全。切任职／会话／关闭／退出清空，延迟回包不能恢复旧记录；403／404 清空禁用，401 退出，临时故障可刷新重试。集团管理员可填写案件引用登记保全；对尚有效的一项点击“解除此项保全”，填写已取得的解除审批引用并勾选目标确认。案件／审批引用为 1～128 个 Unicode 字符，去除首尾空白，拒绝控制字符与无效代理字符。成功后由服务器记录刷新列表，解除一项不影响其他有效保全。每次操作生成独立 request_id，结果待确认时冻结原身份、会话、目标与引用并按原编号重试；15 秒无响应、网络／503 或成功 DTO 不匹配都不宣称失败未写入。待确认期间可关闭弹窗，但切任职／会话／主动退出会被阻止，重开显示原会话名及 ID。明确放弃重试需确认，不代表撤销服务器操作。400／409 提示检查／核对，403／404 清空禁用；强制认证失效清空并忽略旧 ACK。操作信息仅页面内存保留，刷新或强制退出后需重新核查服务器记录；beforeunload 发出离页提示，其最终行为取决于客户浏览器。保全暂停正文及摘要清理，读取仍遮蔽到期正文；引用只记录，不核验外部审批。

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

普通员工可用 `POST /api/v1/conversations/{id}/messages` 写入单聊文本消息。沿用上述身份头，请求体仅含 `{ "client_msg_id": "<uuidv7>", "text": "消息正文" }`。UUIDv7 的时间须在服务端当前时间之前 7 天至之后 5 分钟内；正文须为有效 UTF-8、非空白，最多 16 KiB。成功返回 200，包含 `message_id`、`conversation_id`、`seq`、`server_time` 与 `duplicate`；首次写入为 `false`，相同内容重试为 `true`。同一租户、会话、发送用户和客户端消息 ID 重试，若正文相同则返回原 ACK，不重复占用序号、限流额度或 Outbox；正文不同返回 409。服务端在一个事务中复核双方任职和当前 `send_message` 策略，分配连续序号，保存消息、幂等记录、待发布 Outbox 和审计，提交后才返回 ACK。本人身份失效返回 403；会话、目标或通信边界不可用返回 404；会话任职上下文变化返回 409；客户端消息 ID 过期返回 410；超出每秒发送上限返回 429；数据库或审计故障返回 503。默认每用户每秒 10 条，可用 `IM_MESSAGE_RATE_PER_SECOND` 配置 1～10000 的正整数。当前 ACK 只表示服务端持久化接收，不表示收件人已送达或已读；Outbox 由独立 Worker 异步发布。

Outbox Worker 从 PostgreSQL 领取到期事件并写入 Redis Stream，成功后标记 `published`；失败会按最长 5 分钟的指数退避重试。Redis 事件只含 `event_id`、`tenant_id`、`conversation_id`、`message_id`、`seq` 和 `event_type`，不含正文。Redis 发布与数据库标记之间可能发生重复；每个 API 实例独立读取新事件，按稳定的 `event_id` 在本机有界窗口内去重，并让客户端按 PostgreSQL `seq` 补拉、处理乱序与缺口。`published` 只表示 Redis 接受了事件，**不表示消息已送达设备**。生产者暂不裁剪 Stream；上线前必须监控积压容量并制定可检测缺口的保留策略。PostgreSQL 仍为消息事实来源。

单聊文本可通过 `GET /api/v1/conversations/{id}/messages?after_seq=0&limit=100` 按会话序号升序补拉。`after_seq` 必填，`limit` 可选（默认 100，最大 500）。响应中的 `next_after_seq` 用于下一页；`has_more` 表示是否还有后续序号。不可见或已满当前租户正文保留期的消息只返回 `seq` 和 `redacted:true`，不返回消息 ID、发送者、正文或时间；客户端仍须推进游标。过期消息在读取时遮蔽；启用独立正文清理 Worker 后，在线库的到期正文会被置空，已清理消息始终返回遮蔽占位。Web 客户端先检查一页内的序号连续性及页游标，发现缺口则保留原游标并重试，避免把尚未显示的消息跳过去。补拉依据消息发送时保存的双方任职及组织快照；迁移前无法证明接收任职的旧消息只能返回不可见占位，不能用当前会话任职自动回填。冻结账户或失效任职不能补拉，现行 `send_message` 强制拒绝会撤销匹配内容的读取，普通通信隔离不追溯删除已授权历史。该接口不提供实时推送或设备送达确认。

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
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000009_group_membership.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000010_group_create_request.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000011_group_invitation.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000012_group_owner_transfer.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000013_tenant_retention.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000014_conversation_legal_hold.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000015_message_body_clear.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000016_message_digest_retirement.up.sql
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

在线消息正文清理由独立 `im-retention-worker` 执行，默认关闭。部署顺序为：先执行 `000015`，部署兼容可空正文的 API，再核对租户保留期限与会话法务保全，最后显式启用清理：

```sh
export IM_DATABASE_URL='postgres://postgres:local_only_password@127.0.0.1:55432/enterprise_im?sslmode=disable'
export IM_BODY_CLEANER_ENABLED=true
export IM_BODY_CLEANER_BATCH_SIZE=100
go run ./cmd/im-retention-worker
```

`IM_BODY_CLEANER_ENABLED` 只接受空、`false` 或 `true`；正文和摘要两个开关都关闭时进程直接退出且不连接数据库。批量默认 100，配置范围 1～1000，每租户每轮最多处理一个会话批次；成功或空轮询间隔为 1 秒，错误退避为 1～30 秒。每次租户枚举和批次处理设置 5 秒上下文，单租户错误仍继续处理后续租户；数据库恢复后重新轮询。Worker 只需要 PostgreSQL，Redis 故障不会改变保留与保全判定。

清理事务在任何查询前显式设为 READ COMMITTED，再锁租户期限和会话；数据库、角色或连接配置的默认隔离级别不会改变锁后检查。取得会话锁后重新检查有效保全与数据库时钟。任一案件仍有效时，该会话不清理；只解除部分案件不会解除阻断。清理与登记保全通过会话锁串行化，**保全应在清理前登记**，后续保全不能恢复已经清空的正文。保全只暂停清理，读取仍按期限遮蔽。正文与批次记录一起提交，失败或取消整批回滚；消息行、序号、Outbox 和成员区间保留；正文清理不改变摘要退役前的幂等 ACK。批次记录的首末序号为实际 min/max，不表示区间内每条消息都被清理。

暂停全部自动清理时，向所有清理进程发送 SIGTERM，并用 `IM_BODY_CLEANER_ENABLED=false` 和 `IM_DIGEST_CLEANER_ENABLED=false` 重启或停止对应服务；仅修改环境变量不会改变已经运行的进程。当前事务在提交或回滚后退出，已提交批次不撤销。可通过数据库只读查询观察批次：

```sql
SELECT tenant_id, conversation_id, id, retention_days, cutoff_at,
       cleared_at, first_seq, last_seq, cleared_count
FROM message_body_clear_batches
ORDER BY cleared_at DESC, id DESC LIMIT 100;
```

`000015` 的表变更与索引构建可能阻塞写入，部署前需按表规模评估维护窗口、锁超时及耗时；在线 Worker 不等于无锁迁移。有任何清理行或批次证据后，`000015` Down 拒绝回滚，不能通过回滚恢复正文。清空的是在线当前行的 `text_body`，摘要未退役前，两处 SHA-256 仍保留，低熵正文可能被猜测比对；摘要退役规则见下节。PostgreSQL MVCC 旧版本、WAL、归档、备份及存储介质残留需独立治理，本增量不证明安全擦除或生产保留合规。数据库异常日志仅输出固定类别，批次日志包含标识与计数，不记录正文或连接 URL。

### 消息摘要与永久到期幂等键

摘要清理使用同一进程的独立开关，正文开关不会隐式启用它。先执行 `000016`，部署所有支持可空摘要及永久到期判定的 API 实例和新 Worker，确认没有旧 API 实例后，才可启用：

```bash
export IM_DIGEST_CLEANER_ENABLED=true
export IM_DIGEST_CLEANER_BATCH_SIZE=100
# IM_DATABASE_URL 沿用已配置数据库；正文开关可保持 false。
go run ./cmd/im-retention-worker
```

`IM_DIGEST_CLEANER_ENABLED` 只接受空、`false` 或 `true`，默认关闭；摘要批量默认 100，范围 1～1000。每轮枚举一次租户，分别执行已启用的正文和摘要批次，各有独立 5 秒上下文。一类失败或超时仍处理同租户另一类及后续租户；父取消则退出。仅摘要启用时要求数据库 URL，但不执行正文清理。

只有正文已清理、幂等 `expires_at` 到期、正文清理时间不晚于实际数据库时间且无任一有效法务保全时，才同事务清空消息与幂等表两处 `content_digest`，写相同不可改的 `digest_retired_at` 及证据。幂等至少保留 30 天；正文默认保留 365 天时，摘要通常等待正文清理后才退役。仍保留幂等键、原始 ACK 元数据和消息行，不能把本增量当作完整元数据删除。

现有 UUIDv7 超过 7 天返回 `410 retry_window_expired`；即使服务时钟回退使旧 ID 通过窗口校验，退役键也返回同一 410，不比较正文、不返回重复 ACK、不再写消息或 Outbox。摘要退役前，窗口内同内容仍返回原 ACK，不同内容仍返回 409。两种发送事务显式 READ COMMITTED，查重以一条 SQL 的快照判定；先于清理提交读到旧状态的在途请求可完成原 ACK，清理提交后的新查重只能得到到期拒绝。

可通过只读查询观察摘要批次；最小／最大序号只描述实际集合边界：

```sql
SELECT tenant_id, conversation_id, retired_at, retired_count,
       first_seq, last_seq, min_expires_at, max_expires_at
FROM message_digest_retirement_batches
ORDER BY retired_at DESC, id DESC LIMIT 100;
```

有退役行或批次证据时 `000016` Down 拒绝。开始摘要清理后，不能回退到不认识 NULL 摘要／退役标记的旧 API；发生问题时关闭摘要开关并部署兼容修复版本。表变更、索引和触发器有迁移锁与运行成本，须评估维护窗口。两处当前摘要清空不证明 MVCC、WAL、归档、备份或外部副本已擦除；超级用户禁用触发器或 TRUNCATE 的防护仍依赖数据库权限治理。

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

启用 OIDC 后，`POST /api/v1/groups` 可由当前有效任职创建群，JSON 请求示例：`{"client_request_id":"00000000-0000-4000-8000-000000000851","name":"项目群","member_membership_ids":["00000000-0000-4000-8000-000000000852"]}`。`client_request_id` 由客户端生成；同一创建人用相同 ID 和相同内容重试会返回原群，改动内容则返回 409。一次可指定 1 至 20 位初始成员，服务会逐对检查建群策略；所有检查通过后才写入群及成员。Web 页面“新建群聊”使用当前所选任职作为创建人，可跨多次目录搜索选择成员；同一人只能选择一个任职，跨组织成员仍由服务端策略判定。建群结果未确认时页面锁定群名和成员，并用相同请求编号及内容重试；放弃后应先核对群列表，避免重复建群。

网页会把待确认建群请求的编号、群名、创建任职和成员快照保存在浏览器本地存储，不保存令牌。刷新后重新登录、选择原任职并打开“新建群聊”，即可恢复同编号重试或明确放弃。

群成员可用 `GET /api/v1/groups/{group_id}/membership` 查询本人的当前成员区间，取得 `interval_id`、`role`、`join_seq` 和 `group_status`；用 `POST /api/v1/groups/{group_id}/leave` 提交 `{"interval_id":"<当前区间 ID>"}` 主动退群。退群返回 `status=left` 与 `leave_seq`；相同区间的请求可以安全重试，即使本人之后重新入群，也不会退出新区间。群主须先转让群主身份，然后才能主动退群；直接退群返回 409 `owner_transfer_required`。

网页中，普通成员或管理员可在群聊页确认退群，策略暂停的群也可退出；群主须先转让群主。网页先读取本人当前成员区间，提交前按账号、当前任职、群和区间 ID 保存待确认请求，不保存令牌。结果未确认时可在“我的群聊”区域恢复并以原区间 ID 重试，即使群已从列表消失；放弃后应核对成员状态。切换任职时只显示该任职发起的待确认退群。未确认的消息或同群邀请须先处理。服务端确认退出后，当前群聊关闭并刷新群列表；重新入群后的新区间不受旧请求重试影响。

群主或群管理员可用 `POST /api/v1/groups/{group_id}/invitations` 提交 `{"client_request_id":"<请求 UUID>","target_membership_id":"<目标任职 UUID>"}` 邀请一人。服务对目标与每位现有成员双向检查 `invite_group` 策略，成功返回新区间 `interval_id`、`join_seq` 和 `policy_version`。同一请求重试返回原区间，即使目标已退群或重新入群；更改请求内容返回 409。`policy_blocked` 群暂停新邀请。

网页中，当前活跃群的群主或管理员可点“邀请成员”，从当前任职可见的通讯录中选择一人及其任职。是否有权邀请、目标能否加入仍由服务端判定。结果未确认时，网页按账号、任职、群和请求编号保存待确认邀请，不保存令牌；刷新后重新登录并在“我的群聊”区域打开待确认邀请，可用相同编号和目标重试或放弃，即使原群已不在群列表中。放弃后应先核对成员状态，避免重复邀请。网页目前不支持批量邀请。

群主或管理员可用 `GET /api/v1/groups/{group_id}/members?limit=20&cursor=<游标>` 分页读取当前活跃成员，默认每页 20、最多 50。每项只包含成员区间 `interval_id`、姓名、角色和加入时来源组织的当前名称；已退出或移除的旧区间不返回。`policy_blocked` 群仍可读取，以便处理成员；普通成员、已退群者和其他租户均不能查看。结果仅供选择移除或转让目标，具体操作仍由对应写接口再次授权。网页中群主和管理员可打开成员名册并继续加载；关闭、切换群或任职、退出登录时会清空内容，网页不会持久保存名册。群主可从名册移除管理员或普通成员，管理员只能移除普通成员；移除前显示姓名与组织供确认。群主也可从名册选择活跃成员接任群主。

群主或群管理员可用 `POST /api/v1/groups/{group_id}/removals` 提交 `{"interval_id":"<目标当前区间 UUID>"}` 移除成员。群主可移除管理员或普通成员；管理员只能移除普通成员，不能移除自己或群主。返回 `status=removed` 与 `leave_seq`，同一区间重试安全，目标重新入群后的新区间不受旧请求影响。`policy_blocked` 群允许移除冲突成员，但本接口不会自动恢复群状态；恢复须由群主或管理员调用下述策略复核接口。

网页确认移除前仅将租户、用户、当前任职、群和目标成员区间 ID 保存在浏览器本地；不持久保存名册姓名或组织。网络故障或响应不能确认结果时，侧栏保留原区间的待确认操作，重新登录后可重试或手动放弃。放弃前应核对成员现状，因为原请求可能已经成功。成功确认后重新读取名册；写请求仍由服务端重新校验操作者权限与目标当前区间。

当前群主可用 `POST /api/v1/groups/{group_id}/owner-transfers` 提交 `{"client_request_id":"<请求 UUID>","source_interval_id":"<本人当前区间 UUID>","target_interval_id":"<接任成员当前区间 UUID>"}` 转让所有权。接任者须为群内活跃成员，且其来源任职仍有效；原群主降为普通成员，之后可主动退群。首次成功返回 201，同一请求重试返回 200；即使群主后来再次变更，旧请求也不会重新执行。更改同一请求 ID 的内容返回 409。`policy_blocked` 群允许转让，但不会自动解除封锁。批量邀请尚未开放。

群主或群管理员可用 `POST /api/v1/groups/{group_id}/policy-rechecks`（无请求体和查询参数）显式复核暂停的群。服务端以当前发布的策略版本检查所有活跃成员的来源任职及每对成员的双向 `send_message` 决策；全部允许才返回 `200 {"status":"active","policy_version":N}` 并恢复发送与邀请。仍有冲突或成员任职失效时返回 409 `group_policy_blocked`，群继续暂停；普通成员返回 403，非成员与跨租户请求返回 404。已正常的群也可再次复核；成员移除和策略发布本身不会自动解除暂停。状态和审计同事务提交。当前 Web 页面在策略暂停群的群主或管理员视图显示“复核群策略”；复核后重新读取群列表，确认状态正常才开放新消息和邀请。409 保持暂停并提示处理冲突；请求结果不明时重新核对群列表，仍暂停可重试。切换任职或群聊后不展示旧请求结果。

网页在选择接任者后读取本人当前成员区间，并在确认前仅保存租户、用户、当前任职、群、请求编号及源/目标区间 ID；姓名、组织与访问令牌不会随转让请求持久保存。结果不确定时，侧栏保留原请求，可在刷新或重新登录后重试，即使原群主已降为普通成员。确认成功后刷新群列表，原群主可自行退群；服务端仍会重新校验首次转让的当前权限与接任者状态。

群历史补拉使用 `GET /api/v1/groups/{group_id}/messages?after_seq=0&limit=100`。当前或历史成员可在账号和选定任职有效时，读取本人曾参与区间内且未满当前租户正文保留期的消息。对于退群、移除与重新入群期间仍存的消息行，以及错误发送任职和正文过期的消息，接口只返回 `redacted=true` 占位，不包含正文和发送人。普通策略变化不追改保留期内的旧正文；当前 `hard_deny` 命中读者与群内任一成员时会遮蔽整页历史正文，账号冻结也会阻断读取。`policy_blocked` 群仍可按历史授权补拉。租户级正文保留期已可配置，启用独立清理进程后清空到期正文；P4 清理或历史导入必须保留连续序号的安全占位，或同步升级补拉契约，否则 Web 客户端会检测缺口并暂停该会话补拉。

`GET /api/v1/groups?limit=20&cursor=<游标>` 返回本人当前仍在群内的群，默认每页 20、最多 50，按群更新时间和 ID 倒序排列；`has_more` 和 `next_cursor` 用于继续读取。结果包含群名、`active` 或 `policy_blocked` 状态、本人角色、群来源任职 ID、最后消息序号和更新时间，不返回其他成员资料。本人可使用任一当前有效任职读取群列表；发送群消息时仍须选择该群的来源任职并通过实时策略校验。退群、被移除或已结束的群不出现在当前群列表，历史消息仍按区间授权规则通过已知群 ID 补拉。群列表只反映请求时的状态，客户端断线恢复时应重新从第一页核对。

群成员可用 `POST /api/v1/groups/{group_id}/messages` 发送文本，请求体与单聊发送相同。新消息要求所选任职等于当前群成员区间的来源任职，且全部活跃成员的任职及双向 `send_message` 策略仍有效；发现不合规时群在同一事务中进入 `policy_blocked`，拒绝新消息且不消耗序号。`policy_blocked` 群和已退群成员不能发送新消息，但本人当前账号及所选任职仍有效时可用相同 `client_msg_id` 重放原 ACK。消息、幂等键、连续 `seq`、Outbox 与请求审计同事务提交。Outbox 通知按消息序号对应的群成员区间解析收件人；通知只提示客户端补拉，不包含正文。当前 Web 页面可查看本人有效加入的群列表及其历史消息；仅当群状态为 `active` 且当前任职等于群成员来源任职时，可发送群文本消息。跨任职查看和 `policy_blocked` 群仅可查看历史；网络或服务故障导致发送结果未确认时保留原 `client_msg_id` 重试，切换会话或任职前须重试或放弃；服务端明确拒绝或重试窗口过期时解除待确认状态，后者应先核对历史。策略阻断后的恢复须由群主或管理员调用策略复核接口。

基础 Web 页面默认关闭。先在身份源注册支持授权码和 PKCE S256 的**公共客户端**，将其客户端 ID 同时加入 `IM_OIDC_ALLOWED_CLIENT_IDS`，并把精确回调 URL 注册为公开 HTTPS 地址，例如 `https://im.example.com/web/`。身份源须为此客户端签发满足上文约束的 API 访问令牌。经 HTTPS 反向代理提供同源页面和 API 后，额外配置：

```sh
export IM_WEB_ENABLED=true
export IM_WEB_AUTHORIZATION_URL='https://sso.example.com/group/authorize'
export IM_WEB_TOKEN_URL='https://sso.example.com/group/token'
export IM_WEB_CLIENT_ID='enterprise-im-web'
export IM_WEB_REDIRECT_URL='https://im.example.com/web/'
export IM_WEB_SCOPE='openid profile'
```

然后访问 `https://im.example.com/web/`。页面完成授权码登录和当前有效任职选择，可浏览当前任职下的已有单聊、查看本人跨任职加入的群列表、从可见目录选人新建群聊、按姓名查找可见同事、发起单聊、发送文本并补拉单聊消息。群列表展示群状态、角色及来源任职，支持分页和约 30～40 秒定时刷新；已展开的页会重新读取。点击群可按服务端区间授权规则补拉历史消息，不可见消息显示占位；选择群的来源任职且群状态正常时，可在网页发送群文本。跨任职查看和 `policy_blocked` 群禁用新消息输入，但结果未确认的旧消息仍可按原编号重试。网络或服务故障导致发送结果未确认时，需先重试或放弃该条消息，再切换会话或任职，以保留幂等重试编号；明确拒绝或重试过期会解除待确认状态。多页请求没有共享数据库快照，群在翻页期间因新消息改变排序时可能短暂缺漏，后续刷新会重新核对；实际访问权限始终由服务端检查。令牌只保存在页面内存，刷新或退出后需重新登录；重新登录并选择任职后，会话列表会从服务端重新加载。可配置 Redis 实时通知；即使 WebSocket 保持连接，页面也会在约 30～40 秒无通知后核对一次单聊会话、群列表和当前聊天，避免静默漏通知使页面长期停留在旧状态。连接断开或实时功能不可用时每 5 秒补拉一次当前聊天，群列表仍约 30～40 秒核对一次。核对间隔带随机错峰，页面处于后台时浏览器可能延后计时；这不是设备送达确认或生产故障恢复验收。当前 ACK 文案仅表示服务器持久化接收。`IM_WEB_SCOPE` 默认 `openid profile`；令牌兑换只访问服务器配置的 HTTPS 地址，不使用浏览器请求中的目标地址。部署前仍需用客户 IdP、HTTPS 入口和真实组织数据做联调；本地模拟测试不等于已完成联调。

集团管理员可从侧栏打开“消息保留期配置”，查看当前租户正文期限、版本及审批记录，填写 1～3650 天和已取得的审批引用，并确认作用于全集团后保存。已有消息历史时禁止延长期限，版本冲突须刷新后重新填写。网络异常或超时后先查询服务器状态；只有仍为原版本时才允许以原参数重试，不会自动使用新版本。当前配置一致只代表状态一致，不能独立证明哪次请求已执行。关闭弹窗仍保留待确认参数；未核对完成前不能手动切换任职／会话、退出或发起另一项保全管理操作。放弃核对不撤销服务器操作；刷新、强制退出会丢失本页核对信息。审批引用仅记录，不核验外部审批；保存不会启动清理任务，也不能恢复已清除的正文。详见 [P4-10 验收记录](docs/开发增量-P4-10-验收记录.md)。

同一弹窗可展开“查看审批历史”，每页 20 条展示期限、版本、审批引用、登记人及时间，支持刷新和加载更多。关闭、切换身份、当前配置刷新或写入时清空旧历史；权限失效同时禁用配置入口。历史仅供查询，不能回滚配置。详见 [P4-11 验收记录](docs/开发增量-P4-11-验收记录.md)。

这些地址和客户端 ID 必须替换为身份提供方实际配置；JWKS 地址必须经 HTTPS 直接访问，重定向会被拒绝，密钥须声明 `use=sig`，访问令牌须携带 `kid`。启用时配置不完整或初次获取验签密钥失败，服务启动失败。身份绑定不自动按姓名或邮箱创建。非标准或不透明令牌需另建适配器。

## 测试

```sh
go test ./...
IM_TEST_DATABASE_URL='postgres://postgres:local_only_password@127.0.0.1:55432/enterprise_im?sslmode=disable' go test ./... -count=1
IM_TEST_DATABASE_URL='postgres://postgres:local_only_password@127.0.0.1:55432/enterprise_im?sslmode=disable' IM_TEST_REDIS_URL='redis://127.0.0.1:16379/0' go test ./... -count=1
go vet ./...
```

浏览器恢复测试使用 Playwright；将其 `node_modules` 目录设为 `NODE_PATH`，如浏览器未由 Playwright 管理，再将 `CHROMIUM_EXECUTABLE` 设为 Chromium 可执行文件路径，运行 `node internal/webclient/e2e/safety_reconcile.cjs`、`node internal/webclient/e2e/multi_device_recovery.cjs`、`node internal/webclient/e2e/group_list.cjs`、`node internal/webclient/e2e/group_history.cjs`、`node internal/webclient/e2e/group_send.cjs`、`node internal/webclient/e2e/group_create.cjs`、`node internal/webclient/e2e/group_invite.cjs`、`node internal/webclient/e2e/group_leave.cjs`、`node internal/webclient/e2e/group_roster.cjs`、`node internal/webclient/e2e/group_policy_recheck.cjs`、`node internal/webclient/e2e/retention_records.cjs`、`node internal/webclient/e2e/legal_holds.cjs`、`node internal/webclient/e2e/legal_hold_actions.cjs`、`node internal/webclient/e2e/retention_policy.cjs`、`node internal/webclient/e2e/retention_history.cjs` 和 `node internal/webclient/e2e/audit_records.cjs`。

`multi_device_recovery.cjs` 使用共享模拟 HTTP 数据和模拟 WebSocket 信号验证两个 Web 页面；真实 Go/Redis 双节点广播由下述 Go 集成测试覆盖。`TestRealBrowserLoginRealtimeAndOfflinePull` 在本地 Chrome/Chromium 中经临时 HTTPS 入口完成两次 OIDC PKCE 登录，连接生产 API/Worker 和真实 PostgreSQL/Redis；第二个浏览器在测试中关闭定时轮询，验证实际 WebSocket 通知、重连 `ready` 帧触发的增量补拉和离线恢复，同时从生产管理员 API 展示正文／摘要清理批次与已解除／有效法务保全，并通过浏览器实际登记／解除一项保全，断言查询、写入审计与事件记录；同时实际修改租户正文保留期，通过页面读取已提交审批历史、按允许／拒绝结果查询管理审计并核对查询审计、已有消息时拒绝延长和另一租户不受影响。客户环境的身份源、证书、代理与浏览器兼容性仍需联调验收。

集成测试为每个用例创建独立 schema 并清理；未提供 `IM_TEST_DATABASE_URL` 或 `IM_TEST_REDIS_URL` 时分别跳过 PostgreSQL 或 Redis 集成测试。两个变量都配置时，`TestTwoDeviceRealtimeFromCommittedMessageThroughRedisAndReconnect` 会使用真实 PostgreSQL、显式调用的 Outbox Worker、Redis Stream、同一测试进程中的两个独立 API/WebSocket 服务实例和 HTTP 补拉。`TestMultiProcessRealtimeWorkerFanoutAndReconnect` 会编译并启动生产 Worker 可执行文件，另启两个独立进程运行生产 HTTP/WebSocket 处理器，验证持续发布、双节点通知和断线补拉。`TestProductionAPIWithOIDCAndRealtimeProcesses` 进一步启动两个生产 `im-api` 进程和生产 Worker，使用本地 TLS JWKS、签名访问令牌及数据库身份绑定验证 OIDC 验签、错误签名与未绑定身份拒绝、双节点通知和断线补拉。本地身份源和测试证书仅供验收；客户 IdP 和实际部署环境仍需联调。测试开始前可先在临时库创建 `btree_gist` 扩展，避免并行用例同时创建它。回滚时按 `000016` 至 `000001` 的逆序执行 Down 脚本，只对可丢弃的开发或测试数据库执行回滚。`000016` 在有摘要退役或批次证据时、`000015` 在有清理行或批次证据时、`000014` 在有保全历史时、`000013` 在租户保留期曾修改时、`000011` 在有邀请请求记录时、`000010` 在有建群请求记录时、`000009` 在有群会话时会拒绝回滚，需先妥善迁移或清理对应数据。

真实浏览器集成测试在 macOS/Linux 上运行，需额外设置 `IM_TEST_BROWSER_NODE`（Node 可执行文件）、`NODE_PATH`（包含 Playwright 的 `node_modules`）；使用外部安装的 Chrome/Chromium 时设置 `CHROMIUM_EXECUTABLE`，再运行 `go test ./internal/policystore -run '^TestRealBrowserLoginRealtimeAndOfflinePull$' -count=1`。未设置 `IM_TEST_BROWSER_NODE` 时该用例跳过；需同时设置上述 PostgreSQL 与 Redis 测试 URL。
