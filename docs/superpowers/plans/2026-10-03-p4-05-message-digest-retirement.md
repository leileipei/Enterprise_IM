# P4-05 消息摘要与幂等记录到期治理 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在正文已清理、幂等到期且无有效保全时原子清空两处消息摘要，保留永久到期键与回执，阻止旧请求重建消息。

**Architecture:** 在现有正文清理服务旁新增摘要 Worker，各自独立事务并共享会话锁规则。两表可空摘要、永久到期时间和延迟约束保证提交状态一致；单聊与群聊查重识别永久到期记录。进程提供独立启用开关，默认关闭。

**Tech Stack:** Go、pgx/v5、PostgreSQL 16；回归使用 Redis 7 与真实 Chrome。沿用现有依赖，不新增服务或第三方库。

**Spec:** [已批准设计](../specs/2026-10-03-p4-05-message-digest-retirement-design.md)，设计提交 `6213871`，产品代码基线 `56b1bc2`。

## Global Constraints

- 只有有效租户、正文已清理、关联幂等记录存在、两行尚未退役、`expires_at <= retired_at`、`body_cleared_at <= retired_at`、无任一有效保全时可清理。
- 幂等记录至少保留 30 天；新消息继续设置 30 个 24 小时天；客户端消息 ID 超过既有 7 天窗口返回 `410`，未来超过 5 分钟仍拒绝。
- 退役后摘要不可恢复、标记与回执不可改写、到期幂等键不可删除；时钟回退不恢复 ACK 或新消息写入。
- 两种发送事务和摘要清理事务在任何查询前显式设为 `READ COMMITTED`；清理锁顺序为租户 → 会话 → 消息 → 幂等，锁后用新语句复核保全。
- 每租户每轮最多一个会话摘要批次，默认 100 条，上限 1000；摘要更新集合必须相同，证据同事务提交，每条消息只计一次。
- 死锁 `40P01` 最多三次完整事务尝试；取消及其他错误不继续重试。批次时间来自租户／会话锁后的数据库 `clock_timestamp()`。
- 正文与摘要清理独立开关；都关闭不连接数据库。租户枚举及每类批次有独立 5 秒上下文；成功或空轮 1 秒，错误退避 1～30 秒。
- 身份、参与者及历史任职校验顺序不变；分页、Outbox、消息行及原始回执保留；日志与证据不含正文、摘要、原始数据库错误或连接 URL。
- 迁移／兼容 API 全部部署完成后才能启用摘要清理；有退役或证据历史时 Down 拒绝。MVCC、WAL、备份、外部副本和完整元数据删除另行治理。

## Review Focus

1. 默认 `REPEATABLE READ` 的连接不得返回清理前的旧状态或漏掉已提交保全；Task 2、3 用非默认隔离连接制造竞态。
2. 等待行锁时幂等到期时间被延后，必须按锁后的记录跳过；Task 3 用真实幂等行锁和更新验证。
3. 两表 UPDATE 返回数量相同但消息 ID 集合不同，也必须回滚；Task 3 注入等数量错集合及少行结果验证。
4. 清理已提交后到达的重复请求和清理提交前已读旧状态的请求，必须分别得到到期拒绝和合法旧 ACK；Task 2 明确 SQL 读取判定点。
5. 正文批次错误／超时不能跳过同租户摘要批次及后续租户；Task 4 用 fake Worker 验证调用顺序、独立 deadline 与取消。

## 文件结构与准备

- `db/migrations/000016_message_digest_retirement.{up,down}.sql`：两表状态、提交时成对约束、不可逆保护、候选索引和批次证据。
- `internal/policystore/message_idempotency.go`：从 `messages.go` 移出共用查重函数并实现退役判定；其他发送代码仅加事务隔离设置。
- `internal/retention/digest_worker.go`、`digest_batch.go`：配置／结果／重试与批次事务分离；复用现有常量、`rollback` 和租户枚举，不进行无关抽象重构。
- `cmd/im-retention-worker/main.go` 与新增 `sweep.go`：配置／进程入口与双类轮询分离；README 记录部署顺序及边界。
- 测试分别放在下列任务列出的文件；复用现有隔离 schema、真实发送夹具、保全服务和事务暂停包装器。

执行沿用用户已选择的 **Native：由当前会话逐项实现**。实施前检查分支 `codex/p4-05-digest-retirement` 干净，启动本增量专用 PostgreSQL，提前创建 `btree_gist`，设置 `IM_TEST_DATABASE_URL` 并运行已有受影响包基线；缺少依赖而 SKIP 不代表通过。专用日志写入本计划执行目录，禁止连接客户数据库。

### Task 1: 不可逆退役迁移与提交一致性

**Files:** Create `db/migrations/000016_message_digest_retirement.up.sql`、`.down.sql`、`internal/policystore/digest_retirement_migration_test.go`；modify `internal/policystore/migration_test.go`、`conversations_test.go`、`body_clear_migration_test.go`、`internal/access/migration_test.go`、`internal/oidcauth/store_test.go`。`internal/retention/migration_test.go` 已自动枚举迁移，保持其夹具兼容。

**Interfaces:** 两表新增 `digest_retired_at timestamptz`，摘要未退役仍为非空 32 字节。批次表字段为 `id uuid`、`tenant_id`、`conversation_id`、`retired_at timestamptz`、`retired_count integer`、`first_seq/last_seq bigint`、`min_expires_at/max_expires_at timestamptz`。两表退役时间相同且只能 UPDATE 进入退役；消息必须已清正文，幂等必须到期。延迟触发器读最终关联状态；未退役且缺少幂等行的旧消息允许存在。

- [ ] 写 `TestDigestRetirementMigrationConstraints`：旧消息与新非空摘要有效；长度非 32、空摘要无标记、直接 INSERT 退役行、未清正文或早于到期的退役拒绝；两表同事务更新后提交成功。单侧更新和错配时间在 COMMIT／`SET CONSTRAINTS ALL IMMEDIATE` 时失败，显式回滚后两处摘要仍非空。
- [ ] 运行 `test -n "$IM_TEST_DATABASE_URL" && go test ./internal/policystore -run '^TestDigestRetirementMigration' -count=1 -v`；RED 必须来自缺少迁移／列或缺少约束，不是数据库连接错误。
- [ ] 实现 Up 的 CHECK、INSERT／UPDATE／DELETE 保护与延迟成对触发器；退役后禁止改两表关联键、消息 `id/seq/accepted_at`、幂等 `message_id/accepted_at/expires_at`、摘要／标记及删除到期幂等行。证据包含合法计数、序号／到期时间边界和租户会话外键，UPDATE/DELETE 拒绝。
- [ ] 写 `TestDigestRetirementMigrationRollbackAndEvidence`：无历史 Down/Up 成功；有退役行或仅有证据时 Down 拒绝；并发退役事务使 Down 等待，提交后拒绝。Down 在检查前对消息、幂等与证据取 `ACCESS EXCLUSIVE` 锁；拒绝后 ROLLBACK 并验证状态未变。覆盖跨租户证据、0/1001 计数、反向序号及晚于退役时间的最大到期时间拒绝。
- [ ] 更新显式 Up 列表；单聊及正文清理回滚夹具先 Down `000016`，再按原顺序 Down，并在恢复时先恢复旧迁移再 Up `000016`。运行目标测试及 `go test ./internal/access ./internal/policystore ./internal/oidcauth -count=1`，检查无依赖缺失跳过；提交 `feat(retention): add irreversible digest retirement schema`。

### Task 2: 单聊与群聊永久到期查重

**Files:** Create `internal/policystore/message_idempotency.go`、`digest_retirement_send_test.go`；modify `internal/policystore/messages.go`、`group_message_send.go`；extend `internal/httpserver/conversations_test.go`、`group_message_send_test.go` 的错误映射用例。

**Interfaces:** 保持 `existingMessageACK(ctx context.Context, tx pgx.Tx, tenantID, conversationID, senderUserID, clientMessageID string, digest [32]byte) (MessageACK, bool, bool, error)`；退役返回既有 `ErrRetryExpired`，HTTP `410 retry_window_expired`。查重 SQL 以消息幂等键查询消息并 LEFT JOIN 关联幂等行，一次读取两处摘要和退役时间；任一退役标记优先拒绝。消息存在却缺关联幂等行、未退役摘要缺失、错误长度或状态错配返回内部错误，映射既有存储不可用 `503`，不作为键不存在。`SendTextMessage` 与 `sendGroupTextMessageOnce` 在 Begin 后、任何查询前设置 READ COMMITTED；签名保持。

- [ ] 写 `TestDigestRetiredMessagesRejectReplay`，单聊／群聊通过真实发送建消息；退役前窗口内同内容原 ACK、不同内容冲突，退役后正常时钟及回退到原窗口均拒绝；同时断言旧键不会增加消息／幂等／Outbox、限流与 `last_seq`，补拉仍仅遮蔽占位且分页连续。

```go
if !errors.Is(err, policystore.ErrRetryExpired) { t.Fatalf("retired replay: %v", err) }
if !ack.ServerTime.IsZero() || ack.MessageID != "" { t.Fatalf("retired ACK: %+v", ack) }
```

- [ ] 运行 `test -n "$IM_TEST_DATABASE_URL" && go test ./internal/policystore -run '^TestDigestRetired' -count=1 -v`，确认 RED 是 NULL 摘要处理或错误 ACK／冲突；移出并改造查重函数、添加两处隔离设置，保留现有身份与参与校验顺序。
- [ ] 写 `TestDigestRetiredReplayAuthorizationAndCorruption`：跨租户、从未参与群、错误任职不暴露到期状态；用独立测试 schema 临时放宽约束构造 NULL 无标记与状态损坏，证明返回存储错误且无新写入，测试清理恢复／删除该 schema。HTTP 单聊与群聊断言既有 410 错误码且无 ACK 字段。
- [ ] 写 `TestDigestRetiredSendSnapshots`：事务包装器／真实第二连接配合 channel，默认 RR 连接的发送在首次身份查询后暂停，此时另一事务提交成对退役；随后查重必须为 410。单聊在查重已扫描旧状态后暂停并提交退役，允许原 ACK；群聊此时持有会话锁，验证清理等待，释放发送使 ACK 提交后清理才能提交。两种路径的退役后新查重均拒绝；使用刚创建消息与确定时钟，禁止猜测性 sleep。
- [ ] 目标测试、既有单聊／群聊发送及 HTTP 错误映射测试 PASS 后提交 `fix(messages): reject permanently retired idempotency keys`。

### Task 3: 有保全保护的摘要批次事务

**Files:** Create `internal/retention/digest_worker.go`、`digest_batch.go`、`digest_worker_test.go`、`digest_concurrency_test.go`；复用 `migration_test.go` 的数据库夹具和 `concurrency_test.go` 的实际保全与暂停工具。

**Interfaces:** `DigestWorker{DB access.Beginner, BatchSize int; clock func(context.Context, pgx.Tx) (time.Time,error)}`，`DigestBatchResult{TenantID, ConversationID, BatchID string; RetiredCount int; FirstSeq, LastSeq int64; MinExpiresAt, MaxExpiresAt, RetiredAt time.Time}`；`(DigestWorker).ProcessTenant(ctx context.Context, tenantID string) (DigestBatchResult,error)`。未配置 DB／非法批量复用 `ErrWorkerUnconfigured`／`ErrInvalidBatchSize`；0 采用 100；无候选返回零值结果和 nil。私有 `processDigestBatch(ctx context.Context, tenantID string, size int)` 返回相同结果，时钟默认查询数据库，测试允许私有注入。

- [ ] 写 `TestDigestProcessTenantConditionsAndEvidence`：精确到期／差 1 微秒、正文未清、幂等未到期、延长到期、正文清理时间在未来、缺幂等行、跨租户、停用租户；批量 2 只改两条，重复运行不重计；混合乱序时间的序号与 expires 边界按实际集合计算。
- [ ] 运行 `test -n "$IM_TEST_DATABASE_URL" && go test ./internal/retention -run '^TestDigestProcessTenant' -count=1 -v`，确认 RED 后实现 RC → 租户 SHARE → 会话 SKIP LOCKED → 独立保全复核 → 实际时钟 → 消息及幂等锁 → 条件复核 → 两表更新 → 证据。更新前固定实际 ID 集合，比较两表 UPDATE RETURNING 的完整 ID 集合，任一差异即错误回滚。

```go
if result.RetiredCount != 2 || result.BatchID == "" { t.Fatalf("batch: %+v", result) }
// Query committed rows: two message NULL digests, two idempotency NULL digests,
// equal retirement times, exactly one batch with retired_count=2.
```

- [ ] 写 `TestDigestProcessTenantHoldsAndConcurrency`：真实 PlaceLegalHold／ReleaseLegalHold，多案件仅解除一个仍跳过；保全先锁和清理先锁两种次序；最早会话被保全／锁住仍处理后续；两个摘要 Worker 不重计；与正文 Worker 并发只处理已提交清正文记录。非默认 RR 连接在租户读取后暂停、真实保全提交后，后续清理必须跳过。
- [ ] 写 `TestDigestProcessTenantWaitsAndRollback`：另一事务锁幂等行并延长 expires，Worker 等待后必须跳过；分别在消息锁、两表更新和证据插入阶段取消／注入失败，查询证明无单侧退役或无证据更新；包装 UPDATE RETURNING 注入少行和等数量不同 ID，证明回滚；首次已更新后注入 `40P01`，重试前登记保全，下一次不得退役，连续三次死锁停止且取消不重试。
- [ ] 实际默认时钟烟测、候选 EXPLAIN（足够候选规模／ANALYZE，不把小表顺序扫描当性能缺陷）、`go test ./internal/retention -count=1 -v` 和 `go test -race ./internal/retention -count=1` 通过后提交 `feat(retention): retire expired message digests atomically`。

### Task 4: 独立开关与双类清理轮询

**Files:** Modify `cmd/im-retention-worker/main.go`、`main_test.go`、`README.md`；create `cmd/im-retention-worker/sweep.go`、`digest_sweep_test.go`。

**Interfaces:** `workerConfig` 保留 `DatabaseURL string; Enabled bool; BatchSize int` 并新增 `DigestEnabled bool; DigestBatchSize int`。`configFromEnv` 与 `run` 签名不变。`tenantLister.ListActiveTenants(context.Context) ([]string,error)`、`bodyProcessor.ProcessTenant(context.Context,string) (retention.BatchResult,error)`、`digestProcessor.ProcessTenant(context.Context,string) (retention.DigestBatchResult,error)`；`runSweep(ctx context.Context, tenants tenantLister, body bodyProcessor, digest digestProcessor, logger *slog.Logger) (sweepCounts,error)`，`sweepCounts{BodiesCleared, DigestsRetired int}`，nil processor 表示未启用。枚举复用 `retention.Worker.ListActiveTenants`，即使仅摘要启用也不调用正文 ProcessTenant。

- [ ] 写 `TestDigestCleanerConfigAndDisabled`：四种开关组合；摘要默认 false/100，合法 1/1000；非法开关含 `1`、空格，批量 0/-1/1001/非数字；仅摘要启用缺 URL 拒绝；全关配非法 URL 仍不连接数据库。先运行 `go test ./cmd/im-retention-worker -count=1 -v` 确认 RED，再实现配置。
- [ ] 写 `TestDualCleanerSweepFairnessAndCancellation`：只枚举一次，调用次序 `a-body,a-digest,b-body,b-digest`，正文失败／超时仍调用摘要与下一租户；摘要失败仍调用下一租户；每调用独立 5 秒 deadline；父取消不再处理后续，分离两类计数。fake 错误内正文／摘要／URL 不出现在日志，空轮与退避继续覆盖 1～30 秒。
- [ ] 实现 `sweep.go` 双类轮询与 main 的按开关初始化；保持原正文单开行为、信号退出、固定错误日志及退避。更新旧 fake 与 runSweep 测试签名，避免计数混为“清理消息总数”。
- [ ] 更新 README 的 `000016`、独立摘要开关／批量、观察批次、暂停与部署顺序：先迁移，全部 API 升级兼容 NULL 后才启用，启用后仅能回退到兼容版本；说明元数据保留与外部副本边界。
- [ ] 用专用 PostgreSQL 运行实际二进制：全关不改数据；仅摘要开处理预先清正文且幂等已到期的消息；双开一轮完成两类工作；SIGTERM 正常退出且日志无敏感字段。包测试 PASS 后提交 `feat(retention): add opt-in digest cleaner scheduling`。

### Task 5: 整体验证、独立评审与 PR 交付

**Files:** Review Tasks 1～4；更新本计划的实际验收与执行记录，确认缺陷在所属文件修正。

**Interfaces:** 分支 `codex/p4-05-digest-retirement`，叠加草稿 PR 基于 `codex/p4-04-body-cleaner`（PR #51）。Native 执行结束后、推送和 PR 前做一次独立整分支评审；不增加逐任务独立评审。

- [ ] 启动本增量专用 PostgreSQL 16／Redis 7 并核对服务实际端口；设置 `IM_TEST_DATABASE_URL`、`IM_TEST_REDIS_URL`、`IM_TEST_BROWSER_NODE`、`NODE_PATH`、`CHROMIUM_EXECUTABLE`，创建 `btree_gist`。依赖就绪后运行 `go test ./... -count=1 -v`、`go test -race ./internal/retention ./cmd/im-retention-worker -count=1`、`go test -race ./internal/policystore -run '^TestDigestRetired' -count=1`、`go vet ./...`、`go build ./...`、`git diff --check`，日志保存并检查各结果。
- [ ] 逐项核对设计验收矩阵、五项 Review Focus 与所有关键测试；真实 `TestRealBrowserLoginRealtimeAndOfflinePull` 必须 PASS，缺依赖 SKIP 不能作为验收。区分 subprocess helper 的正常 SKIP 与未运行的集成用例。
- [ ] 按 `superpowers:executing-plans` 做一次新鲜、独立的整分支评审，重点是延迟约束最终状态、更新 ID 集合、快照判定点、保全与行锁等待、租户隔离、启用／回滚部署顺序。修正确认的问题并重跑受影响测试；记录其余边界与执行判断。
- [ ] 记录实际测试数及证据、检查提交和干净状态，推送分支，创建并 attach 叠加草稿 PR。PR 明确永久到期键、双开关、全部 API 升级前不得启用及外部副本边界；停止本增量专用服务，保留工作区。

**计划状态：**待用户评审。评审通过后按既定 Native 方式实施；本文件的未勾选步骤与测试均尚未执行，不作为已实现或生产验收证据。
