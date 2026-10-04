# P4-23 单聊／群聊附件消息 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将原会话、原上传用户及任职的已扫描文件一次性绑定到可靠消息，完成安全重试、补拉兼容及保全感知摘要退役。

**Architecture:** 扩展现有消息事务和接口，以正式 file 类型及独立关联表保存绑定。说明复用受清理约束的 text_body，HTTP 按类型投影；Outbox／Redis／WebSocket 仍只传消息引用及补拉提示。生产附件发送保持关闭。

**Tech Stack:** Go 1.27.1（沿用 go.mod）、pgx/v5、PostgreSQL、现有 Redis／WebSocket／OIDC／Chrome 测试工具、P4-22 私有版本化 S3 与真实 ClamAV／qpdf。无新产品依赖。

**Spec:** [已确认规格](../specs/2026-10-04-p4-23-file-message-design.md)，确认版本 `885f62fd1ace1bf40bc896bf76298f1175254e65`；产品基线 `b03a1977cf39f2f19db9d45cd4bce9a7338a2372`。用户已确认实施计划。全部10项实施任务完成：固定归档门禁、一次整体评审、资源清理和草稿PR #71完成。详见阶段验收记录。

## Global Constraints

- 执行方式已选：当前助手逐项实现，使用 executing-plans；最后一次独立整体评审。不重新选择执行方式，不合并／部署。
- 使用现有隔离工作树 `/Users/leo.cui/.codex/worktrees/p4-04-body-cleaner/企业IM系统`，分支 `codex/p4-23-file-message-design`；执行前复核 clean、HEAD 和父分支，不改主目录及其他用户文件。
- 每条附件消息恰好绑定一个 ready 文件；同租户、原会话、原上传用户和原任职；同文件只绑定一次，删除或退役不释放绑定。
- 消息、关联、幂等、seq、Outbox 与成功审计原子提交。失败无临时消息／绑定／seq／配额残留；相同逻辑重试返回原 ACK。
- message_type 仅 text／file，默认 text、创建后不可改。file caption 缺省为原始空字符串；合法 UTF-8、无 NUL、最多 16 KiB，可为空／空白；非清理不能为 NULL。
- text 保留现有非空、非空白、UTF-8、无 NUL、16 KiB 限制。原文本 SHA-256、UUIDv7 时间规则、30 天幂等期限、现有限流和 ACK 字段不变。
- 附件摘要为 Go encoding/json.Marshal 默认字符串数组编码：无缩进、无结尾换行、默认 HTML 转义。固定数组为 ["file-message-v1","file",tenant_id,conversation_id,sender_user_id,sender_membership_id,file_id,sealed_sha256_lower_hex,caption]；UUID 小写，caption 不 trim、不改换行。
- 新发送按当前配置 enabled、大小和类型重新判定；仅 ready 不构成发送授权。文件删除后的重放用关联封存指纹；摘要退役后 HTTP 410 retry_window_expired，不能重用旧键新建。
- 显式 READ COMMITTED；成员／会话／策略→文件配置→文件→绑定，消息及摘要锁沿用现有顺序；NOWAIT／死锁有界重试最多 3 次，不把外部 I/O 放进数据库重试闭包。
- 锁等待及成功审计写入后、实际 commit 前以新鲜时间复核身份和期限。群成员区间先于 policy_blocked／ACK；新发送要求当前原任职参与，不能借重放开放新写入。
- GET 参数 message_format=typed_v1 显式 opt-in；缺省旧 DTO。旧 file 响应 text 固定为“附件消息（当前客户端不支持查看）”，不返回 caption、file_id 或名称。
- 到期、物理清理或历史权限遮蔽时只返回 seq／redacted，省略类型、身份、时间、说明及附件全部字段。法务保全暂停物理清理及摘要退役，不延长逻辑可见期限。
- sealed_sha256 与两份 content_digest 在同一事务同一时间退役；关联来源／file_id 保留，三份状态须由 DB 约束一致，不允许回填或删除证据。
- 现有文本搜索只检索 text；caption／文件名不参与。Outbox／Redis／WebSocket／日志／审计无 caption、名称、SHA、对象定位、扫描输出或下载令牌。
- cmd/im-api 的 file 发送始终 503 file_message_unavailable；本阶段没有生产启用环境变量。typed_v1 附件 download_available=false，不输出任何下载 URL。
- 有附件数据时 down 必须先拒绝且不破坏证据；无数据才恢复旧结构。下载／对象清理／文件保留配置属 P4-24，Web 附件交互／文件名搜索属 P4-25。
- 本轮凭据 0600、日志目录 0700，不输出 DSN／密钥。固定提交归档运行门禁；真实依赖无跳过，完整回归单列既有辅助启动器 TestRealtimeAPIChild。

## Review Focus

- RF1：JSON 转义后的孤立代理码点不能静默变成替换字符；合法代理对、组合字符和 CRLF 必须原样进入规范 caption。归 Task 2／5。
- RF2：同一 UUID 的大小写别名、caption 缺省与显式空、text／file 同键不能绕过冲突或形成两条消息。归 Task 2／3／4。
- RF3：ready 后等待配置／审计锁期间配置或人员资格变化，必须在实际写入处形成确定先后，迟到成功不能提交。归 Task 3／4／8。
- RF4：文件缺失、LEFT JOIN 不完整或已删除不能把 caption 渲染成文本，也不能阻断正常文本页的序号推进；不完整关联必须失败封闭。归 Task 6。
- RF5：down 检查空表后遇到并发附件提交，必须通过先锁表后检查拒绝破坏，不能依赖检查时的瞬时为空。归 Task 1。

---

## 文件职责与接口路线

所有路径相对上述工作树。新增文件集中维护附件契约；大文件只增加必要接入点。

| 文件 | 职责／任务 |
|---|---|
| db/migrations/000020_file_message.up.sql、.down.sql | 类型、复合来源约束、单次绑定、指纹退役和保护性回退；Task 1 |
| internal/policystore/file_message_schema_test.go、file_message_test_helpers_test.go | 真实 SQL 约束与专用数据库夹具；Task 1 |
| internal/policystore/migration_test.go、conversations_test.go、file_foundation_migration_test.go、file_runtime_schema_test.go、body_clear_migration_test.go、digest_retirement_migration_test.go | 显式迁移列表与嵌套回退先处理 20；Task 1 |
| internal/policystore/message_content.go、message_content_test.go、file_message_idempotency.go | 类型化请求、规范摘要与重放证据；Task 2 |
| internal/policystore/message_idempotency.go、file_message_idempotency_test.go | 文本／附件类型冲突及三份证据一致性；Task 2 |
| internal/policystore/file_message_binding.go、file_message_send.go、messages.go、file_message_send_test.go | 原文件绑定、单聊发送与最终复核；Task 3 |
| internal/policystore/group_message_send.go、group_file_message_send_test.go | 群成员、全群策略、file 事务及重试；Task 4 |
| internal/httpserver/message_content.go、message_content_test.go、file_messages_test.go、conversations.go、groups.go | 严格请求、默认关闭及显式测试装配；Task 5 |
| internal/policystore/file_message_history.go、file_message_history_test.go、history_read_tx.go、message_pull.go、group_history.go | 类型化历史查询、遮蔽与卡片；Task 6 |
| internal/policystore/message_search.go、cross_message_search.go、file_message_search_test.go | 文本候选限定、游标及最终过滤；Task 6 |
| internal/httpserver/message_projection.go、file_message_pull_test.go、conversations.go | typed_v1 与旧 DTO 投影；Task 6 |
| internal/retention/attachment_digest.go、attachment_digest_test.go、digest_batch.go、digest_concurrency_test.go | 三份证据原子退役及保全竞态；Task 7 |
| internal/policystore/file_message_concurrency_test.go | 两会话并发、删除／配置／成员／审计故障；Task 8 |
| internal/policystore/file_message_real_integration_test.go、file_message_browser_integration_test.go、cmd/im-api/main_test.go | 真实扫描→发送→实时补拉、旧 Web 与生产关闭；Task 9 |
| scripts/test-file-messages.sh、testdata/file-runtime/README.md | 新链路前置检查与零跳过门禁；Task 9 |
| docs/开发增量-P4-23-验收记录.md、docs/企业IM-开发计划与实施路径-v0.1.md | 本轮证据与进度；Task 10 |

## Task 1：000020 类型、附件约束及保护性回退

**Files:** 新建两份 000020 SQL、file_message_schema_test.go、file_message_test_helpers_test.go；修改职责表中六份现有迁移测试。

**Interfaces:** 产出 messages.message_type 和规格第 5 节 message_attachments 六类字段；主键 (tenant_id,message_id)，唯一 (tenant_id,file_id)。测试助手 `fileMessageFixture(t *testing.T,c *pgx.Conn,conversationID,userID,membershipID string) files.Metadata` 只建立专用 SQL 约束夹具，不充当真实扫描证据。Task 3～8 的数据库边界测试可使用该夹具，真实扫描和端到端成功证明必须来自 Task 9。

- [x] **1. 写 RED 测试**：TestFileMessageMigrationPreservesText、TestFileMessageSchemaSources、TestFileMessageSchemaCardinality、TestFileMessageSchemaReadyBinding、TestFileMessageSchemaFingerprintRetirement、TestFileMessageDownGuards、TestFileMessageDownConcurrentInsert。断言跨租户／会话／用户／任职、text 带附件、file 无附件／两附件、错误扫描指纹、未 ready 插入均被真实 SQL 拒绝；空 caption 合法，清理后不可恢复；退役只改两份或一份指纹均拒绝。RF5 以第二连接证明 down 锁后不会漏掉并发提交。
代表断言（TestFileMessageSchemaCardinality 中建立 file 消息而不写关联，提交应失败）：
```go
err := tx.Commit(ctx)
if err == nil { t.Fatal("file message without attachment committed") }
expectFileSQLState(t, err, "23514")
```

- [x] **2. 运行 RED**：`go test ./internal/policystore -run '^TestFileMessage(Migration|Schema|Down)' -count=1 -v`，在已配置专用 IM_TEST_DATABASE_URL 下因缺少 000020／列失败；数据库连接错误或 SKIP 不计 RED。
- [x] **3. 实现 SQL 与夹具顺序**：为消息和文件来源增加必要复合唯一键／FK；绑定守卫按会话→文件锁序校验 ready／同指纹。用延迟约束证明 file 恰一个、text 零附件及三份退役时间一致；默认 text 前进兼容。down 在结构修改前按固定次序锁 messages／message_attachments，检查存在 file／绑定则拒绝。显式回退链先 down20，再 down19／18；重新前进逆序。旧 body_clear／digest_retirement 的回退夹具也先移除 20，使其拒绝仍由被测 15／16 的原保护产生，不能用新依赖冲突替代原证明。新测试按旧迁移版本验证前进后的历史正文和摘要未变化。
- [x] **4. GREEN**：重复上述命令，再 `go test ./internal/policystore ./internal/retention -run 'Migration|Schema|Down' -count=1 -v`；所有涉及真实数据库的用例 PASS，无跳过，旧清理／退役约束继续有效。
- [x] **5. 提交**：仅暂存本任务列出的文件；`git commit -m "feat(db): enforce single file message bindings"`。

## Task 2：类型化内容、规范摘要与重放证据

**Files:** 新建 message_content.go／_test.go、file_message_idempotency.go／_test.go；修改 message_idempotency.go。

**Interfaces:**
- `MessageSendRequest`：ClientMessageID、MessageType、Text、FileID、Caption 均 string；常量 MessageTypeText="text"、MessageTypeFile="file"。
- `validateMessageSendRequest(req MessageSendRequest) (MessageSendRequest,error)`：内部校验，UUID 规范小写；UUIDv7 时间校验仍由发送事务调用既有 validateClientMessageID。HTTP 字段有无／NULL 由 Task 5 处理。
- `fileMessageDigest(id access.TrustedIdentity,conversationID string,req MessageSendRequest,sealedSHA [32]byte) ([32]byte,error)`，固定编码见 Global Constraints。
- `existingTypedMessageACK(ctx context.Context,tx pgx.Tx,id access.TrustedIdentity,conversationID string,req MessageSendRequest) (MessageACK,bool,bool,error)`：分别表示 ACK、exists、same、error；一条 SELECT 同时读取消息、幂等和附件证据。
- ErrFileNotBindable、ErrFileMessageUnavailable 为 policystore 可 errors.Is 的新增错误；损坏证据用既有 errInvalidMessageIdempotency；文本现有错误保持。

- [x] **1. 写 RED 测试**：TestMessageContentBounds、TestFileMessageDigestCanonical、TestFileMessageReplayEvidence。RF1／RF2 断言 16,384 字节通过、16,385 拒绝、空／空白 caption 通过，NUL／无效 UTF-8 拒绝；固定含 <&、CRLF、组合字符的编码向量；UUID 大小写同摘要；caption 内容变化不同摘要。SQL 测试同键跨类型必冲突、删除后绑定指纹可重建、三份退役只返回 ErrRetryExpired；缺失／损坏证据通过单查询行测试替身验证失败封闭，不放宽真实数据库约束来制造坏数据。
代表断言（TestFileMessageDigestCanonical 以固定字面规范向量求预期，不调用摘要函数生成预期）：
```go
want := sha256.Sum256([]byte(wantCanonicalJSON)) // wantCanonicalJSON 是写在测试里的规范字面向量
if got != want { t.Fatalf("canonical digest mismatch: got %x want %x", got, want) }
if gotUppercaseUUID != got { t.Fatal("UUID alias changed the logical request") }
```

- [x] **2. 运行 RED**：`go test ./internal/policystore -run 'TestMessageContent|TestFileMessageDigest|TestFileMessageReplayEvidence' -count=1 -v`，期望新类型／函数尚不存在或行为不满足，不能把环境失败当 RED。
- [x] **3. 实现契约**：每个新函数放在上述确切文件；共享单查询读取解析助手，防止两次查询跨越退役。existingMessageACK 的旧签名保留，但只将 text 记录判定为同请求；新入口使用 existingTypedMessageACK。保持旧文本 raw SHA；file 重放使用关联指纹且不查文件状态，不复制 caption 或另存低熵正文摘要。
- [x] **4. GREEN**：上述命令及 `go test ./internal/policystore -run 'Test.*(Idempotency|DigestRetirement|MessageContent|FileMessageDigest|FileMessageReplayEvidence)' -count=1 -v` 全部 PASS；固定向量须以字面规范字节校验，不能由被测函数生成预期值。
- [x] **5. 提交**：仅本任务文件；`git commit -m "feat(messages): define typed file content and replay proofs"`。

## Task 3：单聊附件原子发送与最终授权复核

**Files:** 新建 file_message_binding.go、file_message_send.go／_test.go；修改 messages.go。

**Interfaces:**
- `Service.SendMessage(ctx context.Context,id access.TrustedIdentity,conversationID string,req MessageSendRequest) (MessageACK,error)`；旧 SendTextMessage 的签名保留并委托同一单聊链路。本文代码片段为具名测试中的核心断言；输入、锁夹具、查询计数与响应解析在该测试中准备，不另引入产品接口。
- `preparedFileBinding`：FileID string、SealedSHA [32]byte；`prepareFileBindingTx(ctx context.Context,tx pgx.Tx,id access.TrustedIdentity,conversationID,fileID string) (preparedFileBinding,error)`，消费 Task 1 来源约束，在同一发送事务锁配置→文件并复核类型／大小／来源／ready。
- `insertFileBindingTx(ctx context.Context,tx pgx.Tx,id access.TrustedIdentity,ack MessageACK,binding preparedFileBinding) error`；只写关联，无独立 commit。
- `Service.finishFileMessageSend(ctx context.Context,tx pgx.Tx,id access.TrustedIdentity,conversationID,reason string,at time.Time,recheck func(time.Time) error) error`：成功审计先写，后用 s.now() 的新鲜时间调用复核，再 commit；复核回调不自行 commit，返回否决后调用方回滚 savepoint 再登记拒绝／群暂停证据。调用方的 savepoint 包含配额／seq／消息／绑定／幂等／Outbox／成功审计。

- [x] **1. 写 RED 测试**：TestFileMessageDirectAtomic、TestFileMessageDirectReplay、TestFileMessageDirectOrigin、TestFileMessageDirectPolicy、TestFileMessageDirectAuditRollback。断言新发送六类记录同提交；同请求 duplicate=true 且消息／绑定／幂等／Outbox／seq／配额不增加，允许既有 idempotent_replay 审计；文件删除／caption 清理后 ACK 相同；同键不同内容冲突。RF2 原任职变化拒绝；RF3 当前配置 disabled、声明／检测类型不允许、大小降限、身份期限或规则期限跨过审计等待拒绝；失败 seq 和限流计数不增长。旧文本 ACK 与原摘要保持。
代表断言（TestFileMessageDirectReplay，两个请求的 ACK 和实际数据库计数）：
```go
if !replay.Duplicate || replay.MessageID != first.MessageID || replay.Seq != first.Seq || !replay.ServerTime.Equal(first.ServerTime) { t.Fatal(first, replay) }
if messageCount != 1 || bindingCount != 1 || keyCount != 1 || outboxCount != 1 { t.Fatal("duplicate send produced extra records") }
```

- [x] **2. 运行 RED**：`go test ./internal/policystore -run '^TestFileMessageDirect' -count=1 -v`，先确认缺少 SendMessage 或未绑定的具体失败。
- [x] **3. 实现单聊接入**：复用原成员选择、双方策略和 recipient 身份快照；附件分支先验证原当前参与资格再读重放证据。附件新发送锁定 policy_current 当前版本后加载规则，避免发布并发越过本次授权；调用 prepareFileBindingTx，消息 INSERT 指明 type 并以 caption 作受清理正文，绑定加入同一事务；成功审计后的 recheck 验证原选任职、当前身份／目标及规则期限。事务异常全部回滚；有界处理 40P01／55P03，最多 3 次；不调用独立 fileTransaction。
- [x] **4. GREEN**：上述命令与 `go test ./internal/policystore -run 'Test.*(SendText|MessageSend|FileMessageDirect|DigestRetirementSend)' -count=1 -v` 全部 PASS；证据断言通过 SELECT 实际数据库计数和原 ACK 字段完成。
- [x] **5. 提交**：仅本任务文件；`git commit -m "feat(messages): send direct file messages atomically"`。

## Task 4：群聊附件与历史参与／当前成员边界

**Files:** 修改 group_message_send.go；新建 group_file_message_send_test.go。复用 Task 3 文件，不另建文件事务。

**Interfaces:** `Service.SendGroupMessage(ctx context.Context,id access.TrustedIdentity,groupID string,req MessageSendRequest) (MessageACK,error)`；旧 SendGroupTextMessage 签名保持。内部单次函数 `sendGroupMessageOnce(ctx context.Context,id access.TrustedIdentity,groupID string,req MessageSendRequest) (MessageACK,error)` 使用 Task 2、3 契约；群 policy_blocked 与历史参与证据检查顺序保持。

- [x] **1. 写 RED 测试**：TestFileMessageGroupAtomic、TestFileMessageGroupReplay、TestFileMessageGroupMembership、TestFileMessageGroupFullPairPolicy、TestFileMessageGroupFinalTime。从当前有效群成员发送 ready 文件；完整 ordered pair 判定、来源任职不匹配、未曾加入／已离开／被移除、群停用／policy_blocked 和审计故障分开断言。RF2 同键 text／file 冲突。附件重放要求当前原任职参与；现有离群人员重放自己文本的已批准语义继续通过，不扩展为附件新发送。
代表断言（TestFileMessageGroupMembership，退出后发起新附件发送）：
```go
_, err := service.SendGroupMessage(ctx, identity, groupID, req)
if !errors.Is(err, policystore.ErrMessageNotAvailable) { t.Fatalf("former group member sent a file: %v", err) }
if lastSeqAfter != lastSeqBefore { t.Fatal("denied send consumed a sequence") }
```

- [x] **2. 运行 RED**：`go test ./internal/policystore -run '^TestFileMessageGroup' -count=1 -v`，确认新增群入口／类型绑定缺失。
- [x] **3. 实现群分支**：保持群锁、历史区间、当前 active intervals 和全群策略；新附件在读取规则前锁定 policy_current，并使用 Task 3 配置与文件锁。原 savepoint 同时覆盖绑定；成功审计后调用 finishFileMessageSend 并复核 actor／members／规则期限。暂停群的否决证据可提交，临时写入必须回滚；最多 3 次重新授权重试。text 分支保留原重放边界。
- [x] **4. GREEN**：上述命令及 `go test ./internal/policystore -run 'Test.*(GroupText|GroupMessage|FileMessageGroup)' -count=1 -v` 全部 PASS，包括历史资格先于 policy_blocked 的回归。
- [x] **5. 提交**：仅本任务改动；`git commit -m "feat(messages): bind group file messages in reliable send"`。

## Task 5：HTTP 类型请求与生产默认关闭

**Files:** 新建 httpserver/message_content.go／_test.go、file_messages_test.go；修改 conversations.go、groups.go。cmd/im-api 继续使用默认 HandlerWithConversations。

**Interfaces:**
- `FileMessageService` 接口有 SendMessage／SendGroupMessage，签名与 Task 3／4 完全一致。
- `HandlerWithFileMessages(base http.Handler,auth Authenticator,conversations ConversationService,files FileMessageService) (http.Handler,error)` 为显式依赖装配；参数不可 nil。现有 HandlerWithConversations 共用内部路由，附件服务为 nil，file 始终 503；不能通过 conversations 的类型断言自动启用。
- `decodeMessageSendRequest(raw []byte) (policystore.MessageSendRequest,error)`：解析有无字段及类型，file 重复键与孤立代理码点拒绝；旧文本调用既有文本入口；typed file 调用显式文件服务。

- [x] **1. 写 RED 测试**：TestFileMessageHTTPParsing、TestFileMessageHTTPClosed、TestFileMessageHTTPRouting、TestFileMessageHTTPACK。覆盖旧／显式 text、缺省／空 caption、NULL、未知／重复键、多 JSON、非字符串、128 KiB 请求上限及 16 KiB caption 边界；RF1 合法代理对保留、孤立代理码点拒绝。关闭装配请求任意 UUID file 均 503 且 stub 调用次数为 0；认证失败仍按既有 401／403。ACK 无 URL／SHA；错误按规格 4.2 映射。
代表断言（TestFileMessageHTTPClosed）：
```go
if response.Code != http.StatusServiceUnavailable || errorCode != "file_message_unavailable" { t.Fatal(response.Code, errorCode) }
if fileServiceCalls != 0 { t.Fatal("closed production route inspected a file") }
```

- [x] **2. 运行 RED**：`go test ./internal/httpserver -run '^TestFileMessageHTTP' -count=1 -v`，期望新解析／装配不存在或行为失败。
- [x] **3. 实现严格分支**：保持 Content-Type、无 query 和现有体上限；检测字段有无／NULL 和 file 重复键，不将 NULL 与缺省混为一谈。解析后依 Task 2 校验，显式 file 走测试装配；默认 nil 文件服务直接统一拒绝，不访问 file_id。错误新增映射到 409 file_not_bindable／503 file_message_unavailable，ErrRetryExpired 继续 410 retry_window_expired。
- [x] **4. GREEN**：上述命令与 `go test ./internal/httpserver ./cmd/im-api -count=1` 全部适用用例 PASS，旧发送及群路由无回归；进程真实证据在 Task 9 执行。
- [x] **5. 提交**：仅本任务文件；`git commit -m "feat(api): parse typed messages with file sends closed by default"`。

## Task 6：类型化补拉、旧占位与文本搜索隔离

**Files:** 新建 policystore/file_message_history.go／_test.go、file_message_search_test.go、httpserver/message_projection.go、file_message_pull_test.go；修改历史／补拉／搜索及 HTTP 文件，见职责表。

**Interfaces:**
- PulledMessage 新增 MessageType string、Attachment *MessageAttachment；Text 对 file 仅在内部保存 caption，不直接输出旧 DTO。
- `MessageAttachment`：FileID string、Available bool、OriginalFilename string、ActualSizeBytes *int64、DetectedMediaType string；不含 SHA／key／version／下载凭据。公开附件对象字段为 attachment，包含 file_id、available、download_available=false；available=true 时另有 original_filename、actual_size_bytes（十进制字符串）、detected_media_type。typed file 的 caption 即使为空也显式返回。
- `historyReadMode`：historyReadAll／historyReadTextOnly。新增 readDirectHistoryBatchTxMode／readGroupHistoryBatchTxMode，签名为 `(ctx context.Context,tx pgx.Tx,scope historyReadContext,conversationID string,afterSeq int64,limit int,mode historyReadMode) (historyReadBatch,error)`；原 read*HistoryBatchTx 保留同签名，委托 all；新 readDirectTextSearchBatchTx／readGroupTextSearchBatchTx 保留原签名，委托 textOnly。
- 现有 PullTextMessages／PullGroupTextMessages 保留；查询返回类型化内部事实。HTTP projector `messagePageDTO(page policystore.MessagePage,typed bool) any` 在最终授权过滤后投影，typed 由唯一有效 message_format=typed_v1 决定。

- [x] **1. 写 RED 测试**：TestFileMessageHistoryDirect、TestFileMessageHistoryGroup、TestFileMessageHistoryRedaction、TestFileMessageHistoryUnavailable、TestFileMessagePullFormat、TestFileMessageSearchExcludesAttachments。RF4 混合 text/file 页仍按 seq 正常推进；空 caption 附件不得当空文本；file deleted／delete_pending 隐藏名称等内容字段；损坏／缺失关联失败封闭。精确 JSON 断言 redacted 只有 seq／redacted，旧格式只固定占位、typed file 有 caption 和最小卡片。搜索中相同关键词出现在 caption／文件名时不匹配，真正文本仍匹配；游标不遗漏后续文本，最终时间及 hard_deny 遮蔽整个卡片。
代表断言（TestFileMessagePullFormat，检查页中单条遮蔽消息的精确字段）：
```go
if len(item) != 2 || item["seq"] != float64(7) || item["redacted"] != true { t.Fatal("redacted item leaked metadata", item) }
if legacyItem["text"] != "附件消息（当前客户端不支持查看）" { t.Fatal(legacyItem) }
if _, exists := legacyItem["attachment"]; exists { t.Fatal("legacy item leaked attachment") }
```

- [x] **2. 运行 RED**：`go test ./internal/policystore ./internal/httpserver -run '^TestFileMessage(History|PullFormat|Search)' -count=1 -v`，确认类型、投影或文本候选不满足预期。
- [x] **3. 实现同事务查询与投影**：历史 SQL 在既有事务左连接唯一附件和文件事实，无独立文件事务或下载授权。保留历史 sender／recipient 及群区间证明，最终过滤整体重建遮蔽项。search action 和跨会话读取选 textOnly SQL，匹配及最终过滤也显式判断类型，防御 SQL／内部结构误用；all 模式分页不跳过附件。旧 DTO 不含 type／caption／附件字段；typed text 仍返回 text。查询参数未知值／重复拒绝，现有 strict keys 加入 message_format。
- [x] **4. GREEN**：上述命令及 `go test ./internal/policystore ./internal/httpserver -run 'Test.*(Pull|History|MessageSearch|CrossSearch|FileMessageSearch)' -count=1 -v` 全部 PASS；既有搜索预算与游标测试也必须通过，不因新 SQL 过滤丢失时间线。
- [x] **5. 提交**：仅本任务文件；`git commit -m "feat(history): project file messages without leaking legacy payloads"`。

## Task 7：保全感知三份证据原子退役

**Files:** 新建 retention/attachment_digest.go／_test.go；修改 digest_batch.go、digest_concurrency_test.go。现有正文 Worker 无需另存／另清 caption。

**Interfaces:** `retireAttachmentFingerprints(ctx context.Context,tx pgx.Tx,tenantID string,messageIDs []string,at time.Time) ([]string,error)`，返回实际退役 file 消息 ID；processDigestBatch 在同一事务查出预期 file 子集，验证返回集合精确相等。沿用批次最多 1000 与既有候选条件／会话保全锁。

- [x] **1. 写 RED 测试**：TestFileMessageDigestRetirementAtomic、TestFileMessageDigestRetirementHold、TestFileMessageDigestRetirementRollback、TestFileMessageDigestRetirementIrreversible。断言空 caption 也被正文 Worker 正常清理；未清理／幂等未到期／有效保全不退役；三份证据同时间戳清空；注入附件 UPDATE 或批次审计失败时三份都保持；退役后回填和删除重用拒绝；保全并发以会话锁后的实际状态为准。
代表断言（TestFileMessageDigestRetirementAtomic，查询三份状态）：
```go
if messageDigest != nil || keyDigest != nil || bindingSHA != nil { t.Fatal("partial retirement") }
if messageRetired == nil || keyRetired == nil || fingerprintRetired == nil || !messageRetired.Equal(*keyRetired) || !messageRetired.Equal(*fingerprintRetired) { t.Fatal("retirement stamps diverged") }
```

- [x] **2. 运行 RED**：`go test ./internal/retention -run '^TestFileMessageDigestRetirement' -count=1 -v`，应因旧 Worker 未清空新指纹而失败，不能通过删去迁移约束规避。
- [x] **3. 实现批次接入**：复用同一 tx／候选 ID 和退役时间，messages→message_idempotency→message_attachments 更新，依 Task 1 延迟约束提交；返回集合必须是候选 file 子集且无遗漏，任一不匹配整批回滚。仍使用既有批次证据，不新增 caption／hash 副本或把文件扫描证据清理当消息退役。
- [x] **4. GREEN**：上述命令和 `go test ./internal/retention ./internal/policystore -run 'Test.*(DigestRetirement|BodyClear|LegalHold)' -count=1 -v` 全部 PASS；Task 2／3／4 的退役重试同时验证 HTTP 410／内部错误。
- [x] **5. 提交**：仅本任务文件；`git commit -m "feat(retention): retire attachment replay fingerprints atomically"`。

## Task 8：并发提交、撤销和故障注入

**Files:** 新建 file_message_concurrency_test.go；按实际失败仅修正 Task 1～7 对应实现文件，不进行无关重构。

**Interfaces:** 使用已确定 SendMessage／SendGroupMessage、filePeer(t,c) 第二连接和 PostgreSQL 锁／测试事务代理；无产品测试开关、无生产故障注入入口。

- [x] **1. 写 RED／风险测试**：TestFileMessageConcurrentSameKey、TestFileMessageConcurrentSameFile、TestFileMessageDeleteRace、TestFileMessageConfigWait、TestFileMessageMembershipWait、TestFileMessageAuditWait、TestFileMessageFaultRollback、TestFileMessageReplayRetirementRace。两个实际连接确定先后而非靠 sleep 猜时序；相同键同请求 8 次仅一 message／attachment／idempotency／Outbox／seq 增量，different key 同文件只一成功；RF3 配置降限／关停、用户冻结／任职期限、群移除、规则期限跨等待拒绝；审计与 Outbox 任意写入失败无配额残留。重放／退役读到完整旧三份或完整退役三份，不能因中间状态新建。
代表断言（TestFileMessageConcurrentSameKey，8 个实际并发调用结束后）：
```go
if newACKs != 1 || duplicateACKs != 7 { t.Fatal(newACKs, duplicateACKs) }
if messageCount != 1 || bindingCount != 1 || keyCount != 1 || outboxCount != 1 || seqAfter-seqBefore != 1 { t.Fatal("concurrent send violated one-record invariants") }
```

- [x] **2. 运行风险测试**：`go test ./internal/policystore -run '^TestFileMessage(Concurrent|DeleteRace|ConfigWait|MembershipWait|AuditWait|FaultRollback|ReplayRetirementRace)' -count=1 -v`；如发现缺陷，保存失败断言为 RED；若全部通过记录首次 PASS，不虚构 RED。
- [x] **3. 修正真实失败**：定位具体锁／快照／savepoint／时间或约束问题；最小修改到所属文件，不能放宽“只绑定一次”、资格判定或最终复核来换 PASS；对拒绝类验证群暂停／拒绝审计可保留、成功审计不保留。
- [x] **4. GREEN／race**：`go test -race ./internal/policystore ./internal/retention -run 'TestFileMessage|Test.*DigestRetirement' -count=1 -v`；所有数据库用例真实执行，无死锁泄露、数据竞争或 SKIP。
- [x] **5. 提交**：只列实际新增／修正文件；`git commit -m "test(messages): cover file send races and rollback invariants"`。

## Task 9：真实扫描→发送→通知→补拉及生产关闭

**Files:** 新建 file_message_real_integration_test.go、file_message_browser_integration_test.go、scripts/test-file-messages.sh；修改 cmd/im-api/main_test.go、testdata/file-runtime/README.md。需要浏览器断言时在新测试自己的临时 Node 脚本中完成，不改产品 Web 流程。

**Interfaces:** 测试通过真实 S3＋filescanner＋filetransfer 生成 ready，使用 HandlerWithFileMessages 显式装配，连接现有 Outbox publisher／Redis fanout／WebSocket；生产 cmd/im-api 仍默认关闭。脚本只有一个命令 `scripts/test-file-messages.sh run-all`，依赖环境缺失／不可用、扫描证明失效或选定组件 SKIP 均非零退出。

- [x] **1. 写 RED／集成测试**：TestFileMessageRealScanSendPull、TestFileMessageRealRealtime、TestFileMessageRealBrowserLegacy、TestFileMessageProductionClosed。普通 TLS OIDC 夹具映射真实 TrustedIdentity；真实上传扫描后单聊／群聊提交、通知及 typed／旧 HTTP 补拉；真实旧 Web 显示固定占位且文本发送继续有效。实际 cmd/im-api 子进程即使上传已启用或设置未识别 IM_FILE_MESSAGE_ENABLED=true，file 请求仍 503，文本仍成功。选用独特名称／caption 检查其未出现在 Redis、通知、默认补拉、搜索、审计及产品日志。
代表断言（TestFileMessageRealScanSendPull，真实扫描完成后才允许发送）：
```go
if state != "ready" || !bytes.Equal(scanSHA, sealedSHA) { t.Fatal("no trusted clean scan proof") }
if typedItem["message_type"] != "file" || attachment["download_available"] != false { t.Fatal(typedItem) }
if responseACK["message_id"] != persistedMessageID { t.Fatal("ACK differs from committed message") }
```

- [x] **2. 配置本轮受控依赖并运行**：准备专用 PG／Redis／versioned 私有 S3、qpdf、可信正向及旧配置负向 clamd。沿用固定镜像与已批准扫描限额；重新核验病毒库≤24h、实际 PID／二进制／配置／socket，并生成新的绑定 manifest。设置现有 IM_TEST_DATABASE_URL／IM_TEST_REDIS_URL／IM_TEST_S3_*／扫描及浏览器变量，私有文件中保管凭据，不能沿用已停止进程的旧就绪结论。运行 `go test ./internal/policystore ./cmd/im-api -run '^TestFileMessage(Real|Production)' -count=1 -v`，记录实际初次失败／通过，不将缺依赖 SKIP 当 RED 或通过。
- [x] **3. 实现集成夹具与脚本门禁**：脚本执行具名测试并保存 `go test -json`，检查选定测试真的出现且无 SKIP／FAIL；支持独立私有 IM_TEST_FILE_MESSAGE_OUTPUT_DIR 绝对目录。声明自建资源归属、清理与证据路径；真实扫描失败不得手写 ready 绕过。
- [x] **4. GREEN**：`scripts/test-file-messages.sh run-all` 的单聊／群聊、实时、浏览器及生产关闭全部 PASS，无 SKIP。证明检测事实只报告在授权 typed DTO，ACK 不代表送达／已读；文件下载本轮仍未实现。对受控测试对象只清理本轮归属，不放宽正式文件生命周期或删除正式绑定证据。
- [x] **5. 提交**：仅本任务代码／脚本／说明；`git commit -m "test(files): verify scanned attachments through reliable message delivery"`。

## Task 10：固定版本门禁、一次最终评审及交付

**Files:** 新建 docs/开发增量-P4-23-验收记录.md；修改本计划任务状态、已确认规格状态与中文总开发路径。评审要求的修正只触及对应产品／测试文件。

**Interfaces:** 使用 git archive 的固定产品提交、既有全量与受影响包 race；草稿 PR base 为 codex/p4-22-upload-scan-design，head 为 codex/p4-23-file-message-design。最终新鲜独立 reviewer 只评审本轮增量及对应规格／计划，无逐任务实现代理。

- [x] **1. 固定候选与完整门禁**：先提交 Task 1～9，再记录不可变 candidate SHA，以 `git archive <candidate SHA>` 解包到本轮私有验证目录；所有命令在归档目录运行且日志写独立证据目录。运行 `go test -json ./... -count=1` 和 `scripts/test-file-messages.sh run-all`；命令完整记录在验收文档，凭据不进入命令日志。正文／扫描旧资源夹具按实际测试要求准备；不重新开展无关容量／生产演练。
- [x] **2. 受影响包 race**：归档目录运行 `go test -json -race ./internal/policystore ./internal/httpserver ./internal/retention ./internal/realtime ./internal/outbox ./cmd/im-api -count=1`。分别统计顶层／子测试 PASS、FAIL、SKIP；组件必须 0 FAIL／0 SKIP，全量允许明确单列既有辅助 TestRealtimeAPIChild，实际调用方必须执行；新用例或缺依赖跳过均不接受。
- [x] **3. 一次整体独立评审与修正**：门禁通过后按 executing-plans／requesting-code-review 要求派发一次新鲜 reviewer，对比产品基线 b03a197..候选固定 SHA，要求逐条证据／Critical、Important、Minor 分类。核验报告中的发现；必要修正一次完成，以失败回归先 RED 再 GREEN，重新归档最终产品提交并执行受影响及规定最终门禁；不重复派发整体评审。无有效发现也如实记录，不能把没有检查的风险宣称排除。
- [x] **4. 文档和清理**：验收记录写最终产品 SHA、测试计数、真实依赖／浏览器证据、评审及修正、实际局限、资源清理；更新实际完成的复选框与总路径。停止本轮自建进程／容器，验证已退出，私有凭据／样本不提交；不得把准备完成或本地联调说成生产验收。纯文档提交后用 git diff 验证产品源码与被测提交一致。
- [x] **5. 推送与草稿 PR**：核对只包含本轮可交付提交后推送当前分支，以指定 base 创建草稿 PR；正文用临时 UTF-8 文件及 gh --body-file，随后 attach_artifact 关联 PR。确认远端 head 与本地一致、工作树 clean；中文最终回复链接 PR／验收文档、核心证据和 P4-24／25 未完成范围，不合并／部署。

## 规格覆盖与自检

| 规格要求 | 实施任务 |
|---|---|
| 第 1～3 节边界和可靠链路 | Task 1～4、9～10 |
| 第 4 节请求、ACK 和错误 | Task 2～5、9 |
| 第 5 节类型、来源、单次绑定及回退 | Task 1、3～4、8 |
| 第 6 节实际授权、并发和失败回滚 | Task 3～4、8 |
| 第 7 节摘要、删除后重放和退役 | Task 2～4、7～8 |
| 第 8 节 typed_v1、旧占位与整项遮蔽 | Task 6、9 |
| 第 9 节搜索、实时与审计隐私 | Task 3～6、9 |
| 第 10 节生产关闭 | Task 5、9 |
| 第 11 节真实链路、固定提交、评审和交付 | Task 8～10 |

自检完成：所有跨任务类型／签名与字段来源已定义；五个 Review Focus 均有具名测试及归属；步骤为先失败证据、最小实现、真实验证、限定提交。计划不把新测试首次通过伪记为 RED，不把环境缺失当测试通过；实际完成状态以本页复选框、隔离工作树执行台账及阶段验收记录为准。

## 审阅与执行衔接

用户已确认本计划，按既定逐项实施方式推进。此前官方daily.cvd28142超过24小时，已如实记录失败并停止；继续后官方下载／验证28143并重新绑定实际运行进程，Task9完整门禁通过。Task10固定提交20c073e全量660顶层／617子例和六包race464顶层／430子例通过，各单列一个既有辅助启动器；新附件与旧文件组件均零跳过，Linux资源门禁通过。一次最终整体评审无有效Critical／Important／Minor，资源已停止；[草稿PR #71](https://github.com/leileipei/Enterprise_IM/pull/71)已创建并关联，未合并／部署。精确race辅助入口规则及task-done交付验证器裁决见验收记录。
