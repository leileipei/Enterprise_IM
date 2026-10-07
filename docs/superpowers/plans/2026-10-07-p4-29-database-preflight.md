# P4-29 目标库只读冲突预检实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付单租户、九表的只读存量冲突比较命令，完整区分新增、一致和冲突，不回显人员内容、不执行导入。

**Architecture:** 共享离线规范化和安全文件读取，独立实现纯比较、固定报告及 PostgreSQL 快照读取。新命令的数据库运行部分置于同一可执行文件的私有子进程中，以专用环境隔离驱动默认配置；子进程读取一次输入并执行完整流程，父进程只处理受限报告和退出。全部库存读取使用同一 REPEATABLE READ READ ONLY 事务，结束事务后进行有界模型比较。

**Tech Stack:** Go 1.27.1；现有锁定 pgx/v5 v5.11.0、x/sys v0.30.0；PostgreSQL 16；已有 Python 验证脚本模式。不新增／升级产品依赖。

**Spec:** [已确认的 P4-29 书面规格](../specs/2026-10-06-p4-29-database-preflight-design.md)。用户于 2026-10-07 回复“确认”。执行方式沿用当前助手逐项实现；仅最终一次整体只读评审使用评审代理，不派实现子代理。

## Global Constraints

- 当前工作树 `/Users/leo.cui/.codex/worktrees/p4-29-database-preflight-design/企业IM系统`，分支 `codex/p4-29-database-preflight-design`。不重新创建工作树，不改原目录或已交付草稿。
- 实施开始前在此分支显式 cherry-pick ACK 生产修复 `2019c240ccf03357d52c713dcf742c21ddeb41cb` 与测试修复 `7148b5ec787745784a74bb11f98e866c8048f529`；以 P4-27 `96c32564bdda035de74647573a51af9be3e0d6bc` 为功能基线。不挑入 P4-28 的文档提交，不更新 PR #76／#77，不合入 main。
- 文件输入保持 10 MiB、10,000 行、16 层、单字符串 4,096 字节；九表全部出现、内部引用闭合，只接受一个已存在目标租户。
- 库存九表合计 20,000 行、字段 UTF-8 内容合计 64 MiB、每字符串 4,096 字节；合并最多 30,000 行；每批最多 1,000 个全局键；全程 SQL 最多 128 条。
- 连接最多 3 秒、SQL 最多 5 秒、读输入起共享 30 秒；子进程使用父进程同一绝对 deadline，不重置预算；取消／诊断／清理共享最多 1 秒额外预算。
- 最多展示 200 个诊断、报告最多 256 KiB；截断不减少总数和冲突行统计。读取不完整时分类计数全部 null，不输出暂时通过。
- 仅显式 `IM_IMPORT_COMPARE_DATABASE_URL`（最多 8 KiB）及 `IM_IMPORT_COMPARE_SCHEMA`；schema 默认 public，ASCII 小写字母起始，其后小写／数字／下划线，1～63 字节。
- 连接键仅 host、port、dbname、user、password、sslmode、sslrootcert、sslcert、sslkey、connect_timeout。TCP 显式端口 1～65535、TLS verify-full；Unix socket 缺 port 固定 5432，才允许 disable；connect_timeout 仅 1～3 秒。拒绝未知／重复／多目标键、service、options、search_path、PG* 与默认凭据回退。
- 专用有效角色：SELECT 九表、无九表 INSERT／UPDATE／DELETE／TRUNCATE，无 superuser／BYPASSRLS；表为普通持久表、无 RLS；唯一键文本 collation deterministic。产品不执行 DDL、DML、任意 SQL、临时表、审计写入或身份重绑定。
- scope=`database_snapshot_insert_compatibility`，validation_profile=`group_identity_database_v1`；identity_provider_checked、additional_database_rules_checked、write_concurrency_checked、import_authorized 恒 false。
- 不修改原离线命令的输出／上限／离线性质，不增加 REST、迁移、Worker、客户接入或部署。本轮规划不运行以上集成或产品测试。

## 实现中锁定的设计细节

1. pgx 的 `ConnStringAllowedKeys` 仅限制显式连接串，不限制 PG* 和默认 home 文件；依据本地锁定源码 `pgconn/config.go`。不能通过普通 ParseConfig 再擦除 Config 字段补救已经发生的 service／凭据文件读取。
2. 私有工作者仍是 `im-import-compare` 同一个二进制：父进程以固定私有 argv[0] 和 fd 3 的有界 deadline 控制管道启动，不增加公共参数；公开 `--worker` 等参数仍拒绝。子进程环境只含两个显式配置及固定 `TZ=UTC`，不带 HOME、PG*、其他 IM 配置、GODEBUG、PATH；不修改父进程全局环境。绝对可执行文件路径由 os.Executable 获取，工作目录保持调用者目录。
3. 父进程先处理参数／help／version，普通运行才启动工作者；文件只由工作者读取一次。父进程缓冲至多 256 KiB + 1 字节，禁止透传子进程 stderr；只转发经过固定报告契约核验的完整 JSON。工作者和父进程的可恢复异常、控制管道无效及非法子报告统一转 DATABASE_READ_FAILED／incomplete／2；取消用 CANCELED、超时用 TIMEOUT；stdout 写入失败只在有界 stderr 写固定 OUTPUT_WRITE_FAILED 并退出 2，不能承诺已经断开的 stdout 收到 JSON。
4. 同一事务在九表固定顺序取得 ACCESS SHARE 表锁后核对结构和读取；不取行写锁，不阻塞正常 DML。表锁稳定并发 DDL 的结构，锁等待受 5 秒及父预算限制；所有锁在只读事务结束时释放，SQL 数量包含它们。
5. 库存 SQL 的文本投影先在服务端按 UTF-8 字节检查单格 4,096 字节，再传递受限值与异常标志；不能先把超大字段完整扫描到 Go 再判限。读取 profile 同时要求 server_encoding=UTF8，避免按其他编码字节数判定。

## Review Focus

- RF1：污染的 PGSERVICEFILE／PGPASSFILE／HOME 证书与降级 TLS 不能改变目标、触发隐式文件读取或泄露；任务 5、7、8 的实际进程测试负责。
- RF2：20,000 行／64 MiB 边界、超大单格和终止过程中不能返回部分零冲突，全部分类必须 null；任务 6、7、8 负责。
- RF3：库存开始较晚的区间冲突、合并内部行号及超过 200 条明细不能丢掉输入冲突行；任务 1、4 负责。
- RF4：全局 issuer＋subject 或 UUID 被其他租户占用时，只暴露输入位置及固定码，角色／RLS 不能隐藏占用导致假通过；任务 4、6、8 负责。
- RF5：九表间并发提交／DDL／撤销读取权限不能混成一个有效报告；任务 6、8 的真实数据库并发验证负责。

## 文件与类型归属

| 文件组 | 职责 |
| --- | --- |
| `internal/importpreflight/{validate,normalize,collector,schema}.go`，新增 `reuse.go` | 返回规范化文档，提供固定字段描述／单行校验及未截断诊断观察；仍无数据库依赖 |
| `internal/importinput/{read,open_unix,open_unsupported}.go`；旧 CLI `input.go` 适配 | 共享同一次安全本地读取，兼容原文件与平台测试 |
| `internal/importcompare/{report,collector,compare,origins,config,dsn}.go` | 固定报告、纯追加比较、输入来源映射、纯显式配置解析 |
| `internal/importcompare/{driver,profile,profile_contracts,postgres,budget}.go` | 清洁环境内的驱动配置、只读权限／结构核对、九表及全局键读取 |
| `cmd/im-import-compare/{main,arguments,run,process_unix,process_unsupported}.go` | 公共参数、私有工作者、单 deadline／报告输出／信号 |
| 各组件同名 `_test.go`、`internal/importcompare/postgres_helpers_test.go` | 行为单测、实际 DB／进程夹具；测试写入与产品读取分开 |
| `scripts/test-import-compare.py`、`scripts/test_import_compare_gates.py` | 固定源码、独占无网络 PG、TLS、门禁计数／失败归档及清理 |
| `README.md`、`docs/开发增量-P4-29-验收记录.md`、本计划及规格 | 用法、边界、真实运行证据和执行状态 |

所有新增类型／接口由下列任务首次定义；不在后续任务另造同义接口。新报告字段必须有明确 JSON tag，私有连接／库存类型没有 JSON 导出或日志功能。

---

### Task 1：共享规范化结果与完整模型诊断

**Files:** 修改 `internal/importpreflight/validate.go`、`normalize.go`、`collector.go`；新增 `reuse.go`、`reuse_test.go`。

**Interfaces:**
- `EvaluateDocument(ctx context.Context, raw []byte) (Report, *Document)`：仅文件 valid 且完整时返回文档；旧 `Evaluate` 委托它并仍返回完全相同 Report。
- `TableSchema{Entity Entity; Fields []FieldSchema}`、`FieldSchema{Name Field; Kind string; Nullable bool}`；`Schema() []TableSchema` 返回按原声明顺序的深复制，Kind 只为 text／uuid／time／bool。
- `NormalizeRecord(ctx context.Context, entity Entity, ordinal int, values map[Field]json.RawMessage) (Record, []Issue, error)`：共享原字段默认／标量逻辑，无顶层行数提升。
- `ValidateModel(ctx context.Context, doc Document, collector *Collector) error`：调用现有 BuildIndex／CheckReferences／CheckGraphs／CheckIntervals，不重复模型规则。
- `NewObservingCollector(observe func(Issue)) *Collector`：每个去重后的 issue 首次 Add 时观察，包含超过明细 200 的 issue；旧 NewCollector 行为不变。

- [x] **Step 1：写 RED。**`TestReuseDocumentParity` 比较原样本的 Report 字节、SHA 和 74 行结果；`TestReuseObserverBeyond200` 制造 201 个不同诊断，观察器收到 201、报告明细 200、总数 201；`TestReuseSingleRecord` 对默认值／NULL／微秒／4097 字节检查；`TestReuseSchemaCopy` 修改返回副本不影响下一次 Schema。现有最大输入与原报告回归继续保留。

  代表性测试断言（变量及 mustEncode／runRealBinary 等为该测试文件内的夹具助手）：

```go
r, doc := EvaluateDocument(ctx, sampleRaw)
if doc == nil || r.Status != "valid" || !bytes.Equal(mustEncode(r), baselineReportBytes) { t.Fatal("offline parity") }
```

- [x] **Step 2：观察失败。**运行 `go test ./internal/importpreflight -run '^TestReuse' -count=1`，确认缺失新接口导致失败；补齐最小测试桩后必须看到行为 RED，不能只凭编译错误计逻辑覆盖。
- [x] **Step 3：最小实现。**共享原来的规范化与模型步骤，旧错误排序／去重／预算不变；观察器在保留完整去重状态之后、200 明细限制之前执行，不能从 sorted() 的截断结果重建冲突计数。
- [x] **Step 4：验证 GREEN。**新增用例通过；原离线单元测试通过，DB oracle 另在任务 8 实际执行。`go list -deps ./internal/importpreflight` 不出现 pgx／Redis／对象存储客户端。
- [x] **Step 5：提交。**仅提交本任务实现和测试，消息 `refactor: expose reusable offline validation results`。

### Task 2：提取共享安全读取并保持旧 CLI

**Files:** 新增 `internal/importinput/read.go`、`open_unix.go`、`open_unsupported.go`、`read_test.go`；修改旧 CLI `input.go`；移除旧 `open_unix.go`／`open_unsupported.go` 的重复实现。

**Interfaces:** `type Reader func(context.Context,string) ([]byte,*importpreflight.Issue,error)`；`importinput.Read(context.Context,string) ([]byte,*importpreflight.Issue,error)`；为旧测试保留 `OpenRegular(string) (*os.File,error)`、`VerifyOpened(os.FileInfo,*os.File) error` 的共享实现。旧 CLI 的 readInput／openRegular／verifyOpened 变为薄适配，inputReader 签名不变。

- [x] **Step 1：写 RED。**`TestSharedRead` 验证普通文件字节、10 MiB／超一字节、FIFO／链接／路径替换、同 deadline 取消、错误无路径标记；复用旧 CLI 既有文件和进程测试，新增两调用方读取同样文件得到同样结果。

  代表性测试断言（变量及 mustEncode／runRealBinary 等为该测试文件内的夹具助手）：

```go
raw, issue, err := Read(ctx, regularFile)
if err != nil || issue != nil || !bytes.Equal(raw, wantBytes) { t.Fatal("same protected read") }
```

- [x] **Step 2：观察失败。**`go test ./internal/importinput -run '^TestSharedRead' -count=1`，保留实际文件行为 RED。
- [x] **Step 3：最小实现。**移动已有 openat／NOFOLLOW／fstat／有界读逻辑，拒绝不支持的平台，不降低保证；不增加数据库配置或第二次打开计算哈希。
- [x] **Step 4：验证 GREEN。**`go test ./internal/importinput ./cmd/im-import-preflight -count=1` 通过；实际 SIGTERM／关闭 stdout 继续用现有进程用例；`GOOS=windows GOARCH=amd64 go build ./cmd/im-import-preflight` 可构建且读取路径固定拒绝。
- [x] **Step 5：提交。**消息 `refactor: share protected import file reader`，不提交生成二进制。

### Task 3：新报告和运行状态契约

**Files:** 新增 `internal/importcompare/report.go`、`collector.go`、`report_test.go`、`test_helpers_test.go`。

**Interfaces:**
- `Stage string`（file／database）；`Issue{Stage Stage; Issue importpreflight.Issue}`，显式 MarshalJSON 展开 stage 及原诊断字段。
- `Classification{New *int; Identical *int; Conflict *int}`；`Classifications map[string]Classification`，固定九表及 total。
- `Report` 完整字段名／JSON tag 与规格 §7 一致；`FileStatus importpreflight.Status`，`FileChecksComplete bool`，原文件 Counts 与 SHA 不重定义；导出字段 ClassificationCounts Classifications 对应 classification_counts。
- `RowRef{Entity importpreflight.Entity; Row int}`；`Snapshot{TenantFound bool; Data importpreflight.Document; GlobalKeys map[RowRef]bool; SQLCount int}`，只用于内部比较，不序列化输出。
- `SnapshotReader` 接口：`Read(context.Context,string,importpreflight.Document) (Snapshot,error)`。
- `Collector` 新报告诊断收集器：`NewCollector() *Collector`、`Add(Issue)`、`Issues() []Issue`、`Total() int`；排序 file 在前，再按原固定元组，保留前 200。
- `BuildReport(file importpreflight.Report, status importpreflight.Status, complete bool, classes Classifications, collector *Collector) Report`，显式 status 不由 complete 推断；complete=false 时强制全部分类 null／checks_complete=false／database_checked=false；`Incomplete(file importpreflight.Report, stage Stage, code importpreflight.Code) Report` 追加固定阶段运行诊断并调用 BuildReport(status=incomplete, complete=false)；`Report.ExitCode() int`、`EncodeReport(Report) ([]byte,error)`。

- [x] **Step 1：写 RED。**`TestCompareUnitReportContract` 断言 false 四标志、固定 profile／scope、0／1／2 退出语义；`TestCompareUnitIncompleteCounts` 传入已有非零临时分类而 complete=false，仍全部 null；`TestCompareUnitReportPrivacy` 任意动态值不能进入固定 enum；`TestCompareUnitReportTruncation` 总数 201、明细 200、稳定字节；未开始文件时用 file_status=incomplete、未知 Counts／SHA=null。

  代表性测试断言（变量及 mustEncode／runRealBinary 等为该测试文件内的夹具助手）：

```go
r := BuildReport(fileReport, "invalid", false, nonzeroClasses, collector)
if r.ExitCode() != 1 || r.DatabaseChecked || r.ClassificationCounts["total"].New != nil { t.Fatal("invalid is not partial success") }
```

- [x] **Step 2：观察失败。**`go test ./internal/importcompare -run '^TestCompareUnitReport|^TestCompareUnitIncomplete' -count=1`。
- [x] **Step 3：最小实现。**新增报告与旧 Report 分开；输入 invalid／租户不符／目标不存在采用 incomplete comparison 状态而整体 invalid，退出 1；运行 Failure 固定原因码采用整体 incomplete／2；读入及文件阶段 Failure 为 file，配置及之后为 database。所有字段、枚举、空值及排序来自规格。
- [x] **Step 4：验证 GREEN。**相同输入同条件报告字节一致；非法结构、超 256 KiB 和伪造枚举返回固定失败，不把 unknown enum 输出；类型／字段与后续任务接口核对。
- [x] **Step 5：提交。**消息 `feat: define database comparison report contract`。

### Task 4：追加兼容比较和输入来源映射

**Files:** 新增 `internal/importcompare/compare.go`、`origins.go`、`compare_test.go`、`origins_test.go`。

**Interfaces:** `Compare(ctx context.Context, input importpreflight.Document, snapshot Snapshot) (Classifications,*Collector,error)`；私有 `origin{InputRows []int; Stored bool}` 以 entity／组合 Ordinal 为索引，不用库存行号构造公共报告。

- [x] **Step 1：写 RED。**`TestCompareUnitClassify` 覆盖一致／新增／全部声明字段差异、默认值、NULL≠空字符串、时间偏移、UUID 大小写；`TestCompareUnitGlobalKeys` 只输出 GLOBAL_KEY_CONFLICT；`TestCompareUnitStoredUnique` 覆盖 code／工号／同用户同 issuer；`TestCompareUnitReverseInterval` 将库存与输入起点先后交换，均定位原输入；`TestCompareUnitMoreThan200Rows` 201 个冲突输入仍全部归 conflict；`TestCompareUnitStoredInvalid` 返回 DATABASE_DATA_INVALID，无库存行内容。

  代表性测试断言（变量及 mustEncode／runRealBinary 等为该测试文件内的夹具助手）：

```go
classes, issues, err := Compare(ctx, input201, snapshot201)
if err != nil || *classes["total"].Conflict != 201 || len(issues.Issues()) != 200 { t.Fatal("truncation changed classification") }
```

- [x] **Step 2：观察失败。**运行上述 `TestCompareUnit` 组，确认 RED 针对比较与来源定位，而非仅接口不存在。
- [x] **Step 3：最小实现。**先独立验证库存模型；按固定主键和字段比较，相同记录去重但关联输入别名，差异及全局／业务键占用排除候选。构造库存＋其余候选并用完整观察器检查模型：若 issue 在库存侧而 related 在输入侧，投影到该输入；双方输入时保留原 related_row；各输入涉及冲突均更新行集合。不能只处理保留下来的 200 项；无法归因的库存问题返回固定 Failure。最后按原输入集合计三类，保证每表与 total 数量守恒。
- [x] **Step 4：验证 GREEN。**输入／库存排列变化不改变定位与分类；历史 ended／suspended、半开邻接、主任职、部门包含、组织／部门环逐项通过；共享 context 取消返回 error，不带部分分类；200 限制只作用于明细。
- [x] **Step 5：提交。**消息 `feat: compare additive tenant data against stored records`。

### Task 5：显式连接解析与驱动隔离要求

**Files:** 新增 `internal/importcompare/config.go`、`dsn.go`、`driver.go`、`config_test.go`。

**Interfaces:** `Config` 使用私有 connectionSettings 与导出 `Schema string`；`LoadConfig(getenv func(string) string) (Config,error)` 仅读取两个指定变量；`ParseDSN(string) (connectionSettings,error)` 为纯解析，无驱动／文件／网络调用；私有 `driverConfig(Config) (*pgx.ConnConfig,error)` 仅在私有工作者的清洁环境调用。Config 的 String／GoString 固定脱敏，不返回连接详情。

- [x] **Step 1：写 RED。**`TestCompareUnitExplicitConfig` 对 URI／keyword 两种格式、显式必填项、schema 63／64 字节、8 KiB／超限、IPv6、Unix 缺 port、转义密码、TLS 和 timeout 断言；`TestCompareUnitNoAmbiguousDSN` 检查所有未知／重复键，包括被后值覆盖的 service／passfile／ssl alias，均拒绝；`TestCompareUnitDriverEnvironmentGuard` 在 PG*／HOME 非空的非工作者环境拒绝直接使用驱动，且无文件／网络副作用；`TestCompareUnitConfigRedaction` 不含标记。

  代表性测试断言（变量及 mustEncode／runRealBinary 等为该测试文件内的夹具助手）：

```go
_, err := LoadConfig(pollutedGetenvWithExplicitServiceDSN)
if err == nil { t.Fatal("service must be rejected before driver parsing") }
```

- [x] **Step 2：观察失败。**`go test ./internal/importcompare -run '^TestCompareUnitExplicit|^TestCompareUnitNoAmbiguous|^TestCompareUnitDriver|^TestCompareUnitConfigRedaction' -count=1`。
- [x] **Step 3：最小实现。**纯 parser 先逐个 raw key 校验，再组装 canonical 连接串，自己处理重复和 alias；不将原始 DSN直接交给驱动。清洁工作者中使用 pgx.ParseConfigWithOptions 的明确键白名单，清空 RuntimeParams／Fallbacks，QueryExecModeExec，无 statement／description 缓存、无默认密码文件或 service，TLS 不降级；设置最大 3 秒超时。驱动原始错误全部映射 fixed Failure，不保存 DSN 到 error。
- [x] **Step 4：验证 GREEN。**纯解析在污染 PG* 环境下结果与正常一致，进程全局 env 原值不变；直接驱动误用被拒绝；真实清洁进程＋TLS 行为由任务 7／8 接续验证，不把纯 parser PASS 作为已隔离。
- [x] **Step 5：提交。**消息 `feat: require explicit import comparison connection settings`。

### Task 6：真实 PostgreSQL 只读快照与结构权限

**Files:** 新增 `internal/importcompare/postgres.go`、`profile.go`、`profile_contracts.go`、`budget.go`、`postgres_test.go`、`postgres_helpers_test.go`。

**Interfaces:** `PGReader{Config Config}` 实现 SnapshotReader；私有 `queryBudget` 包装全部 Begin／SET／LOCK／Query／Rollback，计尝试数，最多 128；返回 Snapshot.SQLCount 仅给测试使用。`newCompareFixture(t *testing.T) *compareFixture` 提供专属 owner／schema／只读角色／单租户闭合样本；fixture 只在 IM_TEST_DATABASE_URL 指向本轮容器时启用，产品永不读取这个测试变量。

- [x] **Step 1：写 RED。**`TestComparePGSnapshotReadOnly` 核对九表前后内容摘要及专用角色真实 DML 被拒绝；`TestComparePGIsolation` 在首表读后从第二连接提交变更，剩余读取仍同旧快照；`TestComparePGProfile` 覆盖缺权限／有 DML／super／bypass／RLS／view／foreign／缺字段或错误类型／缺键和区间排斥结构／非 deterministic collation／非 UTF8；`TestComparePGLimits` 检查 20,000／20,001 行、64 MiB／超限、4097 单格、infinity／BC 时间及第 129 次 SQL。

  代表性测试断言（变量及 mustEncode／runRealBinary 等为该测试文件内的夹具助手）：

```go
snapshot, err := reader.Read(ctx, tenantID, input)
if err != nil || snapshot.SQLCount > 128 || !snapshot.TenantFound { t.Fatal("bounded complete snapshot") }
if beforeDigest != afterDigest { t.Fatal("business data changed") }
```

- [x] **Step 2：观察失败。**由本轮实际 PG 夹具执行 `go test ./internal/importcompare -run '^TestComparePG' -count=1`；未配置导致 SKIP 不能充当 RED。仅夹具迁移、填充与 oracle 写入，产品路径保持只读。
- [x] **Step 3：最小实现。**开始 REPEATABLE READ READ ONLY，固定 schema 引用与 pg_catalog 函数；取得九表 ACCESS SHARE 后检查模式、角色、对象、列及约束。必要 profile 列取任务 1 Schema；PK／unique／FK／四个区间排斥结构取基线 000001／000002／000004，按类型、字段与作用域核对，接受后续迁移的额外非投影列，不靠输入 baseline_commit 授权。表／字段只来自固定描述，经 pgx.Identifier 引用；禁止插入式字符串数据值。
- [x] **Step 4：验证 GREEN。**每表 SQL LIMIT=剩余行数+1，文本先用服务端 UTF8 字节门限投影，逐行累加字段内容，时间有限且 0001～9999；转换用 NormalizeRecord。全局输入键每批≤1,000，参数化查询只返回输入位置与外租户占用 bool，完整核对结果数。结束事务及连接后才返回 Snapshot；缺目标租户只返回 TenantFound=false。真实权限／DDL 改变、超时、撤销权限、锁等待／取消异常都返回固定 Failure；不泄露库存原文或部分 Snapshot。
- [x] **Step 5：提交。**消息 `feat: read consistent tenant snapshots with bounded readonly access`。

### Task 7：新 CLI、私有工作者、预算和输出

**Files:** 新增 `cmd/im-import-compare/main.go`、`arguments.go`、`run.go`、`process_unix.go`、`process_unsupported.go`、`arguments_test.go`、`run_test.go`、`process_test.go`。

**Interfaces:** `Run(context.Context,[]string,io.Writer,io.Writer) int` 为父进程入口；私有 `runWorker(ctx context.Context,args []string,stdout,stderr io.Writer,reader importinput.Reader,getenv func(string)string,provider func(importcompare.Config) importcompare.SnapshotReader) int`，直接消费任务 2 的 Reader 类型和任务 3 的 SnapshotReader。私有 fd 3 消息只含 deadline UnixNano，最大 128 字节；argv[0] 固定 `im-import-compare-worker`，没有新的公开 worker 参数。

- [x] **Step 1：写 RED。**`TestCompareProcessHelpVersion` 不调用 env／reader／DB；`TestCompareProcessNoNetwork` 无效／多租户／选择不符文件不实际连接；`TestCompareProcessEnvIsolation` 以污染 PG*／HOME 默认文件和故意可达诱饵目标运行，报告相同、诱饵零连接；`TestCompareProcessSignalAndPipe` 验证实际 SIGTERM、短写、stdout关闭、缺 fd／异常工作者输出／worker panic 的 2／incomplete；`TestCompareProcessDeadline` 父 deadline 比30秒短时工作者共享它。

  代表性测试断言（变量及 mustEncode／runRealBinary 等为该测试文件内的夹具助手）：

```go
result := runRealBinary(t, poisonedEnvironment, validInputArgs)
if result.ExitCode != 0 || decoyConnections.Load() != 0 || !bytes.Equal(result.Stdout, cleanReportBytes) { t.Fatal("environment changed target") }
```

- [x] **Step 2：观察失败。**先实际构建新命令，再执行指定 TestCompareProcess；保存真实进程 RED，不以 fake provider 的计数替代 env／连接／信号验收。fake reader／provider 仅用于内部状态优先级单测。
- [x] **Step 3：最小实现。**固定 usage=`usage: im-import-compare --input FILE --tenant-id UUID | --help | --version\n`、version=`im-import-compare 0.1.0\n`。父 context 最大30秒，私有子进程只带两个配置／TZ，不带 HOME、PG*；fd 控制消息无凭据，禁止凭据 argv。工作者先 Read＋EvaluateDocument＋租户选择，再 LoadConfig＋PGReader＋Compare＋BuildReport。公共 unknown／duplicate／mixed flags 拒绝，不将私有 argv[0] 当作用户权限。
- [x] **Step 4：验证 GREEN。**父只接受单份、≤256 KiB、无未知／重复字段、enum 合法且退出语义一致的完整 Report，再 EncodeReport 输出；子 stderr 只作为未公开的失败状态，不转发原文。两层 SIGPIPE 忽略、可恢复 panic 固定失败；取消先终止 DB／工作者，1秒总宽限后 kill＋Wait回收，输出 incomplete，分类全部 null。unsupported 平台正常 help／version，普通读取固定拒绝。P4-27 离线输出仍逐字节一致。
- [x] **Step 5：提交。**消息 `feat: add isolated readonly import comparison command`。

### Task 8：固定源码数据库对照、兼容门禁与交付

**Files:** 新增 `scripts/test-import-compare.py`、`scripts/test_import_compare_gates.py`、`internal/importcompare/postgres_oracle_test.go`；修改 README、规格状态、本计划账本；新增 `docs/开发增量-P4-29-验收记录.md`。旧 P4-27 脚本保留历史 ACK 排除说明，不改旧记录为通过。

**Interfaces:** `python3 scripts/test-import-compare.py --commit SHA --output DIR`：输出目录必须新建；固定源码归档与来源哈希；manifest 区分 required_gates_passed、full_suite_passed、customer_acceptance=not_executed。脚本 `validate_required_gates(events: list[dict]) -> dict` 返回必需门禁布尔值和具名失败／跳过，正测试数及测试组齐全才允许 true；同目录验证器测试直接调用它。`IM_COMPARE_TEST_BINARY` 仅用于测试实际命令调用；工作者不继承它。

- [x] **Step 1：写 RED。**`TestComparePGOracle` 对同一个输入＋库存构造追加候选，产品比较与夹具真实 INSERT 的 PK／unique／FK／区间结果对照；图／父区间包含等应用模型规则由具名纯模型测试核对，不能声称 PostgreSQL 原有约束自动拒绝所有这些问题；额外自定义约束证明 known-compatible 不代表真实 INSERT 全部可用。脚本验证器用有 FAIL／SKIP／零测试／错误数量守恒／未清理资源结果验证不能通过必需门禁；异常进程输出只保留固定原因，不保存令牌或凭据。

  代表性测试断言（变量及 mustEncode／runRealBinary 等为该测试文件内的夹具助手）：

```python
bad = validate_required_gates(events_with_skip)
assert bad["required_gates_passed"] is False
assert manifest_with_full_suite_fail["full_suite_passed"] is False
```

- [x] **Step 2：观察失败。**运行 oracle 与验证器 RED；fixture 必须专属，本轮容器、角色、schema 有 owner 标签／清单。不得连接客户或现有业务库，不能删除其他工作树、容器或凭据。
- [x] **Step 3：最小编排。**git archive 固定代码提交；宿主构建 macOS arm64／Linux arm64 两命令及测试二进制；缓存 PG16 镜像 network=none、无主机端口、tmpfs；在容器内部配置本轮 CA／服务器证书并实测 verify-full 及错 CA／主机名拒绝，不安装 Go／Node 到运行镜像。Linux测试启动前也清除 PG*／HOME，driver默认凭据实验使用专属合成文件。每阶段独立日志、hash、版本和查询计数，不覆写失败尝试。
- [x] **Step 4：验证 GREEN。**必需门禁：纯库／真实进程／真实PG权限与快照／TLS／追加oracle／资源边界；P4-27离线CLI与oracle（原37顶层及对应子用例保持，不机械当作新总数）；P4-28消息回归及完整OIDC包（不排除原失败HTTP ACK用例）；构建／vet及相关host race。另实际运行完整go test ./...，逐名记录缺夹具失败／跳过；新必需门禁要求0FAIL、0SKIP、正测试数，但全仓失败时 full_suite_passed=false，不声称合入就绪。最大允许数据必须在30秒内完整通过或明确incomplete；不可放宽deadline掩盖失败。
- [x] **Step 5：评审和交付。**一次整体独立只读代码评审；有实际问题先按 receiving-code-review 验证修正，对受影响代码重新RED／GREEN及最终固定提交门禁。确认新提交以后只有文档改动时产品SHA不变。保存中文验收、角色／schema／容器删除确认、SHA256与失败列表；普通推送独立分支、创建／关联draft PR（base依赖P4-27分支并说明ACK已显式集成），不修改旧PR或自动合并／部署。

## 自检与执行交接

- 规格§1～2由任务3／4／7／8覆盖；§3由5／6／7覆盖；§4由1／2／5／6／7覆盖；§5由1／4／6覆盖；§6由2／3／5／6／7／8覆盖；§7由3／4／7覆盖；§8由1／2／5／6／7覆盖；§9的11项均进入上述RED／GREEN与固定门禁；§10保留写入、身份源与生产验收边界。
- RF1→5／7／8；RF2→6／7／8；RF3→1／4；RF4→4／6／8；RF5→6／8。没有将库内行号、200明细截断或PG默认文件行为留给实现者猜测。
- 接口来自任务1～3的定义；Reader 类型由任务2首次定义、任务7直接消费；BuildReport显式接收status、完整性，Incomplete显式接收stage。pgx只进入importcompare；旧预检模块不引入DB客户端；所有诊断值来自固定枚举。
- 本次仅编写及自检计划并更新已确认规格的状态，没有cherry-pick、实现产品、迁移、安装依赖、启动服务或运行门禁。
- 按writing-plans交接规则，用户审阅本计划后沿用当前助手＋executing-plans逐任务执行；不重新询问执行方式，不把规格确认当作已确认尚未出现的实施计划。

## 2026-10-07 执行完成记录

用户确认后，当前助手依次完成8任务。最终代码与门禁源码367d80a；一次整体独立只读评审的两项Important验收缺口已一轮RED→GREEN补齐，无延期Minor。必需门禁全部0FAIL/0SKIP，完整套件105具名失败/825跳过按名保留。客户/生产验收未执行；只交付草稿、不自动合并。见 [中文验收记录](../../开发增量-P4-29-验收记录.md)。上文计划编写阶段的“未执行”描述保留为历史状态，执行证据以本节及验收为准。
