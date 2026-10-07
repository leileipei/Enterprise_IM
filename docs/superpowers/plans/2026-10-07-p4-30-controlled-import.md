# P4-30 受控追加导入实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为已有集团租户交付受 OIDC 与当前集团管理员权限保护的主数据追加接口，做到整批写入/拒绝、审计与回执原子提交、相同批次安全恢复。

**Architecture:** 保留两个只读预检命令；共享纯校验和逐行比较，新增调用方拥有的 SERIALIZABLE 写事务读取器。认证后先在专用连接取得批次 session advisory lock，再建立事务快照；参数化 INSERT 六表、不可变回执和审计，结果不明时按原编号查询或重试。

**Tech Stack:** 仓库锁定 Go 1.27.1、pgx/v5 v5.11.0、现有 JWT/OIDC 组件、PostgreSQL 16、Python 验证脚本；不新增或升级产品依赖。

**Spec:** [P4-30 已确认规格](../specs/2026-10-07-p4-30-controlled-import-design.md)。用户于 2026-10-07 回复“确认”。本计划用户于 2026-10-07 确认，正在执行；执行方式沿用当前助手逐项实现，不派实现代理，最终一次独立整体评审。

## Global Constraints

- 工作树 `/Users/leo.cui/.codex/worktrees/p4-30-controlled-import-design/企业IM系统`，分支 `codex/p4-30-controlled-import-design`；产品基线 `7f5868aa11a3d4d8b758f0e332813502efe7eb6e`，设计提交 `5e7a7aa828c0ec4b3aba551f751876f6078a3c0b`。复用本工作树，不改原工作区或已交付 PR #76/#77/#78，不合并或部署。
- 九张数组、内部引用闭合、单个已有可信租户。六表 legal_entities/organizations/departments/users/user_organizations/user_departments 仅追加；tenants/external_identities/admin_grants 仅已有投影一致。禁止更新、删除、改绑、自动合并、授予管理员权限、创建租户。
- POST/GET `/api/admin/import-batches/{request_id}`；单一 Bearer、单一 X-Acting-Membership-ID；操作者 tenant/user、issuer/subject、ExpiresAt 均来自认证适配器。
- 读取正文和探查批次忙碌状态前，短事务检查当前 group_admin；主事务再锁定并检查映射及全部授权依据，提交前按实际 UTC 时间复核。认证失败、不完整来源不得默认放行。
- `SERIALIZABLE READ WRITE`；先 session lock 后 BEGIN；GET 使用 READ COMMITTED 和同键 transaction advisory lock；库连接必须指向写入主库。不得自动重试 SQL 或事务。
- 输入：10 MiB、10,000 行、16 层 JSON、单字符串 4,096 字节，拒绝压缩正文。库存：20,000 行、规范化字段 64 MiB、单字符串 4,096 字节。合并：30,000 行。
- 读取 SQL 最多 256 条；主数据 INSERT 最多 10,000 条，每行最多一条；另最多 32 条事务/回执/审计语句。已有全局占用查询按最多 1,000 键一批，禁止逐输入行查询库存。
- 总预算 30 秒，单 SQL 5 秒，锁等待 1 秒，均取请求取消、剩余时间与令牌到期最早值；rollback/session unlock/关闭全部清理共享最多 1 秒。
- 每 API 实例最多 1 个导入正文/校验/写入请求，其他 POST 立即忙碌；GET 不占此信号量。展示问题最多 200 条，完整内部决策覆盖全部行；receipt 与响应最大 256 KiB。
- 回执 protocol_version=`controlled_append_v1`；state=applied/rejected；reason=NONE/INPUT_CONFLICT/DATABASE_CONSTRAINT_CONFLICT；输入 SHA-256 为原始收到正文 32 字节摘要。同编号空格变化也冲突，POST 绑定 tenant/actor/membership/protocol/hash，GET 同租户任意当前 group_admin 可查。
- 审计 action=`controlled_import.apply`、resource_type=`import_batch`、resource_id=request_id、outcome=allow/deny；不回显输入/库存原文、外租户信息、令牌、issuer/subject、DSN、SQL 原文或错误 detail。
- `IM_IMPORT_ENABLED` 默认 false；开启需要 OIDC、迁移与正式写角色，缺失时启动失败。角色不能是 superuser/BYPASSRLS，目标表不能启用 RLS；不能放宽 P4-29 的只读角色限制。服务无 UPDATE/DELETE SQL，不声称现有 API 凭据只具备追加权限。
- 用户未批准本计划前只准备文档；真实数据导入、生产迁移/开启不属于本轮开发。每项提交仅包含本轮自建文件和明确列出的修改，不混入其他本地编辑。

## Review Focus

- RF1：POST 结束后携 session lock 的池连接被复用、advisory 哈希碰撞导致误认同批次：任务 5/7 验证释放或关闭，完整 UUID 回执主键仍决定匹配。
- RF2：仅 JSON 空白变化、同编号改 acting membership、不同管理员查旧批次：任务 2/5/6 固定摘要、绑定与 GET/POST 不同语义。
- RF3：签名通过后本地映射停用、任职/授权时间到期、COMMIT 时到期：任务 3/5/7 验证锁定、实际时间复核及结果不明。
- RF4：10,000 层组织链、库存异常标量、额外 CHECK 和触发器错误：任务 1/4/5/7 验证迭代拓扑、完整拒绝或回滚，不提前宣称可写。
- RF5：非法 URL/错误正文/输入字段携敏感标记，以及未授权用户探查忙碌批次：任务 6/7 验证固定日志、拒绝先于探查与读取。

## 文件与类型归属

| 文件组 | 职责 |
| --- | --- |
| importcompare/decisions.go、compare.go；importapply/plan.go | 单一比较规则、逐行决策、六表候选与迭代拓扑 |
| importapply/receipt.go、receipt_store.go；000022_import_batches 迁移 | 严格回执契约、不可变持久记录与幂等匹配 |
| access/import_authorization.go；httpserver/admin.go；oidcauth/auth.go | 可信身份来源、可复用事务授权与固定审计 |
| importcompare/append_reader.go、append_profile.go、postgres.go、profile.go；importapply/budget.go | 写事务投影及权限 profile；共享原结构/投影，保持只读路径严格 |
| importapply/service.go、batch_lock.go、insert.go、get.go、errors.go | 批次执行、保存点、session 清理、结果查询与固定错误 |
| httpserver/import_admin.go；cmd/im-api/import_runtime.go、main.go | HTTP 授权/正文限制/并发控制、默认关闭开关与启动验证 |
| 同名 _test.go、importapply/test_helpers_test.go；scripts/test-import-apply.py、test_import_apply_gates.py | 纯单测、真实库/HTTP/进程故障与固定提交验证 |
| README、docs/开发增量-P4-30-验收记录.md、docs/verification/p4-30-results.json | 操作与恢复说明、固定提交证据、结果边界 |

以下接口定义为实现目标，不是当前已存在能力。`p` 指 importpreflight，`c` 指 importcompare；每个新类型在首次所属任务定义，后续直接引用，不新造同义结构。

---

### Task 1：单一逐行比较与六表写入顺序

**Files:** 修改 `internal/importcompare/compare.go`；新增 `internal/importcompare/decisions.go`、`decisions_test.go`、`internal/importapply/plan.go`、`plan_test.go`。

**Interfaces:**
- `c.DecisionKind string`，值 new/identical/conflict；`c.RowDecision{Ref c.RowRef; Kind DecisionKind}`，RowRef 仍是 Entity/Row（原输入 1 起行号）。
- `c.Decide(ctx context.Context,input p.Document,snapshot c.Snapshot)([]c.RowDecision,*c.Collector,error)`：每个输入行恰一个决策，顺序按 schema、原行号；旧 Compare 委托该结果计算原 Classifications，旧输出不变。
- `importapply.Plan{Rows []PlannedRow; Counts map[string]TableCounts; Issues []Issue; ErrorsTotal int; IssuesTruncated bool}`；`PlannedRow{Ref c.RowRef; Record p.Record}`；`BuildPlan(ctx context.Context,input p.Document,decisions []c.RowDecision,issues *c.Collector)(Plan,error)`。
- Task 2 的 TableCounts/Issue 在本任务先定义于 plan.go，下一任务移动至 receipt.go：TableCounts 是 Input/New/Identical/Conflict/Inserted 五个 int；Issue 是 Entity p.Entity、Row int、Field p.Field、Code string，仅固定枚举。Plan/PlannedRow 拒绝 JSON 编码，避免库存/输入流入日志。

- [x] **Step 1：写 RED。** `TestAppendDecisionParity` 断言旧 Compare 分类及报告字节不变；`TestAppendProtectedEntities` 断言禁止表 new 转 conflict，码 PROTECTED_ENTITY_NEW，计数闭合；`TestAppendPlanStableOrder` 将 org/department 父子顺序打乱，断言六表顺序及父在子前；`TestAppendPlanDeepTree` 10,000 层链不递归溢出；`TestAppendDecisionTruncation` 第 201 个冲突仍不进入 Rows。

代表性断言（变量由本任务测试夹具建立）：

```go
// TestAppendPlanStableOrder：夹具为乱序父子输入。
if indexOf(plan.Rows, parentRef) >= indexOf(plan.Rows, childRef) { t.Fatal("parent must precede child") }
if plan.Counts["admin_grants"].Inserted != 0 { t.Fatal("protected entity was selected") }
```

- [x] **Step 2：观察失败。** `go test ./internal/importcompare ./internal/importapply -run 'TestAppend(Decision|Protected|Plan)' -count=1`，新符号缺失或行为断言 FAIL；归档红日志。
- [x] **Step 3：最小实现。** 从现有完整 conflicts/identical 映射提取决策，禁止从截断 Issues 反推冲突。Kahn 迭代拓扑按规范 UUID 稳定排序，已有父节点无需再次插入；只将六表 new 放入 Rows。
- [x] **Step 4：验证 GREEN。** 重跑 Step 2，再 `go test ./internal/importpreflight ./internal/importcompare -count=1`，纯测试 PASS；数据库测试缺夹具的 SKIP 必须保留，不能视为 DB 通过。
- [x] **Step 5：提交。** `feat: derive append decisions and stable insert plan`，仅本任务 Files。

### Task 2：回执协议、幂等记录与迁移

**Files:** 新增 `db/migrations/000022_import_batches.up.sql`、`.down.sql`、`internal/importapply/receipt.go`、`receipt_store.go`、`receipt_test.go`、`migration_test.go`、`test_helpers_test.go`；修改任务 1 类型归属。

**Interfaces:**
- `State/Reason string` 是固定枚举；`Receipt{ProtocolVersion string; State State; Reason Reason; Counts map[string]TableCounts; Issues []Issue; ErrorsTotal int; IssuesTruncated bool; CompletedAt time.Time}`，明确 snake_case JSON tags；九表和 total 必须完整。
- `EncodeReceipt(Receipt)([]byte,error)`、`DecodeReceipt([]byte)(Receipt,error)`，拒绝未知/缺失/重复键、非法计数/枚举/时间、尾随 JSON、超过 256 KiB；展示问题最多 200。
- `BatchBinding{TenantID,RequestID,ActorUserID,ActingMembershipID string; ProtocolVersion string; InputSHA256 [32]byte}`；`StoredBatch{Binding BatchBinding; Receipt Receipt}`，两者不可 JSON/日志输出。
- 私有 `lookupReceipt(ctx context.Context,tx pgx.Tx,schema,tenantID,requestID string)(StoredBatch,bool,error)`、`insertReceipt(ctx context.Context,tx pgx.Tx,schema string,batch StoredBatch)error`；`MatchBinding(a,b BatchBinding)bool` 比较全部绑定值。

- [x] **Step 1：写 RED。** `TestAppendReceiptContract` 断言 applied 的 inserted=new、rejected inserted=0、只读三表 inserted=0、总量闭合；`TestAppendReceiptStrictDecode` 覆盖 malformed/未知/重复/漏键/超 256 KiB；`TestAppendRawBinding` 空格差异及 membership 改变不匹配；`TestAppendPGMigration` 空白/已有迁移 up/down、非法摘要/state/reason/JSONB/重复 tenant+request 拒绝。

代表性断言（变量由本任务测试夹具建立）：

```go
// TestAppendReceiptContract：每实体和 total 均闭合。
for _, counts := range receipt.Counts {
    if counts.Input != counts.New+counts.Identical+counts.Conflict { t.Fatal("counts do not conserve input") }
    if receipt.State == "rejected" && counts.Inserted != 0 { t.Fatal("rejected receipt claims inserts") }
}
```

- [x] **Step 2：观察失败。** `go test ./internal/importapply -run 'TestAppend(Receipt|RawBinding|PGMigration)' -count=1`；PG 测试必须使用下面专属夹具，不能 SKIP 作为 RED。
- [x] **Step 3：最小实现。** 创建不可变终态表及 SELECT/INSERT 存储器，schema 仅受验证的内部配置并用 pgx.Identifier；completed_at 不标为数据库 commit 时间，回执无正文/摘要/actor 来源。
- [x] **Step 4：验证 GREEN。** 同命令在 PG16 普通角色运行；检查主键、tenant 外键、digest 长度、JSONB 字节和 state/reason 配对约束。applied 零新增也合法。
- [x] **Step 5：提交。** `feat: persist immutable controlled import receipts`。

**专属夹具契约（任务 2 建立，任务 7 接入脚本）：** 测试仅在 `IM_IMPORT_APPLY_TEST_DATABASE_URL` 显式提供时创建自身随机 schema，迁移 000001..000022；普通 writer 角色运行产品代码，管理连接只创建/清理角色与 schema。复用 P4-29 测试库隔离模式，不从 IM_DATABASE_URL 或真实库回退。额外 catalog/DDL 夹具使用 `IM_IMPORT_APPLY_TEST_ADMIN_URL`，缺失应明确 SKIP；最终门禁禁止这些 SKIP。

### Task 3：可信来源与事务内集团授权

**Files:** 修改 `internal/httpserver/admin.go`、`internal/oidcauth/auth.go`、`auth_test.go`；新增 `internal/access/import_authorization.go`、`import_authorization_test.go`。

**Interfaces:**
- VerifiedIdentity 新增 `Issuer,Subject string`，认证适配器从验证后的 claims 与配置填写，`json:"-"`；不改变既有 Authenticator 方法签名。
- `access.ImportPrincipal{Identity access.TrustedIdentity; Issuer,Subject string; ExpiresAt time.Time}`，仅内部可信输入，拒绝 JSON/日志输出。
- `access.AuthorizeImport(ctx context.Context,tx pgx.Tx,schema string,p ImportPrincipal,at time.Time)error`：传入事务，按规格锁顺序、固定 schema 验证所有依据，不自行提交。
- `access.AuditImportTerminal(ctx context.Context,tx pgx.Tx,schema string,p ImportPrincipal,requestID,state,reason string,at time.Time)error`：仅固定两种 outcome 与协议原因，复用 audit 规则但禁止 deny 自行提交。

- [x] **Step 1：写 RED。** `TestAppendOIDCVerifiedSource` 来源只来自已验签 claims，未知 subject/过期不能映射；`TestAppendPGAuthorization` group_admin 允许，org_admin/冻结/无效 tenant/会员/法人/组织/映射拒绝；`TestAppendPGAuthorizationExpiry` 实际时间越过区间末端、缺来源/ExpiresAt 失败；`TestAppendPGAuthNoCommit` 授权/审计辅助调用不能结束外层事务。

代表性断言（变量由本任务测试夹具建立）：

```go
// TestAppendPGAuthNoCommit：调用后外层事务仍可查并回滚。
if _, err := tx.Exec(ctx, "SELECT 1"); err != nil { t.Fatal("authorization ended caller transaction") }
if err := tx.Rollback(ctx); err != nil { t.Fatal(err) }
```

- [x] **Step 2：观察失败。** `go test ./internal/oidcauth ./internal/access -run '^TestAppend' -count=1`；PG 与 OIDC 整合所需环境显式提供，记录真实失败。
- [x] **Step 3：最小实现。** 共享现有 resolve 检查条件，不增加全局用户可提交的身份字段；锁先 membership 后 tenant，再其余依据。提交前复核使用新 actual UTC 时间，不复用首次 at。
- [x] **Step 4：验证 GREEN。** 重跑新用例，加完整 `go test ./internal/oidcauth ./internal/access -count=1` 专属夹具回归；已有令牌/普通目录/ACK 测试不可因新来源要求被整体改变。
- [x] **Step 5：提交。** `feat: authorize imports with verified identity and locked grants`。

### Task 4：写事务投影、权限检查与预算

**Files:** 新增 `internal/importcompare/append_reader.go`、`append_profile.go`、`append_reader_test.go`、`internal/importapply/budget.go`、`budget_test.go`；限定修改 `internal/importcompare/postgres.go`、`profile.go`、`profile_contracts.go` 的共享内部函数。

**Interfaces:**
- `c.ReadAppendSnapshot(ctx context.Context,tx pgx.Tx,schema,tenantID string,input p.Document)(c.Snapshot,error)`，不 BEGIN/COMMIT，读九表并 FOR SHARE，归因兼容原 Snapshot。
- `c.CheckAppendProfile(ctx context.Context,tx pgx.Tx,schema string)error`：核对 SERIALIZABLE READ WRITE/UTF8/角色/表权限、结构/排序规则与 import_batches/audit 写权限/序列使用权限（九表 SELECT、至少一列 UPDATE 用于 FOR SHARE，六表所需列 INSERT，回执 SELECT/INSERT，审计 INSERT/序列 USAGE）；旧 checkModes/checkProfile 继续强制 READ ONLY 和无写权限。
- 私有 `newMeteredTx(tx pgx.Tx,budget *sqlBudget)*meteredTx` 实现 pgx.Tx，包装 Query/QueryRow/Exec 并分类计数：Read 256、主数据 INSERT 10000、Control 32；每 SQL 取 min(5s,剩余)，lock_timeout 1s。`sqlBudget{Read,Insert,Control int; Deadline time.Time}` 是请求共享私有计数器；认证后 RequestContext 放入内部 context key，前置短事务、session lock、主事务与清理引用同一计数器，不逐事务/阶段重置，BEGIN/COMMIT/ROLLBACK/session lock/unlock 均计 Control。
- `RequestContext(parent context.Context,start,expiry time.Time)(context.Context,context.CancelFunc,error)`，deadline=min(start+30s,expiry,parent)，零 expiry 拒绝，创建同请求共享 sqlBudget；启动 CheckReady 使用独立预算。

- [x] **Step 1：写 RED。** `TestAppendPGSnapshotLocks` 现有行更新/删除阻塞、跨表同快照；`TestAppendPGWriterProfile` 只读角色/superuser/BYPASSRLS/RLS/非确定排序/缺迁移拒绝；`TestAppendPGStoredLimits` 20000/20001、64 MiB/+1、4096/4097/infinity；`TestAppendBudget` 边界次数及总 deadline 不续期，Query 的取消函数在 Rows.Close/耗尽后释放，QueryRow 的取消函数在 Scan 后释放，不能返回前取消导致合法读取失败。

代表性断言（变量由本任务测试夹具建立）：

```go
// TestAppendBudget：所有阶段沿用同一绝对到期。
want := start.Add(30*time.Second)
if expiry.Before(want) { want = expiry }
if got, ok := requestCtx.Deadline(); !ok || !got.Equal(want) { t.Fatal("deadline renewed") }
```

- [x] **Step 2：观察失败。** `go test ./internal/importcompare ./internal/importapply -run '^TestAppend(PG(Snapshot|Writer|Stored)|Budget)' -count=1`，PG 普通角色夹具运行。
- [x] **Step 3：最小实现。** 复用有限服务端投影，先限单格再扫描；UUID/identity 全局占用仍仅布尔。读完整库存后才返回 Snapshot；FOR SHARE 固定表与主键顺序，不读取其他租户原文，不重用旧 CLI 的 Config 或放宽其权限。
- [x] **Step 4：验证 GREEN。** 同命令；原 `scripts/test-import-compare.py --help` 确认其实际运行参数，并用其独立普通只读角色验证旧命令仍拒绝 writer。预算代理不允许 SendBatch/CopyFrom 绕过计数，产品不用二者。
- [x] **Step 5：提交。** `feat: read locked append snapshots inside caller transactions`。

### Task 5：原子执行、批次锁与状态恢复

**Files:** 新增 `internal/importapply/service.go`、`batch_lock.go`、`insert.go`、`get.go`、`errors.go`、`service_test.go`、`postgres_test.go`、`batch_lock_test.go`。

**Interfaces:**
- `NewService(pool *pgxpool.Pool,schema string)(*Service,error)`；`(*Service) CheckReady(context.Context)error` 以可回滚写事务检查 Task 4 profile。
- `(*Service) Preauthorize(ctx context.Context,p access.ImportPrincipal)error`；`(*Service) Apply(ctx context.Context,p access.ImportPrincipal,requestID string,raw []byte)(Result,error)`；`(*Service) Get(ctx context.Context,p access.ImportPrincipal,requestID string)(Result,error)`。
- `Result{Receipt Receipt; Encoded []byte; Replay bool}`：仅已提交或已验证终态；错误不得携带部分 Result。私有固定错误种类 Busy/NotRecorded/KeyConflict/InvalidInput/Forbidden/DatabaseUnavailable/Retryable/AuditUnavailable/CommitUnknown，HTTP 映射由 Task 6 定义。
- 私有 `batchLockKey(tenantID,requestID string)int64`：规范 UUID 字节、固定域 enterprise_im:controlled_append_v1，经 SHA-256 前 8 字节 big-endian int64；GET 与 POST 必须同键。
- 私有 `insertPlan(ctx context.Context,tx pgx.Tx,schema string,plan Plan)error`：每 new 行一条参数 INSERT、RowsAffected=1；使用 schema 字段描述映射 NULL/bool/time/text，不构造请求 SQL。

- [x] **Step 1：写 RED。** `TestAppendPGApplyAtomic` 六表新增/一致/任一差异、全相同 applied；`TestAppendPGReplayBinding` 同编号正文/membership/actor变化，GET 可由另一有效管理员查；`TestAppendPGSavepointReject` 中间 CHECK/唯一/区间错误、所有 inserted=0；`TestAppendPGAuditFailure` 主数据与回执均回滚；`TestAppendPGLockCleanup` session unlock 失败连接关闭、碰撞不得回放错误 UUID。

代表性断言（变量由本任务测试夹具建立）：

```go
// TestAppendPGAuditFailure：故障前后六表与终态数均不变。
if !reflect.DeepEqual(beforeCounts, afterCounts) { t.Fatal("partial business or receipt commit") }
if applyAuditCount != 0 { t.Fatal("failed audit produced a terminal apply event") }
```

- [x] **Step 2：观察失败。** `go test ./internal/importapply -run '^TestAppendPG(Apply|Replay|Savepoint|Audit|Lock)' -count=1`，产品 SQL 普通角色，夹具管理连接只注入故障。
- [x] **Step 3：最小实现。** Preauthorize 由导入 Service 拥有 READ COMMITTED 短事务，传入带共享预算的 tx 调用 Task 3 AuthorizeImport，再结束短事务；Apply/Get 重检不把前置结论缓存为授权。验证 request UUID/可信来源，取连接 session try-lock 后 BEGIN；当前授权后读原回执，未存在再读库存/BuildPlan/保存点 INSERT。23502/23503/23505/23514/23P01 仅在 rollback-to-savepoint 成功后转固定约束 rejected；其他错误全事务回滚。序列值回滚空洞不算主数据残留。
- [x] **Step 4：验证 GREEN。** 同命令；编码并核验回执/响应后提交，最终时间授权复核；提交成功后保留原计数，未知 COMMIT 一律 CommitUnknown。GET 先 Preauthorize，短 READ COMMITTED try-xact-lock，再次 AuthorizeImport/查回执，记录固定 action=controlled_import.query、resource_type=import_batch 的 allow 查询审计，不追加 apply 审计。全部清理使用一个额外 1 秒绝对 deadline，不能连续各给 1 秒。
- [x] **Step 5：提交。** `feat: apply atomic import batches with idempotent recovery`。

### Task 6：受控 HTTP 入口、限额与默认关闭

**Files:** 新增 `internal/httpserver/import_admin.go`、`import_admin_test.go`、`cmd/im-api/import_runtime.go`、`import_runtime_test.go`；修改 `cmd/im-api/main.go`；新增 `internal/httpserver/import_integration_test.go`。

**Interfaces:**
- `httpserver.ImportService` 接口与 Task 5 Preauthorize/Apply/Get 签名相同；`HandlerWithImports(next http.Handler,auth Authenticator,service ImportService)(http.Handler,error)` 只处理规格的两个路径，其余委托 next。
- 私有 `importEnabledFromEnv(getenv func(string)string,oidcEnabled bool)(bool,error)`：空/false 关闭、true 开启，其他值配置错误；`startImportService(ctx context.Context,pool *pgxpool.Pool,enabled bool)(*importapply.Service,error)`，固定正式 schema public，开启时 CheckReady。
- handler 持有每实例容量 1 的 POST semaphore；从请求进入记录 start，认证后调用 Task 4 RequestContext 创建同绝对 deadline 与 SQL预算的上下文。POST Preauthorize 后抢槽位，再检查 Content-Type application/json、拒绝非空非 identity Content-Encoding、MaxBytesReader 10 MiB 后读取；GET 无正文，不能抢 POST 槽位。

- [x] **Step 1：写 RED。** `TestAppendHTTPStatusContract` 201/200/409/202/400/413/422/401/403/503 与固定码逐项覆盖；`TestAppendHTTPAuthBeforeRead` 无权限时标记 reader 从未 Read，busy 不可见；`TestAppendHTTPNoSecrets` 恶意 path/header/body/DB error 标记不入响应/日志；`TestAppendHTTPIngressLimits` 重复认证头、压缩/超限/并发忙碌、慢正文取消；`TestAppendRuntimeDefaultOff` 默认两个路径404、true 无 OIDC/迁移/权限启动失败。

代表性断言（变量由本任务测试夹具建立）：

```go
// TestAppendHTTPAuthBeforeRead：正文 reader 记录读取次数。
if response.Code != http.StatusForbidden || body.reads != 0 { t.Fatal("unauthorized request read body") }
if response.Header().Get("Cache-Control") != "no-store" { t.Fatal("cacheable admin response") }
```

- [x] **Step 2：观察失败。** `go test ./internal/httpserver ./cmd/im-api -run '^TestAppend(HTTP|Runtime)' -count=1`；handler 单测不启用真实上传/扫描依赖。
- [x] **Step 3：最小实现。** 不使用丢失 ExpiresAt 的 authenticateAdmin，不把 issuer/subject 从 HTTP 字段赋值；复用 bearer 解析并保留可信来源。独立固定拒绝器不记录原 URL/SQL。new wrapper 在最终组合位置处理导入路径，健康/原管理/文件/Web 路由仍委托原 handler。
- [x] **Step 4：验证 GREEN。** 同命令及 `TestAppendHTTPRealOIDCToReceipt`，用真实签名 OIDC+普通角色 PG 从 HTTP 到回执/审计；所有响应 no-store，applied replay200、rejected replay409、GET终态200。预算包括认证、授权、读取、执行，不阶段重置。
- [x] **Step 5：提交。** `feat: expose guarded import endpoints behind default-off runtime`。

### Task 7：真实并发、提交中断与资源边界

**Files:** 新增 `internal/importapply/concurrency_test.go`、`commit_fault_test.go`、`resource_test.go`；扩展 Task 6 integration_test；新增 `scripts/test-import-apply.py`、`scripts/test_import_apply_gates.py`，复用既有测试工具风格。

**Interfaces:** 脚本参数 `--source-commit SHA --output-dir DIR`；拒绝非不可变提交/已有 output-dir，使用 git archive 固定源；结果含 required_gates_passed、full_suite_passed、customer_acceptance=not_executed。夹具创建独占 PG16 容器/普通角色，产品连接不使用容器管理员；隔离端口、随机 schema 与 role，清理验证零新增残留。

- [x] **Step 1：写 RED。** `TestAppendPGConcurrentSameBatch` 两个服务实例并发同编号；`TestAppendPGSnapshotAfterSessionLock` 精确控制前次 commit 与下一 BEGIN；`TestAppendPGConcurrentMutation` 已有组织父节点/任职区间/映射停用/授权撤销、跨租户 UUID/自然键抢占；`TestAppendPGRetryErrors` 真实 40001/40P01/lock timeout，零自动重试。

代表性断言（变量由本任务测试夹具建立）：

```go
// TestAppendPGCommitOutcome：断应答后使用原编号重试。
if finalReceiptCount != 1 || finalApplyAuditCount != 1 { t.Fatal("batch duplicated or lost") }
if finalInsertedCounts != expectedInsertedCounts { t.Fatal("partial or duplicate insert") }
```

- [x] **Step 2：补 RED 故障/边界。** `TestAppendPGCommitOutcome` 用专属 TCP 代理断开实际 Commit 应答、提交前取消、成功后断 HTTP、进程退出/重启；不能仅 fake Commit 返回错误。`TestAppendHTTPResourceEdges` 实际接口10 MiB/+1、10k/+1、20k/+1、64 MiB/+1、4096/4097、报告200/201、令牌临近到期/总预算、深树及池连接再利用。
- [x] **Step 3：运行并观察 FAIL。** `go test -json ./internal/importapply ./internal/httpserver -run '^TestAppend(PG(Concurrent|SnapshotAfter|Retry|Commit)|HTTPResource)' -count=1`；脚本逐门禁记录 executed_tests，预期所选缺口真实 FAIL，夹具缺失不得替代。
- [x] **Step 4：仅修复这些行为缺口并 GREEN。** 断链后查询/原编号重试只能得到同一回执或重新执行一次完整未提交批次；业务表/回执/apply 审计一致。上限已完整覆盖但耗时超过预算时允许明确回滚失败，不允许部分通过；关闭故障代理、恢复权限、证明 session锁已释放或连接关闭。
- [x] **Step 5：提交。** `test: prove import concurrency recovery and resource boundaries`，留存测试名与实际故障时间线；产品修复也在本任务显式列出，不顺便修旧模块。

### Task 8：固定源码门禁、整体评审与交付

**Files:** 修改 README、本计划状态与规格状态；新增 `docs/开发增量-P4-30-验收记录.md`、`docs/verification/p4-30-results.json`；完善 Task 7 两个脚本。

**Interfaces:** Required gates 名称固定 `unit,race,migration,authorization,append_database,http_oidc,concurrency,commit_fault,resources,readonly_regression,groupdb_regression,access_regression,ack_regression,oidc_regression`。每门禁必须 exit0、FAIL=0、SKIP=0、具名顶层PASS>0，检查必需测试名清单，不以 glob 空匹配通过。

- [ ] **Step 1：测试门禁判定。** `scripts/test_import_apply_gates.py` 对缺测试/零测试/SKIP/FAIL/重复门禁/只跑纯函数/清理失败逐项判失败，full_suite_passed 独立计算；`python3 scripts/test_import_apply_gates.py` 必须 PASS。

代表性断言（变量由本任务测试夹具建立）：

```python
# 门禁校验自身：skip 或零测试不可通过。
assert not validate_required_gates(events_with_one_skip)["required_gates_passed"]
assert not validate_required_gates(events_with_zero_tests)["required_gates_passed"]
```

- [ ] **Step 2：固定交付源验证。** `python3 scripts/test-import-apply.py --source-commit <任务7完整提交> --output-dir <新证据目录>`；在 archive 源执行 go build ./...、go vet ./...、go test -race 新模块与受影响边界、全部 required gates。ACK 及完整 OIDC 不得缩减为选取通过测试；P4-29 用自身独立只读夹具，不能拿 writer 角色代跑。
- [ ] **Step 3：完整仓库套件一次。** 同固定源 `go test -json -timeout=10m -count=1 ./...`，解析具名顶层/子测试 PASS/FAIL/SKIP，保留完整失败和跳过名称。P4-29 记录的105失败/825跳过仅作历史参考，不当成此次结果；缺Redis/S3/角色/扫描器等夹具不能写全套通过。
- [ ] **Step 4：最终一次独立整体只读评审。** 对产品基线到固定源的整个差异，重点授权/权限、COMMIT不明、保存点拒绝、池session锁清理、旧CLI兼容及实际接口证据。沿用原执行方式的最终评审代理，禁止实现代理或逐任务额外评审；按评审结果修正具体问题；若产品源码变更，重新固定最终源码并重跑全部必需门禁，完整仓库套件仅为受影响的具体风险补跑，历史全套结果必须标明它自己的源码提交，不冒充最终源码全套通过。
- [ ] **Step 5：整理交付并提交。** 文档列出API示例（无真实凭据）、默认关闭、拒绝批次修正文需新编号、结果不明保留原编号/正文、无自动授权/登录映射、专属夹具与生产未验收。记录最终源码SHA、证据SHA256、独立评审和实际耗时；提交 `docs: deliver P4-30 controlled import verification`。
- [ ] **Step 6：检查分支并交付。** `git diff <固定源> HEAD -- cmd internal scripts db go.mod go.sum` 应为空（否则固定验证源失效，重新验证变更）；工作树干净，旧PR不变。按已有草稿交付方式创建以 P4-29 分支为基线的独立 draft PR 并 attach_artifact，核对远端 head；禁止合并/部署。如推送授权或网络不可用，明确未发布且交付本地提交与文档，不宣称PR已创建。

## 规格覆盖与计划自检

| 规格 | 实现与证据归属 |
| --- | --- |
| §1追加范围/原比较兼容 | Task1、5、8 |
| §2可信身份/前置与提交前授权 | Task3、5、6、7 |
| §3复用边界/权限 | Task1、3、4、6 |
| §4事务/锁/保存点/拓扑 | Task1、4、5、7 |
| §5幂等/终态/提交不明 | Task2、5、7 |
| §6报告/错误/审计 | Task2、3、5、6 |
| §7限额/关闭开关/兼容 | Task4、6、7、8 |
| §8十类验收与真实证据 | Task7、8；前六任务保留各自RED/GREEN |

计划自检已明确文件、跨任务签名、五项 Review Focus 的所属测试和全部规格覆盖；未写产品实现或运行产品测试。用户批准后，读取 executing-plans 技能，逐项更新状态与证据；最后依原方式做一次独立整体评审。
