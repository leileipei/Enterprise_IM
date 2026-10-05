# P4-26 正式服务装配与附件进程验收 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. 用户已选择当前助手逐项实现，最后一次整体独立评审；不重新选择执行方式。Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 通过默认关闭的附件业务开关，将现有发送、授权下载和文件名搜索装配到正式 API，并取得独立 Worker、双 API 与 Chrome 的完整进程证据。

**Architecture:** 复用既有领域服务、迁移和授权规则；在 cmd/im-api 拆出配置、启动装配与生命周期协调。下载使用独立只读存储及稳定节点目录，审计修复使用数据库专用模式；新进程夹具只启动正式二进制，TLS 代理不提供业务成功响应。

**Tech Stack:** Go 1.27.1、pgx/v5、PostgreSQL、既有 AWS S3 SDK、OIDC、Redis、ClamAV／qpdf、原生 JavaScript、既有 Playwright／Chrome；不新增产品依赖。

**Spec:** [已确认 P4-26 书面设计](../specs/2026-10-05-p4-26-file-business-runtime-design.md)，用户于 2026-10-05 回复“确认”，确认时提交 d0d6d5157e225d03663fa3c2c654b4109f9e8d02。用户于2026-10-05回复“是”确认本计划，已开始逐项实施；以下未完成任务的测试、接口与门禁仍为待实施要求。

## Global Constraints

- 使用已附隔离工作树 /Users/leo.cui/.codex/worktrees/p4-04-body-cleaner/企业IM系统，分支 codex/p4-26-file-runtime-design；P4-25 基线 6cee53c03e7e83d83690a7788c45c8a52ff13bf2。主工作区及 P4-25 草稿 PR #73 保留。
- IM_FILE_BUSINESS_ENABLED 只接受空、false、true；默认关闭，启动冻结，重启变更。原 IM_FILE_UPLOAD_ENABLED 独立；U/B 四组合的能力分别是 0000、1000、0111、1111。
- 能力仅表示装配，临时故障不改成 false；typed_v1.download_available 保持 false。OIDC 未启用不提供能力；所有授权、租户禁用规则、历史及当前资格、保全与动态保留逐次复核。
- 保持迁移000001～000021，无新迁移、名称副本、索引、预签名链接、外链、预览、OCR、Range、移动端或自动对象删除。仅 PDF／PNG／JPEG／UTF-8 TXT，最大25MiB，既有中文及FEFF名称契约不变。
- B=false 不解析或使用新增下载配置、不建下载目录／客户端、不发读取探针；B=true/U=false 不要求上传凭据、上传目录、qpdf或ClamAV配置，API不创建扫描器。
- 下载凭据仅 IM_FILE_DOWNLOAD_S3_ACCESS_KEY／SECRET_KEY，来源 download_environment；禁止回退或与本进程已配置上传／清理访问键相同。四个真实 IAM 主体分别用于上传、扫描、下载、删除。
- IM_FILE_DOWNLOAD_SPOOL_DIR 为专属绝对目录0700、当前用户所有，内容文件0600；IM_FILE_DOWNLOAD_OWNER_ID 为非零、小写规范UUID，同节点／目录重启保持稳定，不同节点分离。
- 探针键 _im_runtime/read-probe/v1，内容 UTF-8 enterprise-im-file-read-probe-v1 后跟一个LF；固定 IM_FILE_READ_PROBE_VERSION_ID，最多读取1KiB。独立引导权限预先创建，API与ready零创建／覆盖／删除。
- B=true文件初始化共享至多30秒，原上传专属检查仍不超过15秒，文件Worker原90秒校验保持。监听前全部初始化成功，否则非零退出并释放已建资源，不部分开放。
- ready整个请求共享原2秒总上下文；普通API WriteTimeout=10秒；名称搜索总5秒／statement4秒／lock1秒、500候选／20会话；下载60秒及在途复核窗口保持。Web原120秒扫描窗口、65秒下载总限／60秒Blob保留保持。
- SIGTERM原10秒HTTP等待，超时关闭连接并取消请求，然后另给服务关闭10秒收尾上限；失败非零退出、保留证据。未知目录／来源／owner保持拒绝，不宽泛删除。
- repair-only须 --execute，允许 --once；每批20条／5秒、正常间隔1秒、失败退避1/2/4/8/16/30秒，取消停止。无S3删除适配器、Step或清理作业消费，不自动启动。
- RP01～RP14真实必选场景0 FAIL／0 SKIP；旧 TestRealtimeAPIChild 辅助入口SKIP可单列，真实调用方须PASS。全仓／race／重进程门禁顺序执行，旧脚本期限不改。
- 敏感配置不入库；token、凭据、名称查询q、文件内容、完整业务响应不进证据。目录0700／文件0600，只停止PID／label／路径证明属于本轮的资源。
- 执行方法保持当前助手逐任务实施＋一次整体独立评审；Critical／Important一轮具名RED→GREEN修正后重新固定源码、重跑七门禁，不派第二reviewer；Minor另列。
- 结果仅为本地正式进程验收；客户IdP、M4、生产发行版、客户联调、安全评审、HA／容量、备份DR、生产放行另验。草稿PR以P4-25分支为base，创建后关联任务，不合并／部署。

## Review Focus

- RF1（Task 1／9）：B关闭时下载配置恶意或残留，必须零解析／网络／目录；“TRUE”、空格和共享访问键必须按规则拒绝。
- RF2（Task 2／6／9）：存储把探针请求重定向、给错版／额外字节或在ready中耗尽预算，必须有界失败，不借健康检查产生业务副作用。
- RF3（Task 4／7／11）：运行中目录inode替换、owner改变、慢客户端下载与SIGTERM同时发生，不能假健康、死等Close或错误清除未知内容。
- RF4（Task 5／6／9）：U=false/B=true时PUT、HEAD、OPTIONS及名称搜索与通用files/{id}冲突，必须正确Allow、精确路由且无上传入口。
- RF5（Task 8／11）：修复重复参数、审计已提交但进程失联、永久数据库故障，必须零对象删除、恰一次机器审计、有界退避且能取消。

---

## 文件职责与依赖

路径相对隔离工作树。新增文件按职责拆分；不复制领域授权SQL到cmd，不整体重构旧main。Task 1～8形成正式装配，9～12形成进程门禁，13固定证据交付；依序执行，不能先启用半成品。每项先核对HEAD／clean，RED只认缺失行为或接口，不认缺依赖／编译器等环境故障。

| 区域 | 文件与责任 | Task |
| --- | --- | --- |
| cmd/im-api | file_business_config.go配置；file_runtime.go启动；file_routes.go路由；lifecycle.go退出；main.go协调 | 1／6／7 |
| internal/objectstore | s3_readonly.go独立凭据／能力；s3_read_probe.go固定探针；复用s3.go | 2 |
| internal/policystore | file_business_runtime_check.go及file_business_schema_contracts.go，只读目录／权限检查；不修改迁移 | 3 |
| internal/filedownload | health.go及spool.go，运行状态、root／manifest无副作用检查 | 4 |
| internal/httpserver | file_content_modes.go方法；files.go拆本人状态／预约；file_download.go保留流；server.go及realtime.go健康衔接 | 5／6 |
| cmd/im-file-cleaner | repair_only.go／arguments.go，数据库工厂与专用循环；main.go分派 | 8 |
| internal/policystore外部测试包 | file_business_process_*_test.go正式进程、生命周期与安全测试 | 9～12 |
| internal/webclient/e2e | file_business_runtime.cjs正式代理浏览器驱动，复用现有模块的断言 | 12 |
| scripts／testdata | test-file-business-runtime.sh必选白名单；file-runtime/business-runtime.md夹具规则 | 9～13 |
| docs | 本计划、开发增量-P4-26-验收记录.md、企业IM-P4-26-附件运行配置.md、总开发路径 | 13 |

固定约定：新接口在其拥有任务定义；后续引用此处Interfaces，不重新命名。表中的新测试文件对应同名实现加_test.go；进程测试均为package policystore_test。每项5步，每个步骤一种动作；提交显式暂存该项Files及计划复选框，先git diff --cached --check，再用指定message。text代码块为待实现测试的断言DSL，变量由该项具名夹具产生，Go／JS测试按对应typed接口实现；夹具断言用现有helper，不打印敏感差异。最终七门禁前不宣称阶段通过。

## Task 1：严格业务配置与四项能力

**Files:** 新建cmd/im-api/file_business_config.go、file_business_config_test.go；修改main.go、main_test.go。

**Interfaces:** fileBusinessConfig{Enabled bool; Objects objectstore.Config; SpoolDir,OwnerID,ProbeVersionID string}；fileBusinessConfigFromEnv(getenv func(string)string,oidcEnabled bool)(fileBusinessConfig,error)；productionFileCapabilities(uploadEnabled,businessEnabled bool) httpserver.FileCapabilities。沿用fileUploadConfigFromEnv原签名。

- [x] **Step 1：写失败测试。** TestFileBusinessConfigStrict、TestFileBusinessDisabledNoDependencies、TestFileBusinessCredentialSeparation、TestProductionFileCapabilityMatrix；四组合精确四布尔，B非法／缺OIDC或DB／null及>1024字节版本／零或大写UUID／相对目录／访问键共享拒绝。B关闭getenv追踪新增下载字段访问次数=0；U关闭/B开启合法配置不读scanner字段。

~~~text
caps(false,false) == {false,false,false,false}
caps(true,false) == {true,false,false,false}
caps(false,true) == {false,true,true,true}
caps(true,true) == {true,true,true,true}
disabled.download_env_reads == 0; disabled.probe_requests == 0
~~~

- [x] **Step 2：验证RED。** go test ./cmd/im-api -run 'Test(FileBusiness|ProductionFileCapability)' -count=1；因新行为缺失FAIL。
- [x] **Step 3：最小实现。** 在配置文件定义上述接口；源固定download_environment，版本沿objectstore原非空／非null／UTF-8无控制／≤1024契约，错误固定invalid_file_business_configuration，不带字段值。这里只解析及产生能力，不启动服务。
- [x] **Step 4：验证GREEN。** 重跑Step2，具名测试PASS；旧TestFileAPIAssembly／默认关闭断言仍PASS。
- [x] **Step 5：提交。** feat(files): add strict file business runtime configuration。

## Task 2：下载只读适配器与固定版本探针

**Files:** 新建internal/objectstore/s3_readonly.go、s3_read_probe.go及同名测试；修改s3.go／s3_test.go，仅提取可共用客户端代码；store.go原Store不改。

**Interfaces:** ReadOnlyStore interface{Store; ValidateReadProbe(context.Context,string) error}；NewS3ReadOnly(Config)(ReadOnlyStore,error)。ValidateCapabilities、ReadVersion沿用Store签名。只读PutVersion／FindAttemptVersions返回files.ErrDependencyUnavailable、零网络请求。

- [x] **Step 1：写失败测试。** TestS3ReadOnlyCredentials、TestS3ReadOnlyMutationsDenied、TestS3ReadProbeStrict、TestS3ReadProbeBudget；核对下载签名主体、零上传回退、两拒绝方法零请求；固定key／VersionID／LF完整匹配；错版／null／截断／额外字节／>1KiB／重定向／超时拒绝，body恰关闭一次。正常业务ReadVersion仍校验返回版本与尺寸。

~~~text
probe.key == "_im_runtime/read-probe/v1"
probe.content == "enterprise-im-file-read-probe-v1" + LF
probe.version == configured_version; probe.read_bytes <= 1024
readonly.PutVersion.error != nil; readonly.FindAttemptVersions.error != nil
mutation_requests == 0; response_body_close_count == 1
~~~

- [x] **Step 2：验证RED。** go test ./internal/objectstore -run 'TestS3(ReadOnly|ReadProbe)' -count=1；缺新入口或行为FAIL。
- [x] **Step 3：最小实现。** 独立下载客户端固定download_environment及两凭据名；不要embedding暴露底层写方法。探针直接固定GetObject，不构造业务Location；先ValidateCapabilities再ValidateReadProbe由Task6调用，共用传输层及无重试／禁止重定向。探针只读最多1024字节，以期望长度+1侦测多余字节。
- [x] **Step 4：验证GREEN。** 重跑Step2及go test ./internal/objectstore -count=1；单测PASS，真实IAM证明留Task9，不把本项模拟服务当部署证据。
- [x] **Step 5：提交。** feat(files): add isolated read-only storage and versioned probe。

## Task 3：真实数据库契约与角色权限预检

**Files:** 新建internal/policystore/file_business_runtime_check.go、file_business_schema_contracts.go、file_business_runtime_check_test.go；只读db/migrations/000018～000021及原授权／审计SQL。

**Interfaces:** (Service).CheckFileBusinessRuntime(ctx context.Context) error；(Service).CheckFileDownloadAuditRuntime(ctx context.Context) error。两者无写入；后者检查修复必需子集，未要求删除权限。私有契约清单记录表列OID／类型、约束类别／关联列／引用表列、检查表达式和触发器函数关联／启用与延迟属性。

- [x] **Step 1：写失败测试。** TestFileBusinessRuntimeSchema、TestFileBusinessRuntimePrivileges、TestFileDownloadAuditRuntimeSchema；专属完整schema及真实API／repair角色通过。逐项移列／改类型、同名错误FK、NOT VALID CHECK／FK、禁或replica-only关键触发器、同名替换函数／绑定、终态成对非deferrable拒绝；撤销实际业务写入／audit序列权限拒绝。记录预检前后业务各表计数不变。

~~~text
valid_schema.check_error == nil; valid_repair_role.check_error == nil
same_name_wrong_fk.check_error != nil; disabled_trigger.check_error != nil
nondeferrable_terminal_pair.check_error != nil; missing_audit_grant.check_error != nil
business_counts_after == business_counts_before
~~~

- [x] **Step 2：验证RED。** 配置专属IM_TEST_DATABASE_URL，go test ./internal/policystore -run 'Test(FileBusinessRuntime|FileDownloadAuditRuntime)' -count=1；缺新检查FAIL，0SKIP。缺DB先准备隔离夹具，不计RED。
- [x] **Step 3：最小实现。** 从提交迁移列出实际SQL使用的file_objects／策略历史／来源／扫描／绑定／session／terminal／audit／保留／hold关系；用pg_catalog及has_table_privilege／has_sequence_privilege检查，约束实际定义规范化后与基线比较，触发器实际函数体及关联匹配；不只匹配名称。兼容默认search_path，通过当前角色命中的实际relation检查，不能查到其他schema冒充；无DDL及数据修复。
- [x] **Step 4：验证GREEN。** 重跑Step2，完整合法／逐项破坏场景PASS且0SKIP；恢复测试schema只操作本项专属数据库。
- [x] **Step 5：提交。** feat(files): validate runtime schema and database privileges。

## Task 4：下载目录运行健康与归属

**Files:** 新建internal/filedownload/health.go、health_test.go；修改spool.go／spool_test.go、service.go／service_test.go。

**Interfaces:** (*filedownload.Service).CheckHealth(ctx context.Context) error；私有checkSpoolHealth(ctx context.Context) error使用Service持有root／lock／owner。保持NewService、Prepare及Close原签名；记录原root的路径／inode信息供健康复核。

- [x] **Step 1：写失败测试。** TestDownloadRuntimeHealth、TestDownloadRuntimeRootReplacement、TestDownloadRuntimeHealthNoSideEffects；closed／failed／替换inode／符号链接／wrongUID或权限非0700／manifest owner改变拒绝。健康检查前后session／spool内容一致，未知条目不枚举删除；正常同owner重启只回收原完整来源，未知文件保留且启动失败。

~~~text
healthy.CheckHealth.error == nil
closed.CheckHealth.error != nil; replaced_root.CheckHealth.error != nil
wrong_owner.CheckHealth.error != nil
health.created_sessions == 0; health.removed_paths == 0
~~~

- [x] **Step 2：验证RED。** go test ./internal/filedownload -run 'TestDownloadRuntime' -count=1；缺健康或变化检测FAIL。
- [x] **Step 3：最小实现。** 状态在lifecycle锁下快照，root与持有FD及Lstat实际路径比较；只验证root和owner manifest，拒绝ctx取消，不重新claim、不创建目录或回收会话，不扫描客户文件。错误沿ErrUnavailable。
- [x] **Step 4：验证GREEN。** 重跑Step2及go test ./internal/filedownload -count=1；原Spool来源／锁／Prepared清理回归PASS。
- [x] **Step 5：提交。** feat(files): check download runtime ownership without side effects。

## Task 5：内容方法分派与只读本人状态

**Files:** 新建internal/httpserver/file_content_modes.go、file_content_modes_test.go；修改files.go／files_test.go及file_download_test.go。

**Interfaces:** HandlerWithFileContentModes(next http.Handler,uploadEnabled,businessEnabled bool) http.Handler；HandlerWithFileStatus(next http.Handler,auth Authenticator,svc FileMetadataService)(http.Handler,error)。原HandlerWithFileMetadata保持预约+状态行为；两个入口共用私有handlerWithFileMetadata(next http.Handler,auth Authenticator,svc FileMetadataService,reserveEnabled bool)(http.Handler,error)。不改原下载业务接口。

- [x] **Step 1：写失败测试。** TestFileContentModeMatrix、TestFileStatusWithoutReservation、TestFileContentModesResponseController；四组合GET／PUT及HEAD／OPTIONS／POST的状态和Allow精确等于规格§7；U=false/B=true PUT405 Allow=GET，不调用上传；GET开启原下载、关闭原503。本人状态合法／错身份、预约关闭、严格query/body；透传ResponseController.SetWriteDeadline及Flush。

~~~text
content(U=false,B=true,method=PUT) == {status:405,Allow:"GET"}
content(U=true,B=true,method=OPTIONS) == {status:405,Allow:"GET, PUT"}
content(U=false,B=false,method=GET).status == 503
status_only.reserve_calls == 0; response_controller.Flush.error == nil
~~~

- [x] **Step 2：验证RED。** go test ./internal/httpserver -run 'Test(FileContentMode|FileStatusWithout)' -count=1；新行为FAIL。
- [x] **Step 3：最小实现。** 最外层模式wrapper只识别content路径，处理关闭GET及非法方法，再让合法GET／PUT进入原handler，不包裹ResponseWriter。HandlerWithFileStatus仅装状态，不放行ReserveFile；B开启/U关闭的预约返回503／file_dependency_unavailable，复用现有依赖错误DTO；两者关闭仍不注册元数据路由。保留原authenticated身份／错误DTO，不对HTTP流增加压缩或缓存。
- [x] **Step 4：验证GREEN。** 重跑Step2及go test ./internal/httpserver -run 'Test(FileDownload|FileMetadata|FileContent|FileSearch)' -count=1；新旧HTTP合约PASS。
- [x] **Step 5：提交。** feat(files): dispatch content methods by assembled capabilities。

## Task 6：正式 API 启动、路由与共享就绪

**Files:** 新建cmd/im-api/file_runtime.go、file_routes.go及同名测试、file_runtime_startup_test.go；修改main.go／main_test.go；新建internal/httpserver/runtime_health.go、runtime_health_test.go，修改server.go／realtime.go／realtime_test.go。

**Interfaces:** fileRuntime{business fileBusinessConfig; uploadEnabled bool; transfer *filetransfer.Service; download *filedownload.Service; reader objectstore.ReadOnlyStore}；startFileRuntime(ctx context.Context,pool *pgxpool.Pool,getenv func(string)string,uploadEnabled bool,uploadObjects objectstore.Config,uploadSpool string,business fileBusinessConfig)(*fileRuntime,error)；(*fileRuntime).CheckHealth(context.Context) error；(*fileRuntime).Close() error。assembleFileRoutes(next http.Handler,auth httpserver.Authenticator,repo policystore.Service,admin access.Service,rt *fileRuntime)(http.Handler,error)。HandlerWithRuntimeReady(next http.Handler,checks ...Pinger)(http.Handler,error)为最终外层健康包装器，仅处理精确GET /health/ready。

- [x] **Step 1：写失败测试。** TestFileBusinessStartupOrder、TestFileBusinessStartupBudget、TestFileBusinessAssembly、TestRuntimeReadySharedBudget；fake依赖记录schema→私有桶／probe→spool→routes→listen，故障无listen、已有资源逆序close；30秒共享、不刷新旧15秒上限。四组合路由／能力一致，search三精确路径不被通用路由吞掉；只有一个通用会话handler。ready共用2秒、DB／启用Redis／fanout／reader／spool任一失败503恢复200、live不变且零业务写入。

~~~text
file_init.total_budget <= 30s; upload_check.budget <= 15s
startup_failure.listen_calls == 0; conversation_wrapper_count == 1
ready.total_budget <= 2s; ready.failure.status == 503; ready.recovered.status == 200
ready.business_write_calls == 0; live.status == 200
~~~

- [x] **Step 2：验证RED。** go test ./cmd/im-api ./internal/httpserver -run 'Test(FileBusiness(Startup|Assembly)|RuntimeReady)' -count=1；装配／健康行为FAIL。
- [x] **Step 3：最小实现。** startFileRuntime使用Task1～4；B开启时检查路径同inode及祖先重叠（包括已配置上传／扫描目录），before-listen阶段一次共享ctx。Uonly保留原预检。U或B装policy／history／own-status、预约只U；B开启使用HandlerWithFileMessages、FileSearch、FileDownload，否则原关闭。ContentModes最外于内容服务，search优先metadata／groups，capability最后按已完成装配产生。最终RuntimeReady组合pool.Ping、已启用Redis.Ping、fanout.Done检查、rt.CheckHealth；仅一个2秒ctx，不转发ready给内部重复预算。保留realtime原WS资格与故障检查、Web及文字/群管理。固定错误分类，无原始异常输出。
- [x] **Step 4：验证GREEN。** 重跑Step2及go test ./cmd/im-api ./internal/httpserver -count=1；B关闭所有旧关闭合约PASS。真实网络ready故障及无listen在Task9再证。
- [x] **Step 5：提交。** feat(files): assemble production file services and readiness。

## Task 7：退出协调与错误路径资源释放

**Files:** 新建cmd/im-api/lifecycle.go、lifecycle_test.go；修改main.go、file_runtime.go及其测试。

**Interfaces:** runAPI(ctx context.Context,getenv func(string)string,logger *slog.Logger) error，main只建信号ctx、调用并最终退出；shutdownAPI(server *http.Server,closers []func()error) error。fileRuntime.Close消费现有transfer.Close／download.Close；可测试私有shutdownAPIWithBudgets(server *http.Server,closers []func()error,httpGrace,closeGrace time.Duration) error，产品固定10秒／10秒。

- [x] **Step 1：写失败测试。** TestAPIShutdownActiveDownload、TestAPIStartupFailureClosesResources、TestAPIShutdownCleanupTimeout；阻塞真实连接+受控closer：先停接收／Shutdown，超时server.Close取消请求，后close；正常返回0，任一清理失败或10秒收尾超时error。listener／OIDC后续装配错误也逆序释放服务、锁、pool；不在函数内部os.Exit。Close恰一次，错误不输出目录／秘密。

~~~text
shutdown.http_budget == 10s; shutdown.close_budget == 10s
events.index("HTTP_CLOSE") < events.index("SERVICE_CLOSE")
startup_failure.listener_open == false; closer_calls_each == 1
cleanup_failure.exit_code != 0; cleanup_timeout.exit_code != 0
~~~

- [x] **Step 2：验证RED。** go test ./cmd/im-api -run 'TestAPI(Shutdown|StartupFailure)' -count=1；旧直接exit／等待问题FAIL。
- [x] **Step 3：最小实现。** 所有初始化资源登记closers；HTTP优先停止并强制取消剩余请求，服务关闭在单个有界收尾窗口，超时仍保留证据、上报失败。HTTP Serve异常也走同一退出路径。pool最后关，业务ctx/deadline不延长；故障close错误聚合固定类别。禁止通过修改DownloadTimeout或后台无界等待解决退出。
- [x] **Step 4：验证GREEN。** 重跑Step2及Task6命令，顺序／失败／超时PASS。SIGTERM／kill跨进程恢复留Task11证明。
- [x] **Step 5：提交。** fix(files): coordinate HTTP shutdown and download cleanup。

## Task 8：严格 repair-only 参数与数据库循环

**Files:** 新建cmd/im-file-cleaner/arguments.go、repair_only.go及同名测试；修改main.go／main_test.go。

**Interfaces:** cleanerArguments{Execute,Once,RepairOnly bool}；parseCleanerArguments(args []string)(cleanerArguments,error)；repairOperations interface{Repair(context.Context)(int,error); Close()}；newAuditRepair(ctx context.Context,getenv func(string)string)(repairOperations,error)；runAuditRepair(ctx context.Context,once bool,out io.Writer,ops repairOperations) error。cleanerFactories{cleanup func(context.Context,func(string)string)(cleanerOperations,error); repair func(context.Context,func(string)string)(repairOperations,error)}；runCleaner(ctx context.Context,args []string,getenv func(string)string,out io.Writer,factories cleanerFactories) error。原cleanerOperations／newCleaner保留删除路径，main及旧测试迁移工厂参数；repair不实现Step。

- [ ] **Step 1：写失败测试。** TestCleanerArgumentsStrict、TestAuditRepairOnlyNoObjectFactory、TestAuditRepairBudgetBackoff、TestAuditRepairCancellation；缺execute输出关闭且两工厂零调用；未知／重复／布尔矛盾／positional拒绝，包括--execute与--execute=false重复、--repair-only与--repair-only=false冲突。--execute --repair-only --once只Repair，limit20／ctx5秒，零Step／S3factory；失败连续延迟1/2/4/8/16/30/30，成功回1秒、取消即停止、once错误非零。时钟用私有测试注入，非产品环境开关。

~~~text
disabled.cleanup_factory_calls == 0; disabled.repair_factory_calls == 0
repair_only.step_calls == 0; repair_only.s3_calls == 0
repair.batch_limit == 20; repair.batch_budget == 5s
failed_delays == [1s,2s,4s,8s,16s,30s,30s]; cancelled.pending_timers == 0
~~~

- [ ] **Step 2：验证RED。** go test ./cmd/im-file-cleaner -run 'Test(CleanerArguments|AuditRepair)' -count=1；新模式及严格拒绝FAIL。
- [ ] **Step 3：最小实现。** 参数先解析再工厂；newAuditRepair仅pgxpool+随机机器UUID+Task3.CheckFileDownloadAuditRuntime，封装原RepairFileDownloadAudit(ctx,owner,20)。每批5秒ctx，循环用可取消timer退避，结构化输出只固定status及计数；原delete模式语义及租户开关不改，不为repair构造worker/deleter。
- [ ] **Step 4：验证GREEN。** 重跑Step2及go test ./cmd/im-file-cleaner -count=1；旧关闭／once删除门禁PASS；真实恰一次审计及零S3留Task11。
- [ ] **Step 5：提交。** feat(files): add database-only download audit repair mode。

## Task 9：正式进程夹具、四组合与依赖门禁（RP01／02／12／13）

**Files:** 新建internal/policystore/file_business_process_helpers_test.go、file_business_process_configuration_test.go；新建scripts/test-file-business-runtime.sh、testdata/file-runtime/business-runtime.md。

**Interfaces:** fileBusinessProcessFixture{privateRoot,apiURL,webURL,buildSHA,schema string; binaries map[string]string; processes map[string]*exec.Cmd; pool *pgxpool.Pool; oidc,proxy *httptest.Server}；newFileBusinessProcessFixture(t *testing.T)*fileBusinessProcessFixture；(*fileBusinessProcessFixture).startAPI(t *testing.T,upload,business bool,node string)；startWorkers(t *testing.T)、restartAPI(t *testing.T,node string,upload,business bool)、stopOwned(t *testing.T)、assertEvidence(t *testing.T)。nodeRuntimeConfig{URL,OwnerID,SpoolDir string}及fixture.nodes map[string]nodeRuntimeConfig记录两个API配置，节点名固定api-a／api-b。接口无业务Handler／Repo替换字段。TestFileBusinessProcessRP01／TestFileBusinessProcessRP02／TestFileBusinessProcessRP12／TestFileBusinessProcessRP13为四个必选顶层名称。

- [ ] **Step 1：写失败场景。** 从同一源码构建im-api／im-file-worker／im-outbox-worker／im-file-cleaner，独立TLS OIDC与同源代理、PG schema/API及repair角色、私有S3四主体、真实scanner/Redis。RP01实际四组合及方法；RP02缺依赖／探针／schema／目录错误无监听且非零；RP12真实DB／S3拒绝或不可达→ready503、live200→恢复200、scanner停止不伪ready；RP13直接SDK实际IAM允许与拒绝、匿名／跨桶拒绝、双进程同spool冲突。RF1／2／4在正式API再次验证。

~~~text
RP01.capabilities == actual_route_matrix
RP02.exit_code != 0; RP02.listening == false
RP12.ready_fault.status == 503; RP12.live_fault.status == 200; RP12.ready_recovered.status == 200
RP13.real_download_role.PUT == AccessDenied; RP13.real_download_role.DELETE == AccessDenied
RP13.second_process_shared_spool.exit_code != 0
~~~

- [ ] **Step 2：验证RED。** go test -json -timeout=30m ./internal/policystore -run '^TestFileBusinessProcessRP(01|02|12|13)$' -count=1；故障行为尚未可复现或验收缺口FAIL，不能以未安装依赖制造RED；若Task1～8已满足，只记录新增回归通过，不伪造失败。
- [ ] **Step 3：实现夹具与严格门禁。** 新脚本只接受run-all，必需环境缺失退出2、不SKIP；输出必须私有绝对无符号链接目录。配置仅子进程环境、不得打印。必需环境沿旧夹具：IM_TEST_DATABASE_URL、IM_TEST_REDIS_URL、IM_TEST_S3_ENDPOINT、IM_TEST_S3_BUCKET、IM_TEST_S3_POLICY_BUCKET、IM_TEST_FILE_UPLOAD_ACCESS_KEY／SECRET_KEY、IM_TEST_FILE_WORKER_ACCESS_KEY／SECRET_KEY、IM_FILE_CLEANUP_S3_ACCESS_KEY／SECRET_KEY、IM_TEST_QPDF_PATH、IM_TEST_CLAMD_SOCKET、IM_TEST_SCANNER_MANIFEST、IM_TEST_BROWSER_NODE、CHROMIUM_EXECUTABLE；新增IM_TEST_FILE_DOWNLOAD_ACCESS_KEY／SECRET_KEY及IM_TEST_FILE_BOOTSTRAP_ACCESS_KEY／SECRET_KEY。IM_TEST_FILE_BUSINESS_OUTPUT_DIR可指定私有输出，否则mktemp生成；输入schema／桶需专属归属校验。预检与Task13引导说明一致。准备固定探针、记录VersionID供两API复用，禁下载角色自建；编译四binary一次并校验SHA，测试帮助器复用旧OIDC／migration技术但不复用补挂Handler的newWebFileFixture。两节点分别owner／spool，严格资源注册／退出回收。脚本Go JSON解析必选14名称，任何缺失／FAIL／SKIP／非零退出均失败。
- [ ] **Step 4：验证GREEN。** 重跑Step2，四顶层0FAIL／0SKIP；脚本全14白名单直到Task12完成才通过，不得缩小白名单临时宣布通过。
- [ ] **Step 5：提交。** test(files): exercise official process configuration and dependency gates。

## Task 10：正式四类型生命周期及双节点幂等（RP03～06／10）

**Files:** 新建internal/policystore/file_business_process_lifecycle_test.go；修改Task9 helpers；复用旧样本生成及原中文／FEFF断言，不改变产品测试Handler。

**Interfaces:** TestFileBusinessProcessRP03／TestFileBusinessProcessRP04／TestFileBusinessProcessRP05／TestFileBusinessProcessRP06／TestFileBusinessProcessRP10；fileBusinessSample{Filename,MediaType,Path string}；(*fileBusinessProcessFixture).uploadAndAwaitReady(t *testing.T,conversation,kind string,sample fileBusinessSample) files.Metadata 经真实HTTP及独立扫描Worker；(*fileBusinessProcessFixture).assertBinding(t *testing.T,fileID,clientID string)只读核对数据库与对象证据。

- [ ] **Step 1：写失败场景。** RP03四类型×单群预约／PUT／scan／sendACK／Outbox Redis通知／typed补拉／名称搜索／下载字节与名称；RP04有效非参与人／跨tenant／管理员持链零正文且拒绝audit、401另计；RP05 EICAR／坏或加密PDF／MIME伪装／>25MiB及租户更小上限实际拒绝、非ready不可绑定；RP06先U/B开启形成文件，再重启Ufalse/Btrue，GET/search可用、预约/PUT关闭及tenant policy仍约束发送；RP10两API原UUID/body重试恰一attachment/message/outbox，duplicateACK一致，真实通知及离线typed恢复。

~~~text
RP03.clean_files_per_conversation == 4; RP03.download_sha256 == sample_sha256
RP04.valid_login_denial_audits >= 1; RP04.unauthorized_content_bytes == 0
RP05.not_ready_attachment_count == 0; RP06.PUT.status == 405
RP10.same_client_uuid.message_count == 1; RP10.attachment_count == 1; RP10.outbox_count == 1
~~~

- [ ] **Step 2：验证RED或新增回归。** go test -json -timeout=30m ./internal/policystore -run '^TestFileBusinessProcessRP(03|04|05|06|10)$' -count=1；不把环境失败当产品RED，已满足场景如实记录新增回归。
- [ ] **Step 3：实现真实驱动和证据。** ready必须来自正式im-file-worker，不能直接SQL置ready或test ScanWorker.RunOnce；Outbox必须正式im-outbox-worker，通知到达与typed内容分开核对。故障使唯一真实请求失败，不补造成功响应。证据只记录ID／计数／hash／状态／期限，浏览器显式保存由Task12补齐RP03。
- [ ] **Step 4：验证GREEN。** 重跑Step2，5顶层及子例0FAIL／0SKIP，逐项来源／audit／字节证明齐全；RP03的Chrome保存未完成时状态仍是部分证据。
- [ ] **Step 5：提交。** test(files): verify official attachment lifecycle and node idempotency。

## Task 11：在途撤权、进程恢复与审计修复（RP07～09）

**Files:** 新建internal/policystore/file_business_process_recovery_test.go；修改Task9 helpers。

**Interfaces:** TestFileBusinessProcessRP07／TestFileBusinessProcessRP08／TestFileBusinessProcessRP09；(*fileBusinessProcessFixture).signalAPI(t *testing.T,node string,signal os.Signal)；restartRepair(t *testing.T,once bool)；waitTerminal(t *testing.T,sessionID string)。证据查询沿既有下载终态／机器审计表，不修改业务事务。

- [ ] **Step 1：写失败场景。** RP07实际JWT到期、hard_deny、任职／上传者停用、退群再入gap、TTL／计划策略，客户端已接字节与后续禁止输出分列；RP08活跃流SIGTERM≤10+10秒退出、kill后同owner回收合法来源、未知／错误owner保留拒绝、两节点独立锁；RP09停止修复并产生真实audit未结算→同file拒绝→repair-only restart/once恢复，terminal配对／机器audit恰一，无S3请求／清理作业变化。RF3及RF5包括目录替换、提交后杀repair、长期DB故障及取消。

~~~text
RP07.after_revocation.additional_content_is_authorized == false
RP08.SIGTERM_exit_duration <= 20s; RP08.unknown_file_preserved == true
RP09.machine_terminal_audit_count == 1; RP09.session.audit_acked == true
RP09.repair_s3_requests == 0; RP09.cleanup_jobs_after == RP09.cleanup_jobs_before
~~~

- [ ] **Step 2：验证RED或新增回归。** go test -json -timeout=30m ./internal/policystore -run '^TestFileBusinessProcessRP(07|08|09)$' -count=1；新产品差异须先具名失败，不能用测试自行伪造结算通过。
- [ ] **Step 3：实现进程故障驱动。** 启动可观测限速代理但不模拟业务服务；修改真实DB权限／策略产生故障，恢复所有修改仅限本项schema。故障后按原60秒下载deadline／租约等待process_lost/unknown，修复实际调用official binary；S3审计和数据库只读计数同时证明零删除，HTTP shutdown错误不能被测试隐藏。引用原在途每chunk／一秒期限，不宽松放大窗口。
- [ ] **Step 4：验证GREEN。** 重跑Step2，3顶层0FAIL／0SKIP；真实PID退出、锁重开、合法孤儿／未知保留、审计恰一次与机器原因字段证据齐全。
- [ ] **Step 5：提交。** test(files): verify revocation shutdown and audit-only recovery。

## Task 12：Chrome正式链路、未知结果与正常预算（RP11／14，补RP03）

**Files:** 新建internal/policystore/file_business_process_browser_test.go、file_business_process_budget_test.go、internal/webclient/e2e/file_business_runtime.cjs；修改Task9 helpers及RP03测试接入浏览器。

**Interfaces:** TestFileBusinessProcessRP11／TestFileBusinessProcessRP14；(*fileBusinessProcessFixture).browser(t *testing.T,scenario string,options map[string]any)；新CJS从标准输入接私有fixture配置，按既有BrowserNode调用风格输出脱敏断言结果。复用既有file_lifecycle／context／unknown／download fault／policy断言，不修改其P4-25语义。

- [ ] **Step 1：写失败场景。** RP11完整正常proof的501绑定／第21会话，正式API在5秒预算内有界续查，旧损坏500／Unicode回归；RP14七阶段上下文取消、冻结原UUID／body核对、ACK与补拉分离、大整数CAS、真实通知+typed共同等待、四类下载传输故障。RP03改由真实Chrome做四类型单群上传至显式保存，中文／FEFF与文件hash核对；网络代理只能实际请求处理后制造未知或传输故障。

~~~text
RP03.browser_saved_sha256 == sample_sha256
RP11.query_duration < 5s; RP11.candidates_per_page <= 500; RP11.conversations_per_page <= 20
RP14.retry_client_uuid == original_client_uuid; RP14.retry_body == frozen_body
RP14.context_cancel.stale_ui_updates == 0; RP14.bad_download.saveable_blob_count == 0
~~~

- [ ] **Step 2：验证RED或新增回归。** go test -json -timeout=30m ./internal/policystore -run '^TestFileBusinessProcessRP(03|11|14)$' -count=1；具名缺口如实失败，旧Web6模块不为制造RED破坏。
- [ ] **Step 3：实现浏览器／预算夹具。** HTTPS同源proxy仅转发正式API；PKCE及当前任职来自实际身份接口，browser令牌不落证据。501数据允许正常服务建立合法DB绑定／扫描事实，仅作为预算证明；不声称501真实对象扫描，实际Worker证明来自RP03/05。记录业务SQL量／候选量／耗时，不重设statement/lock总限。长下载／超长／截断／压缩／超时只在真实请求后注入，零可保存错误Blob，保留原未知槽。
- [ ] **Step 4：验证GREEN。** 重跑Step2及scripts/test-file-business-runtime.sh run-all，14必选顶层及全部子例0FAIL／0SKIP；若任一旧浏览器场景无法通过正式进程，修正缺口，不改代理造成功。
- [ ] **Step 5：提交。** test(files): gate official browser flows and bounded filename queries。

## Task 13：固定源码七门禁、一次整体评审与中文交付

**Files:** 新建docs/开发增量-P4-26-验收记录.md、docs/企业IM-P4-26-附件运行配置.md；修改本计划、docs/企业IM-开发计划与实施路径-v0.1.md、testdata/file-runtime/business-runtime.md。

**Interfaces:** 交付记录绑定完整40位产品SHA及git archive；记录每门禁实际退出／顶层／子例／包计数、辅助SKIP、Linux非JSON资源结果、RP01～14证据位置和生产未验条件。PR正文中文，以P4-25为base。

- [ ] **Step 1：运行冻结前差异检查。** 对照规格§1～14与下面覆盖表，无产品依赖／迁移新增，无秘密，diff --check通过。确认14RP真实PASS及本轮资源注册完整；缺证据先修对应任务，不把未验写成PASS。
- [ ] **Step 2：固定产品SHA并运行七门禁。** commit之后git archive到私有独立目录；按下列顺序，同源码、专属schema／桶／目录，保存所有失败／取消尝试，不共享环境并发运行。计数从JSON及脚本实际输出统计，所有进程必须结束才认结果。

~~~sh
go test -json -timeout=30m ./... -count=1
go test -json -race -timeout=30m ./internal/policystore ./internal/access ./internal/httpserver ./internal/filedownload ./internal/filetransfer ./internal/objectstore ./internal/realtime ./internal/outbox ./internal/webclient ./cmd/im-api ./cmd/im-file-worker ./cmd/im-file-cleaner -count=1
scripts/test-web-files.sh run-all
scripts/test-file-download-retention.sh run-all
scripts/test-file-messages.sh run-all
scripts/test-file-runtime.sh run-all
scripts/test-file-business-runtime.sh run-all
~~~

- [ ] **Step 3：一次整体独立评审及必要修正。** 沿既定方法使用requesting-code-review，一个fresh reviewer审固定产品SHA和规格／计划／实测证据；Critical／Important一轮具名RED→GREEN，必要代码修正提交后重新archive并重跑七门禁，不能只报定向通过；不派第二reviewer。没有finding时直接记录原报告。Minor独立列，旧P4-24分类后续项不混入。
- [ ] **Step 4：形成中文交付并自检。** 运维说明给出新旧环境矩阵、独立角色最小权限、探针引导／VersionID、稳定owner／目录部署预检、启动顺序、暂停上传与关闭业务／修复／物理删除独立命令、恢复及退出失败处理。验收分列原F01有效登录拒绝audit、生命周期、故障与未知边界；客户生产未验清单保持。证据目录0700／文件0600并生成hash；核对归属后停止本轮进程／容器，数据卷保留或删除分别记录，主工作区clean／前项ref不变。
- [ ] **Step 5：提交与草稿交付。** docs(files): record P4-26 official process acceptance；推本专属分支并创建base=codex/p4-25-web-files-search-design草稿PR，正文用结构化参数或body-file，关联当前任务，记录URL／最终提交；不合并部署。若外部推送受阻，保留所有本地结果并明确阻塞动作，不伪造PR。

## 规格与验收覆盖自检

| 规格节／必选场景 | 实施归属 |
| --- | --- |
| §1～3完成边界／已有服务 | Task6／9／13，禁止测试Handler替代 |
| §4开关／能力／配置 | Task1／5／6，RP01／02／06 |
| §5只读角色／探针 | Task2／9，RP02／12／13 |
| §6启动／schema／权限／预算 | Task3／6／9，RP02 |
| §7路由／方法／既有期限 | Task5／6／12，RP01／03／11／14 |
| §8ready／依赖恢复／scanner停机 | Task4／6／9，RP12 |
| §9归属／退出／恢复 | Task4／7／11，RP08／13 |
| §10审计专用修复 | Task3／8／11，RP09 |
| §11RP03完整单群／RP04 F01／RP05坏内容／RP06 U关闭／RP10双节点 | Task10；RP03 Chrome由Task12补齐 |
| §11RP07在途资格／RP08退出／RP09未结算 | Task11 |
| §11RP11正常预算／RP14未知与上下文 | Task12 |
| §12七门禁／一次评审／草稿交付 | Task13 |
| §13配置／运维／边界，§14交接 | Task13及本计划 |
| RF1／RF2／RF3／RF4／RF5 | Task1+9／2+6+9／4+7+11／5+6+9／8+11 |

计划自检包括所有规格节／14RP归属、跨任务接口一致、五类Review Focus各有具名测试、每项RED→最小实现→GREEN→提交及范围比例；测试片段为断言要求，不是已执行证据。审阅确认本计划后，使用executing-plans按当前助手逐项实施，不再次选择执行方式。
