# P4-21 文件基础 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 建立可验证的文件元数据、封存指纹、生命周期与不可变事件证据，为后续上传／扫描及授权下载提供数据库基础。

**Architecture:** 新增独立 internal/files 领域包处理纯数据校验和状态转移；新增 000018 迁移以租户复合外键、阶段约束和触发器阻断无效写入。沿用 policystore 的隔离 PostgreSQL 测试夹具验证真实约束，不引入文件运行服务。

**Tech Stack:** 现有 Go 1.27.1、标准库 encoding/json／crypto/sha256／unicode／mime／time、既有 pgx/v5 与 PostgreSQL。无新生产依赖。

**Spec:** [P4-21 书面规格](../specs/2026-10-04-p4-21-file-foundation-design.md)，用户于 2026-10-04 以“继续”确认。实现代码基线 b24f963；规格提交 a21dfca。本计划已获用户确认；以下勾选依据实际执行证据，交付状态见验收记录。

## Global Constraints

- 范围仅 internal/files、000018 文件表与事件表、测试夹具和中文交付文档；不注册路由、不安装 MinIO／扫描器、不启用 Worker，不修改消息、策略动作或现有清理行为。
- 文件单一租户、单一会话，真实上传 user／membership 通过复合 FK 关联；ready 仅代表同一封存对象通过扫描，不授予访问权限。
- 首版大小 1～26,214,400 字节（25 MiB）；文件名有效 UTF-8、1～255 字节，无首尾 Unicode 空白／控制字符／NUL／路径分隔符／冒号，不允许 . 或 ..。
- MIME 为小写 ASCII type/subtype、无参数、1～127 字节；元数据校验不证明内容类型或病毒检查通过。
- 来源和创建参数不可变；封存 SHA-256 固定 32 字节，actual size 等于 declared size，object version 不可替换；仅 deleted 可清除内容字段，随后不可恢复。
- 八个状态 allocated／uploaded／scanning／ready／rejected／scan_failed／delete_pending／deleted，初始 allocated／0。每次实际转移 version 恰好 +1；未知状态、同状态实际修改、溢出、倒退都拒绝。完全相同的 SQL no-op 允许且不新增事件。
- created_at／updated_at／upload_expires_at 与各阶段时间遵守规格；封存早于预约到期。扫描可在预约到期后完成；扫描重试不能替换封存内容。
- P4-22 才提供状态＋事件＋业务审计事务。P4-21 不把约束宣称为真实扫描、用户授权或跨系统原子性。
- 沿用当前助手在本会话逐项实现，末尾独立全分支评审。保留隔离工作树，以 P4-20 分支为 PR base；未合并、部署或完成 M4。

## Review Focus

- RF1 字节／码点与 Unicode 空白：中文文件名按字节限制，NBSP／全角空白边缘及 C1 控制字符在 Go 与 PG 都拒绝；FEFF 不擅自当作 TrimSpace 空白。Task 1、3 固定边界。
- RF2 封存后替换：即使大小相同或旧作业稍后返回，摘要／对象版本／作业 ID 的改变也不能把另一份内容标为 ready。Task 2、3 覆盖。
- RF3 失败与清理竞争：scan_failed 不能变 ready，delete_pending 不接受迟到封存／扫描，deleted 清空字段后不能恢复。Task 2、3 覆盖。
- RF4 回滚与租户证据：存在文件／事件时 Down 拒绝；Down 在并发写入之后获得锁须看见新证据，不能删掉另一事务刚提交的数据。Task 3 覆盖。
- RF5 规范幂等输入：UUID 大小写等价、request ID 不参与摘要，而任职／会话／名称／类型／大小改变必须改变摘要；无效参数返回零值而非可用摘要。Task 1 覆盖。

---

## 文件与接口地图

| 文件 | 职责 |
| --- | --- |
| internal/files/create.go、create_test.go | CreateParams、规范输入、固定创建摘要与错误边界 |
| internal/files/metadata.go、metadata_test.go | Metadata 数据类型、各阶段完整性校验 |
| internal/files/lifecycle.go、lifecycle_test.go | State／Reason 常量、允许边、来源／封存／版本转移规则 |
| internal/files/events.go、events_test.go | Event 类型、状态原因闭集、人员／Worker 来源校验 |
| db/migrations/000018_file_foundation.up.sql、.down.sql | 两张表、输入检查函数、阶段／转移防线、证据触发器、索引及保护性回滚 |
| internal/policystore/file_foundation_test_helpers_test.go | 文件 SQL 夹具、封存／扫描阶段夹具、SQLSTATE 断言；只供新测试 |
| internal/policystore/file_foundation_migration_test.go | 租户／输入／唯一键／事件约束与 Up／Down |
| internal/policystore/file_foundation_lifecycle_test.go | 真实 SQL 生命周期、指纹及删除终态 |
| internal/policystore/file_foundation_concurrency_test.go | 真实两连接 CAS 与 Down 等锁后保护证据 |
| internal/policystore/migration_test.go | db(t) 的现有显式 Up 清单追加 000018 |
| docs/开发增量-P4-21-验收记录.md、README.md、开发路径 | 实际验证证据、限制与交付链接 |

所有生产 domain 类型与函数在 internal/files，无 pgx／HTTP／MinIO 依赖。测试函数不能通过修改旧 helper 扫描逻辑或增加跳过条件掩盖失败。retention 的现有 Glob 迁移夹具将自然执行新迁移，应完整回归；无需改其业务行为。

### Task 1：创建参数与固定摘要

**Files:** Create internal/files/create.go、create_test.go。

**Interfaces:**
- Produces `CreateParams`：字符串字段 TenantID、ConversationID、UploaderUserID、UploaderMembershipID、UploadRequestID、OriginalFilename、DeclaredMediaType；DeclaredSizeBytes int64。
- Produces `const MaxFileSizeBytes int64 = 26214400`、`var ErrInvalidMetadata error`。
- Produces `NormalizeCreate(p CreateParams) (CreateParams,error)`；UUID 使用既有 8-4-4-4-12 十六进制形态、只归一为小写，不自动去空白；其他输入原样验证，错误返回零值。
- Produces `CreationDigest(p CreateParams) ([32]byte,error)`，内部先 NormalizeCreate。固定 json.Marshal 字符串数组顺序：v1、tenant、conversation、uploader user、uploader membership、filename、MIME、规范十进制 size，随后 SHA-256；不用 map、拼接分隔符或客户端摘要。

- [x] **0. 准备并记录干净基线。**核对现有隔离工作树和产品代码基线；启动本轮专用 PG／Redis，以 docker port 实测地址生成私有测试环境，不打印凭据；核对 Go／Node／Chrome 路径。在任何产品实现修改前运行 `go test ./... -count=1 -v`，读取退出码与真实计数，保留日志。没有真实 PG／Redis／Chrome 时不把跳过当作完整基线通过。
- [x] **1. 写 RED 输入用例。**`TestNormalizeCreateBoundaries` 在 create_test.go 定义私有 `validCreateFixture() CreateParams`，以五个规范不同 UUID、报告.pdf、application/pdf、size=1 构造独立本地数据；检查 1／26,214,400 通过，0／-1／26,214,401／MaxInt64 拒绝；文件名85个“中”（255字节）通过、再加一个a（256字节）拒绝；无效 UUID、乱码、.／..、斜杠／反斜杠／冒号、NUL、C1、首尾 NBSP／全角空白拒绝，内部正常空格及 FEFF 按规格保留。`TestNormalizeCreateMIME` 检查 application/vnd.openxmlformats-officedocument.wordprocessingml.document 等合法小写、合法 token 字符、127字节通过／128字节拒绝，拒绝大写、参数、空格、换行、非 ASCII、无 type／subtype。不得将类型通过当作允许上传 Office 内容。
- [x] **2. 运行 RED。**`go test ./internal/files -run '^TestNormalizeCreate' -count=1 -v`，预期类型／方法未定义而失败，保留日志；不要先补生产占位绕过 RED。
- [x] **3. 实现 create.go。**使用 unicode/utf8、unicode.IsControl、strings.TrimSpace；文件名限定字节数。MIME 以 ASCII token/type/subtype 和标准库解析双检，要求输入本身规范且无参数，不把解析器接受的空白当作规范输入。
- [x] **4. 固定 RF5 摘要测试。**`TestCreationDigestCanonicalJSON` 使用独立显式字符串数组＋json.Marshal 的期望值，检查 UUID 大小写等价、request ID 改变不变、字段逐一改变均不同、中文／引号／HTML特殊字符安全编码、名称 Unicode 组合形式不合并；非法参数 errors.Is(ErrInvalidMetadata) 且摘要为 [32]byte{}。测试不调用待测摘要函数来生成期望值。

```go
bad := validCreateFixture()
bad.DeclaredSizeBytes = MaxFileSizeBytes + 1
out, err := NormalizeCreate(bad)
if !errors.Is(err, ErrInvalidMetadata) || out != (CreateParams{}) {
    t.Fatalf("invalid size produced usable params: %v %v", out, err)
}
```

- [x] **5. 运行 GREEN 并提交。**`go test ./internal/files -count=1 -v` 全部通过；提交 `feat(files): add canonical creation metadata and digest`。本任务不生成预约／实际文件。

### Task 2：元数据阶段、状态机与事件

**Files:** Create metadata.go／_test.go、lifecycle.go／_test.go、events.go／_test.go。

**Interfaces:**
- Consumes Task 1 的 CreateParams、NormalizeCreate、ErrInvalidMetadata。
- Produces `type State string` 及 StateAllocated、StateUploaded、StateScanning、StateReady、StateRejected、StateScanFailed、StateDeletePending、StateDeleted，值与 Global Constraints 一致。
- Produces `type Reason string`，闭集为 allocated、upload_sealed、scan_started、scan_clean、scan_rejected、scan_error、scan_retry、deletion_requested、object_deleted。
- Produces `Metadata`，嵌入 CreateParams，增加 ID string、RequestDigest []byte、State State、StateVersion int64、CreatedAt／UpdatedAt／UploadExpiresAt time.Time；封存字段 ObjectKey／ObjectVersionID／DetectedMediaType string、ActualSizeBytes *int64、SHA256 []byte、UploadedAt *time.Time；扫描字段 ScanJobID／ScanEngine／ScanDefinitionVersion string、ScannedAt *time.Time、ScanSHA256 []byte；删除时间 DeletionRequestedAt／DeletedAt *time.Time。空 string／nil 表示 SQL NULL，仅在对应状态允许；没有授权布尔值。封存 ObjectKey 固定为 `tenants/<tenant UUID>/files/<file UUID>`，不含原文件名；ObjectVersionID 为非空、无控制字符的存储服务原样版本标识，不解码或改大小写。
- Produces `ValidateMetadata(m Metadata) error`、`ValidateTransition(before,after Metadata) error`、`TransitionReason(from,to State) (Reason,error)` 与 ErrInvalidTransition。ValidateMetadata 的 ID 与身份 UUID 使用规范小写；完整来源调用 Task 1 校验，deleted 来源的保留字段单独校验且不能被当作新创建参数。ValidateTransition 对实际转移检查双端完整性、相同来源、内容指纹、作业 ID、版本与时间；不修改输入，不返回可访问文件。
- Produces `Event`：TenantID／FileID string、StateVersion int64、FromState／ToState State、Reason Reason、OccurredAt time.Time、ActorKind／ActorUserID／ActingMembershipID／WorkerJobID string。`ValidateEvent(e Event) error` 与 ErrInvalidEvent；空 FromState 只允许初始 version0／allocated。

- [x] **1. 写生命周期 RED。**`TestFileLifecycleAllowedAndDeniedEdges` 明确八状态所有 64 对：只允许规格第 6 节列出的 13 条边，拒绝同状态实际变更；分离 TransitionReason 的创建边（空→allocated）与真实 Metadata 转移。`TestFileMetadataStageCompleteness` 以完整 allocated／uploaded／scanning／ready 等夹具检查缺失封存字段、大小不等、摘要 31／33 字节、时间零值／逆序、预约时间相等、scanned_at 早于 uploaded_at、扫描指纹不同均拒绝。
- [x] **2. 运行 RED。**`go test ./internal/files -run '^TestFile' -count=1 -v` 预期接口未定义失败。
- [x] **3. 实现阶段与边。**allocated 的封存／扫描／删除字段均为空；uploaded 的封存组完整、扫描组空；scanning 必有作业 ID 且结果组空；ready 必有引擎、版本、scanned_at 和同指纹结论。rejected 保留完成检测的同指纹结果，scan_failed 留原作业 ID、无清洁结果。delete_pending 保留来源及此前内容／扫描证据，允许从 allocated 进入时封存组为空；deleted 才可一次清空内容组，不能用混合缺失字段伪造可用阶段。扫描完整结果组需全有或全无，sealed 时间证据保留。该细化没有新增状态或边。
- [x] **4. 固定 RF2／RF3 转移测试。**`TestFileTransitionImmutableSealedContent` 前后夹具须深拷贝 byte slice／时间与大小指针，避免改 after 顺便改 before 导致伪通过；从有效前后夹具分别改变来源、声明、request_digest、actual size、SHA256、object key/version，全部拒绝；只有 allocated→uploaded 可以一次写封存组。`TestFileTransitionRejectsStaleScanAndVersion` 检查 scanning→ready 改作业、摘要、version 不加／跳加、MaxInt64 溢出、退回过去时间；scan_failed→scanning 必须新作业，清除旧结果，不改内容。`TestFileDeleteTerminalCannotRevive` 走 allocated 和 ready 两种删除路径、允许规定清空、阻断迟到上传／扫描／恢复字段。
- [x] **5. 固定事件闭集测试并实现 events.go。**`TestFileLifecycleEventOriginAndReason` 检查初始 user 事件、上传 user、所有扫描 worker 事件与 job ID；user 不可带 worker ID，worker 不可带员工字段，扫描不能由 user 登记。拒绝未知 reason／状态、reason 与边不符、version／from 不符、乱码／控制字符和非法身份 UUID。删除边允许 user 或 worker 来源，始终满足各自身份组约束。
- [x] **6. 运行 GREEN 并提交。**`go test ./internal/files -count=1 -v`、`go test -race ./internal/files -count=1` 均通过；提交 `feat(files): guard file lifecycle and immutable evidence`。不添加文件发送／下载／授权服务。

### Task 3：真实数据库约束与保护性迁移

**Files:** Create 000018_file_foundation.up.sql／.down.sql、上述三个 file_foundation 测试及 helpers；Modify internal/policystore/migration_test.go 的 db(t) 清单。

**Interfaces:**
- Consumes Task 1／2 字段、阶段和允许边。SQL 名称使用 snake_case，nullable 与 Go 字段组一致。
- Produces file_objects：主键 id、UNIQUE(tenant_id,id)、UNIQUE(tenant_id,uploader_user_id,upload_request_id)，以及规格定义字段；FK 至同租户 conversations 和 user_organizations。file_lifecycle_events 外键为 (tenant_id,file_id)，唯一为 (tenant_id,file_id,state_version)。**历史事件不能 FK 至 file_objects 的当前 version**，否则推进状态会破坏旧事件。
- Produces 索引 file_objects_conversation (tenant_id,conversation_id,created_at,id)、file_objects_pending (state,updated_at,id)，后者仅非终态待处理 allocated／uploaded／scanning／scan_failed／delete_pending；事件唯一键同时作为序列索引，不重复建同列索引。
- Produces SQL 检查函数 file_metadata_filename_valid(text)、file_metadata_mime_valid(text)、file_lifecycle_reason(text,text) RETURNS text（初始 NULL→allocated 与13条允许边对应 reason，非法边返回 NULL）；CHECK 必须显式判断函数结果非 NULL，不能靠 NULL=reason 在 CHECK 中通过。前两个校验函数返回 boolean、无效输入返回 false。触发器函数 guard_file_object_change()、reject_file_lifecycle_event_change()。函数不得设置 SECURITY DEFINER 或读取其他 schema 的业务数据。
- 新测试 helper `fileFoundationDB(t *testing.T) *pgx.Conn` 复用 db(t)；首次 RED 时 db 不含 000018，helper 显式尝试读并执行新 Up，缺文件失败。Up 成立后 db 清单追加 000018，helper 移除额外重复执行。`expectFileSQLState(t *testing.T, err error, want string)` 必须 errors.As 到 *pgconn.PgError 并比较 SQLSTATE，不能把语法错误／断连也当作正确拒绝。

- [x] **1. 核对数据库依赖。**沿用 Task 1 第0步的已记录完整基线与私有环境，重新核对实际端口和连接可用性；不重新安装依赖或重复无关完整基线。测试过程中所有 PG 只在测试 schema 使用迁移；缺失 PG 造成 SKIP 不计为本任务验收。
- [x] **2. 写 PG RED。**`TestFileFoundationSchemaIsolationAndInput` 用 db／seedDirectConversation，以及新 helper 的合法 allocated 参数；在新 Up 未存在时失败。设计非法写测试精确断言 FK 23503、CHECK／触发器 23514、唯一键 23505；不复用旧 reject(t) 只查非 nil 的弱判定。
- [x] **3. 实现 Up、接入清单。**表 CHECK 显式判断必填／可空字段并匹配阶段，封存对象 key 与 tenant／file ID 一致；防止 NULL 的三值逻辑绕过。trigger 禁止直接 INSERT 为 ready 或非0 version，禁止来源和封存修改／跳边／版本越界／实际同态改写／时间倒退／DELETE 文件行；完全无变化 UPDATE 为幂等 no-op。事件 CHECK reason／边与 actor 身份组，事件 UPDATE／DELETE trigger 拒绝。扫描结论仅验证字段关联，不声称 DB 会执行扫描或证明事件与状态自动原子提交。
- [x] **4. 固定 RF1／RF5 与唯一键。**`TestFileFoundationSchemaIsolationAndInput` 验证跨租户会话、他人 uploader membership、同 request 不同文件冲突；255 UTF-8字节通过／256拒绝、NBSP／全角空白边缘、C1、路径／冒号及规范 MIME／大小边界在实际 PG 拒绝。SQL 需显式覆盖 Unicode 空白端点和 C0/C1 范围，不能只用 btrim 默认 ASCII 或依赖当前 locale 的 cntrl 类。无 NUL SQL 测试以驱动真实拒绝为证，不混称为 trigger CHECK。
- [x] **5. 固定封存／扫描／终态。**`TestFileFoundationSchemaLifecycle` 按真实合法 INSERT＋逐步 UPDATE 测试 13 边，遍历其余禁止边；核对初始 ready、sha 长度、不同实际大小、替换 object_version、改变 scan_job、扫描早于上传、预约等边界拒绝。`TestFileFoundationSchemaDeletedCannotRestore` 删除清空后改回 state／名称／sha／定位均拒绝。delete_pending 的迟到 ready／uploaded 也拒绝。
- [x] **6. 固定事件与 CAS。**`TestFileFoundationSchemaEventEvidence` 验证合法初始／扫描／删除事件、跨租户 file、他人 acting membership、重复 version、wrong reason／actor、UPDATE／DELETE。`TestFileFoundationConcurrentScanCAS` 用当前 schema 第二连接：两者从同 scan version 尝试 ready／scan_failed；先成功者更新1行，迟到者 `WHERE state_version=expected AND scan_job_id=job` 更新0行，最终无双结论。不要把一个 pgx.Conn 同时交给两个 goroutine。
- [x] **7. 实现保护性 Down 并测试 RF4。**Down 在 BEGIN 内先按固定顺序 ACCESS EXCLUSIVE 锁 file_objects、file_lifecycle_events；任一表有行即 SQLSTATE23514 拒绝，无行才按依赖逆序 DROP 两表与函数，避免 CASCADE。`TestFileFoundationMigrationEmptyDownUp` 先写一条旧文本消息、新文件表保持空，Down／Up 后消息与会话仍存在，索引／函数实际恢复。`TestFileFoundationMigrationDownProtectsEvidence` 对有文件及已 deleted 证据都拒绝。`TestFileFoundationMigrationDownWaitsForConcurrentEvidence` 用第二真实连接，未提交 allocated 行持锁，Down 的 backend PID 已等待后再提交 writer；Down 获锁后看见新数据并拒绝。用 pg_stat_activity／pg_locks 和5秒有界等待证明真实等待，不只依靠 Sleep 猜测；出错连接显式 ROLLBACK，schema cleanup 负责移除测试证据。
- [x] **8. 运行 GREEN／race 并提交。**`go test ./internal/policystore -run '^TestFileFoundation' -count=1 -v` 及相同命令加 -race 必须启用 PG、无目标用例 SKIP。记录真实日志；提交 `feat(db): add isolated file metadata and lifecycle constraints`。不执行生产迁移，不启用清理。

### Task 4：回归、中文验收与草稿交付

**Files:** Create docs/开发增量-P4-21-验收记录.md；Modify README.md、docs/企业IM-开发计划与实施路径-v0.1.md，计划勾选仅依据执行证据。

**Interfaces:**
- Consumes Task 1～3 的模型和迁移；不新增对外接口。
- Produces 可复验的运行记录、独立评审结论和堆叠草稿 PR。用户已选择当前会话逐项实施；末尾按 requesting-code-review 技能独立评审，不逐任务另开子代理。

- [x] **1. 验证现有完整链路。**启用真实 PG／Redis／Chrome，运行 `go test ./... -count=1 -v`。确认单聊／群消息、旧搜索与 P4-20 跨会话浏览器及真实 OIDC 流程通过；记录实际顶层 PASS／FAIL／SKIP，辅助 TestRealtimeAPIChild 跳过单独说明，任何新增数据库测试跳过均不能报通过。
- [x] **2. 完成静态与竞态检查。**运行 `go test -race ./internal/files ./internal/policystore -run '^(TestNormalizeCreate|TestCreationDigest|TestFile)' -count=1 -v`、`go vet ./...`、`go build ./...`、`git diff --check`，读取全部退出码。已有完整绿色证据后只为具体修复风险重验，不无故反复全量运行。
- [x] **3. 写实际中文验收。**记录真实 RED／GREEN、schema约束、迁移Up／Down、CAS／回滚并发、完整回归、环境真实端口及日志路径。明确只是元数据／状态基础，没有上传／扫描／发送／下载、F01 或生产容量验收；模型限制25MiB不是性能证据。P4-19 组合预算测试后续项仍保留，不借本切片宣称已补强。
- [x] **4. 独立全分支评审与修正。**按 requesting-code-review 评审 b24f963..HEAD 的功能差异，附规格、计划、实际日志和范围。禁止把评审只读日志写成独立重跑集成；如果代理数量限制使复用评审上下文需如实记录。解决 Critical／Important，Minor明确处理；修复后跑对应真实验证。
- [ ] **5. 提交／推送／草稿 PR。**继续已有 codex/p4-21-file-foundation-design 分支（保留历史，不重复创建工作树）；以 codex/p4-20-web-cross-message-search 为 base，提交剩余验收文档，推送并创建 P4-21 实现草稿 PR。用临时 UTF-8 正文文件传 gh --body-file，成功创建必须 attach_artifact；记录真实链接，再核对 PR OPEN／DRAFT、base／head、local／remote／PR SHA一致、工作树干净。
- [ ] **6. 收尾。**停止仅本轮专用测试容器、保留工作树。用户交付包含中文验收记录、PR、实际验证及后续 P4-22。不合并、不部署、不启用 Worker。

## 自检与执行交接

规格第4～6节的实现要求分别由 Task 1（输入／摘要）、Task 2（完整性／转移／事件）、Task 3（真实数据库防线）承担；第10节由 Task 3～4 验收。第7～9节是 P4-22～P4-25 的接口／权限／保留合约，本计划不把它们列为已实现任务。RF1～RF5均落实到具名用例。

所有函数／字段均由其生产任务定义；迁移编号未占用，新增任务只调整现有 db helper 的迁移清单，Glob helper 自然覆盖。Down 必须保留已登记证据；SQLSTATE精确断言避免因 SQL 自身写错而“测试通过”。计划自检不等同于实际运行验证。

2026-10-04：用户确认计划，按 superpowers:executing-plans 在当前会话逐项实施。任务1～3已提交并验证；任务4的回归、评审和交付按实际证据更新，详见 [验收记录](../../开发增量-P4-21-验收记录.md)。
