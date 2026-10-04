# P4-24 授权下载、文件保留与保全感知清理 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 交付每次及在途重新授权的完整文件下载、租户文件保留配置，以及不越过会话法务保全的精确版本清理和未知结果恢复。

**Architecture:** 短数据库事务保存授权与持久化操作事实；对象读取、校验、流输出和固定版本删除在事务外执行。下载终结队列与逐版本删除承诺分别管理审计缺口和不可逆副作用，复用现有身份、消息历史、文件状态及保全框架。统一计划是因为下载在途义务与删除／保全共享文件终态；Web交互和生产启用继续分开。

**Tech Stack:** Go 1.27.1、pgx/v5、PostgreSQL、现有 AWS S3 SDK、OIDC、私有版本化对象存储；实际验证沿用 Redis、Chrome、ClamAV／qpdf 工具。无新增产品依赖。

**Spec:** [已确认规格](../specs/2026-10-04-p4-24-file-download-retention-design.md)，用户于2026-10-04以“继续”确认；确认前文档提交 `f51e53393f0f8c412511f8457ca5b95c8e695cb0`。产品基线 `57a7812dfaae11b2c62e8c2df2782f94fbbcad43`。用户已确认本实施计划；Task 1～13及一次整体评审必要修正已提交，Task 14规定门禁、资源退出已完成，草稿PR发布按实际进度记录。

## Global Constraints

- 执行方式已选：当前助手用 executing-plans 逐项实现；仅最后一次整体独立评审。用户已确认审阅及执行，不重新选择执行方式。
- 复用隔离工作树 `/Users/leo.cui/.codex/worktrees/p4-04-body-cleaner/企业IM系统`、分支 `codex/p4-24-file-download-retention-design`。每次实施前核对 HEAD／clean；不改主目录或夹带其他修改。
- 完整GET、应用代理；无预签名直连、Range、HEAD、预览、断点续传、条件GET、查询参数或请求体；不返回206／304。
- 默认 file_retention_days=365，范围1～3650；期限为绑定消息 accepted_at＋当前天数×24小时。当前配置适用于全部文件；正文期限和文件期限独立，任一逻辑不可见即拒绝。
- 延长期限仅可重新开放尚ready、未delete_pending且正文仍可见的文件；delete_pending／deleted、已清理正文不可恢复。保全不延长逻辑期限或改变扫描拒绝。
- cleanup_enabled=false默认；集团管理员以expected_version CAS及审批引用修改，不可变历史。暂停清理不暂停期限，关闭上传不自动删除。
- 下载要求当前身份／原参与任职、历史区间覆盖seq、原上传任职有效、会话active及双方当前send_message和file_download允许；历史hard_deny仍优先。跨组织必须有file_download独立显式允许，管理员无旁路。
- 下载前完整核验固定版本长度和SHA与关联／封存／scan三方一致，最大25MiB；无效内容零字节。原文件名不能形成服务器路径。
- 节点最多4个下载；跨节点同tenant／user最多2个不同文件；同tenant／user／file仅一个未结算会话。其他用户独立检查。限额从准备至终结均计入，审计缺口对应文件闸门保持关闭。
- 全流程最多60秒；每次继续输出前检查，权限复核间隔至多1秒；阻塞写截止不晚于下一复核、token到期或总截止。其他API保留10秒WriteTimeout。不支持ResponseController deadline则503。
- download_authorized与授权阶段原子提交；completed仅表示writer接受完整字节。终结事实和事件队列同事务，专用机器身份登记，不冒充原请求者；未知结果不能推断completed。
- 一个会话同时至多一个未决固定版本删除承诺。有效保全先提交则无新承诺；承诺先提交未结算则保全409 file_cleanup_in_progress，不创建／不报告生效。
- 租约到期、接管或配置暂停均不解除删除承诺闸门。恢复只处理原承诺版本；确认仍存在不证明旧DELETE已停止，必须保留未决直到确切不存在。
- 精确key＋version删除，禁止unversioned DELETE、前缀泛删及delete marker替代证明。未知上传、未穷尽清单、未知删除／在途义务不得deleted或释放额度；403／模糊404／204单独均不是证明。
- 保全覆盖绑定、未绑定和隔离对象。deleted仅清空允许内容字段并保留seq、来源关联、幂等；三份摘要仍由原DigestWorker退役。
- 人员／机器审计、队列、日志和实时通知无名称、说明、SHA、对象定位、令牌或凭据。内部删除版本表受限，不进入普通API。
- READ COMMITTED，短事务最多3次有界重试；外部I/O不进入重试闭包。锁等待及审计后用clock_timestamp复核。锁序详见下节，不能凭租约推断远端副作用结束。
- 生产附件发送继续503；生产GET和清理Worker默认关闭，不借上传enabled开启。typed_v1.download_available=false。本阶段显式测试装配，实际启用条件另行设计，不合并／部署。
- 本轮只删除自建专用桶内可证明归属的测试版本；凭据0600、目录0700。新组件及真实依赖门禁0 FAIL／0 SKIP；旧辅助启动器跳过须单列，不沿用P4-23结论。

## Review Focus

- RF1：有效JWT在慢读或阻塞写期间到期，不能等60秒才停止；TestFileDownloadRealTokenExpiryBlockedWrite（Task 13），控制器边界由Task 7验证。
- RF2：Unicode、CRLF、路径分隔或超长原文件名不能注入响应头或临时路径；TestFileDownloadDispositionSafety／TestFileDownloadSpoolPathIsolation（Task 6～7）。
- RF3：版本分页包含相邻file key、删除标记、非前进cursor或未知attempt，不能漏清单或越界删除；TestFileDeleteInventoryIsolation／TestFileDeleteInventoryUnknownAttempt（Task 8～9）。
- RF4：旧DELETE在租约接管且配置暂停后迟到，仍不得创建有效保全或承诺第二版本；TestFileDeleteLateResponseBarrier／TestFileDeleteHoldDirectSQL（Task 10／12）。
- RF5：会话／作业owner或token的ABA重放、终结审计确认重复，不能打开审计闸门、伪记完成或二次释放额度；TestFileDownloadTerminalCAS／TestFileDeleteFinalizeIdempotent（Task 5／11）。

---

## 文件职责与依赖顺序

所有路径相对隔离工作树；表中“／_test.go”表示同名测试文件，提交只限本任务文件。完整接口契约在各任务列出，不暴露SDK类型。Task 1～7下载、Task 8～11清理、Task 12并发、Task 13真实链路、Task 14交付。

| 文件 | 职责 |
|---|---|
| internal/files/retention_policy.go／_test.go | 动态文件期限、纯领域结果契约；Task 1 |
| internal/filedownload/contracts.go、contracts_test.go | 下载票据、结果、Repository；Task 1 |
| internal/filecleanup/contracts.go、contracts_test.go | 清理票据、承诺、Repository；Task 1 |
| internal/policy/evaluator.go／_test.go | 新闭集file_download动作；Task 1 |
| db/migrations/000021_file_download_retention.up.sql、.down.sql | 配置、下载、清理、审计及保全守卫；Task 2 |
| internal/policystore/file_download_schema_test.go、file_download_test_helpers_test.go | 专用SQL夹具与结构证据；Task 2 |
| internal/policystore/migration_test.go、conversations_test.go、file_message_test_helpers_test.go、file_foundation_migration_test.go、file_runtime_schema_test.go、body_clear_migration_test.go、digest_retirement_migration_test.go；internal/oidcauth/store_test.go；internal/realtime/tickets_test.go、stream_test.go；internal/access/migration_test.go、legal_hold_migration_test.go | 显式迁移列表及嵌套down21前置；Task 2 |
| internal/access/file_retention_policy.go／_test.go；internal/httpserver/file_retention_policy.go／_test.go | 管理配置CAS、历史、HTTP；Task 3 |
| internal/policystore/file_download_authorization.go／_test.go | 原来源、历史／当前参与及双方策略证明；Task 4 |
| internal/policystore/file_download_sessions.go／_test.go、file_download_audit.go／_test.go | 下载会话、不可变结果、机器审计修复；Task 5 |
| internal/filedownload/service.go／_test.go、spool.go／_test.go | 完整校验的私有spool及服务；Task 6 |
| internal/httpserver/file_download.go／_test.go、file_content.go、file_content_server_test.go、admin.go；internal/oidcauth/auth.go／_test.go | 方法分派、流时限、可信token期限；Task 7 |
| cmd/im-api/main.go、main_test.go；internal/policystore/file_message_history.go | 默认关闭、配置路由装配及卡片关闭回归；Task 7 |
| internal/objectstore/version_delete.go／_test.go、s3_version_delete.go、s3_version_delete_integration_test.go、s3.go | 独立版本列举／探测／删除能力；Task 8 |
| internal/policystore/file_delete_candidates.go／_test.go、file_delete_inventory.go／_test.go | 候选、状态、来源和穷尽清单；Task 9 |
| internal/policystore/file_delete_commitment.go／_test.go；internal/access/legal_hold.go、legal_hold_place.go、legal_hold_place_test.go；internal/httpserver/legal_hold_admin.go、legal_hold_admin_test.go | 逐版本承诺、保全冲突及恢复；Task 10 |
| internal/policystore/file_delete_finalize.go／_test.go；internal/filecleanup/worker.go／_test.go；cmd/im-file-cleaner/main.go／_test.go | 事务外删除、证明终态及默认关闭Worker；Task 11 |
| internal/policystore/file_download_concurrency_test.go、file_delete_concurrency_test.go、file_reservation.go；internal/access/file_retention_policy_test.go、legal_hold_place_test.go | 两连接故障／锁序验证及必要旧路径接入；Task 12 |
| internal/policystore/file_download_real_integration_test.go、file_delete_real_integration_test.go、file_download_browser_integration_test.go；internal/webclient/e2e/file_download_legacy.cjs | 实际OIDC／扫描／存储／慢客户端／旧Web；Task 13 |
| scripts/test-file-download-retention.sh、testdata/file-runtime/README.md | 本轮资源归属、角色及零跳过门禁；Task 13 |
| docs/开发增量-P4-24-验收记录.md、本计划、docs/企业IM-开发计划与实施路径-v0.1.md | 固定提交证据、真实边界与交付；Task 14 |

### 跨模块锁序与运行边界

1. 下载按tenant／user取得同一事务级advisory闸门（所有下载会话开始／修复路径相同），保全沿用request advisory；它们必须在行锁之前取得，禁止事务后半段反向取advisory。
2. 人员路径先无锁读来源，再按membership UUID排序锁下载者和原上传者，用现有loadGroupSendMemberships锁t／u／m／o／l并验证身份；不用全群发送授权助手替代两方下载授权。Worker仅锁tenant SHARE，无伪造人员。
3. conversation FOR UPDATE → 当前参与区间 SHARE → tenant当前策略／file_retention配置SHARE → 必要file_upload配置锁 → file_objects UPDATE → message／attachment SHARE → session或job／version UPDATE →终结事件／审计。多行同表按UUID排序；配置管理不获取会话或文件锁。
4. 新路径逆序冲突用NOWAIT及最多3次新短事务重试；下载复核事务截止不晚于上次授权＋1秒。发现旧reservation先锁upload配置再锁人员／会话时，Task 12把其配置锁移至授权后、文件前，额度锁保留。仅改会形成新循环的接入点，旧路径已有局部重试继续保留。
5. 保全应用先人员再conversation；直接SQL保全／承诺守卫均先尝试同conversation锁再检查对方事实。调用者已经逆序持行锁时NOWAIT拒绝，禁止靠触发器长期反向等待。承诺使用partial unique(tenant_id,conversation_id) WHERE phase IN ('committed','uncertain')作为第二防线。
6. Begin／Authorize／Check、库存登记／承诺／终态都在取得上述锁后取clock_timestamp；授权／生命周期成功审计后再取一次并校验身份、TTL、owner／token。对象读取、版本分页、DELETE和响应写始终在事务外。
7. 删除作业owner租约120秒；单次存储调用最多30秒，SDK自动重试关闭；每批最多20候选、每页最多100版本、每轮最多10页。未穷尽只持久化cursor，不作全部不存在结论。错误退避1、2、4…60秒，次数仅观测，不能解闸或证明不存在。
8. 下载硬截止60秒，不通过续租延长；spool最多4×25MiB加每文件1字节越界探针，块32KiB。会话deadline届满可恢复unknown结果；旧进程随后回报也必须被terminal CAS拒绝，已发字节不能撤回。

## Task 1：领域期限、下载／清理契约和策略动作

**Files:** 新建files/retention_policy.go／_test.go、filedownload/contracts.go／contracts_test.go、filecleanup/contracts.go／contracts_test.go；修改policy/evaluator.go／_test.go。

**Interfaces:**
- files.RetentionPolicy{Days int64,CleanupEnabled bool,Version int64}；NormalizeRetentionPolicy(p RetentionPolicy)(RetentionPolicy,error)；FileExpiresAt(acceptedAt time.Time,days int64)(time.Time,error)。到边界即到期，UTC时长而非日历日期；拒绝无穷／零时间和溢出。
- filedownload.Ticket{SessionID,OwnerID,LeaseToken string;Identity access.TrustedIdentity;File files.Metadata;MessageID string;MessageSeq int64;Deadline,LeaseExpiresAt time.Time}；Result{Outcome,Reason string;BytesWritten int64}，Outcome仅completed／interrupted／unknown。内部票据绝不作为DTO。
- filedownload.Repository：BeginFileDownload(ctx context.Context,id access.TrustedIdentity,fileID,ownerID string,deadline time.Time)(Ticket,error)；AuthorizeFileDownload(ctx context.Context,id access.TrustedIdentity,t Ticket)error；CheckFileDownload(ctx context.Context,id access.TrustedIdentity,t Ticket)error；FinishFileDownload(ctx context.Context,t Ticket,result Result)error。policystore.Service实现。
- filecleanup.Ticket{JobID,TenantID,FileID,ConversationID,OwnerID,LeaseToken string;PolicyVersion,StateVersion int64;LeaseExpiresAt time.Time}；Version{VersionID,AttemptID string}；Inventory{Versions []Version;NextKey,NextVersion string;Exhausted bool;Reason string}；Commitment{Ticket Ticket;CommitmentID,VersionID string}；AbsenceProof{VersionID string;CheckedAt time.Time;Absent bool}。
- filecleanup.Repository：ClaimFileDelete(ctx context.Context,ownerID string)(Ticket,bool,error)；RecordFileDeleteInventory(ctx context.Context,t Ticket,in Inventory)error；GetFileDeleteInventory(ctx context.Context,t Ticket)(Inventory,error)；CommitFileDeleteVersion(ctx context.Context,t Ticket,versionID string)(Commitment,error)；ClaimFileDeleteRecovery(ctx context.Context,ownerID string)(Commitment,bool,error)；SettleFileDeleteVersion(ctx context.Context,c Commitment,p AbsenceProof)error；FinalizeFileDelete(ctx context.Context,t Ticket)error。类型不能授予HTTP调用者选择版本权。
- policy.ActionFileDownload="file_download"；可errors.Is的下载错误ErrNotFound／ErrInvalidIdentity／ErrBusy／ErrAuditPending／ErrLimit／ErrUnavailable及清理ErrLeaseLost／ErrBlocked／ErrIncomplete，在各contracts.go定义。

- [x] **1. RED测试**：TestFileRetentionBoundsAndUTC、TestFileDownloadResultBounds、TestFileCleanupContractSources、TestPolicyFileDownloadIndependent。断言365默认，1／3650合法，0／3651拒绝，夏令时仍days×24h；错误结果字节／来源拒绝，send允许不自动允许file_download。
```go
expiry, err := files.FileExpiresAt(accepted, 365)
if err != nil || !expiry.Equal(accepted.Add(365*24*time.Hour)) { t.Fatal("incorrect UTC retention") }
```
- [x] **2. RED命令**：`go test ./internal/files ./internal/filedownload ./internal/filecleanup ./internal/policy -run 'TestFileRetention|TestFileDownloadResult|TestFileCleanupContract|TestPolicyFileDownload' -count=1 -v`；预期因新契约／动作缺失失败，不能把依赖故障当RED。
- [x] **3. 最小实现**：仅纯类型／校验／期限计算及supportedAction扩展，不加入IO、配置环境变量或数据库实现。
- [x] **4. GREEN**：同命令全部PASS；再`go test ./internal/policy ./internal/files -count=1`，旧hard_deny与全群发送行为有效。
- [x] **5. 提交**：只暂存本任务Files；`git commit -m "feat(files): define download and retention contracts"`。

## Task 2：000021 持久化证据和保护性回退

**Files:** 新建两份000021 SQL、file_download_schema_test.go、file_download_test_helpers_test.go；修改职责表中的显式迁移／回退夹具。

**Interfaces:** 产出tenant_file_retention_policy／history；file_download_sessions；file_download_terminal_events；file_delete_jobs；file_delete_versions。download session来源复合tenant／file／message／requester membership；阶段preparing→authorized→completed／interrupted／unknown，后3种不可重写，事件唯一session且ack只允许一次。delete job阶段pending／inventory／blocked／deleting／finished，version阶段inventoried→committed／uncertain→absent；commitment ID、来源、版本不可改。下载闸门为terminal未审计ack的唯一tenant／user／file，不以租约终结解除。机器审计扩展现有file_worker_audit_events及必要lifecycle闭集；下载终结事件以session ID关联，专用audit事件FK只接受已有terminal事实，并保留requester来源／独立worker ID，不能把download session冒充scan job或把requester冒充Worker。所有事实不可删，stage／时间约束用IS TRUE，禁止NULL旁路。

- [x] **1. RED测试**：TestFileDownloadSchemaSources／Stages／TerminalImmutable、TestFileDeleteSchemaCommitmentBarrier、TestFileRetentionSchemaHistory、TestFileDownloadDownGuards／DownConcurrentInsert。真实SQL拒绝跨源、无版本承诺、直接插入authorized／absent、证据回填和NULL；保全与承诺双向守卫，单会话第二承诺拒绝；down先锁表再检查，默认未改配置可回退，有规则／历史／会话／作业则拒绝。
```go
if err := tx.Commit(ctx); err == nil { t.Fatal("cross-tenant download evidence committed") }
```
- [x] **2. RED**：`go test ./internal/policystore -run '^TestFile(DownloadSchema|DeleteSchema|RetentionSchema|DownloadDown)' -count=1 -v`；专用PG已连通下因缺000021／列或守卫失败，SKIP不是RED。
- [x] **3. 实现**：添加复合FK、不可变触发器、同会话锁与未决版本唯一索引、合法阶段／lease／byte守卫和file_download策略CHECK；先锁相关父／子表ACCESS EXCLUSIVE再做down证据检查。down21先于旧20／19／18的嵌套回退；旧15／16回退仍需证明原拒绝而非新依赖错误。新租户初始化365／false／version0，history仅version>0且审批来源齐全。
- [x] **4. GREEN**：重复RED命令；`go test ./internal/policystore ./internal/access ./internal/oidcauth ./internal/realtime ./internal/retention -run 'Migration|Schema|Down' -count=1 -v`；新及受影响SQL测试PASS，依赖用例无SKIP。
- [x] **5. 提交**：仅SQL与本任务夹具；`git commit -m "feat(db): persist file download and deletion evidence"`。

## Task 3：租户保留配置、审批历史与管理HTTP

**Files:** 新建access/file_retention_policy.go／_test.go、httpserver/file_retention_policy.go／_test.go。

**Interfaces:** access.FileRetentionPolicyRecord{Policy files.RetentionPolicy;ApprovalReference,ActorUserID,ActorMembershipID string;UpdatedAt time.Time}、FileRetentionPolicyChange{Policy files.RetentionPolicy;ExpectedVersion int64;ApprovalReference string}、FileRetentionPolicyHistoryPage{History []FileRetentionPolicyRecord;NextCursor string}；Service.GetFileRetentionPolicy(ctx,id)(FileRetentionPolicyRecord,error)、SetFileRetentionPolicy(ctx,id,change)(FileRetentionPolicyRecord,error)、ListFileRetentionPolicyHistory(ctx,id,cursor string,limit int)(FileRetentionPolicyHistoryPage,error)，ctx=context.Context、id=TrustedIdentity。httpserver.HandlerWithFileRetentionPolicy(next http.Handler,auth Authenticator,svc FileRetentionPolicyService)(http.Handler,error)，service接口上述3方法。

- [x] **1. RED测试**：TestFileRetentionPolicyAdminCAS、HistoryPaging、LateIdentityExpiry、TestFileRetentionPolicyHTTPStrict。断言集团管理员限定、CAS旧版本409、审批128字节边界沿用现有审批校验、history最多100默认20、查询游标绑定tenant；JSON重复／未知字段／NULL／非规范十进制拒绝。
```go
if !errors.Is(err, access.ErrConflict) { t.Fatal("stale expected_version accepted") }
```
- [x] **2. RED**：`go test ./internal/access ./internal/httpserver -run '^TestFileRetentionPolicy' -count=1 -v`；预期API／服务未实现或断言失败。
- [x] **3. 实现**：沿用filePolicyTransaction及严格解码，GET／PUT配置和GET history；days／version字符串，cleanup_enabled布尔，PUT包括expected_version、approval_reference；当前配置和历史及人员审计原子提交，锁／审计后复核。响应明确current_policy_applies_to_existing_files=true，无员工读取接口。
- [x] **4. GREEN**：重复命令PASS无SKIP；运行旧`go test ./internal/access ./internal/httpserver -run 'FileUploadPolicy|RetentionAdmin|RetentionHistory' -count=1 -v`。
- [x] **5. 提交**：仅本任务4文件；`git commit -m "feat(files): administer tenant file retention policy"`。

## Task 4：实际附件的下载授权证明

**Files:** 新建policystore/file_download_authorization.go／_test.go。

**Interfaces:** 内部`authorizeFileDownloadTx(ctx context.Context,tx pgx.Tx,id access.TrustedIdentity,fileID string)(files.Metadata,string,int64,time.Time,error)`，返回file、messageID、seq、fresh time。供Task 5 Begin／Authorize／Check复用，禁止复用authorizeFileTx(requireSend=true)来要求全群互通；沿用历史助手的body期限、interval和hard_deny语义。

- [x] **1. RED测试**：TestFileDownloadAuthorizationSources／MembershipGap／Policies／UploaderInactive／AdminNoBypass／DynamicRetention。cover同／跨tenant、未绑定、原任职变化、退群再入缺口、policy_blocked、正文先到期、文件先到期、清理正文、延长ready恢复资格而pending不恢复，原scan不匹配拒绝；双方当前send和独立file策略必须同时允许。
```go
if !errors.Is(err, filedownload.ErrNotFound) { t.Fatal("rejoin gap leaked attachment") }
```
- [x] **2. RED**：`go test ./internal/policystore -run '^TestFileDownloadAuthorization' -count=1 -v`；预期缺授权助手／资格断言失败。
- [x] **3. 实现**：按锁序复核绑定来源／原上传者，历史区间先判再当前参与；当前两方策略+历史send hard_deny，无管理员旁路。统一404不给存在性／名称，当前identity失效独立403；无外部IO、消息seq／ACK／Outbox写入。
- [x] **4. GREEN**：重复命令；`go test ./internal/policystore -run 'GroupMessageSend|GroupHistory|FileMessageHistory|FileAuthorization' -count=1 -v`，旧群发送全成员对及历史规则PASS。
- [x] **5. 提交**：仅新助手与测试；`git commit -m "feat(files): authorize bound attachment downloads"`。

## Task 5：下载会话、终结事实与审计修复

**Files:** 新建policystore/file_download_sessions.go／_test.go、file_download_audit.go／_test.go。

**Interfaces:** 实现Task 1 filedownload.Repository四方法，调用Task 4授权助手；新增`Service.RepairFileDownloadAudit(ctx context.Context,ownerID string,limit int)(int,error)`，每批1～20，供Task 11独立Worker。新会话owner/token/session均服务端生成或启动机器ID，deadline不大于DB fresh time+60秒；LeaseExpiresAt不得超过deadline。终结队列唯一session，机器audit以事件ID幂等确认。

- [x] **1. RED测试**：TestFileDownloadSessionLimits、AuthorizationAuditAtomic、TerminalCAS、AuditRepairGate、CrashUnknown。两API实例同时开始同文件只一个、同用户第3个429、不同用户不被他人审计缺口阻断；audit授权失败无authorized；实际写满才completed，owner/token错误无改动；终结DB故障留闸门，expired恢复unknown，迟到completed不能覆盖，重复repair一份机器事实。
```go
if countTerminalFacts(t, sessionID) != 1 || retryAllowed(t, sessionID) { t.Fatal("audit gap gate violated") }
```
- [x] **2. RED**：`go test ./internal/policystore -run '^TestFileDownload(Session|AuthorizationAudit|Terminal|AuditRepair|Crash)' -count=1 -v`；预期Repository／修复缺失。
- [x] **3. 实现**：预检与创建原子，prepare失败也登记interrupted；Authorize audit后新鲜复核才提交；Check不写每块人员审计但强制资格和stage/token。Finish事实+queue原子，未知不得伪记成功；Repair终结audit和ack同事务，expiry依据硬deadline，不看进程alive猜测完成。
- [x] **4. GREEN**：同命令PASS无SKIP；故障注入真实PG savepoint／触发器或连接控制，不仅返回预编错误。
- [x] **5. 提交**：仅新会话／audit文件；`git commit -m "feat(files): settle downloads through durable audit facts"`。

## Task 6：有界私有spool、完整校验与下载服务

**Files:** 新建filedownload/service.go／_test.go、spool.go／_test.go。

**Interfaces:** `NewService(repo Repository,objects objectstore.Store,spoolRoot,ownerID string)(*Service,error)`；Service.Prepare(ctx context.Context,id access.TrustedIdentity,fileID string)(*Prepared,error)、Authorize(ctx context.Context,id access.TrustedIdentity,p *Prepared)error、Check(ctx context.Context,id access.TrustedIdentity,p *Prepared)error、Finish(ctx context.Context,p *Prepared,result Result)error、Close()error。Prepared保持私有Ticket和句柄，公开SizeBytes()int64、Filename()string、Read([]byte)(int,error)、Close()error；不能外部构造有效Prepared。ctx=context.Context、id=access.TrustedIdentity，外部读复用ReadVersion(ctx,VersionRef)。限额服务实例节点共享，不每请求新建limiter。

- [x] **1. RED测试**：TestFileDownloadSpoolIntegrity／Bounds／PathIsolation／CrashRecovery、TestFileDownloadPrepareFailureSettlement。长度差、额外字节、SHA错、版本错、读超时返回503且无可输出Prepared；目录0700文件0600、symlink／非ownedroot拒绝，文件名../../或Unicode不得形成路径；第5个并发拒绝，读失败仍结算session。
```go
if p != nil || err == nil { t.Fatal("unverified object exposed to writer") }
```
- [x] **2. RED**：`go test ./internal/filedownload -run '^TestFileDownload(Spool|Prepare)' -count=1 -v`；预期无Service或保护失败。
- [x] **3. 实现**：随机session目录+exclusive新文件+size+1读取及hash，完整核验后seek0。独占owner spool根带锁和来源manifest；启动只回收本owner可证明已不在途目录，不递归删未知内容／symlink，不完整归属则启动失败；disk上限与并发同时有界。成功／失败Close释放文件和节点额度，DB终结失败不开放持久闸门。
- [x] **4. GREEN**：上述PASS；`go test -race ./internal/filedownload -count=1`，退出／崩溃恢复及并发无泄露，无SKIP。
- [x] **5. 提交**：仅spool／service文件；`git commit -m "feat(files): verify download content in private bounded spool"`。

## Task 7：GET／PUT分派、可信期限和在途流控制

**Files:** 新建httpserver/file_download.go／_test.go；修改file_content.go、file_content_server_test.go、admin.go、oidcauth/auth.go／_test.go、cmd/im-api/main.go／main_test.go。file_message_history.go仅必要保持download_available=false，不添加URL。

**Interfaces:** VerifiedIdentity新增ExpiresAt time.Time，由Authenticator验证的JWT exp赋值，既有TenantID／UserID保持；下载显式拒绝缺expiration的适配器。`HandlerWithFileDownload(next http.Handler,auth Authenticator,svc FileDownloadService)(http.Handler,error)`；service接口使用Task 6四个方法Prepare／Authorize／Check／Finish及其完整签名，Prepared.Read／Close负责内容和回收。新GET wrapper置于旧PUT wrapper外，旧wrapper对非PUT的GET显式委派，unsupported方法Allow按实际装配反映GET, PUT。生产独立closed GET wrapper返回503 file_download_unavailable；仅测试显式装配可下载。

- [x] **1. RED测试**：TestFileDownloadHTTPMethods／DispositionSafety／ZeroBytesBeforeAudit／DeadlineUnsupported／InFlightReauth／TokenExpiry／PartialWrite／ProductionClosed；旧PUT200／错误不变，GET不会被PUT405截断；HEAD／Range／条件headers／query／body拒绝，无206／304。filename以mime.FormatMediaType形成安全attachment；UTF-8、255字节及控制码／路径限制沿用files元数据校验，不合法字段503且零字节，不修剪非法名称后继续下载；授权失败零字节，partialwrite按n计数并终止，不追加JSON。
```go
if unauthorizedBytes != 0 || strings.Contains(responseHeader, "\r\nInjected:") { t.Fatal("unsafe download response") }
```
- [x] **2. RED**：`go test ./internal/httpserver ./internal/oidcauth ./cmd/im-api -run '^TestFileDownload|TestOIDCDownloadExpiry' -count=1 -v`；预期GET／deadline契约未实现。
- [x] **3. 实现**：每32KiB块前重新Authenticate原bearer并核对同一tenant/user/acting任职，Check用新DB资格；设置写deadline=min(总截止、exp、上次有效复核+1秒)，数据库复核也受该界限。进入GET即验证ResponseController并设start+60秒写deadline，避免spool超过旧10秒窗口；首次完整spool后Authorize，再fresh auth／Check后才200；no-store、nosniff、octet-stream、Content-Length、安全attachment，无Location／SHA。error按规格映射；首字节后先登记中断并以http.ErrAbortHandler终止本响应（HTTP/1关闭／HTTP/2终止stream），不追加错误体；结算用独立5秒有界context，失败靠持久恢复，不延长60秒输出窗口。只在显式下载wrapper override本请求写deadline，其他API10秒不改；生产装配管理配置路由、closed GET，file发送和卡片继续关闭，无新启用env。
- [x] **4. GREEN**：同命令PASS；`go test ./internal/httpserver ./internal/oidcauth ./cmd/im-api -run 'FileContent|FileMessageProduction|FileUploadPolicy|FileDownload|OIDC' -count=1 -v`。真实慢TCP停止留Task 13，不以httptest代替。
- [x] **5. 提交**：仅本任务列出文件；`git commit -m "feat(files): stream authorized downloads with bounded revocation"`。

## Task 8：独立精确版本列举、探测和删除适配

**Files:** 新建objectstore/version_delete.go／_test.go、s3_version_delete.go、s3_version_delete_integration_test.go；s3.go只抽取共享受限客户端构造，不扩展Store删除能力。

**Interfaces:** objectstore.VersionCursor{KeyMarker,VersionMarker string}、InventoryVersion{Ref VersionRef;AttemptID string;DeleteMarker bool}、VersionPage{Versions []InventoryVersion;Next VersionCursor;Exhausted bool}；VersionPresence仅present／absent／unknown。`VersionDeleter`：ListVersions(ctx context.Context,location Location,cursor VersionCursor,limit int)(VersionPage,error)；ProbeVersion(ctx context.Context,ref VersionRef)(VersionPresence,error)；DeleteVersion(ctx context.Context,ref VersionRef)error。`NewS3VersionDeleter(c Config)(VersionDeleter,error)`使用独立credential source="cleanup_environment"，键仅IM_FILE_CLEANUP_S3_ACCESS_KEY／SECRET_KEY，不能回落API密钥。Task 11 Worker消费此接口。

- [x] **1. RED测试**：TestFileDeleteVersionExact／InventoryIsolation／Pagination／PresenceFailures／NoAutoRetry；验证versionId始终必填，null／空拒绝，prefix近邻filtered且不能丢页；403、NoSuchBucket、普通key404、marker、cursor重复均unknown或失败，唯授权精确NoSuchVersion并结合穷尽清单才可证明。
```go
if request.URL.Query().Get("versionId") != sealedVersion { t.Fatal("delete lost fixed version") }
```
- [x] **2. RED**：`go test ./internal/objectstore -run '^TestFileDelete' -count=1 -v`；预期缺VersionDeleter或参数保护失败。
- [x] **3. 实现**：现有SDK ListObjectVersions有界页，GET/HEAD精确版本并校验返回版本及attempt元数据；逐个版本来源核验，不下载全部内容用于删除。独立30秒调用、NopRetryer、拒绝redirect；404仅识别确切NoSuchVersion，其余保留unknown。Delete无version绝不发送。
- [x] **4. GREEN**：同命令PASS；旧`go test ./internal/objectstore -run 'TestS3|TestStore|TestVersion' -count=1 -v`。本任务模拟协议只证明参数语义，真实角色权限与缺失语义Task 13。
- [x] **5. 提交**：仅本任务适配及测试；`git commit -m "feat(storage): add narrow fixed version deletion capability"`。

## Task 9：清理候选、在途隔离和来源清单

**Files:** 新建policystore/file_delete_candidates.go／_test.go、file_delete_inventory.go／_test.go。

**Interfaces:** 实现ClaimFileDelete及RecordFileDeleteInventory；实现`Service.GetFileDeleteInventory(ctx context.Context,t filecleanup.Ticket)(filecleanup.Inventory,error)`供Worker取持久cursor／已知版本。Claim一次返回一job，各轮最多20；pending不可重新ready。Inventory记录仅来自sealed版本或数据库attempt事实，cursor与版本表一起提交。

- [x] **1. RED测试**：TestFileDeleteCandidatesBound／Orphan／Hold／InFlight、TestFileDeleteInventoryUnknownAttempt／Exhaustion／SealConflict。bound以当前days判断、orphan以uploadTTL判断；activehold含隔离对象全暂停；未决上传／扫描／下载阻止进入可删除许可；未知attempt、写后未知、穷尽前、版本不匹配均不允许Finalize。
```go
if deleted || quotaReleased { t.Fatal("incomplete inventory treated as absence") }
```
- [x] **2. RED**：`go test ./internal/policystore -run '^TestFileDelete(Candidates|Inventory)' -count=1 -v`；预期候选／库存服务缺失。
- [x] **3. 实现**：按锁序标delete_pending并写lifecycle／机器审计；旧上传Begin／封存／scan／消息绑定状态CAS确认pending不能成功，必要接入Task 12。对于storing／recovery_pending等晚写风险，不以lease／次数证明已停止；记录blocked、暂停危险自动操作，待有来源的明确对账。配置暂停不新Claim，新到期判断fresh；当前days延长不恢复已经pending。库存只登记，尚无DELETE许可。
- [x] **4. GREEN**：同命令PASS；`go test ./internal/policystore -run 'FileUpload|FileScan|FileMessageBinding|FileDeleteCandidates|FileDeleteInventory' -count=1 -v`，旧状态守卫继续PASS，缺少夹具不得SKIP。
- [x] **5. 提交**：仅本任务候选／库存；`git commit -m "feat(files): quarantine retention candidates with proven inventories"`。

## Task 10：逐版本承诺、保全冲突和不可逆恢复

**Files:** 新建policystore/file_delete_commitment.go／_test.go；修改access/legal_hold.go、legal_hold_place.go／_test.go、httpserver/legal_hold_admin.go／_test.go。

**Interfaces:** 实现CommitFileDeleteVersion、ClaimFileDeleteRecovery、SettleFileDeleteVersion；access.ErrFileCleanupInProgress可errors.Is，保全HTTP映射409 file_cleanup_in_progress。新增承诺只允许inventoried、穷尽且无未知写、当前cleanup_enabled=true／到期／无hold。恢复只返回已有Commitment，接管换owner/token但CommitmentID／VersionID保持。

- [x] **1. RED测试**：TestFileDeleteCommitmentHoldFirst／CommitFirst／PausedRecovery／OneVersionPerConversation、TestFileDeleteHoldDirectSQL、TestFileDeleteLateLeaseBarrier、TestLegalHoldFileCleanupConflict。先hold则0许可；先commit则hold零写409；leaseexpired和pause仍同版本；仍present不得settle，旧DELETE未回报时不能第二版本；直接SQL插hold受阻；settle明确absence后hold可以保护剩余版本。
```go
if holdCount != 0 || !errors.Is(err, access.ErrFileCleanupInProgress) { t.Fatal("uncertain deletion reported protected") }
```
- [x] **2. RED**：`go test ./internal/policystore ./internal/access ./internal/httpserver -run '^TestFileDelete(Commitment|Hold|LateLease)|^TestLegalHoldFileCleanup' -count=1 -v`；预期未决闸门／错误映射缺失。
- [x] **3. 实现**：同conversation锁下确认新承诺，在对象调用前落库；lease／configpause不移除barrier。SQL守卫Task 2已保护，应用返回明确业务冲突并保持旧hold幂等重放；先查原合法request replay，不能把原已生效hold误说成新冲突。恢复最多处理原版本，检查absence／owner/token；机器审计故障不settle，直接SQL不能跳阶段解闸。
- [x] **4. GREEN**：重复命令PASS；旧`go test ./internal/access ./internal/httpserver -run 'LegalHold' -count=1 -v`全部PASS无SKIP。
- [x] **5. 提交**：仅本任务承诺／保全文件；`git commit -m "feat(files): serialize version deletion commitments with legal holds"`。

## Task 11：清理执行器、核验终态和默认关闭命令

**Files:** 新建policystore/file_delete_finalize.go／_test.go、filecleanup/worker.go／_test.go、cmd/im-file-cleaner/main.go／_test.go。

**Interfaces:** 实现FinalizeFileDelete；filecleanup.NewWorker(repo Repository,objects objectstore.VersionDeleter,ownerID string)(*Worker,error)、Worker.Step(ctx context.Context)(bool,error)。repo额外消费Task 9 GetFileDeleteInventory。命令内部runner组合Step与RepairFileDownloadAudit；默认-execute=false且不连对象／不删数据；显式-execute仅测试操作者装配，生产启用合同另议。

- [x] **1. RED测试**：TestFileDeleteWorkerFixedVersion／UnknownResponse／PausedReconcile、TestFileDeleteFinalizeIdempotent／QuotaAuditAtomic／MessageEvidence、TestFileCleanerDefaultClosed。DELETE成功但Probe失败仍未决；全页穷尽且无未知义务才终态；Finalize commit失败不清字段／不释放额度；重试只推进一次；seq／关联／两份幂等及封存指纹不变；默认命令零副作用。
```go
if state != files.StateDeletePending || usedBytes != before { t.Fatal("failed finalize released quota") }
```
- [x] **2. RED**：`go test ./internal/filecleanup ./internal/policystore ./cmd/im-file-cleaner -run '^TestFileDelete(Worker|Finalize)|^TestFileCleaner' -count=1 -v`；预期Worker或终态缺失。
- [x] **3. 实现**：先处理旧承诺恢复，暂停配置也对账旧承诺；再有界候选／库存／单版本commit→事务外DELETE→精确Probe→Settle。全部版本已absence再fresh完整重列证明库存未增加、无未知写／在途／未ack下载义务才Finalize；读取报错不返空清单。现额度由state<>deleted总和计算，不加重复退款账；CAS终态／字段清理／lifecycle／audit原子。Worker独立角色，下载audit修复不因cleanup暂停停止。
- [x] **4. GREEN**：同命令PASS；`go test ./internal/files ./internal/filecleanup ./internal/policystore ./cmd/im-file-cleaner -run 'FileLifecycle|FileDelete|FileCleaner|DigestRetirement|AttachmentDigest' -count=1 -v`，三份指纹仍由原Worker处理。
- [x] **5. 提交**：仅本任务执行器／命令／终态；`git commit -m "feat(files): reconcile exact deletion before releasing storage quota"`。

## Task 12：两连接竞态、迟到副作用与旧路径锁序

**Files:** 新建policystore/file_download_concurrency_test.go、file_delete_concurrency_test.go；必要修改file_reservation.go及Task 9指出的file_upload.go、file_scan.go最小状态接入；增补access/file_retention_policy_test.go、legal_hold_place_test.go。不放宽证据触发器。

**Interfaces:** 只消费Task 3～11既定方法，无新增业务签名；对quota reservation的配置锁移至人员／conversation授权之后，其他行为和旧接口不变。

- [x] **1. RED测试**：TestFileDownloadConcurrentPolicyChange／AuditWaitExpiry／MultiNodeGate、TestFileDeleteLateResponseBarrier、HoldDirectSQLConcurrent、ConcurrentQuotaReservation、FinalizeFault、TestFileRetentionConcurrentChange。两连接barrier确定先后，不能用sleep猜锁已取得；成员结束、上传者失效、hard_deny和配置改期在preflight／外部读／audit前后切入；Finalize与预约并发额度不重复释放，保全释放需fresh配置；旧scan迟到不能覆盖pending。
```go
if unauthorizedBytes != 0 || newCommitmentsDuringHold != 0 { t.Fatal("write crossed revocation or hold") }
```
- [x] **2. RED**：`go test ./internal/policystore ./internal/access -run '^TestFileDownloadConcurrent|^TestFileDownloadAuditWait|^TestFileDownloadMultiNode|^TestFileDeleteLate|^TestFileDeleteHoldDirectSQLConcurrent|^TestFileDeleteConcurrent|^TestFileDeleteFinalizeFault|^TestFileRetentionConcurrent' -count=1 -v`；预期具体竞态断言失败；若首次全PASS如实记录已实现的不变量，不制造假RED。
- [x] **3. 修正**：仅实际失败对应的锁序／fresh复核／状态CAS；所有IO在事务外，失败重试最多3次；不能以数据库禁用守卫造正常产品允许的状态。模拟远端延迟仅用于确定故障顺序，真实远端Task 13补证。
- [x] **4. GREEN**：重复命令；`go test -race ./internal/policystore ./internal/access ./internal/filedownload ./internal/filecleanup ./internal/httpserver -run 'FileDownload|FileDelete|FileRetention|LegalHoldFileCleanup' -count=1 -v`；0FAIL／0SKIP，旧预约／扫描受影响测试PASS。
- [x] **5. 提交**：仅本任务测试与失败对应修正；`git commit -m "test(files): prove download and cleanup concurrency boundaries"`。

## Task 13：真实身份／扫描／私有版本／客户端链路门禁

**Files:** 新建policystore/file_download_real_integration_test.go、file_delete_real_integration_test.go、file_download_browser_integration_test.go、webclient/e2e/file_download_legacy.cjs、scripts/test-file-download-retention.sh；更新testdata/file-runtime/README.md。

**Interfaces:** 复用既有真实OIDC测试issuer／JWKS、生产/api/v1/me验证、上传／实际扫描／发送fixture；成功ready只能来自新鲜真实scan。脚本`scripts/test-file-download-retention.sh run-all`支持IM_TEST_FILE_DOWNLOAD_OUTPUT_DIR绝对0700目录，继承现有依赖变量并要求独立清理角色密钥，不能打印密钥／DSN。新增必须出现的测试集合列在下一步，由go test -json严格验证。

- [x] **1. 用例断言**：TestFileDownloadRealOIDCScan、RealTokenExpiryBlockedWrite、RealRevocation、RealAuditRepair、RealBrowserLegacy；TestFileDeleteRealVersionsIAM、RealUnknownDelete、RealHoldOrdering、RealOrphanQuarantine；TestFileDownloadProductionClosed。实际私有版本长度／SHA；中文与Unicodeheader；不同用户并发；真实net.Conn慢读／停读，tokenexp与成员撤权、hard_deny／TTL／DB故障停止。记录撤权提交、最后复核、最后writer接受、client已缓冲字节各时间，不能把client缓冲当可撤回；复核调度间隔≤1秒，阻塞写deadline≤下一复核／exp／60秒。旧Web文本／附件占位仍可用，无新下载入口。
```go
if deleteRoleCanPut || scanRoleCanDelete || terminalAuditMissing { t.Fatal("real capability gate failed") }
```
- [x] **2. 准备本轮资源并运行首轮**：登记自建PG／Redis／S3专用桶、扫描进程、OIDC和browser资源owner清单；制作新鲜scanner manifest并校验版本／定义hash，旧P4-23运行证明不能复用。真实连接和角色预检后运行`go test ./internal/policystore ./cmd/im-api -run '^TestFileDownload(Real|Production)|^TestFileDeleteReal' -count=1 -v`，记录实际失败／通过；准备失败或SKIP不计RED。
- [x] **3. 实现门禁与受控故障夹具**：独立上传／scan只读／cleanup IAM角色，无Get公共开放。通过本轮受控转发器丢失DELETE响应／延迟发送，实际S3仍执行固定版本；存在／缺失／403／marker实测区分，hold顺序及pause恢复有PG与存储证据。仅登记自建对象；创建专用版本证明DeleteVersion可用，不对业务数据做启动删除探测。脚本要求上述10个具名测试全部出现、PASS、无SKIP；fresh定义或角色不合格即fail。
- [x] **4. GREEN**：`scripts/test-file-download-retention.sh run-all`，同时运行`scripts/test-file-messages.sh run-all`和`scripts/test-file-runtime.sh run-all`；所选真实组件0FAIL／0SKIP。实测60秒总截止不被读取／审计／输出分阶段重新起算，临时文件／进程退出独立确认；输出只留受限证据、不把测试桶成功说成生产发行版验收。
- [x] **5. 提交**：仅本任务集成／脚本／说明；`git commit -m "test(files): verify real download and hold-aware deletion paths"`。

## Task 14：固定提交验收、一次整体评审和阶段交付

**Files:** 新建docs/开发增量-P4-24-验收记录.md，更新本计划和总路径；有有效review发现才修改对应产品／回归测试。

**Interfaces:** 固定Task 1～13产品候选SHA；归档复验和评审都比较产品基线57a7812..候选SHA，不读取未提交工作树。最终仅文档提交须证明产品diff为空。

- [x] **1. 完整门禁**：git archive候选解包到本轮owned私有目录，以同一归档运行`go test -json ./... -count=1`和本轮及P4-23／P4-22组件脚本；日志独立0700，分别统计顶层／子测试PASS、FAIL、SKIP。唯一既有辅助启动器TestRealtimeAPIChild若仍跳过须单列，实际调用方需执行；新用例／缺依赖跳过拒绝交付。
- [x] **2. race**：归档内`go test -json -race ./internal/policystore ./internal/access ./internal/httpserver ./internal/filedownload ./internal/filecleanup ./internal/objectstore ./internal/retention ./internal/realtime ./internal/outbox ./cmd/im-api ./cmd/im-file-cleaner -count=1`。测试计数／时限／实际依赖证据单列，不加无关容量演练。
- [x] **3. 一次最终独立评审及必要修正**：使用requesting-code-review派发一个新鲜reviewer，固定基线／候选及规格／计划，Critical／Important／Minor逐项附证据。核验有效发现，针对性RED→GREEN修正；新最终产品SHA重新归档跑受影响和规定门禁，不再派发第二整体review。评审无发现也只报告检查范围。
- [x] **4. 中文记录和资源退出**：填实测SHA／命令／计数／权限与慢网络／未知结果／保全／quota／浏览器证据、review修正和局限；更新已完成复选框。只停止／清理本轮自建资源，确认运行器退出、临时内容回收，私有证据归档、凭据不提交；有未决副作用先按承诺对账，不强删日志／来源来伪造clean。F01、M4退出、HA／客户／生产启用与P4-25仍另验。
- [ ] **5. 堆叠草稿PR**：核对本轮提交并推送当前分支，base=`codex/p4-23-file-message-design`、head=`codex/p4-24-file-download-retention-design`；正文写UTF-8文件并gh --body-file，创建draft后attach_artifact。确认远端head及clean；仅交付已测范围，不合并／部署。规划阶段不执行本步骤。

## 规格覆盖及自检

| 规格 | 任务／验证 |
|---|---|
| §1～3范围、接口边界 | Task 1及文件职责；统一持久化边界，不做Web或生产启用 |
| §4动态配置及历史 | Task 1／3／4／9／12，TTL／CAS／审批与改期竞态 |
| §5历史＋当前来源＋两方策略 | Task 4／5／7／12／13，退群缺口、管理员、原上传者、独立策略 |
| §6完整GET／spool／撤权／期限 | Task 6／7／13，零字节、真实慢TCP、exp、60秒 |
| §7终结审计／闸门／恢复 | Task 2／5／6／11／12／13，unknown及机器来源 |
| §8～10库存、逐版本承诺、保全、删除终态 | Task 2／8～13，未知写与迟到DELETE／直接SQL／quota原子 |
| §11迁移／锁／Down | Task 2／锁序表／Task 12，先锁后检查、NULL及逆序冲突 |
| §12默认关闭 | Task 7／11／13，配置不授予生产能力 |
| §13实际验收及交付 | Task 12～14，新组件零跳过／不可变归档／一次review |
| §14依据 | 规格已有官方版本删除来源，实际存储语义由Task 13另验 |

自检已逐项核对：14项任务中13项有独立实现／测试产物，交付任务单列；Task 1声明的Repository与后续实现签名一致，Task 9的库存读取已在Task 1 Repository声明；未定义的expiry／worker权限没有留给猜测。五个Review Focus均有具名测试。这是规划形成时的覆盖自检；执行阶段代码、迁移、真实资源与门禁证据见[中文验收记录](../../开发增量-P4-24-验收记录.md)。复选框按实测与实施裁决更新；Task12已有保护只补竞态证据，没有制造产品RED。

## 审阅与执行衔接

本计划已经用户确认，沿用当前助手executing-plans逐项实现。每项记录实际RED／GREEN与本项提交；进度、故障修正和验收证据随实施更新。无需再次选择执行方式。
