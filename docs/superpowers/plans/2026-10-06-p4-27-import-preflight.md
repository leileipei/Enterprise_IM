# P4-27 离线组织与身份数据预检实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. 沿用当前助手逐项实施，最后一次整体独立评审。

**Goal:** 提供读取本地 JSON 的组织／身份预检命令，输出有界、不回显人员信息的确定性报告。

**Architecture:** 严格解析先保留词法与字段存在性，再按固定模型规范化标量并检查关系和区间。诊断通过同一收集器排序、去重与截断；CLI 使用安全文件读取和贯穿全流程的取消上下文，不装配任何数据库或身份源客户端。

**Tech Stack:** 仓库 Go 1.27.1，标准库及已锁定的 `golang.org/x/sys/unix`；数据库对照测试沿用已锁定的 pgx。不新增依赖或改变版本，不执行 go mod tidy。

**Spec:** [已确认书面规格](../specs/2026-10-06-p4-27-import-preflight-design.md)，规格提交 `47ba846e271644078ab5a64eceffdeeef64c5d00`，基线 main `80424dbd5a537023c33e56654f4a12b522885a21`。

## Global Constraints

- 产品只读取 `--input` 指定的普通本地文件，并将 JSON 报告写至 stdout；拒绝链接及特殊文件，不接受 stdin、网络 URL、SQL、CSV、写入或自动同步。
- 参数仅 `--input`、`--help`、`--version`，重复／混用／未知参数退出 2，不回显值。版本首版为 `0.1.0`。
- 原始输入最大 10 MiB；读取最多上限加一字节；JSON 深度最大 16；九表合计最大 10,000 条；字符串值最大 4,096 UTF-8 字节。
- 展示最多 200 个问题，继续统计全部去重问题；完整序列化报告最多 256 KiB。
- 从处理输入开始共享 10 秒，上下文覆盖读取、扫描、索引、图、排序及输出前准备；取消／超时为 incomplete／2。
- 九表与 62 个输入字段按规格 §4.1。无标记必填，`?` 可缺失／null，带默认值字段可缺失但不能显式 null。
- 元数据按规格 §4；重复键、未知字段、BOM、非法 UTF-8／surrogate、尾随内容拒绝，不能由解码器静默替换。
- UUID 按值比较，不限制版本；业务文本不 trim／折叠／规范化，issuer／subject 的空白检查仅去掉两端 U+0020。
- 时间是带时区 RFC3339，0001～9999 年、最多六位小数；区间左闭右开，历史状态记录也参与排斥校验。
- 报告仅含统计、哈希、固定诊断位置及代码；始终 database_checked=false、identity_provider_checked=false、import_authorized=false。
- Linux／macOS 提供受保护打开；其他平台明确拒绝，不降级。只使用同一次读取的原始字节校验和计算 SHA-256。
- 保持 go.mod／go.sum、现有业务 API、数据库迁移及生产开关不变；不读取 `IM_*` 服务连接配置。

## Review Focus

1. **RF1 默认值与 Unicode：**转义等价重复键、未配对 surrogate、显式 null、UUID 大小写、NEL 与普通空格不能被静默转换；Task 2、3 固定解析和标量断言。
2. **RF2 依赖歧义：**重复 UUID、跨租户复合外键、缺失任职使引用不可解析时不得任选一条；Task 4 校验根因及派生问题边界。
3. **RF3 历史与区间：**ended 行仍排斥，等价时区与相邻微秒边界一致，部门主任职和授权 Scope 都有独立覆盖；Task 5 固定见证。
4. **RF4 超限、取消与截断：**未知字段内部嵌套、巨大无效输入、10,000 层组织链、超过 200 个问题仍计数且确定性；Task 1、2、4、6、7 覆盖。
5. **RF5 隐私与安全 I/O：**未知字段名、文件路径和底层异常不能进入报告；打开期间替换为链接／FIFO不能阻塞或跟随；Task 1、6、7 覆盖。

规格 RF6 的打开／输出失败归入 RF5；无网络装配及同一字节哈希另由 Task 6、7 验证。

## 文件和职责

| 文件 | 职责 |
| --- | --- |
| `internal/importpreflight/report.go`, `collector.go` | 报告 DTO、固定枚举、诊断去重／排序／截断 |
| `internal/importpreflight/schema.go` | 九表字段顺序、类型、默认值、枚举、空值及键声明 |
| `internal/importpreflight/json_scan.go`, `decode.go` | 有界词法校验、严格对象／数组读取、保留字段存在性 |
| `internal/importpreflight/scalars.go`, `normalize.go` | UUID、文本、时间、类型及默认值规范化 |
| `internal/importpreflight/index.go`, `references.go`, `graphs.go` | 主键／唯一键索引、复合引用、迭代图校验 |
| `internal/importpreflight/intervals.go`, `validate.go` | 排斥／包含、完整流水线与结果状态 |
| `cmd/im-import-preflight/arguments.go`, `input.go` | 参数与共享上下文中的有界读取 |
| `cmd/im-import-preflight/open_unix.go`, `open_unsupported.go` | 平台安全打开，不支持平台拒绝 |
| `cmd/im-import-preflight/run.go`, `main.go` | 输出、退出码、信号及入口；无业务服务装配 |
| 上述文件相邻的 `_test.go` | 各组件及真实进程测试 |
| `internal/importpreflight/testdata/sample_data_v1.json`, `fixtures_test.go` | 固定虚构样本、快照变异与大数据构造 |
| `internal/importpreflight/postgres_oracle_test.go` | 仅测试使用的真实数据库对照 |
| `scripts/test-import-preflight.py` | 冻结源码门禁、专属 Linux／PG 夹具及清理 |
| `README.md`, `docs/开发增量-P4-27-验收记录.md` | 用法、边界和新执行证据 |

所有下述新文件都只在 `codex/p4-27-import-preflight` 工作树创建。样本源为已交付包中的 `sample_data.json`；复制前比对该包 SHA256SUMS，测试样本不携带源目录路径。

### Task 1：报告与有界诊断收集器

**Files:** 创建 `internal/importpreflight/report.go`, `collector.go`, `schema.go`, `report_test.go`, `collector_test.go`。

**Interfaces:**
- 固定字符串别名类型 `Entity`, `Field`, `Code`, `Status`，合法值仅来自规格；不得从未知输入成员名构造诊断枚举。`Issue` 含 Entity、Row、Field、Code、RelatedRow，行号 0 在 JSON 中表示 null；不包含记录值。
- `Failure` 含固定 Code，实现 error 接口时只返回该代码，不能保存或输出路径与系统原始错误；ctx 错误映射 CANCELED／TIMEOUT。
- `Counts` 为 `map[string]*int`，固定九表名及 `total`，每值可空；`Outcome` 包含 Status、ChecksComplete、InputSHA256、Counts。
- `Report` 公开 Status、ChecksComplete、InputSHA256、Counts、ErrorsTotal、Issues、IssuesTruncated，编码附加规格的固定边界字段。测试 helper `hasCode(r Report, code Code) bool` 仅查固定诊断代码。
- `NewCollector() *Collector`; `(*Collector).Add(Issue)`; `BuildReport(Outcome, *Collector) Report`; `EncodeReport(Report) ([]byte, error)`。
- `Report.ExitCode() int` 按 valid／invalid／incomplete 返回 0／1／2；固定 profile／scope／三个 false 由 BuildReport 设置，不由输入传入。
- schema.go 声明规格 §4.1 的 Entity／Field 顺序和字段描述。后续任务只扩展描述的类型／键判定，不能重复声明字段顺序。

- [ ] **Step 1：写失败用例。**`TestReportBoundaryAndExitCodes` 断言三个状态、空计数及固定边界；`TestCollectorSortedDedupAndTruncation` 乱序加入 251 个不同位置问题及重复项，errors_total=251、issues=200、truncated=true，保留规格排序最前 200 项；重复项不增加数量。`TestReportNoDynamicValues` 断言 issue 的序列化键集合不允许人员内容；`TestReportEncodingBound` 构造报告超限，EncodeReport 返回固定错误且无可用输出。

```go
c := NewCollector()
for i := 251; i > 0; i-- {
    c.Add(Issue{Entity: Entity("users"), Row: i, Field: Field("id"), Code: Code("PK_DUPLICATE")})
}
r := BuildReport(Outcome{Status: Status("invalid"), ChecksComplete: true}, c)
if r.ErrorsTotal != 251 || len(r.Issues) != 200 || !r.IssuesTruncated { t.Fatal("wrong truncation") }
```
- [ ] **Step 2：运行 RED。**`go test ./internal/importpreflight -run 'TestReport|TestCollector' -count=1`；应因缺实现或上述断言失败，不把测试夹具错误计为有效 RED。
- [ ] **Step 3：实现接口。**使用最多 200 项的最大堆保留排序最小项；去重只保存固定枚举及整数构成的位置键，不保存输入值或超出上限的报告对象。所有排序含 related_row，编码使用固定 DTO，完整编码后检查 256 KiB；禁止向错误消息拼接业务值。
- [ ] **Step 4：验证 GREEN。**重跑同命令，全部通过；加入 map 插入顺序扰动及 nil／默认数组序列化，字节稳定。
- [ ] **Step 5：提交本任务文件。**提交信息 `feat: add bounded preflight diagnostic reports`。

### Task 2：严格 JSON 解析

**Files:** 创建 `json_scan.go`, `decode.go`, `json_scan_test.go`, `decode_test.go`；按 Task 1 schema 描述使用已声明字段名。

**Interfaces:**
- `RawRow` 为 `map[Field]json.RawMessage`；`RawDocument` 含 `Header map[string]json.RawMessage` 与 `Tables map[Entity][]RawRow`，数组位置保持原样。
- `DecodeRaw(ctx context.Context, raw []byte, c *Collector) (RawDocument, bool, error)`；bool 为文件结构完整，固定解析问题由 Collector 接收；error 仅用于父取消／超时等运行失败。
- 不创建任意 JSON DOM：先做有界词法扫描，再按固定根／tables／记录结构用 decoder 读取 RawMessage；每层只保存已知字段和有上限的记录。

- [ ] **Step 1：写失败用例。**`TestDecodeStrictKeysAndEnvelope` 覆盖重复键、`id` 与 `\u0069d`、未知顶层／表／记录字段、九表缺失、null／非数组表、非对象记录、尾随第二文档和非字符串 key。`TestDecodeUnicodeLexemes` 覆盖非法 UTF-8、BOM、未配对 surrogate 与合法配对，不能接受替换后的 U+FFFD。`TestDecodeDepthAndRowBudget` 固定深度 16／17、10,000／10,001 条和未知结构内超深；`TestDecodeCancellation` 固定取消时返回运行错误、没有完整成功结构。

```go
c := NewCollector()
_, complete, err := DecodeRaw(context.Background(), []byte(`{"format_version":1,"format_version":1}`), c)
r := BuildReport(Outcome{Status: Status("invalid")}, c)
if err != nil || complete || !hasCode(r, Code("DUPLICATE_JSON_KEY")) { t.Fatal("duplicate key accepted") }
```
- [ ] **Step 2：运行 RED。**`go test ./internal/importpreflight -run 'TestDecode' -count=1`，核对失败对应解析契约。
- [ ] **Step 3：实现接口。**扫描原始字节时区分字符串与转义，检查 UTF-8／surrogate／深度并周期性检查 context；解码对象时以解码后键名查重，未知键只报告 `_unknown`。RawMessage 保留 format_version 字面量和 null／缺失的区别；结构不成立时不进入模型。记录计数在追加前检查，不能先分配超限完整数组。
- [ ] **Step 4：验证 GREEN。**重跑上述用例；加入所有标量 JSON 类型、合法空白及随机未知字段名标记，诊断不回显键名／内容。
- [ ] **Step 5：提交本任务文件。**提交信息 `feat: strictly decode preflight JSON inputs`。

### Task 3：字段规范化与模型标量

**Files:** 创建 `scalars.go`, `normalize.go`, `scalars_test.go`, `normalize_test.go`, `fixtures_test.go` 及固定 JSON testdata；完成 `schema.go` 字段类型、默认值和枚举描述。

**Interfaces:**
- `Value` 含有效性、空值及规范化后的 Text／Bool／Time；UUID 的 Text 是规范带连字符小写值，其他文本保留原值。
- `Record` 含 `Ordinal int` 与 `Values map[Field]Value`；`Document` 含 `Tables map[Entity][]Record` 及通过验证的元数据。
- `Record.Text(Field) (string, bool)`、`Record.Bool(Field) (bool, bool)`、`Record.Time(Field) (*time.Time, bool)` 返回规范化值及是否有效；Time 的 nil／true 表示合法空结束时间，不是非法时间。
- 测试 helper `sampleBytes(t *testing.T) []byte`、`sampleDocument(t *testing.T) Document` 每次读取／构造独立样本，不共享可变记录；`fixtureRows(t *testing.T, total int) []byte` 构造含一租户和 total-1 个唯一人员的完整九表文件，其他数组为空。
- `Normalize(ctx context.Context, raw RawDocument, c *Collector) (Document, Counts, bool, error)`；bool 表示结构／元数据成立。每字段 Valid 保留依赖判定依据，不用零 UUID／零时间掩盖非法字段。
- `normalizeUUID(string) (string, bool)`; `parseTimestamp(string) (time.Time, bool)`；时间函数只接受规格格式与微秒精度。

- [ ] **Step 1：写失败用例。**`TestNormalizeSampleAndDefaults` 核对样本 74 条／62 字段、省略默认字段与显式默认等价、可空字段缺失与 null 等价。`TestNormalizeNullAndTypes` 覆盖默认字段显式 null、缺必填、format_version=1.0、元数据类型和 baseline 格式。`TestScalarsUUIDTextAndTime` 覆盖 UUIDv5、大小写、非法 UUID、NUL、ASCII 空格与 NEL 的 btrim 区别、保留 Unicode／空字符串合法业务字段、最多六位小数、七位拒绝、闰日及无时区拒绝。`TestStringByteBudget` 覆盖 4,096／4,097 字节以及多字节字符。

```go
id, ok := normalizeUUID("AE5368D5-748F-56C0-A4F8-1D486BA4E6CA")
if !ok || id != "ae5368d5-748f-56c0-a4f8-1d486ba4e6ca" { t.Fatal("UUID normalization") }
if _, ok := parseTimestamp("2026-10-01T00:00:00.0000001Z"); ok { t.Fatal("precision silently rounded") }
```
- [ ] **Step 2：运行 RED。**`go test ./internal/importpreflight -run 'TestNormalize|TestScalars|TestString' -count=1`。
- [ ] **Step 3：实现接口。**按 schema 描述逐字段规范化；保留合法业务文本，不调用通用 TrimSpace 或自动合并。按原文件数组位置报告，非法字段只填 Valid=false，不制造正常默认值。reference_time 只验证格式，不以系统 Now 过滤历史状态。
- [ ] **Step 4：验证 GREEN。**重跑，核对数据不被修改；测试字段清单与固定迁移／规格枚举及样本字段集一致，created_at 不进入允许列表。
- [ ] **Step 5：提交本任务文件。**提交信息 `feat: validate preflight scalar model contracts`。

### Task 4：唯一键、复合引用与组织图

**Files:** 创建 `index.go`, `references.go`, `graphs.go`, `index_test.go`, `references_test.go`, `graphs_test.go`。

**Interfaces:**
- `BuildIndex(ctx context.Context, doc Document, c *Collector) (*Index, error)`；Index 区分不存在、唯一有效和歧义目标，不因重复而覆盖原行。
- `(*Index).Find(entity Entity, key ...string) (Record, bool)` 使用对应表主键顺序（通常 id，身份表为 issuer／subject）；缺失、非法或歧义返回 false。
- `CheckReferences(ctx context.Context, doc Document, idx *Index, c *Collector) (Relations, error)`；Relations 记录每条引用及依赖是否可用，供区间包含判断使用。
- `Relations.Valid(entity Entity, row int, field Field) bool` 只对完整匹配的有效引用返回 true，未检查／缺失依赖为 false。
- `CheckGraphs(ctx context.Context, doc Document, idx *Index, relations Relations, c *Collector) error`；图边只能来自有效、同 Scope 的引用。

- [ ] **Step 1：写失败用例。**`TestIndexCompositeUniqueness` 对应 DB01／02／11，另覆盖跨租户相同主键、UUID 大小写等价、部门 code 的组织边界。`TestReferencesScopesAndAmbiguity` 对应 DB03／05／06，覆盖各复合外键、缺失及重复目标，禁止任选一条；同一无效引用不得再产生派生时间／类型问题。`TestGraphsCyclesAndCrossLegalDivision` 对应 DB04，覆盖组织／部门自环、长环、有效跨法人父组织。`TestDeepGraphBudgetAndCancellation` 以总计不超过 10,000 条的长链检验迭代算法及父取消。

```go
doc := sampleDocument(t)
copyRow := doc.Tables[Entity("users")][0]
copyRow.Ordinal = len(doc.Tables[Entity("users")]) + 1
doc.Tables[Entity("users")] = append(doc.Tables[Entity("users")], copyRow)
idx, err := BuildIndex(context.Background(), doc, NewCollector())
key, _ := copyRow.Text(Field("id"))
if err != nil { t.Fatal(err) }
if _, ok := idx.Find(Entity("users"), key); ok { t.Fatal("ambiguous target selected") }
```
- [ ] **Step 2：运行 RED。**`go test ./internal/importpreflight -run 'TestIndex|TestReferences|TestGraphs|TestDeepGraph' -count=1`。
- [ ] **Step 3：实现接口。**主键按表全文件范围索引；外部身份的主键是 issuer／subject，其他唯一键按规格 §5.1。复合引用逐字段比对 tenant／organization，不以单 UUID 找到目标即接受。迭代颜色状态检查环；自环只报告 SELF_PARENT，其他环对每个参与节点报告 TREE_CYCLE，指向环的前缀不是环节点。
- [ ] **Step 4：验证 GREEN。**重跑；扰动字段输入顺序、无效主键及依赖组合，Collector 的总数与排序稳定，所有循环定期检查 context。
- [ ] **Step 5：提交本任务文件。**提交信息 `feat: check preflight identity references and organization graphs`。

### Task 5：任职区间与完整预检流水线

**Files:** 创建 `intervals.go`, `validate.go`, `intervals_test.go`, `validate_test.go`。

**Interfaces:**
- `CheckIntervals(ctx context.Context, doc Document, idx *Index, relations Relations, c *Collector) error`。
- `Evaluate(ctx context.Context, raw []byte) Report`：验证原始大小、按同一字节分块计算哈希、DecodeRaw → Normalize → BuildIndex → CheckReferences → CheckGraphs → CheckIntervals → BuildReport。每次运行使用独立 Collector，不存全局输入状态。
- 结构／元数据失败为 invalid 且 checks_complete=false；模型问题扫描完为 invalid 且 checks_complete=true；取消／超时为 incomplete，不能返回先前部分成功。

- [ ] **Step 1：写失败用例。**`TestIntervalsHistoricalAndAdjacent` 对应 DB07／08，含 ended／suspended 重叠、同一 UTC 时刻不同偏移、相邻微秒、开结束区间。`TestDepartmentIntervalsAndPrimary` 对应 DB09／10，含部门主职重叠、父区间包含和非法依赖跳过。`TestGrantScopeAndHistoricalData` 检查角色／scope 形状与引用，授权区间晚于任职结束仍不新增数据库没有的包含约束。`TestEvaluateSampleAndElevenMutations` 样本 valid／0，再对十一类变异逐项断言 invalid、位置和固定原因；合法空表完整文件通过。

```go
r := Evaluate(context.Background(), sampleBytes(t))
n := r.Counts["total"]
if r.ExitCode() != 0 || !r.ChecksComplete || n == nil || *n != 74 { t.Fatal("sample rejected or incomplete") }
```
- [ ] **Step 2：运行 RED。**`go test ./internal/importpreflight -run 'TestIntervals|TestDepartment|TestGrant|TestEvaluate' -count=1`。
- [ ] **Step 3：实现接口。**组织任职按 tenant／user／org 分组，主任职按 tenant／user；部门任职按 tenant／org-membership／department，主任职按 tenant／org-membership。按起始时刻及输入行号排序，维护此前最大结束时刻和确定性见证；每组每行最多产生一个重叠见证，结束等于开始合法。包含检查只消费有效关系／有效时间，不因状态 ended 跳过约束。
- [ ] **Step 4：验证 GREEN。**重跑；`TestEvaluateHashAndCancellation` 确认合法空白变动只改变哈希、同一输入多次结果字节相同；取消不能将 checks_complete 设 true。共享 Collector 只保留 200 明细而统计全部适用问题。
- [ ] **Step 5：提交本任务文件。**提交信息 `feat: complete offline membership interval preflight`。

### Task 6：安全文件输入、CLI 与真实进程

**Files:** 创建 `cmd/im-import-preflight/arguments.go`, `input.go`, `open_unix.go`, `open_unsupported.go`, `run.go`, `main.go` 及对应测试。

**Interfaces:**
- `parseArguments(args []string) (arguments, error)`；arguments 含 Input、Help、Version。
- `openRegular(path string) (*os.File, error)`，Linux／macOS build tag 与其他平台实现分离。
- `readInput(ctx context.Context, path string) ([]byte, *importpreflight.Issue, error)`；超限返回 INPUT_TOO_LARGE 诊断而不提供可哈希的部分输入，运行错误使用固定 Code。
- `type inputReader func(context.Context, string) ([]byte, *importpreflight.Issue, error)`；`Run(ctx context.Context, args []string, stdout, stderr io.Writer) int`；内部 `runWithReader(ctx context.Context, args []string, stdout, stderr io.Writer, reader inputReader) int` 供确定性取消／I/O 故障测试。
- main 通过 signal.NotifyContext 接 SIGINT／SIGTERM，再调用 Run；Run 施加共享 10 秒 timeout，不在每个阶段重新开始预算。

- [ ] **Step 1：写失败用例。**`TestArgumentsExclusiveAndPrivate` 覆盖两种 input 赋值形式、重复 input、混用 help／version、未知参数、不回显值。`TestSecureInputKindsAndReplacement` 实际目录、最终链接、目录路径组件链接、FIFO／socket、打开边界替换及原路径元数据变化；均拒绝，FIFO不能阻塞。`TestInputByteBoundAndCancellation` 验证最多读取 10 MiB+1、输入取消及读取错误隐私；`TestCLIReportsAndExitCodes` 使用真实新命令进程断言 0／1／2 与 stdout 一份 JSON，help／version 单独模式。`TestCLIOutputFailure` 写失败、部分写及输出大小上限不能退出 0。

```go
var stdout, stderr bytes.Buffer
code := Run(context.Background(), []string{"--input", "private-marker", "--input", "other-marker"}, &stdout, &stderr)
if code != 2 || strings.Contains(stdout.String()+stderr.String(), "private-marker") { t.Fatal("unsafe duplicate arguments") }
```
- [ ] **Step 2：运行 RED。**`go test ./cmd/im-import-preflight -run 'TestArguments|TestSecureInput|TestInput|TestCLI' -count=1`，保留 syscall／进程失败证据，不以未找到测试二进制作为有效 RED。
- [ ] **Step 3：实现接口。**用已锁定 unix 包按目录 fd 逐级 openat，目录与最终组件都使用 NOFOLLOW，最终只读／NONBLOCK／CLOEXEC；打开后 fstat 必须是常规文件，检查初查与打开对象的设备／inode身份。输入路径中的链接不跟随，测试临时目录先取得物理路径。所有 fd 由本次打开所有者关闭；有界读取同时观察 context，取消丢弃部分字节。其他平台固定拒绝。固定诊断映射不得直接输出 os／unix 错误。
- [ ] **Step 4：验证 GREEN。**重跑，真实进程附带不可达 IM_DATABASE_URL／OIDC／S3／Redis 环境值仍只完成文件预检；stdout／stderr 不包含参数、路径、姓名、issuer／subject 或预置错误标记。实际 SIGTERM 验证 incomplete／2。测试 reader 向下游报告其收到的同一 deadline，不采用等待 10 秒的测试替代 deadline 断言。

进程测试 helper 在宿主只构建一次专属命令；Linux 编排用测试专属环境变量 `IM_PREFLIGHT_TEST_BINARY` 提供已交叉编译的绝对路径，避免运行镜像依赖 Go 编译器。该变量只由测试读取，产品命令不读取。
- [ ] **Step 5：提交本任务文件。**提交信息 `feat: add secure offline import preflight command`。

### Task 7：真实 PostgreSQL／Linux 对照与资源门禁

**Files:** 创建 `internal/importpreflight/postgres_oracle_test.go`, `budget_test.go`, `privacy_test.go`，`scripts/test-import-preflight.py`；仅测试可引入现有 pgx。

**Interfaces:** 消费 Task 5 Evaluate、Task 6 实际命令；测试入口 `TestPreflightPostgresOracle` 用 `IM_TEST_DATABASE_URL`，未提供时明确跳过。正式门禁必须提供专属 DSN 并核对该测试实际 PASS，不能把 SKIP 计为通过。

- [ ] **Step 1：写失败门禁。**`TestPreflightPostgresOracle` 在专属 schema 应用固定 21 项迁移，装载正样本及十一类对应快照变异，核对数据库接受／指定 SQLSTATE 与 Evaluate 判定；DB10 将 SQL UPDATE 对照父区间缩短快照。`TestPreflightResourceBoundaries` 覆盖 10 MiB 与 +1、10,000 与 +1、深度 16／17、字符串 4,096／4,097；`TestPreflightDiagnosticsOver200` 核对全量去重计数和最前 200；`TestPreflightPrivacyAndDeterminism` 覆盖所有敏感标记位置。Linux 安全打开／进程门禁验证 Task 6 的真实系统行为，不仅交叉编译。

```go
r := Evaluate(context.Background(), fixtureRows(t, 10001))
if r.ExitCode() != 1 || r.ChecksComplete || !hasCode(r, Code("ROW_LIMIT")) { t.Fatal("row limit bypassed") }
```
- [ ] **Step 2：核对 RED。**新增门禁首次若失败，区分产品契约失败、测试编排故障及缺依赖，分别保存；不制造没有依据的失败或删除先前日志。若前六任务已使新增覆盖通过，记录其覆盖证据，不能声称发生了不存在的 RED。
- [ ] **Step 3：完成测试编排。**脚本以明确固定 commit 归档源码，使用现有本地 PostgreSQL 镜像，唯一名称／所有权标签、network=none、无主机端口和临时数据。从宿主 Go 交叉编译 Linux arm64 命令／测试二进制，源码与二进制只读挂载；Linux 容器中执行文件／进程测试和数据库对照，记录镜像 ID、版本、commit、输入及报告哈希。确认本轮标签与 ID 后停止并有界等待自动删除，不操作其他容器。不安装 Go 到运行镜像，不启动附件依赖。
- [ ] **Step 4：执行 GREEN 与相关回归。**固定源码运行 `go test -json -timeout=5m ./internal/importpreflight ./cmd/im-import-preflight -skip '^TestPreflightPostgresOracle$' -count=1`、`go test -race -json -timeout=10m ./internal/importpreflight ./cmd/im-import-preflight -skip '^TestPreflightPostgresOracle$' -count=1`；宿主这两个命令明确仅排除另列门禁的真实 PG 对照。在专属真实 PG 中执行 TestPreflightPostgresOracle 及 `internal/groupdb`、`internal/access`、`internal/oidcauth` 相关回归，对照测试必须实际 PASS。构建 `go build ./...`、静态检查 `go vet ./...`，均需实际退出 0。新门禁必须 PASS，无关键 SKIP；辅助入口若有跳过，单列并确认真实调用方已执行。
- [ ] **Step 5：提交本任务文件。**提交信息 `test: verify preflight against PostgreSQL and platform bounds`。固定产品 SHA 后如修正产品，必须重新冻结及重跑受影响门禁，不沿用旧 SHA 的通过。

### Task 8：整体评审、文档与候选交付

**Files:** 修改 `README.md` 的工具用法；创建 `docs/开发增量-P4-27-验收记录.md`，更新本计划任务状态；独立证据在任务专属输出目录保存。

**Interfaces:** 交付对应固定源码 SHA 的命令、样本、报告、SHA256 清单及验证日志；不创建数据库导入入口。

- [ ] **Step 1：执行一次整体独立评审。**使用 requesting-code-review 技能，reviewer 读取已确认规格、计划、固定产品 SHA 与新执行证据。检查 RF1～5 和明确的文件安全／无网络边界；不新增逐任务多 reviewer 流程。
- [ ] **Step 2：处理有效发现。**确认的 Critical／Important 使用具名失败用例 → 最小修复 → GREEN，并按影响重新固定源码、跑专项／平台／对照及相关回归。Minor 独立记录；不把同一 reviewer 的发现修复称为第二次独立批准。
- [ ] **Step 3：整理用法与边界。**README 包含构建、input／help／version、报告字段、退出码及资源值；说明完整文件引用闭合、只读预检及客户接入未执行。中文验收记录逐项列真实命令、计数、SKIP、失败历史、平台版本及清理证据，不能借用旧 32 项样本检查冒充新命令验证。
- [ ] **Step 4：核对候选包。**只从最终固定 commit 导出源码与指定平台二进制；校验二进制来源、报告输入哈希、所有文件 SHA256 和 ZIP CRC。明确本地／Linux／数据库对照与客户／生产验收的差异。门禁有阻断项时保留源码和失败证据，不标成已完成。
- [ ] **Step 5：提交文档并交付草稿 PR。**按既有项目交付方式建立 base=main 的草稿 PR，关联当前任务；不合并或部署。产品提交之后仅文档变更时核对 diff 并说明测试源 SHA，代码有变更则补做影响验证。

## 计划自检与交接

规格 §3 → Task 1／2／6／7；§4 → Task 2／3；§5.1～5.2 → Task 3／4；§5.3 → Task 5；§5.4 与报告边界 → Task 1／5／8；§6 → Task 1／5／6；§7 八项验收 → Task 2～8；§8 → Task 6／8。

共享接口按上述声明固定：RawDocument → Document／Counts → Index／Relations → Report；各校验器接收同一 context 与 Collector。不会引入未定义的清理／写入阶段。专属外部依赖只有验证用 PG；产品不包含连接客户端。

当前状态：2026-10-06 用户确认书面规格，本轮仅编写并自检这份计划，所有 checkbox 未执行；未写产品代码、运行新增测试或启动依赖。按 writing-plans 交接要求，计划审阅确认后再实施，沿用当前助手逐项实现及最后一次整体独立评审，无需重新选择执行方式。
