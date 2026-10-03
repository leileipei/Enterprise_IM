# P4-04 消息正文清理 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 按租户保留期限清空在线库到期正文，并以会话法务保全阻断清理，保持补拉时间线与发送幂等契约。

**Architecture:** 新增独立 PostgreSQL 清理 Worker，每租户每轮只处理一个会话小批次。按租户 → 会话锁顺序取得锁后复核保全和数据库时钟；正文更新与不可修改的批次证据同事务提交。先完成迁移和两种补拉的可空兼容，再提供默认关闭的可执行入口。

**Tech Stack:** 现有 Go 1.27.1、pgx/v5、PostgreSQL 16；复用现有测试服务与 Chrome 验收，不新增产品依赖。

**Spec:** `docs/superpowers/specs/2026-10-03-p4-04-message-body-cleaner-design.md`（已审阅的提交 `8c342d9`）。

## Global Constraints

- 正文非空时清理时间为空；正文为空时清理时间非空；清理后不得恢复正文或改写清理时间。
- 任一 `released_at IS NULL` 的会话保全阻断清理；正文与批次证据同事务提交。
- 默认保留 365 天，持久化期限为 1～3650 天；到期条件为 `accepted_at + 保留天数 × 24 小时 <= 本批实际清理时间`。
- 每会话每事务默认 100 条，上限 1000；生产清理时间来自取得锁后的数据库 `clock_timestamp()`。
- 默认关闭；仅 `IM_BODY_CLEANER_ENABLED=true` 开始循环，每轮每有效租户至多一批。
- 已清理消息仅返回 `seq` 与 `redacted:true`；分页、幂等 ACK、摘要、Outbox、成员区间和 `last_seq` 保持原契约。
- 本次范围是在线当前行的正文置空；摘要、MVCC 旧版本、WAL、归档、备份和磁盘安全擦除边界须写入交付说明。

## Review Focus

- 租户枚举后被停用：取得租户锁后重新检查 `status='active'`，不清理（Task 3）。
- `accepted_at` 与 `seq` 不同序：批量按时间选择，证据记录实际最小/最大序号及条数，不假定序号区间内全部被清理（Task 3）。
- 非法启用值或批量 0、负数、1001：配置失败且不开始清理；未启用时不连接数据库（Task 4）。
- 另一事务长时间占用候选会话：跳过该会话并处理其他候选，下一轮重新尝试（Task 3）。
- 单个租户报错或整个数据库断开：其他租户仍获得处理机会，循环退避、取消可及时结束，日志不含原始 SQL 错误详情或正文（Task 4）。

---

### Task 1: 迁移与不可逆数据约束

**Files:** Create `db/migrations/000015_message_body_clear.up.sql`、`.down.sql`、`internal/policystore/body_clear_migration_test.go`; modify `internal/policystore/migration_test.go`、`internal/policystore/conversations_test.go`、`internal/oidcauth/store_test.go`、`internal/access/migration_test.go`。

**Interfaces:** `messages.body_cleared_at timestamptz` 可空；`text_body` 可空。`message_body_clear_batches` 包含 `id uuid`、`tenant_id`、`conversation_id`、`retention_days`、`cutoff_at`、`cleared_at`、`first_seq`、`last_seq`、`cleared_count`；租户/会话组合外键，期限 1～3650、计数 1～1000、首末序号正数且有序，`cutoff_at = cleared_at - retention_days * INTERVAL '24 hours'`，UPDATE/DELETE 拒绝。候选索引为 `(tenant_id,conversation_id,accepted_at,seq) WHERE body_cleared_at IS NULL`。

- [x] 写 `TestBodyClearMigrationConstraints`：旧消息正文可读；空字符串、全空白、超过 16384 字节、正文/清理时间错配失败；合法清理成功，恢复正文和改写/撤销清理时间失败；跨租户证据及非法条数失败；批次证据 UPDATE/DELETE 失败。
- [x] 运行 `test -n "$IM_TEST_DATABASE_URL" && go test ./internal/policystore -run '^TestBodyClearMigration' -count=1 -v`；预期 RED，缺少迁移或列。
- [x] 实现 Up 的列、约束、触发器、索引和证据表；Down 先对消息与证据表取 `ACCESS EXCLUSIVE` 锁，再检查历史，存在已清理行或证据即拒绝。修改四处 Up 夹具，单聊回滚测试先执行 `000015` Down，重新应用时最后执行 Up。
- [x] 写 `TestBodyClearMigrationRollback`：无清理历史的 Down/Up 成功；有清理行或独立证据的 Down 均失败；另一连接持有清理事务时 Down 等待，清理提交后 Down 拒绝；拒绝后显式 ROLLBACK 再检查数据未变。
- [x] 运行上述迁移测试及 `go test ./internal/access ./internal/policystore ./internal/oidcauth -count=1`，检查集成测试未跳过；PASS 后提交 `feat(retention): add irreversible body clearing schema`。

### Task 2: 单聊、群聊补拉及幂等兼容

**Files:** Modify `internal/policystore/message_pull.go`、`internal/policystore/group_history.go`; create `internal/policystore/body_clear_read_test.go`。

**Interfaces:** 现有 `PullTextMessages`、`PullGroupTextMessages` 与 `MessagePage` 保持签名。SQL 同时读取可空正文与清理时间；内部群消息结构增加清理状态，已清理状态优先于可见正文构造。

- [x] 写 `TestClearedBodiesPullAndReplay`，分别覆盖单聊/群聊：通过现有发送 API 建立消息，再清空正文；读取服务时钟退回到保留期内，仍只得到遮蔽占位；混合未清理与已清理行分页无缺口且 `has_more` 正确。同内容重发 ACK 与原 ACK 相同，改内容返回原有冲突错误，消息/幂等/Outbox 各仍一行。
- [x] 运行 `test -n "$IM_TEST_DATABASE_URL" && go test ./internal/policystore -run '^TestClearedBodiesPullAndReplay$' -count=1 -v`；预期 RED，可空扫描错误或清理状态未遮蔽。
- [x] 改造两处扫描与遮蔽判断；NULL 正文不拼成可见的空消息。保留原有身份、历史任职、策略、期限和读取审计判断；现有发送逻辑仅在回归证明需要时修改。
- [x] 在同一测试中断言遮蔽项满足以下契约，运行目标测试及既有拉取/发送测试至 PASS，再提交 `fix(messages): redact cleared bodies in direct and group history`。

```go
if !item.Redacted || item.MessageID != "" || item.SenderUserID != "" ||
    item.Text != "" || !item.ServerTime.IsZero() {
    t.Fatalf("cleared message leaked fields: %+v", item)
}
```

### Task 3: 会话批次事务、保全竞态与公平租户枚举

**Files:** Create `internal/retention/worker.go`、`internal/retention/batch.go`、`internal/retention/worker_test.go`、`internal/retention/concurrency_test.go`、`internal/retention/migration_test.go`（独立 schema/连接池夹具）。

**Interfaces:** `Worker{DB access.Beginner, BatchSize int}`，私有测试注入字段 `clock func(context.Context,pgx.Tx) (time.Time,error)`；`BatchResult{TenantID, ConversationID, BatchID string; ClearedCount int; FirstSeq, LastSeq int64; RetentionDays int; CutoffAt, ClearedAt time.Time}`。`(Worker).ListActiveTenants(ctx context.Context) ([]string,error)` 按 ID 枚举有效租户；`(Worker).ProcessTenant(ctx context.Context, tenantID string) (BatchResult,error)` 返回一批或零条结果。`BatchSize=0` 采用 100，其他值必须为 1～1000，未配置 DB 返回明确错误。每批独立事务，最多三次完整死锁重试；`40P01` 以外错误直接返回。测试包使用私有时钟注入得到确定边界，生产默认时钟始终查询数据库。

- [x] 写 `TestProcessTenantExpiryAndBatchEvidence`：固定 UTC 时间，365 天精确到期、到期前 1 微秒、1 天/3650 天及跨租户分别断言；批量为 2 时只改两行，重复执行不重复计数；乱序时间的证据序号为实际 min/max；有效枚举后停用的租户返回零条。
- [x] 运行 `test -n "$IM_TEST_DATABASE_URL" && go test ./internal/retention -run '^TestProcessTenantExpiryAndBatchEvidence$' -count=1 -v`；预期 RED，Worker 未实现。
- [x] 实现租户 `FOR SHARE` → 会话 `FOR UPDATE SKIP LOCKED` → 锁后保全复核 → 锁后时钟 → bounded UPDATE RETURNING → 同事务证据。候选查询提前排除有效保全，按会话 ID 稳定排序；到期计算采用固定 24 小时而非当地日历天，统计实际更新的条数与 min/max 序号。
- [x] 写 `TestProcessTenantLegalHoldsAndLockedCandidates`：两个案件保全、只解除一个、全部解除；最早会话保全或被另一事务锁住时，后续可清理会话仍处理；暂停租户、空租户和未到期会话无证据。写 `TestProcessTenantRollbackAndCancellation`，用失败证据触发器、租户锁等待期间取消，以及正文 UPDATE 后证据 INSERT 阻塞时取消，证明正文/证据均回滚。
- [x] 写 `TestProcessTenantConcurrentWorkersAndHoldPlacement`：两个 Worker 合计计数与实际被清理行一致，无重复；保全先锁并提交后清理跳过；清理先锁提交后保全成功但不恢复正文。使用现有 `PlaceLegalHold`/`ReleaseLegalHold` 验证实际服务锁顺序，测试连接同步不使用猜测性 sleep。
- [x] 写 `TestProcessTenantRetentionLockAndDeadlockRetry`：期限更新持锁后 Worker 读取新值；Worker 持锁时更新必须等待；测试事务包装器在首次已执行 UPDATE 后注入 `40P01`，确认实际 PostgreSQL 事务回滚，并在重试前登记保全，重试后无越界清理/重复证据。连续三次 `40P01` 后返回错误，取消不继续重试。用私有时钟桩验证调用发生在租户/会话锁之后，另有生产默认时钟烟测。
- [x] 运行 `go test ./internal/retention -count=1 -v` 与 `go test -race ./internal/retention -count=1`，均带隔离 PostgreSQL 配置、无跳过；PASS 后提交 `feat(retention): clear expired bodies with legal hold locking`。

### Task 4: 默认关闭的 Worker 进程与部署说明

**Files:** Create `cmd/im-retention-worker/main.go`、`main_test.go`; modify `README.md`。

**Interfaces:** `workerConfig{DatabaseURL string; Enabled bool; BatchSize int}`；`configFromEnv(getenv func(string)string) (workerConfig,error)`。启用值只允许空、`false`、`true`；`IM_BODY_CLEANER_BATCH_SIZE` 默认 100，显式值为 1～1000；仅启用时必须提供 `IM_DATABASE_URL`。`run(ctx context.Context, config workerConfig, logger *slog.Logger) error` 未启用直接返回；`sweepWorker` 消费 Task 3 的两个方法，`runSweep(ctx context.Context, worker sweepWorker, logger *slog.Logger) (int,error)` 为每租户设置 5 秒上下文，不因单租户失败终止轮询。

- [x] 写 `TestCleanerConfigDefaultsAndRejections` 与 `TestCleanerDisabledDoesNotConnect`：默认 false/100，合法范围端点，非法启用值、批量及缺失 URL 拒绝；关闭状态配无效 DB 地址仍零连接返回。运行 `go test ./cmd/im-retention-worker -count=1 -v`，预期 RED。
- [x] 实现配置与 signal 上下文；启用后创建 pgxpool、执行轮询，成功每轮间隔 1 秒，无候选每轮间隔 1 秒，错误退避 1～30 秒，成功恢复初值。只记录批次标识/计数和固定错误类别，不将连接 URL、原始数据库错误或正文写入日志。
- [x] 写 `TestCleanerSweepFairnessAndCancellation`：使用 fake `sweepWorker` 检查每租户至多一调用，第一租户错误仍调用第二租户，每调用有 deadline，取消不再处理后续租户。用日志缓冲断言 fake 错误内的敏感正文不出现。用专用 PostgreSQL 启动生产二进制，验证默认不改数据、启用清理、SIGTERM 正常退出。
- [x] 更新 README 的 `000015` 迁移、部署顺序、显式启用与批量、暂停/观察证据方法和回滚拒绝边界；先部署迁移及兼容 API，再在核对期限/保全后启动清理。说明迁移锁与索引成本、摘要保留及 WAL/备份边界，修正原“物理清理尚未实现”描述。
- [x] 运行配置/进程测试至 PASS，提交 `feat(retention): add opt-in body cleaner process`。

### Task 5: 全量验证、复核与交付

**Files:** Review Tasks 1～4；记录勾选本计划；必要缺陷在原所属文件修正。

**Interfaces:** 交付分支 `codex/p4-04-body-cleaner`，stacked draft PR 基于 `codex/p4-03-conversation-legal-hold`（PR #50），验收结果必须区分已实现软件行为与外部存储清除。

- [ ] 启动本增量专用 PostgreSQL/Redis，设置 `IM_TEST_DATABASE_URL`、`IM_TEST_REDIS_URL`、`IM_TEST_BROWSER_NODE`、`NODE_PATH`、`CHROMIUM_EXECUTABLE`；提前创建 `btree_gist`。确认连接隔离及服务就绪后运行 `go test ./... -count=1`、`go vet ./...`、`go build ./...`、`git diff --check`；显式检查 `TestRealBrowserLoginRealtimeAndOfflinePull` 的 PASS，不能把缺少变量的 SKIP 当作验收。
- [ ] 独立代码复核迁移不变量、锁后保全判断、跨租户筛选、NULL 读取、重发 ACK 和默认关闭行为；修复确认的缺陷，重跑受影响测试。
- [ ] 确认 Git 提交与状态，推送分支，创建并 attach stacked draft PR；PR 说明正文置空、摘要保留、迁移锁、验证范围和启用方式。停止本增量专用服务，交付 PR 与测试证据。

执行方式沿用本会话逐项实现（Native），使用 `superpowers:executing-plans`；完成实施后做独立整分支复核。
