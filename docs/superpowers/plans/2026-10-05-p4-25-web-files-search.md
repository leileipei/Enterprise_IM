# P4-25 Web 附件交互与文件名搜索 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. 用户已选择当前助手逐项实现，最后一次整体独立评审；不重新选择执行方式。Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 完成员工 Web 附件上传／扫描／发送／受控下载、管理员文件设置，以及有权限和到期复核的单会话／个人跨会话文件名搜索。

**Architecture:** 复用文件预约、消息幂等和下载服务，将文件授权事实加载与最终纯判断共用到名称搜索。新增独立 HTTP／JS 模块，通过实际装配能力控制客户端入口；完整业务由专用测试装配验收，生产默认关闭。统一计划保留完整附件流程的接口依赖，任务均有独立测试与提交。

**Tech Stack:** Go 1.27.1、pgx/v5、PostgreSQL、现有 S3 SDK、OIDC、原生 JavaScript、现有 Playwright／Chrome、Redis、ClamAV／qpdf；不新增产品框架或第三方产品依赖。

**Spec:** [已确认书面规格](../specs/2026-10-05-p4-25-web-files-search-design.md)，用户于 2026-10-05 回复“确认”；确认时文档提交 `43b04af`，产品基线为 P4-24 `84aa1d9`。用户于 2026-10-05 确认本实施计划，已开始 Task 1；后续任务及整体验收未完成。

## Global Constraints

- 复用已附隔离工作树 `/Users/leo.cui/.codex/worktrees/p4-04-body-cleaner/企业IM系统`，分支 `codex/p4-25-web-files-search-design`；每次任务核对 HEAD／clean，不改主目录，不夹带其他任务修改。
- 生产能力后三项始终 false，上传项沿用现有开关。生产附件发送／下载／名称搜索保持闭合；无新增发送／下载生产开关，不通过手工 URL、静态配置或环境变量绕过。typed_v1.download_available=false。
- 允许 PDF／PNG／JPEG／UTF-8 TXT；文件大小 1～26,214,400 字节，租户上限优先；单页面一个上传／待发送槽、一次下载。无预览、Range、分片／续传、OCR、正文索引、外链、预签名 URL、批量上传或跨会话复用。
- 文件名有效 UTF-8、1～255 字节；拒绝首尾空白、控制字符、NUL、路径分隔符、冒号、`.`／`..`。声明只用于预约，不能替代实测内容和扫描。
- 上传状态轮询间隔 2 秒，单次 10 秒，单窗口最多 120 秒，隐藏页面暂停；未知结果不自动 PUT、不生成第二 file ID。预约重试保持 upload_request_id 和冻结参数。
- 发送未知结果保持原 UUIDv7 client_msg_id／file_id／caption；ACK 才称“已保存”。附件与文字共享待发送槽，不存储跨刷新草稿。
- 下载完整 GET、最大 25 MiB；服务端继续不超过 60 秒、原逐段复核和限额。浏览器 65 秒总时限，仅完整校验响应创建 Blob 保存入口；未点击最多保留 60 秒，保存点击／上下文变化即释放。
- token、File、Blob、搜索结果仅页面内存；不新增 localStorage／IndexedDB／Service Worker，不写入日志、埋点或实时消息。字节已保存到用户磁盘不可撤回；浏览器取消不等于对象已删除。
- 查询去首尾空白后 2～100 Unicode 字符，Go ToLower 后字面子串匹配；只匹配 original_filename。limit 默认 20、1～50；cursor 最长 2048 字节；名称游标与文本游标类型隔离。
- 单次最多扫描 500 条 file 消息候选；跨会话每次最多 20 个个人候选会话、总计 500 条；单会话 seq 升序，跨会话会话 UUID 升序／会话内 seq 升序。
- 搜索总时限 5 秒，statement_timeout 4 秒、lock_timeout 1 秒；并发类错误至多重试 2 次且共用总时限。授权事实全部加载后取 DB 最终时间，随后仅纯判断，审计提交后返回；无对象 I/O／spool／下载会话副作用。
- 名称资格沿用 P4-24 完整下载资格，含历史及当前参与、上传者有效、双方 send_message／file_download、hard deny、正文和文件期限、ready／完整扫描来源。无管理员旁路，不把历史卡片元数据当下载授权。
- 管理编辑 expected_version CAS＋approval_reference；保留 1～3650 天适用于已有文件。cleanup_enabled 不启动默认关闭命令；延长不能恢复 pending／deleted。上传 TTL 60～3600 秒、预算 25 MiB～1 TiB。
- 新必选真实场景 0 FAIL／0 SKIP；所有规定门禁固定产品 commit 归档执行；旧辅助入口 SKIP 单列。只清理本轮可证明归属资源，凭据文件 0600、证据目录 0700。
- 用户已审阅并确认本计划；按下面任务逐项执行。M4、生产启用、客户联调、HA／容量／备份 DR 继续单独验收。

## Review Focus

- RF1（Task 6／9）：HTTP 200 已收到但流截断、Content-Length 欺骗、超长或压缩响应，用户不能保存错误内容，零 Blob 保存入口。
- RF2（Task 4／13）：未来生效 hard deny 或 TTL 在候选读取与最终检查间跨过边界，名称不能泄露，游标仍能有界前进。
- RF3（Task 7／8／13）：取消请求后服务器其实已封存／保存，不能产生第二文件或新消息 ID，切换任职／会话后不得回填。
- RF4（Task 9／13）：中文 filename*、路径／控制字符、恶意 disposition 或 CSP 阻止保存，不能注入 UI／形成异常文件名或放宽脚本来源。
- RF5（Task 10／13）：大于 JS 安全整数的十进制版本、精确额度、CAS 冲突及未知写结果，不得浮点舍入或自动覆盖。

---

## 文件职责与执行顺序

以下路径均相对隔离工作树，新增文件须配测试。任务依次执行，后面的 Interfaces 消费前面的固定定义，不先装配半成品到生产。Task 1～5 后端，6～11 Web，12～13真实链路，14整体门禁及交付。没有数据库新增表／名称副本；若确实需要迁移，先回到书面设计审阅，不能实施中偷偷扩展数据模型。

| 新增或修改区域 | 责任 | Task |
| --- | --- | --- |
| httpserver/file_capabilities.go；access/file_capabilities.go；cmd/im-api | 实际装配能力、有效身份及生产关闭 | 1 |
| policystore/file_visibility.go；file_download_authorization.go | 授权事实加载／纯判断、下载不变 | 2 |
| policystore/file_search_contracts.go、file_search_cursor.go、file_search_candidates.go、file_search.go | 独立查询、游标和单会话搜索 | 3 |
| policystore/cross_file_search.go | 个人跨会话有界搜索 | 4 |
| httpserver/file_search.go；cmd/im-api | 三路由、错误／DTO、准确路径闭合 | 5 |
| webclient/assets/file-transport.js；app.js；handler.go | 二进制认证、上下文取消、静态资产 | 6 |
| webclient/assets/file-transfer.js | 单文件预约／PUT／扫描恢复 | 7 |
| webclient/assets/file-messages.js；app.js | 幂等发送、类型化卡片 | 8 |
| webclient/assets/file-download.js | 完整二进制下载和短时保存入口；由 file-messages 调用 | 9 |
| webclient/assets/file-policy.js | 上传／保留编辑及历史 | 10 |
| webclient/assets/file-search.js；message-search.js；cross-message-search.js | 名称／文字模式、结果及打开会话 | 11 |
| policystore/web_file_*_test.go；webclient/e2e/file_*.cjs；scripts/test-web-files.sh | 专用真实装配、浏览器、严格必选门禁 | 12～13 |
| 中文验收记录、总路径、本计划 | 固定证据、一次整体评审及草稿交付 | 14 |

### 固定接口与测试约定

- Go 新公共类型放在 Task 指定的 contracts 文件；HTTP 不暴露 pgx／S3／私有授权类型。新增 error：ErrInvalidFileSearch／ErrFileSearchUnavailable。
- JS 保持当前 window.Class＋defer 资产风格；`FileContext` 为只读快照：identityKey、membershipId、identityEpoch、conversation、conversationEpoch、kind（direct／group），`sameContext(a,b)` 比较全部字段。
- `FileCapabilities` 固定四个布尔字段，按规格命名；`fileContext()` 由 app.js 提供，令牌仅经 Task 6 transport 内部 snapshot 取得，不传到 DOM、搜索模块或日志。FileContext 的 identityEpoch 显式从 app.js 当前变量提供，不以 retentionContext 的 identityKey 拆字符串推断。
- 测试代码块是待新增测试的关键断言片段，夹具用已有文件／授权 helper；真实浏览器 fixture 在 Task 12 定义。没有把此处测试断言当作已执行证据。
- 每项 RED→最小实现→GREEN→提交；故障先按 systematic-debugging 查因。每个提交只显式 git add 本任务文件，使用列出的 commit message；git diff --check 必须成功才提交。任务结束更新对应复选框。

## Task 1：实际装配能力与生产关闭

**Files:** 新建 internal/httpserver/file_capabilities.go、file_capabilities_test.go；internal/access/file_capabilities.go、file_capabilities_test.go；修改 cmd/im-api/main.go、main_test.go。

**Interfaces:**
- `httpserver.FileCapabilities{UploadEnabled,MessageSendEnabled,DownloadEnabled,FilenameSearchEnabled bool}`，JSON 使用规格四个字段。
- `access.Service.ValidateFileIdentity(ctx context.Context,id TrustedIdentity) error`；HTTP 消费相同方法的窄 interface。
- `HandlerWithFileCapabilities(next http.Handler,auth Authenticator,identity FileIdentityValidator,caps FileCapabilities)(http.Handler,error)`。

- [x] **Step 1：新增失败测试。** TestFileCapabilitiesStrictRequest／TestFileCapabilitiesCurrentIdentity／TestProductionFileCapabilitiesClosed；合法／退出任职、错 tenant、GET body、重复头、query／ForceQuery、HEAD 和缺依赖。

```text
GET validIdentity -> status=200, exact four booleans, no-store/nosniff
production(upload=true) -> upload_enabled=true; other three=false
HEAD -> 405; ? -> 400; expired membership -> 403
nil dependency -> constructor error
```

核心断言片段（变量由本项具名测试的请求记录／夹具提供）：

```go
if res.Code != 200 || res.Header().Get("Cache-Control") != "no-store" { t.Fatal(res.Code, res.Header()) }
if got.MessageSendEnabled || got.DownloadEnabled || got.FilenameSearchEnabled { t.Fatal("production capability opened") }
```

- [x] **Step 2：运行 RED。** `go test ./internal/httpserver ./internal/access ./cmd/im-api -run 'Test(FileCapabilities|ProductionFileCapabilities)' -count=1`。必须因缺失的接口／行为失败；不能把缺环境、编译器或浏览器不可用当成有效 RED。
- [x] **Step 3：最小实现。** ValidateFileIdentity 用现有身份 snapshot 和 DB 时间，不能以 token 合法代替当前任职。生产路由按实际 fileEnabled 构造，关闭能力仍可已认证查询，不向前端输出角色／对象／凭据。
- [x] **Step 4：运行 GREEN。** 重跑 Step 2 命令；预期全部具名测试 PASS，0 FAIL。真实必选场景还必须 0 SKIP。
- [x] **Step 5：提交。** 仅暂存 Files 所列的本任务变化与计划复选框，执行 `git diff --cached --check` 后提交：`git commit -m "feat(files): expose authenticated assembly capabilities"`。

## Task 2：共用文件可见性证据与最终纯判断

**Files:** 新建 internal/policystore/file_visibility.go、file_visibility_test.go；修改 file_download_authorization.go；测试 file_download_authorization_test.go、file_download_concurrency_test.go。

**Interfaces:**
- 私有 `fileVisibilityFacts` 持有加载完成的人员／策略／历史／来源／期限快照，无 DB、tx、网络 client 字段。
- `loadFileVisibilityFactsTx(ctx context.Context,tx pgx.Tx,id access.TrustedIdentity,fileID string)(fileVisibilityFacts,error)`。
- `evaluateFileVisibility(f fileVisibilityFacts,at time.Time)(files.Metadata,string,int64,error)`。
- `authorizeFileDownloadTx` 原签名保持不变，消费两函数并返回最后 DB 授权时间。

- [x] **Step 1：新增失败测试。** TestFileVisibilityFinalClock／TestFileVisibilitySourceMismatch／TestFileVisibilityNoSideEffects；复用原下载授权矩阵，覆盖未来 hard deny、上传者／身份失效、正文空说明与到期边界。

```text
now=expiresAt -> ErrNotFound; now=expiresAt-1ns -> valid
reverse hard_deny or stale uploader -> ErrNotFound
DB facts loaded then finalClock -> evaluate performs 0 SQL/object calls
search proof -> download_sessions delta=0; outbox delta=0
```

核心断言片段（变量由本项具名测试的请求记录／夹具提供）：

```go
_, _, _, err := evaluateFileVisibility(facts, expiresAt)
if !errors.Is(err, filedownload.ErrNotFound) { t.Fatal("expiry must close at boundary", err) }
if sqlCallsAfterFinalClock != 0 || objectCalls != 0 { t.Fatal("visibility check performed I/O") }
```

- [x] **Step 2：运行 RED。** `go test ./internal/policystore -run 'Test(FileVisibility|FileDownloadAuthorization|FileDownloadConcurrent|FileDownloadAuditWaitExpiry|FileRetentionConcurrentChange)' -count=1`。必须因缺失的接口／行为失败；不能把缺环境、编译器或浏览器不可用当成有效 RED。
- [x] **Step 3：最小实现。** 从现有下载授权提取事实，加载群所有匹配 hard deny 和当前参与信息后才读最终 clock_timestamp。沿用 P4-24 锁顺序及 NOWAIT／有限重试；不修改下载审计 Minor 分类或外部副作用。字段变化仍受保护锁控制，保持原拒绝语义。
- [x] **Step 4：运行 GREEN。** 重跑 Step 2 命令；预期全部具名测试 PASS，0 FAIL。真实必选场景还必须 0 SKIP。
- [x] **Step 5：提交。** 仅暂存 Files 所列的本任务变化与计划复选框，执行 `git diff --cached --check` 后提交：`git commit -m "refactor(files): share complete visibility proofs with search"`。

## Task 3：单会话名称搜索与独立游标

**Files:** 新建 internal/policystore/file_search_contracts.go、file_search_cursor.go、file_search_cursor_test.go、file_search_candidates.go、file_search.go、file_search_test.go。

**Interfaces:**
- `FileSearchMatch{ConversationID,Kind,MessageID,SenderUserID,FileID,OriginalFilename,DetectedMediaType string;Seq,ActualSizeBytes int64;ServerTime time.Time}`；`FileSearchPage{Matches []FileSearchMatch;HasMore bool;NextCursor string}`。
- `Service.SearchFileMessages(ctx context.Context,id access.TrustedIdentity,conversationID,kind,query,cursor string,limit int)(FileSearchPage,error)`，kind 仅 direct／group。
- 私有 `fileSearchBinding{Tenant,User,Membership,Conversation,Kind,Query string}`、`fileSearchPosition{Conversation,Phase string;After int64}`。
- `encodeFileSearchCursor(binding fileSearchBinding,pos fileSearchPosition)(string,error)`／`decodeFileSearchCursor(raw string,binding fileSearchBinding)(fileSearchPosition,error)`；版本 file_name_v1，复合 envelope 严格规范编码。

- [ ] **Step 1：新增失败测试。** TestFileSearchLiteral／TestFileSearchDirectAndGroup／TestFileSearchCursorBinding／TestFileSearchBoundedProgress／TestFileSearchAuditFailure。

```text
q="%_" -> literal name match only; caption/body match -> 0 matches
limit=20; 501 file candidates -> scanned<=500, has_more=true
text cursor or changed user/membership/q/kind -> ErrInvalidFileSearch
no visible match but remaining candidates -> empty page + next_cursor
audit insert fails -> no result/name released
```

核心断言片段（变量由本项具名测试的请求记录／夹具提供）：

```go
_, err := decodeFileSearchCursor(textCursor, binding)
if !errors.Is(err, ErrInvalidFileSearch) { t.Fatal("accepted text cursor") }
if len(page.Matches) != 0 || !page.HasMore || page.NextCursor == "" { t.Fatal("empty page lost progress") }
```

- [ ] **Step 2：运行 RED。** `go test ./internal/policystore -run '^TestFileSearch' -count=1`。必须因缺失的接口／行为失败；不能把缺环境、编译器或浏览器不可用当成有效 RED。
- [ ] **Step 3：最小实现。** 复用 NormalizeMessageSearchQuery；按 seq 读取 file 消息候选，不以文件名 SQL LIKE 提前过滤。Task 2 事实按稳定身份／会话／文件锁序加载，最终 DB 时间后纯判断／名称匹配，最小审计成功提交才返回；保留消息扫描进度，无隐藏数。定义 ErrInvalidFileSearch／ErrFileSearchUnavailable。
- [ ] **Step 4：运行 GREEN。** 重跑 Step 2 命令；预期全部具名测试 PASS，0 FAIL。真实必选场景还必须 0 SKIP。
- [ ] **Step 5：提交。** 仅暂存 Files 所列的本任务变化与计划复选框，执行 `git diff --cached --check` 后提交：`git commit -m "feat(search): add authorized conversation filename search"`。

## Task 4：个人跨会话有界名称搜索

**Files:** 新建 internal/policystore/cross_file_search.go、cross_file_search_test.go、file_search_concurrency_test.go；复用现有 cross_message_search_candidates.go 的历史候选，必要共用纯 helper 不改文本合约。

**Interfaces:**
- `Service.SearchAllFileMessages(ctx context.Context,id access.TrustedIdentity,query,kind,cursor string,limit int)(FileSearchPage,error)`；kind=all／direct／group。
- 消费 Task 2／3 proof、match／page 与独立 cursor；跨会话 binding.Conversation 为空，用 position within／after 前进。

- [ ] **Step 1：新增失败测试。** TestCrossFileSearchPersonalScope／TestCrossFileSearchBudget／TestCrossFileSearchFinalBoundary／TestCrossFileSearchCursorProgress；两连接锁阻塞后跨过未来 deny／TTL，最终无名称。

```text
21 conversations -> visited<=20; total file candidates<=500
results ordered by conversation UUID then seq; not global timestamp
final deny/expiry removes staged match -> no leaked name, cursor progresses
return limit=50 -> matches<=50; no unprocessed candidate skipped
5s total exhausted -> ErrFileSearchUnavailable, no partial page
```

核心断言片段（变量由本项具名测试的请求记录／夹具提供）：

```go
if visitedConversations > 20 || scannedFileMessages > 500 { t.Fatal("search budget exceeded") }
if len(page.Matches) != 0 { t.Fatal("name released after final deny/expiry") }
```

- [ ] **Step 2：运行 RED。** `go test ./internal/policystore -run 'Test(CrossFileSearch|FileSearchConcurrency)' -count=1`。必须因缺失的接口／行为失败；不能把缺环境、编译器或浏览器不可用当成有效 RED。
- [ ] **Step 3：最小实现。** 使用个人候选集合，单事务有界加载全部待返回资格；不得 per-file 起独立已提交事务拼结果。身份／来源父表按 UUID 排序取得锁，再多会话按 UUID 锁，沿用后续策略／file／attachment 锁序，无法遵守时 NOWAIT 重试。完成所有授权 I/O 后统一最后 DB clock、纯过滤、file_name_search_all 审计并提交。预先按上述顺序取得整批来源父表／会话／文件保护锁，后续逐文件事实加载只能重取已持有的锁；不能在第二会话反向取得新父表锁。复用 5／4／1 秒与总计最多三次尝试。
- [ ] **Step 4：运行 GREEN。** 重跑 Step 2 命令；预期全部具名测试 PASS，0 FAIL。真实必选场景还必须 0 SKIP。
- [ ] **Step 5：提交。** 仅暂存 Files 所列的本任务变化与计划复选框，执行 `git diff --cached --check` 后提交：`git commit -m "feat(search): add bounded personal cross-conversation filenames"`。

## Task 5：名称搜索 HTTP 与准确闭合装配

**Files:** 新建 internal/httpserver/file_search.go、file_search_test.go；修改 cmd/im-api/main.go、main_test.go；新增 internal/policystore/file_search_api_integration_test.go。

**Interfaces:**
- `FileSearchService` 接口含 Task 3 SearchFileMessages 和 Task 4 SearchAllFileMessages 的完整签名。
- `HandlerWithFileSearch(next http.Handler,auth Authenticator,svc FileSearchService)(http.Handler,error)`。
- `HandlerWithClosedFileSearch(next http.Handler,auth Authenticator) http.Handler`，准确匹配三路由，对已认证合法 GET 返回 503 file_search_unavailable。

- [ ] **Step 1：新增失败测试。** TestFileSearchHTTPStrict／TestFileSearchRoutePrecedence／TestFileSearchDTOPrivacy／TestFileSearchProductionClosed；真实服务对应状态与 API DTO。

```text
/files/search -> search route, never GetOwnFile("search")
q missing/duplicate or body/query unknown -> 400 invalid_file_search
DTO exact spec fields; seq/size are canonical decimal strings
HEAD -> 405; valid closed GET -> 503 file_search_unavailable
no caption/token/SHA/object path; all responses no-store/nosniff
```

核心断言片段（变量由本项具名测试的请求记录／夹具提供）：

```go
if ownFileCalls != 0 || searchCalls != 1 { t.Fatal("file route shadowed search") }
if res.Code != 503 || !strings.Contains(res.Body.String(), `"file_search_unavailable"`) { t.Fatal(res.Code, res.Body.String()) }
```

- [ ] **Step 2：运行 RED。** `go test ./internal/httpserver ./internal/policystore ./cmd/im-api -run 'TestFileSearch(HTTP|Route|DTO|Production|API)' -count=1`。必须因缺失的接口／行为失败；不能把缺环境、编译器或浏览器不可用当成有效 RED。
- [ ] **Step 3：最小实现。** parseFileSearchQuery(r *http.Request,cross bool)(q,kind,cursor string,limit int,err error) 严格参数；路径 kind 从 conversations／groups 得出；专用 wrapper 在 files/{id} 之前。生产仅安装 closed wrapper，无 filename enable 环境变量；测试使用完整 handler。
- [ ] **Step 4：运行 GREEN。** 重跑 Step 2 命令；预期全部具名测试 PASS，0 FAIL。真实必选场景还必须 0 SKIP。
- [ ] **Step 5：提交。** 仅暂存 Files 所列的本任务变化与计划复选框，执行 `git diff --cached --check` 后提交：`git commit -m "feat(search): freeze filename HTTP contracts and production closure"`。

## Task 6：Web 文件传输认证与上下文取消

**Files:** 新建 internal/webclient/assets/file-transport.js、internal/webclient/e2e/file_transport_unit.cjs、internal/webclient/file_transport_test.go；修改 assets/app.js、index.html、handler.go、handler_test.go。

**Interfaces:**
- `window.FileTransport` constructor({snapshot,onHTTPError})；snapshot 返回 FileContext 加 token／tokenExpiresAt，只在闭包使用。
- `putFile(fileID:string,file:File,signal:AbortSignal):Promise<object>`；`readDownload(fileID:string,signal:AbortSignal):Promise<{filename:string,blob:Blob}>`；`contextChanged():void`。
- app.js 提供 fileContext():FileContext，与所有文件模块共享；onHTTPError 保留当前 401退出／403任职清除规则。

- [ ] **Step 1：新增失败测试。** TestWebFileTransport；Node harness 中 fileTransportIdentityEpoch、fileTransportTruncatedBody、fileTransportHeaders。错误响应不在异常字符串输出文件内容。

```text
fetch options: credentials="omit", cache="no-store", redirect="error"
identity changes before/after stream read -> AbortError/stale, no Blob result
200 length<declared or >declared or encoding present -> reject, no save URL
65s timeout or token expiry -> abort; every controller unregistered on finish
static new JS GET -> 200 correct MIME, existing CSP unchanged
```

核心断言片段（变量由本项具名测试的请求记录／夹具提供）：

```javascript
assert.equal(fetchOptions.credentials, "omit");
assert.equal(fetchOptions.redirect, "error");
await assert.rejects(transport.readDownload(fileID, controller.signal));
assert.equal(saveURLCount, 0);
```

- [ ] **Step 2：运行 RED。** `go test ./internal/webclient -run TestWebFileTransport -count=1`。必须因缺失的接口／行为失败；不能把缺环境、编译器或浏览器不可用当成有效 RED。
- [ ] **Step 3：最小实现。** JSON API 继续现有 request；二进制函数捕获原身份／token／会话快照，登记 controller，逐块校验 Content-Length 和最大 25 MiB。认证有效期定时中断。所有 File／Blob 依赖只在模块内，不建立窗口缓存。静态路由 allowlist 加入新资产，defer 排在 app.js 前。新增 Go 测试用 IM_TEST_BROWSER_NODE 启动 Node 测试脚本，Task 14 环境必须配置，不能静默跳过。
- [ ] **Step 4：运行 GREEN。** 重跑 Step 2 命令；预期全部具名测试 PASS，0 FAIL。真实必选场景还必须 0 SKIP。
- [ ] **Step 5：提交。** 仅暂存 Files 所列的本任务变化与计划复选框，执行 `git diff --cached --check` 后提交：`git commit -m "feat(web): add bounded authenticated file transport"`。

## Task 7：上传、扫描状态与未知结果核对

**Files:** 新建 internal/webclient/assets/file-transfer.js、e2e/file_transfer_unit.cjs、file_transfer_test.go；修改 assets/index.html、style.css、app.js、handler.go。

**Interfaces:**
- `window.FileTransfer` constructor({request,transport,context,onReady,onClear})。
- `select(file:File):Promise<void>`；`upload():Promise<void>`；`queryStatus():Promise<void>`；`retryOriginal():Promise<void>`；`contextChanged():void`。
- onReady({fileID:string,context:FileContext}) 只在当前 ready 响应；onClear() 撤回待发送可用状态，不删除服务器对象。

- [ ] **Step 1：新增失败测试。** TestWebFileTransfer；fileTransferReserveReplay／fileTransferUnknownPut／fileTransferPollWindow／fileTransferRejectScan／fileTransferHiddenPage。

```text
UTF8 filename 256 bytes or size=26214401 -> reject before POST
reserve timeout -> retry same upload_request_id/name/MIME/size
unknown PUT -> no automatic PUT; query allocated not treated as absence
poll: interval=2000ms, per request=10000ms, window<=120000ms, concurrency=1
rejected/scan_failed/pending/deleted -> onReady never called
hidden page -> no new poll; switched context -> late ready ignored
```

核心断言片段（变量由本项具名测试的请求记录／夹具提供）：

```javascript
assert.deepEqual(reserveRequests[0].body, reserveRequests[1].body);
assert.equal(putRequests.length, 1); // Unknown PUT: status query must not resend.
assert.equal(activePollsMaximum, 1);
assert.equal(readyEvents.length, 0); // A late ready response from old context.
```

- [ ] **Step 2：运行 RED。** `go test ./internal/webclient -run TestWebFileTransfer -count=1`。必须因缺失的接口／行为失败；不能把缺环境、编译器或浏览器不可用当成有效 RED。
- [ ] **Step 3：最小实现。** 冻结单 File 来源。类型声明与规格映射完全一致，2秒轮询仅串行且有界；PUT 只用原 file ID／File，手动重试接受服务器 busy／recovery／expired 结论。file upload controls 要求三项能力及租户策略／当前可发送共同满足。取消与刷新释放内存，明确提示服务器结果可能已完成。
- [ ] **Step 4：运行 GREEN。** 重跑 Step 2 命令；预期全部具名测试 PASS，0 FAIL。真实必选场景还必须 0 SKIP。
- [ ] **Step 5：提交。** 仅暂存 Files 所列的本任务变化与计划复选框，执行 `git diff --cached --check` 后提交：`git commit -m "feat(web): add file upload and scan recovery flow"`。

## Task 8：附件发送幂等与类型化补拉卡片

**Files:** 新建 internal/webclient/assets/file-messages.js、e2e/file_messages_unit.cjs、file_messages_test.go；修改 assets/app.js、index.html、style.css、handler.go。

**Interfaces:**
- `window.FileMessages` constructor({request,context,uuidV7,canSend,onPending,onACK})。
- `attach(ready:{fileID:string,context:FileContext}):void`；`send(caption:string):Promise<void>`；`retry():Promise<void>`；`contextChanged():void`；`render(message:object):HTMLElement`。
- onPending(boolean) 占用统一文字／附件发送槽；onACK(ack) 只消费有效服务端 ACK，随后补拉，不自行伪造 file message。

- [ ] **Step 1：新增失败测试。** TestWebFileMessages；fileMessageUnknownRetry／fileMessageSharedPendingSlot／fileMessageTypedRedaction／fileMessageLegacyFallback。

```text
unknown send followed retry -> deepEqual(payload1,payload2), same client_msg_id
caption UTF8 byte count=16384 valid; 16385 or NUL reject
text pending -> cannot attach; file pending -> text send disabled
redacted file with forged name -> DOM has no filename/caption
download_available remains false; card never exposes object/token/SHA
ACK -> label="已保存"; unknown/error -> never claim saved
```

核心断言片段（变量由本项具名测试的请求记录／夹具提供）：

```javascript
assert.deepEqual(sendRequests[0].body, sendRequests[1].body);
assert.equal(savedLabels.length, 0); // Both replies were unknown, no ACK.
assert.equal(redactedCard.textContent.includes(privateFilename), false);
```

- [ ] **Step 2：运行 RED。** `go test ./internal/webclient -run TestWebFileMessages -count=1`。必须因缺失的接口／行为失败；不能把缺环境、编译器或浏览器不可用当成有效 RED。
- [ ] **Step 3：最小实现。** 发送路径按 context.kind 选择 conversations／groups；冻结 caption（沿用 P4-23 16,384 字节及 UTF-8规则）和一次 UUIDv7，未知不能用新 ID 编辑重发。typed_v1 可用时参与既有消息 schema校验／seq合并；旧默认能力显示 legacy 占位。file card 仅用 textContent，available 缺失或异常按不可用。
- [ ] **Step 4：运行 GREEN。** 重跑 Step 2 命令；预期全部具名测试 PASS，0 FAIL。真实必选场景还必须 0 SKIP。
- [ ] **Step 5：提交。** 仅暂存 Files 所列的本任务变化与计划复选框，执行 `git diff --cached --check` 后提交：`git commit -m "feat(web): send idempotent attachment messages and render typed cards"`。

## Task 9：完整下载与短时 Blob 保存

**Files:** 新建 internal/webclient/assets/file-download.js、e2e/file_download_unit.cjs、file_download_test.go；修改 assets/file-messages.js、index.html、handler.go；必要仅调用点修改 app.js。

**Interfaces:**
- `window.FileDownload` constructor({transport,context,capabilities,saveContainer})。
- `request(fileID:string):Promise<void>`；`save():void`；`contextChanged():void`。
- Task 8 卡片可用＋capabilities.download_enabled 时调用 request；不依赖保留为 false 的 download_available 判断实际授权。

- [ ] **Step 1：新增失败测试。** TestWebFileDownload；fileDownloadDispositionSafety／fileDownloadNoPartialSave／fileDownloadBlobLifetime／fileDownloadOneAtTime。

```text
filename*=UTF-8 Chinese valid -> parsed safe filename
path/CRLF/malformed encoding/duplicate ambiguous disposition -> reject
truncated/oversized/JSON/redirect response -> 0 save links, 0 createObjectURL
complete response -> one save link; never auto-save/preview
after click OR 60000ms OR context change -> URL revoked and link removed
concurrent second request -> refused; client timeout=65000ms
```

核心断言片段（变量由本项具名测试的请求记录／夹具提供）：

```javascript
await assert.rejects(download.request(brokenFileID));
assert.equal(saveContainer.querySelectorAll("a").length, 0);
assert.equal(createdObjectURLs.length, 0);
assert.deepEqual(revokedObjectURLs, createdObjectURLsAfterCompleteRead);
```

- [ ] **Step 2：运行 RED。** `go test ./internal/webclient -run TestWebFileDownload -count=1`。必须因缺失的接口／行为失败；不能把缺环境、编译器或浏览器不可用当成有效 RED。
- [ ] **Step 3：最小实现。** Transport 完整返回后再次确认 FileContext 再创建临时 Blob URL，用户显式保存；controller／对象 URL／定时器全部按上下文取消。解析 disposition 仅接收安全明确名称，CSP 维持原限制；浏览器不支持保存则显示失败，不改为导航 token URL 或预览。
- [ ] **Step 4：运行 GREEN。** 重跑 Step 2 命令；预期全部具名测试 PASS，0 FAIL。真实必选场景还必须 0 SKIP。
- [ ] **Step 5：提交。** 仅暂存 Files 所列的本任务变化与计划复选框，执行 `git diff --cached --check` 后提交：`git commit -m "feat(web): add complete verified download save lifecycle"`。

## Task 10：管理员文件设置与变更历史

**Files:** 新建 internal/webclient/assets/file-policy.js、e2e/file_policy_unit.cjs、file_policy_test.go；修改 assets/index.html、style.css、app.js、handler.go。

**Interfaces:**
- `window.FilePolicy` constructor({request,context,canOpen})；`open(kind:"upload"|"retention"):Promise<void>`、`save():Promise<void>`、`loadHistory(reset:boolean):Promise<void>`、`contextChanged():void`。
- 消费现有管理 upload／retention API，版本／额度按 BigInt／规范字符串处理，不转 Number。

- [ ] **Step 1：新增失败测试。** TestWebFilePolicy；filePolicyExactDecimal／filePolicyCASConflict／filePolicyUnknownResult／filePolicyIdentitySwitch／filePolicyHistoryProgress。

```text
expected_version="9007199254740993" -> request exact same string
size decimal fractional byte conversion -> reject, never round
409 -> no auto PUT; show reload/review action
network unknown -> reread version/history before further edit; no auto write
missing approval_reference -> no PUT; cleanup true never launches command
retention=0/3651; uploadTTL=59/3601; budget>1099511627776 -> reject
```

核心断言片段（变量由本项具名测试的请求记录／夹具提供）：

```javascript
assert.equal(submitted.expected_version, "9007199254740993");
assert.equal(writeRequestsAfter409, 0);
assert.equal(writeRequestsAfterUnknownUntilReadback, 0);
```

- [ ] **Step 2：运行 RED。** `go test ./internal/webclient -run TestWebFilePolicy -count=1`。必须因缺失的接口／行为失败；不能把缺环境、编译器或浏览器不可用当成有效 RED。
- [ ] **Step 3：最小实现。** 两类设置与历史共用只读 context，不复制员工读取策略权限。提交前展示完整新旧值、版本及批准引用；CAS冲突／未知状态禁止直接覆盖。历史手动20条分页和重复游标保护；明确已有文件期限适用、pending不可恢复和清理命令独立关闭。
- [ ] **Step 4：运行 GREEN。** 重跑 Step 2 命令；预期全部具名测试 PASS，0 FAIL。真实必选场景还必须 0 SKIP。
- [ ] **Step 5：提交。** 仅暂存 Files 所列的本任务变化与计划复选框，执行 `git diff --cached --check` 后提交：`git commit -m "feat(web): add tenant file policy settings and history"`。

## Task 11：名称搜索模式与结果卡片

**Files:** 新建 internal/webclient/assets/file-search.js、e2e/file_search_unit.cjs、file_search_test.go；修改 assets/message-search.js、cross-message-search.js、index.html、style.css、app.js、handler.go。

**Interfaces:**
- `window.FileSearch` constructor({request,context,scope:"conversation"|"all",download,openConversation})；`load(reset:boolean):Promise<void>`、`contextChanged():void`、`clear():void`。
- openConversation(conversationID:string,kind:"direct"|"group"):Promise<void> 复用现有授权列表与补拉。
- 单／跨文本类增加 setMode(mode:"text"|"file"):void，文字分支原 request／validate 不变，文件分支转交独立实例。

- [ ] **Step 1：新增失败测试。** TestWebFileSearch；fileSearchModeIsolation／fileSearchSchema／fileSearchEmptyPage／fileSearchContextLateResult／fileSearchUnicodeLiteral。

```text
change mode -> previous request aborted, records/cursor cleared
q="%_" -> encoded literal; 1 or 101 chars rejected; no content/caption matching
empty matches + next_cursor -> show "本页无结果，可继续", no automatic next fetch
page matches>20, invalid size/id/kind, repeated cursor -> protocol failure
identity/conversation/query changed -> late page never appended
text mode -> original /messages/search and original DTO exactly
```

核心断言片段（变量由本项具名测试的请求记录／夹具提供）：

```javascript
assert.equal(requestsAfterEmptyPage, 1);
assert.equal(nextButton.hidden, false);
assert.equal(recordsAfterContextChange.length, 0);
assert.equal(textRequestPath, originalTextSearchPath);
```

- [ ] **Step 2：运行 RED。** `go test ./internal/webclient -run TestWebFileSearch -count=1`。必须因缺失的接口／行为失败；不能把缺环境、编译器或浏览器不可用当成有效 RED。
- [ ] **Step 3：最小实现。** fileName 查询规范化与 Go simple lowercase 对齐，复用当前文本模块 Unicode trim/lower 语义但使用独立游标。单次15秒请求，结果只驻内存，手动翻页、严格顺序／去重验证；打开会话不直接注入历史，名称卡片调用相同下载实例。
- [ ] **Step 4：运行 GREEN。** 重跑 Step 2 命令；预期全部具名测试 PASS，0 FAIL。真实必选场景还必须 0 SKIP。
- [ ] **Step 5：提交。** 仅暂存 Files 所列的本任务变化与计划复选框，执行 `git diff --cached --check` 后提交：`git commit -m "feat(web): add conversation and personal filename search modes"`。

## Task 12：专用真实装配与附件完整浏览器链路

**Files:** 新建 internal/policystore/web_file_test_helpers_test.go、web_file_real_integration_test.go；internal/webclient/e2e/file_lifecycle.cjs、file_settings.cjs；scripts/test-web-files.sh；testdata/file-runtime/web-files.md。

**Interfaces:**
- `newWebFileFixture(t *testing.T) *webFileFixture`（外部 policystore_test）；持有真实 PostgreSQL repo、TLS baseURL、OIDC、版本化S3、Redis／worker及私有日志路径；复用原 realFileMessageFixture 资源／凭据读取，但单独装配完整测试 handler＋四项能力。
- 浏览器进程使用 IM_TEST_BROWSER_NODE、现有 playwright／Chrome，正常 PKCE登录；每脚本输出最小 JSON 布尔证据，不输出 token、名称或字节。
- scripts/test-web-files.sh run-all：核对所需环境，按具名 required 集合读取 go test -json，缺测试／FAIL／SKIP任一则失败。

- [ ] **Step 1：新增失败测试。** TestWebFileRealLifecycle／TestWebFileRealRejectedScan／TestWebFileRealSettings／TestWebFileRealProductionClosed。

```text
real PDF/PNG/JPEG/TXT: browser reserve->PUT->actual scan->send->pull->save
saved bytes equal source, Chinese safe name; direct and group both pass
retry unknown same request -> 1 attachment/message/Outbox and no duplicate seq
EICAR/damaged/encrypted/spoofed/too large -> cannot send
CAS settings/history real DB; current policy applied to old ready file
actual production binary: final three caps=false; send/download/search closed
CSP unchanged, browser explicit save works; two PKCE exchanges on reload
```

核心断言片段（变量由本项具名测试的请求记录／夹具提供）：

```javascript
assert.equal(sourceBytes.equals(await fs.readFile(savedPath)), true);
assert.equal(await page.getByText("已保存", {exact:true}).count(), 1);
assert.equal(typedAttachment.original_filename, expectedFilename);
```

- [ ] **Step 2：运行 RED。** `scripts/test-web-files.sh run-all`。必须因缺失的接口／行为失败；不能把缺环境、编译器或浏览器不可用当成有效 RED。
- [ ] **Step 3：最小实现。** 测试装配仅在 _test.go 内，不在产品 CLI 加运行测试模式。执行本任务前只读检查 Docker／Chrome／Node／扫描工具与原门禁所需版本；为 P4-25 新建带 codex.plan=p4-25 标签的专用 PG／S3／Redis 资源与 scanner PID 记录，固定版本，复制角色最小权限配置到私有env，不直接借用或更改 P4-24 测试数据。缺必需运行能力明确报告，不能用模拟替代。Go helper 提供真实 ResponseController 可用 TLS监听和真实 OIDC tokenexpiry，不以 auth mock 或扫描mock代替验收。脚本第一版 required 包含本任务四测试；Task13扩展并精确限定新增测试 run regex。每个必选测试缺环境 t.Fatal；私有输出0700／0600及计划标记资源，子进程超时主动退出。
- [ ] **Step 4：运行 GREEN。** 重跑 Step 2 命令；预期全部具名测试 PASS，0 FAIL。真实必选场景还必须 0 SKIP。
- [ ] **Step 5：提交。** 仅暂存 Files 所列的本任务变化与计划复选框，执行 `git diff --cached --check` 后提交：`git commit -m "integration(files): exercise complete real browser attachment lifecycle"`。

## Task 13：撤权、未知结果、分页和浏览器上下文真实门禁

**Files:** 新建 internal/policystore/web_file_security_integration_test.go、web_file_search_integration_test.go；internal/webclient/e2e/file_search.cjs、file_context.cjs、file_download_faults.cjs；修改 scripts/test-web-files.sh、web_file_test_helpers_test.go。

**Interfaces:**
- 消费 Task12 newWebFileFixture 和既有文件／租约／保全测试夹具。
- 新 required 测试：TestWebFileRealSearch、TestWebFileRealSearchFinalBoundary、TestWebFileRealContextIsolation、TestWebFileRealUnknownUploadSend、TestWebFileRealDownloadFaults、TestWebFileRealRevocation、TestWebFileRealPolicyConflict。最终 required 共11个顶层测试。

- [ ] **Step 1：新增失败测试。** 上述七测试覆盖 RF1～5；使用可控真实客户端／代理故障及 DB 双连接改变状态，不用任意 sleep 猜并发时点。

```text
candidate paused -> scheduled hard deny/TTL becomes effective -> final 0 names
500/20 scan bounds measured, no hidden-match count, cursor resumes correctly
cross tenant/stale uploader/leave-rejoin/source seal/pending/deleted -> no name
transport read abort/length lie/redirect -> no save link even after 200
context switch at reserve/PUT/scan/ACK/search/read/Blob -> 0 late UI refill
unknown already sealed/saved -> original request replay, no second file/message
slow stream token/policy expiry -> measured stop; already saved file not claimed removed
CAS version above 2^53 and unknown write -> exact new/readback version, no overwrite
```

核心断言片段（变量由本项具名测试的请求记录／夹具提供）：

```javascript
assert.equal(await page.locator("[data-file-save]").count(), 0);
assert.equal(await page.getByText(privateOldContextName, {exact:true}).count(), 0);
assert.equal(observedSecondFileReservations, 0);
```

- [ ] **Step 2：运行 RED。** `scripts/test-web-files.sh run-all`。必须因缺失的接口／行为失败；不能把缺环境、编译器或浏览器不可用当成有效 RED。
- [ ] **Step 3：最小实现。** 将缺失测试纳入脚本required，11个必选均0FAIL／0SKIP。JS unit模拟负责恶意disposition／长度等异常协议，真实浏览器故障代理验证客户端不保存部分流；真实production旧浏览器门禁另原样运行。检查所有私有日志不含关键名称／caption／q／token，查询日志只写path不含query。
- [ ] **Step 4：运行 GREEN。** 重跑 Step 2 命令；预期全部具名测试 PASS，0 FAIL。真实必选场景还必须 0 SKIP。
- [ ] **Step 5：提交。** 仅暂存 Files 所列的本任务变化与计划复选框，执行 `git diff --cached --check` 后提交：`git commit -m "integration(files): prove revocation search and context isolation"`。

## Task 14：固定源码门禁、一次整体评审与草稿交付

**Files:** 新建 docs/开发增量-P4-25-验收记录.md；修改本计划、docs/企业IM-开发计划与实施路径-v0.1.md；仅评审发现的必要修正可改对应产品／测试文件。

**Interfaces:** 消费 Task 1～13 交付；产品源码 SHA 固定并通过 `git archive <SHA>` 创建独立验证目录。输出中文门禁结论、一次 reviewer 报告／逐项处置、私有证据和草稿 PR；不是生产验收。

- [ ] **Step 1：冻结产品提交与任务覆盖。** 确认13项提交／GREEN证据、工作树干净，记录完整 SHA；生成独立归档，带计划标记的依赖与私有env只对该归档门禁提供。产物不包含凭据、数据库转储、完整文件或token。
- [ ] **Step 2：运行规定完整门禁。** 所有命令在同一归档源码及真实依赖环境运行，JSON日志0600。原脚本时限保持，不用包装器延长已有门禁。

```bash
go test -json -timeout=30m ./... -count=1
go test -json -race -timeout=30m ./internal/policystore ./internal/access ./internal/httpserver ./internal/filedownload ./internal/filetransfer ./internal/realtime ./internal/outbox ./internal/webclient ./cmd/im-api -count=1
scripts/test-web-files.sh run-all
scripts/test-file-download-retention.sh run-all
scripts/test-file-messages.sh run-all
scripts/test-file-runtime.sh run-all
```

预期全部命令退出0；新11必选真实测试 PASS、0FAIL／0SKIP；JSON计数分顶层／子例，旧辅助 TestRealtimeAPIChild 的SKIP单列并检查真实调用测试PASS。RF及上下文单元测试在带Node环境的全仓中不得SKIP。记录测得时限／扫描量，不能用回归通过声称HA／容量达标。

- [ ] **Step 3：最后一次整体独立评审。** 按执行技能用一名新 reviewer 检查书面规格、全部代码diff、门禁证据及RF1～5，列出Critical／Important／Minor。逐项验证并一轮RED→GREEN修正必要项；需要改产品即重新固定新SHA并重跑受影响及规定门禁。不再派新的实施agent或第二轮reviewer。意见不成立须用代码／证据解释；影响设计范围则停止并回到规格审阅。
- [ ] **Step 4：汇总并退出本轮资源。** 中文记录逐项必选和回归计数／具体SKIP、默认关闭、浏览器实际保存及撤权测量、未验收能力；证据去敏后0700归档。按PID／label／源目录／版本确认归属，只停本轮进程与容器、清理本轮scratch；数据保留和对象删除分别描述，不操作其他计划或客户资源。随后重检working tree、diff及日志。
- [ ] **Step 5：提交并交付。** 仅提交中文记录／本计划／总路径，`git diff --cached --check` 后 `git commit -m "docs(files): record P4-25 immutable acceptance and delivery"`；推送P4-25分支，创建草稿PR，base=`codex/p4-24-file-download-retention-design`，正文从文件传入。成功创建必须调用 attach_artifact。不合并／部署。核对最终HEAD、PR head及base；若只补文档，明确最终HEAD相对测试SHA无产品差异。

## 规格覆盖与计划自检

| 书面规格 | 本计划 |
| --- | --- |
| §1～2 目标、兼容和边界 | Global Constraints、Task14 |
| §3 模块及能力 | Task1、5、6，静态asset allowlist在各页面任务更新 |
| §4 预约、上传、轮询、未知核对 | Task7、12～13 |
| §5 幂等发送与卡片 | Task8、12～13 |
| §5 下载与短时保存 | Task6、9、12～13 |
| §6 查询合约／权限／期限／分页／审计 | Task2～5、11、13 |
| §7 配置编辑与历史 | Task10、12～13 |
| §8 安全、兼容、取消、默认关闭 | Task1、5～11、13～14 |
| §9 真实门禁／证据／交付 | Task12～14 |

自检：各Task都有固定Files／Interfaces、具名失败断言、RED／GREEN命令和提交；跨模块签名一致，Task2输出无副作用纯判定，Task3输出的Match／Page被Task4／5消费；四个能力名称与JS上下文一致；Review Focus五项均有归属。file-download.js是§3的file-messages下载职责的内部拆分，不扩大功能。无名称数据副本／新增生产开关，浏览器真实夹具不进入产品装配。没有把14项复选框或拟运行门禁写为已完成。

**交接：** 用户已确认书面规格、本实施计划和当前助手执行方式；当前助手使用 executing-plans 自 Task 1 逐项实现。任务状态以复选框和本计划专用台账为准，完整 P4-25 验收与交付尚未完成。
