# P5-01 设备、送达、已读及 Web 接入规格

日期：2026-10-08。状态：总体方案已确认；本细化规格与实施计划待联合评审。

## 1. 目标与约束

从 P4-31 `b3b706c96a3f99e853163f88545437558fe4709a` 新建 `codex/p5-01-multi-client-receipts`。新增可独立测试的设备、送达、已读协议，并在现有 Web 验证完整链路，为随后四平台 Flutter 客户端提供固定契约。用户已确认采用推荐总体方案，由当前实施者逐项实现。

所有业务数据带 tenant；OIDC 验证身份来自现有认证器，acting membership 实时验证。不得信任请求体中的 tenant/user/device 所有者。保留既有消息写入、ACK、UUIDv7、Outbox、历史区间、hard_deny、动态保留与法务保全语义。现有 HTTP 消息接口兼容，不以新增协议替换其旧 JSON。

新增服务使用 Go 1.27.1、现有 pgx/PostgreSQL 16、Redis 及锁定 go.mod/go.sum。新增开关 `IM_RECEIPTS_ENABLED` 默认为 false；仅明确 true 且 OIDC 启用、迁移完整时装配。关闭时新增路由404，现有 Web 收发保持可用；不能显示虚假的送达／已读。

## 2. 状态的严格含义

1. sent：现有服务端持久化 ACK。
2. delivered：收件用户至少一台在确认时有效的登记设备，声明完整接收了该条可见消息。通知、请求开始、发送者自己读取及遮蔽占位均不算。
3. read：收件人设备已经 delivered，随后在前台激活会话且该消息进入可见区域，声明展示确认。read 蕴含 delivered；不声称证明人的理解或文件字节已下载。
4. 用户事实按 `(tenant,conversation,seq,user)` 去重，设备事实按附加 device 去重。已合法确认的事实不随注销设备回退。撤销只阻止后续使用。
5. 最高游标是进度，不是每条消息阅读证明。以逐消息事实为真相，禁止以 `last_seq-read_seq` 计算未读或填补不可见区间。
6. 群统计排除发送人；候选收件用户按消息 seq 落入的历史群成员区间确定，不按当前成员名单回填。同一用户不同设备／任职只计一人。

## 3. Tenant 配置与边界

新增 `tenant_client_policies`，主键tenant，配置缺行等同下列默认；更新使用版本比较并同事务审计。普通员工只读取有效能力，集团管理员可设置本租户配置。

| 键 | 默认 | 合法范围 |
| --- | --- | --- |
| device_lease_days | 30 | 1～90 |
| max_active_devices | 10 | 1～100 |
| offline_cache_seconds | 300 | 0～3600 |
| group_receipt_visibility | owner_admin | owner_admin / sender_aggregate |

配置版本和proof等新增协议整型范围与序号相同，HTTP以规范十进制字符串编码；小范围count/limit为JSON整数。同步证明有效期固定300秒；每页默认100、最大500；read每批1～500个去重seq；状态批查最多100条；设备列表默认20、最大100；群明细默认20、最大50。写入请求体最大64KiB；设备登记／续期最大2KiB；写入接口每用户每秒默认10请求，运行时配置1～10000，超限429。每设备最多20份未过期证明，达到上限429；可先确认、过期或明确放弃，不自动删除其它在用证明。

offline_cache_seconds 是本地阅读授权上限；取其与JWT剩余有效期、设备剩余期限、内容保留期中的最小值。配置0只允许在线授权阅读。法务保全不延长普通用户的正文可读期限。

## 4. 数据模型：新迁移000023和000024

### 4.1 000023_devices

- `tenant_client_policies(tenant_id,version,device_lease_days,max_active_devices,offline_cache_seconds,group_receipt_visibility,updated_at)`；version bigint非负，配置CHECK范围。
- `user_devices(id,tenant_id,user_id,registration_id,platform,name,credential_digest,created_at,expires_at,revoked_at,last_seen_at)`；platform闭集 web/windows/macos/android/ios；name有效UTF8、去首尾空白、1～80字符、无控制字符；digest bytea长度32。`UNIQUE(tenant_id,user_id,id)`、`UNIQUE(tenant_id,user_id,registration_id)`；复合用户外键；expires_at>created_at；撤销时间不可早于创建。
- 平台、所有者、登记ID、凭据摘要创建后不可变；revoked_at只能由NULL进入非NULL，不可恢复。续期只改expires_at，必须仍未过期且未撤销。
- `device_registration_windows(tenant_id,user_id,window_start,created_count)`：登记默认每用户每小时10次，配置 `IM_DEVICE_REGISTRATIONS_PER_HOUR` 1～1000。并发登记锁定用户行，数量限制及窗口计数同事务，失败／重复不额外计数。

### 4.2 000024_message_receipts

- `device_delivery_cursors(tenant_id,user_id,device_id,conversation_id,acting_membership_id,scanned_seq,delivered_seq,updated_at)`，五个身份键＋会话为主键；0≤delivered_seq≤scanned_seq。scanned_seq覆盖占位，delivered_seq是已收到的可见收件消息最高位，不代表连续收件集合。
- `conversation_user_read_cursors(tenant_id,user_id,conversation_id,read_seq,updated_at)`；read_seq≥0，单调更新。
- `device_sync_proofs(id,tenant_id,user_id,device_id,acting_membership_id,conversation_id,token_digest,after_seq,next_after_seq,issued_at,expires_at,confirmed_at)`，digest长度32且唯一；0≤after_seq≤next_after_seq；expires_at=issued_at+300秒。
- `device_sync_proof_items(tenant_id,proof_id,conversation_id,seq,message_id,recipient_interval_id)`，主键proof＋seq。仅保存该页最终未遮蔽、非本人发送且本人有接收资格的消息引用；无正文、名称、附件定位。单聊interval为NULL，群记录历史reader interval。页内其它seq用扫描边界表达，不伪造收件项。
- `message_device_receipts(tenant_id,conversation_id,seq,user_id,device_id,delivered_at,read_at)`，主键全部身份键。read_at为NULL或≥delivered_at。送达、已读合法事实不可删除或改为未确认；后续元数据退役必须另有保留设计。
- `message_user_receipts(tenant_id,conversation_id,seq,user_id,delivered_at,read_at)`，主键去掉device。首次合法时间保留；读事实不回退。首次设备送达才新增用户送达，首个用户读才新增用户读。
- `conversation_receipt_versions(tenant_id,conversation_id,revision)`；revision非负，行锁后仅有新增用户事实才递增。
- `receipt_outbox_events(id,tenant_id,conversation_id,revision,actor_user_id,event_type,status,attempt_count,next_retry_at,created_at,published_at)`；event_type固定receipt_changed，pending/published检查；唯一 `(tenant_id,conversation_id,revision)`。
- `receipt_outbox_targets(tenant_id,event_id,target_user_id)`：写入新用户事实对应消息的发送人＋确认用户本人，去重。用于通知其它发送者及本人的其它设备，不通过当前群名册扩大接收范围。

设备／用户／任职／会话／消息引用采用同tenant复合外键；proof增加 `(tenant_id,id,conversation_id,user_id,device_id,acting_membership_id)` 唯一约束，items按tenant/proof/conversation复合外键绑定页，不能换会话。群interval增加同tenant/conversation/user/id的唯一键，并以应用最终授权和数据库复合外键共同绑定reader资格；proof item的seq＋message_id与消息复合唯一键关联。历史回执无正文副本，正文清理不删除回执。索引覆盖token摘要、到期证明、用户设备有效期、用户未读anti-join、按消息统计、pending队列。down按依赖逆序，拒绝丢弃带业务数据的迁移须由部署流程决策；测试库验证空库回滚与重装。

总体提案中的 `message_receipt_ranges` 在此细化为逐消息两张事实表，原因是现有群历史含不可见区间，逐消息证据更易验证和去重；保留原有状态语义。

## 5. 设备协议

设备请求头为 `Authorization: Bearer ...`、`X-IM-Device-ID: <uuid>`、`X-IM-Device-Token: <base64url32bytes>`；消息动作另需既有 `X-Acting-Membership-ID`。UUID规范化小写；Token仅接受无padding的43字符base64url且解码32字节，摘要使用SHA256，比较使用恒定时间。不得记录这些头。

登记 `POST /api/v1/devices`：当前有效任职＋严格JSON `{registration_id:<uuidv7>,platform,name}`。服务端生成256 bit随机设备凭据；201返回 `{device_id,device_token,expires_at,server_time}`。凭据只返回一次。重复registration_id返回409 `{error_code:"registration_exists",device_id}`，仅对本人可见；网络丢失首次响应时客户端撤销该孤立登记，再用新ID登记，不能从摘要恢复凭据。

`GET /api/v1/devices?cursor=...&limit=20`、`DELETE /api/v1/devices/{id}`使用已验证有效账号身份，不要求仍有可选任职，允许结束最后任职后管理自己的设备；数据库复核tenant/user状态。删除是撤销，首次和重复都204；其它人的设备404。默认列表按created_at、id降序，返回active/expired/revoked，名称及期限，不返回凭据摘要。

续期 `POST /api/v1/devices/{id}/renew`：有效任职＋本人有效设备凭据；200 `{device_id,expires_at,server_time}`。已过期／撤销凭据401 `invalid_device`，不能复活。登记和续期需要有效任职，普通设备所有权不带管理其它账号的权限。

Web凭据只在页面内存；每次登录新登记，登出主动撤销，异常关闭按期限失效。默认设备额度可能因反复登录耗尽，登录时提供本人设备列表及撤销入口，不能自动撤销未知设备。

## 6. 同步、送达与已读协议

新增 `GET /api/v1/conversations/{id}/sync?after_seq=0&limit=100` 同时处理单聊与群；旧接口保持原序号JSON。新增协议的所有seq、revision与游标用规范十进制字符串传输，范围0～9223372036854775807，避免JS超过2^53的精度问题。

返回 `{conversation_id,messages,next_after_seq,has_more,sync_proof,proof_expires_at,server_time,receipt_revision}`。messages使用typed_v1内容字段，seq为字符串；未遮蔽内容另有 `content_expires_at`、`cache_until`。完全空页返回null证明；占位页可返回证明以推进扫描。服务器不要求客户端从0重新开始已有扫描；允许重拉已扫描页，但不可确认跳过尚未扫描的缺口。

服务端同期生成页面和proof：复用history_read_tx中的最终授权，锁身份与当前policy版本并在完成前再次检查时限，证明的items与最终返回页完全一致。当前读取审计在同事务提交，失败不返回proof。

`PUT /api/v1/conversations/{id}/delivered` JSON `{sync_proof:<token>}`：锁当前设备、actor、proof、conversation，再锁本设备该任职扫描行；proof.after_seq必须≤当前scanned_seq，proof.next_after_seq可以重叠但不得倒退扫描；after_seq>scanned_seq返回409 `sync_gap`。初始scanned_seq为0。重新检查证明条目的当前历史授权、hard_deny和内容期限；任一原可见项现在无效则整页409 `sync_context_changed`，重新同步才能推进，不将旧页部分成功冒充整页收到。

成功送达：推进扫描max；只为proof.items新增device事实和user事实；重复不生成新用户事件。与proof确认、游标、审计、回执revision／队列／targets同事务。未过期且已确认proof重放仍返回当前状态；过期proof410，已有事实保持。响应200 `{scanned_seq,delivered_seq,read_seq,receipt_revision,duplicate}`。

`PUT /api/v1/conversations/{id}/read` JSON `{seqs:["1","3"]}`：数组1～500条，升序严格递增，无重复，无0。锁合法设备及当前身份，确认每条都有该设备本人delivered事实且当前正文可见、为收件消息；不满足则404整批拒绝，不告知是哪条。无需保留已过期的同步proof即可读此前已送达消息。更新device与user read事实及read_seq=max；只为新增用户read产生revision事件；200返回与送达相同字段，duplicate表示没有新增该设备阅读事实。跨设备已有用户read时，另一设备仍须收到并确认自己的device事实，用户计数不增加。

消息级授权使用现有历史sender／recipient快照及群reader interval；新读/送达不能使用目前会话任职猜测旧接收人。历史事实可保留，但当前被遮蔽的消息不能用于新写入或查询敏感状态。

## 7. 查询、群权限与未读

`GET /api/v1/conversations/{id}/receipts?seqs=1,2`：至多100条升序seq，只允许本人发送且当前正文可见的消息，整批无权404。单聊返回sent/delivered/read及匿名确认时间，永不返回对方device信息。

群默认owner_admin：仍须是当前有效来源任职下的active owner/admin，沿用已有名册门槛；普通成员返回每条 `{seq,status:"sent",summary_available:false}`，UI显示“已保存，群回执不可查询”。租户显式sender_aggregate时，发送人可看匿名汇总；用户明细仍限owner/admin。汇总只覆盖历史区间合格且当前目录对操作者可见的接收用户，不泄露隐藏用户是否存在；字段名明确为 `visible_recipient_count,visible_delivered_count,visible_read_count`，界面显示“可见接收者已读 X/Y”。隐藏用户仍可合法确认自己的回执，但不被未授权查询者统计。群明细沿用同一可见集合、按user去重、分页最多50，不暴露历史interval id。

`GET /api/v1/conversations/{id}/receipts/{seq}/users?cursor=...&limit=20` 只对本人发送消息的当前owner/admin开放。源成员离群、换任职或目录不可见即重新拒绝，旧页面的下一页游标不构成授权。

`GET /api/v1/conversations/{id}/unread`：查询本人当前授权可见、未到期、非本人发送且没有user read事实的消息。固定head_seq后每页最多500条，单次最多检查10000条、3秒预算；返回 `{unread_count,exact,head_seq,receipt_revision,policy_version}`。未完成扫描时返回已实际发现数量作为下界、exact=false，不能返回未经扫描的精确数或显示0；UI显示“至少N条”，N=0则显示“未读待核对”。缓存结果必须带身份代次、head_seq、receipt_revision和policy_version，通知／权限变化后刷新。

`GET/PUT /api/v1/admin/client-policy` 仅集团管理员可修改，PUT包含expected_version及四个配置字段，CAS冲突409。`GET /api/v1/me/client-capabilities` 返回有效配置、receipts_enabled及server_time，需要当前有效身份/任职；无管理员grant写权限。

## 8. 事务与并发

消息动作统一锁序：有效身份和相关历史行的既有顺序→policy_current SHARE→conversation SHARE→device SHARE（生成proof时为UPDATE，用于额度串行化）→proof UPDATE（送达）→device cursor UPDATE→按seq升序receipt事实→user cursor UPDATE→conversation_receipt_versions UPDATE。设备撤销独立以用户身份行SHARE＋device UPDATE，与回执device SHARE串行化；并发撤销完成前已合法提交的事实保留，撤销提交后新操作拒绝。

首次device cursor或receipt version使用INSERT ON CONFLICT DO NOTHING后行锁；不依赖预先存在。状态更新时间用数据库／服务端时钟，不接收客户端时间；proof最后提交检查身份、到期与设备期限。数据库可重试故障不在内部隐式无限重试：返回503，客户端以相同证明/seq重试。20份proof额度在device UPDATE锁下计数生成，避免并发超额。

证明清理 `Service.CleanupExpiredSyncProofs(ctx,limit)` 只删除已过期proof及items，limit1～1000；专用回执Worker调用，不扫凭据或合法事实。正常到期凭据仅变为不可使用，保留设备列表可撤销。worker审计与任务错误分别记录，不把cleanup异常当回执业务成功。

所有业务写的必要审计失败回滚；错误日志只记录闭集代码、操作与非敏感资源引用，不记录payload、token、用户名或JWT。无访问数据的401/400可使用既有拒绝日志；已鉴权业务拒绝按既有项目审计模式处理。

## 9. 回执通知

新增 `internal/receiptoutbox` 与 `cmd/im-receipt-worker`；有界SKIP LOCKED、5秒发布预算、现有指数退避、Redis成功后数据库失败可重投。独立publisher presence键避免伪装原message worker。API开启回执实时通知时同时检查回执publisher存在；缺失则明确降级到查询/轮询，不让原消息推送失败。

Redis最小事件 `{event_id,event_type:"receipt_changed",tenant_id,conversation_id,revision}`；Resolver从持久化outbox＋targets验证事件，不信任Redis直接提供用户列表。读取器识别两种合法事件，保持旧message_created严格解析。最终WebSocket仍只发送sync_required，receiver通过HTTP更新自己的read状态或发送消息状态。同event_id可重复，revision不得越过持久化证据；无消息正文和设备信息。

## 10. Web集成与完成定义

新增 `assets/device-session.js`、`assets/receipts.js`，保持app.js只添加适配接口。通过能力返回值决定启用；启用后当前聊天使用新sync接口，旧客户端继续可用。同步证明须在整页连续验证和消息状态提交后确认；前台可见消息read合批300ms，最多500条。切会话／任职／账号取消旧generation，晚到响应不能提交。

阅读条件：document.visibilityState=visible、窗口焦点、激活会话且消息元素≥50%可见连续500ms；仅当前已送达消息。后台接收可delivered，不read。通知预览、搜索结果与历史定位未展示不read。页刷新无device凭据仍可重新登录补拉。设备额度满时允许本人管理旧设备。

独立验收：真实PG migration up/down、送达/read幂等与并发、500条/64bit边界、区间遮蔽、到期/撤销/审计回滚、Redis重投和两个真实浏览器端到端全部通过；保存实际退出与资源归属证据。此阶段可完整交付设备与回执、Web，不宣称Flutter四平台完成。R1/R2关键场景在实施计划逐项列出。
