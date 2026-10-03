# P4-19 跨会话文本搜索 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 交付普通员工在一个有效任职下跨本人单聊／群聊历史的文本搜索 API，同时维持现有历史读取、权限与审计合约。

**Architecture:** 候选按本人历史参与关系枚举，以 UUID／seq 稳定推进，并共用一个 READ COMMITTED 事务、全页预算和成功审计。先整理事务内历史读取与最终可见性助手，再增加候选索引和跨会话服务，最后注册生产 HTTP 路由并验证真实 OIDC 双节点续查。Web 跨会话入口另属 P4-20。

**Tech Stack:** 现有 Go、pgx/v5、PostgreSQL、net/http、OIDC；沿用项目测试中的 Redis／Chrome 回归环境，不引入新的生产依赖或正文索引副本。

**Spec:** [已确认设计](../specs/2026-10-03-p4-19-cross-conversation-text-search-design.md)，用户于 2026-10-03 确认；设计提交 `39395e3c9bfec009a2b7640836e9dc4965b9f71a`，依赖 P4-18 基线 `33b3b8eebdc8f70afe51a6ebed1af8f6ba1c3170`。

## Global Constraints

- 每页预算：最多 **20 个会话**，合计 **500 条时间线消息**，最多 limit 条授权匹配。
- 合并后候选探测上限 21 个；三个来源各取最多 21 个 ID，因此 all 范围内候选 SQL 最多向服务传递 63 个 ID（群区间去重的底层扫描另计）。
- 消息 lookahead 最多每处理会话额外 1 行，因此读取行最多 500＋20；其中最多 500 条进入授权／匹配管道。
- 服务内部整页 deadline 5 秒，重试共享同一 deadline；statement_timeout=4 秒／lock_timeout=1 秒使用事务局部设置，不污染连接池。
- 对 PostgreSQL 40P01／40001 最多重试整页两次，每次重新取时间与授权，超出或请求取消返回 503。
- `GET /api/v1/messages/search?q=关键词&kind=all&limit=20&cursor=...`；q 复用 Go TrimSpace＋简单小写、2～100 码点；kind 为 all／direct／group；limit 默认 20、1～50；cursor 最多 2048 字节。
- 当前身份不合并不同任职授权；管理员不增加历史参与权限；法务保全不延长可见期。先授权再匹配，不写关键词／正文／原始查询串日志或审计。
- 全页同事务；最终复核全部待返回正文；成功审计 action=`message_search_all`、resource_type=`tenant`、resource_id 为可信 tenant、reason=`cross_conversation_search_page`。失败不返回部分消息或游标。
- seq 为十进制字符串；按 PostgreSQL UUID 会话 ID 升序、会话内 seq 升序；不返回会话名称、候选列表、总命中数和隐藏命中。
- 文档、迁移验证及本地集成通过不等于生产容量、上线、客户联调或完整 M4 验收。

## Review Focus

1. **RF1 身份有效但旧进度会话已不属于本人：**安全跳过至更大 ID，不能读正文、抛出会话存在信息或循环；新加入／新消息落在旧进度前时须刷新首页。归属 Task 3。
2. **RF2 重试与连接池状态：**最多 3 次尝试共享 5 秒 deadline；事务超时不能残留到连接后续业务，提交失败不能泄漏本页结果。归属 Task 3。
3. **RF3 先取得的正文在后续会话等待时到期：**最终按同一新时间过滤所有暂存结果；未交付匹配不被游标跳过；群级 hard_deny 按最终时间生效。归属 Task 1、3。
4. **RF4 两个页预算同时到边界：**第 20 个空会话、合计第 500 条消息及结果上限附近都必须严格前进，无重复／漏交；has_more 不保证下页命中。归属 Task 3。
5. **RF5 页面中途失败或审计不可用：**前面会话读成功也不返回部分结果；失败事务中的成功审计不留存，成功页只留一条成功审计。归属 Task 3、4。

## 文件与职责

| 文件 | 职责 |
| --- | --- |
| `internal/policystore/history_read_tx.go`（新） | 私有读取上下文、授权证据、事务内批次与最终可见性助手；不提交、不写成功审计 |
| `internal/policystore/message_pull.go`、`group_history.go` | 既有单会话包装器调用私有助手，保持补拉／搜索合约及审计 |
| `internal/policystore/history_read_tx_test.go`（新） | 私有最终授权函数的时间／策略回归，package policystore |
| `db/migrations/000017_cross_message_search_indexes.{up,down}.sql`（新） | 单聊双方按租户／用户／ID 枚举的两个部分索引 |
| `internal/policystore/cross_message_search_migration_test.go`（新）、`migration_test.go` | 索引 Up／Down 及通用测试迁移清单 |
| `internal/policystore/cross_message_search_cursor.go`、`cross_message_search_cursor_test.go`（新） | 严格规范游标及可信绑定，私有纯测试 |
| `internal/policystore/cross_message_search_candidates.go`（新） | 三个候选 keyset 流的有界枚举、去重／合并及候选资格复核 |
| `internal/policystore/cross_message_search.go`、`cross_message_search_test.go`（新） | 跨会话公开服务、预算、整页事务、审计及真实数据库回归 |
| `internal/httpserver/cross_message_search.go`、`cross_message_search_test.go`（新） | 独立新路由与严格参数，保持旧路由与解析器 |
| `cmd/im-api/main.go` | OIDC 配置分支中的生产路由注册 |
| `internal/policystore/cross_message_search_api_integration_test.go`（新）、`realtime_production_api_integration_test.go` | 复用真实 OIDC 双进程 API 验证新合约 |
| `README.md`、`docs/开发增量-P4-19-验收记录.md`（新）、开发路径文档 | 实际运行证据、范围、审阅与交付状态 |

本文勾选步骤已实施，未勾选步骤以文末状态和验收记录为准。实施时复用当前附加的隔离工作树，并从确认计划的 HEAD 建 `codex/p4-19-cross-conversation-search`，保留堆叠草稿依赖；不移动主检出、不合并、不启动清理 Worker。

---

### Task 1：可复用的事务内历史读取与最终授权（P4-19a）

**Files:** Create `history_read_tx.go`、`history_read_tx_test.go`；Modify `message_pull.go`、`group_history.go`、`group_history_test.go`；Test 既有 `message_pull_test.go`、`group_history_test.go`、`message_search_test.go`。以上均在 `internal/policystore/`。

**Interfaces:**
- Produces 私有 `historyReadContext`，包含可信 Identity、Actor policy.Membership、Rules []policy.Rule、Retention time.Duration；私有 `historyReadBatch` 保存 MessagePage 和逐消息历史授权证据，原始正文不得输出至 HTTP。
- `newHistoryReadContextTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, lockPolicy bool) (historyReadContext,error)`：锁定及载入身份／期限／策略，不提交；失效返回 ErrForbidden，由包装器完成原有 deny 审计。
- `readDirectHistoryBatchTx(ctx context.Context, tx pgx.Tx, scope historyReadContext, conversationID string, afterSeq int64, limit int) (historyReadBatch,error)`；同签名 `readGroupHistoryBatchTx`：复核并锁定会话／历史关系，最多 limit 条＋1 探测，不做关键词匹配。
- `filterDirectHistoryBatch(scope historyReadContext, batch historyReadBatch, at time.Time) MessagePage`；`filterGroupHistoryBatchTx(ctx context.Context, tx pgx.Tx, scope historyReadContext, batch historyReadBatch, at time.Time) (MessagePage,error)`：按最终时点检查期限及 hard_deny，保留 redacted 序号；群沿用 groupHistoryHardDenied。
- Consumes 既有 `loadMembership`、`memberActiveAt`、`messageBodyRetentionForTenant`、`loadRules`、`policy.HistoryHardDeny`、`groupHistoryHardDenied`；不新增导出授权捷径。

- [x] **1. 建立回归基线。**启用专用 PostgreSQL／Redis 测试容器及现有 Chrome 环境，记录实际端口、测试 DSN、HEAD 和完整 `go test ./... -count=1 -v` 日志；必须退出 0，除辅助 TestRealtimeAPIChild 外不得因缺失依赖跳过。
- [x] **2. 写私有助手 RED 测试。**`TestHistoryFinalDirectVisibility` 在 acceptedAt＋Retention 之前正文可见、恰好到期时仅保留原 seq；规则 EffectiveFrom 在最终 at 生效时正文遮蔽。`TestHistoryBatchKeepsUnauthorizedEvidencePrivate` 验证缺失／不匹配历史快照不产生可见正文。首先以新增接口未定义而编译失败记录 RED；不把既有 Green 回归称为 RED。
- [x] **3. 实现上述私有上下文与批次接口。**保持消息上限 1～500；不 Begin／Commit／写 allow 审计；最终过滤复用现有策略，单聊序号、群区间和发送者身份检查不变。
- [x] **4. 接入两个现有包装器。**包装器仍负责事务、deny／allow 原动作与原因及提交；搜索仍显式 READ COMMITTED；单聊政策锁及群搜索政策锁不减弱，普通补拉原隔离／错误合约保留。核对与发送／成员管理／策略发布／清理的行锁顺序，记录变化，避免无关结构重写。
- [x] **5. 验证 RF3 和原合约。**在既有外部测试包 `group_history_test.go` 增加 `TestHistoryFinalGroupPolicyTime`，复用现有真实 DB 与群夹具：原先可见正文在最终 at 到期或群级 hard_deny 生效后遮蔽。运行 `go test ./internal/policystore -run '^(TestHistory|TestPullTextMessages|TestPullGroupTextMessages|TestMessageSearch)' -count=1 -v`，确认全部通过且 DB 用例未跳过；再运行旧 HTTP 补拉／搜索用例。
- [x] **6. 提交独立增量。**`git add` 本任务文件；`git commit -m 'refactor(history): share transaction scoped reads and final authorization'`。此时只有重构和回归，不宣称新 API 已交付。

### Task 2：单聊候选 keyset 索引（P4-19b 前置）

**Files:** Create 两个 `000017_cross_message_search_indexes` SQL 及 `cross_message_search_migration_test.go`；Modify `internal/policystore/migration_test.go` 迁移清单。

**Interfaces:**
- Produces `conversations_direct_low_search ON conversations(tenant_id,direct_user_low_id,id) WHERE kind='direct'` 与 high 对应索引。
- Consumes 既有 conversations 复合租户约束、群区间 `conversation_membership_user_lookup`、消息 `(tenant_id,conversation_id,seq)` 唯一索引；不改正文表。

- [x] **1. 写索引 RED。**`TestCrossMessageSearchIndexesUpDown` 检查 pg_indexes 中两个索引均存在，列序和 kind 谓词正确；Down 后两个索引不存在但既有消息／会话数量不变，再 Up 可恢复。首次因索引／迁移缺失失败。
- [x] **2. 实现 Up／Down 并追加测试清单。**Up 用标准 CREATE INDEX，Down 只 DROP 本任务两个索引；不删除数据，不新增正文投影，迁移编号为 000017。
- [x] **3. 验证迁移。**`go test ./internal/policystore -run '^TestCrossMessageSearchIndexesUpDown$' -count=1 -v`，全部通过；已有 DB 初始化仍正常。生产并发建索引与上线窗口留在部署评估中。
- [x] **4. 提交。**`git commit -m 'feat(search): index direct participants for history discovery'`，仅加入本任务迁移与测试。

### Task 3：严格游标、候选枚举及跨会话事务服务

**Files:** Create `internal/policystore/cross_message_search_{cursor,candidates}.go`、`cross_message_search_cursor_test.go`、`cross_message_search.go`、`cross_message_search_test.go`。

**Interfaces:**
- Produces 导出 `CrossConversationMatch{ConversationID string, Kind string, Message PulledMessage}` 与 `CrossConversationSearchPage{Messages []CrossConversationMatch, HasMore bool, NextCursor string}`。
- `Service.SearchAllTextMessages(ctx context.Context, id access.TrustedIdentity, query, kind, cursor string, limit int) (CrossConversationSearchPage,error)`；kind 必须为 all／direct／group，HTTP 在省略时补 all；复用 ErrInvalidMessageSearch／ErrForbidden，其他存储或时间错误由 HTTP 映射 503。
- 私有 `crossSearchBinding{Tenant,User,Membership,Query,Kind string}`、`crossSearchPosition{Conversation,Phase string; After int64}`。`encodeCrossSearchCursor(binding crossSearchBinding, position crossSearchPosition) string` 与 `decodeCrossSearchCursor(cursor string, binding crossSearchBinding) (crossSearchPosition,error)`；空 cursor 仅表示首页位置。
- 私有 `historyCandidate{ID,Kind string}`；`listCrossSearchCandidatesTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, kind string, position crossSearchPosition) ([]historyCandidate,error)`，返回合并后最多 21 个。within 来源使用 >= 当前 ID，after 用 >，使续查会话也占来源查询额度；三流共最多 63 个 ID，不额外加载第 64 个来源 ID。
- Consumes Task 1 上下文／批次／最终过滤接口及 Task 2 索引。实现私有 `searchAllTextMessagesOnce` 承载一次事务，外层按 40P01／40001 最多重试 2 次，共享 deadline。`refreshCrossSearchPolicyTx(ctx context.Context, tx pgx.Tx, scope historyReadContext) (historyReadContext,error)` 在最终校验前重新查询并共享锁定 current 指针；首查无行时不能声称锁住缺失行，若首版策略已发布则重载规则用于全部最终过滤。

- [x] **1. 写纯游标 RED。**`TestCrossSearchCursorStrictBinding` 检查规范 roundtrip、绑定改变拒绝、未知／重复 JSON 字段、非法类型、v 非 1、非小写 UUID、超过 2048、非法 phase、after 阶段非零 After、负数／前导零／int64 溢出均返回 ErrInvalidMessageSearch；q 等价输入可续查，limit 调整不改变绑定。
- [x] **2. 实现游标。**JSON 固定字段 `v,t,u,m,q,k,c,p,a`，a 为字符串；检查精确字段集合、值和规范编码，不接受 P4-17 游标；不依赖签名密钥，也不把进度作为授权。
- [x] **3. 写服务 RED。**`TestCrossMessageSearchHistoricalScope` 用现有 db／seed／seedDirectConversation／群成员夹具，覆盖当前及旧任职单聊、已退／再入群、policy_blocked／ended；断言跨租户、非参与人与管理员额外身份无消息；结果无名称、无 redacted 正文。首次因 SearchAllTextMessages 未定义失败。
- [x] **4. 实现候选枚举。**low／high 单聊和本人历史群区间分别 keyset 取最多 21 个 ID，以 UUID 合并、去重并检查同租户类型；每一会话复核资格，不能用现有当前会话列表替代。大量群区间去重不声称底层扫描有 63 行上限。
- [x] **5. 实现一次整页事务。**入口校验输入／可信 UUID 后创建整页 5 秒 context；Begin 并显式 READ COMMITTED、事务局部 set_config 超时 4s／1s；锁身份、期限及策略；沿 Task 1 助手收集历史批次、按 20 会话／500 消息合计预算授权后匹配，最后复核策略指针并以一个新 at 复核所有结果、记录单条 allow 审计并提交。任何错误回滚，返回零值页。
- [x] **6. 验证 RF3／RF4 的进度。**`TestCrossMessageSearchCombinedBudgets`：21 个空会话第一页处理 20 个且可续查；一个会话 300 条＋下个 250 条时第一页合计处理 500；命中 seq501／502 续查无漏交；limit1 在同／异会话边界逐页无重复。`TestCrossMessageSearchFinalExpiry` 使第二会话等待后时间跨第一会话正文到期，第一正文不返回；未交付的合法匹配不被游标跳过。
- [x] **7. 验证 RF1 的候选重查。**`TestCrossMessageSearchCursorDoesNotGrantScope` 篡改 within 至他人会话只安全推进，不能读正文；资格消失、空会话及 all／direct／group切换有确定结果。`TestCrossMessageSearchLiveProgressNeedsRefresh` 在已走过的会话追加消息／插入更小 ID，不要求旧游标发现；首页刷新能找到，文档明确该限制。
- [x] **8. 验证历史、Unicode及 RF5。**`TestCrossMessageSearchVisibilityAndLiteralQuery` 覆盖退群缺口、发送者任职错误、缺失快照、hard_deny、保全过期、清空正文、NEL／FEFF／İ／sigma及字面 `%_\\`。`TestCrossMessageSearchAuditAndPartialFailure` 在后续会话或 audit 写失败时无部分正文／cursor、无留存 allow；成功页 audit 恰好一次，reason 与 resource 可信且不含 q。
- [x] **9. 验证 RF2 及真实撤权。**`TestCrossMessageSearchDeadlineRetryAndCommit` 用包装 Beginner／Tx 控制 40P01／40001、提交失败与次数，断言最多3次、5秒共享 deadline、最终失败返回零值页。`TestCrossMessageSearchRechecksIdentityAfterWait` 用两个真实 PG 连接控制账号冻结／任职失效及默认 REPEATABLE READ，拒绝旧身份且无成功审计。真实锁等待触发 timeout 后借同一连接查 SHOW，验证局部超时不污染后续事务；`TestCrossMessageSearchFirstPolicyPublication` 用两连接在初查无 current 行后发布首个 hard_deny，确认最终复核能采用新规则；真实清理／成员并发覆盖最终状态。
- [x] **10. 运行目标、race 与 SQL 测量。**`go test ./internal/policystore -run '^(TestCross|TestHistory|TestMessageSearch)' -count=1 -v` 全部通过；对应 `-race` 无竞态。记录至少 1000 个人会话、10000 消息及单群 100 个历史区间的本地 EXPLAIN ANALYZE（候选及消息 keyset）；给出实际规模／底层扫描／耗时，不硬断言 planner 必选索引或达到生产 SLA。
- [x] **11. 提交。**`git commit -m 'feat(search): add bounded authorized cross conversation queries'`，保留实际 RED／GREEN／race／SQL 日志路径供最终验收记录。

### Task 4：HTTP、真实双节点注册与交付

**Files:** Create `internal/httpserver/cross_message_search.go`、`cross_message_search_test.go`、`internal/policystore/cross_message_search_api_integration_test.go`、中文验收记录；Modify `cmd/im-api/main.go`、`realtime_production_api_integration_test.go`、README 和开发路径。

**Interfaces:**
- Consumes Task 3 导出服务／结果类型。
- Produces `CrossMessageSearchService`，方法签名与 SearchAllTextMessages 完全相同；`HandlerWithCrossMessageSearch(base http.Handler, auth Authenticator, service CrossMessageSearchService) (http.Handler,error)`，只匹配精确 `/api/v1/messages/search`。
- DTO 为 Spec 第 5 节定义；数组为空时为 []，seq 为字符串，不输出 PulledMessage.Redacted。旧 MessageSearchService／parseMessageSearchQuery 不增加 kind 参数、不改变合约。
- 新 `assertProductionCrossMessageSearch(t *testing.T, conn *pgx.Conn, client *http.Client, first, second, token string)` 在现有 assertProductionMessageSearch 之后调用，复用已存在两条单聊及一条群消息，不引入 UI 产品改动。

- [x] **1. 写 HTTP RED。**`TestCrossMessageSearchRouteContract` 发 GET 新路径，未注册时实际404；实现后检查可信身份、默认 kindall／limit20、不同会话同 seq、字符串9007199254740993、空页续查、no-store及无正文名称／redacted字段。
- [x] **2. 写严格参数与 RF5 错误测试。**`TestCrossMessageSearchRouteRejectsUnsafeInput` 覆盖未知／重复参数、kind空／大小写／非法值、q边界、cursor空／过长、limit符号／非整数、GET正文、编码错误。`TestCrossMessageSearchRouteMapsErrors` 断言400／401／403／503／405＋Allow，服务返回内部错误不泄漏详情且503无messages／cursor，未知其他路径交回base。
- [x] **3. 实现新路由与生产注册。**用既有可信 authenticator 和请求拒绝日志，独立解析最多4种参数；仅在 cmd/im-api 已启用 OIDC 的分支注册新包装器，避免无认证时开放查询。
- [x] **4. 写真实生产 RED。**在两个真实 OIDC 认证 API 上请求新路径，注册前404；注册后 q=生产、kindall、limit1交替节点续查收集两条单聊及一条群消息，核对完整集合、ID／kind／seq与排序。成功审计数量按实际成功页数（含合法空末页）精确比较，不假定3次分页请求。kinddirect／group分别验证过滤，变词／变kind游标400，无效令牌401；原 message_search 的3条审计数量不受影响。
- [x] **5. 跑生产与全量验证。**`go test ./internal/httpserver ./internal/policystore -run '^(TestCross|TestProductionAPIWithOIDCAndRealtimeProcesses)' -count=1 -v`；再跑 `go test ./... -count=1 -v`、必要 race、`go vet ./...`、`go build ./...`、`git diff --check`。启用 DB／Redis／Chrome，记录真实退出码／顶层计数／唯一辅助skip；原 P4-18 页面和 OIDC 实时／离线恢复必须通过。
- [x] **6. 更新实际验收文档并独立评审。**只记录已运行的行为、计数、迁移、SQL规模、风险与日志；P4-19为API交付，不写成Web跨会话搜索。按 requesting-code-review 技能请求一次独立全分支评审，解决发现问题，再运行受影响验证；不得把评审只读日志当作评审独立跑过集成。
- [x] **7. 提交／推送／草稿 PR。**`git commit -m 'feat(api): expose cross conversation text search'`；推送 codex/p4-19-cross-conversation-search，以确认计划的设计分支为base创建草稿PR并attach，核对OPEN／DRAFT、base／headSHA与远端一致、工作树干净。关闭本轮专用测试容器，保留工作树；中文交付链接、验证、边界和P4-20后续。

## 自检与执行交接

已核对 Spec 的范围、授权、API、候选预算、游标、事务、索引、失败和验收矩阵，分别落入 Task 1～4；RF1～RF5均有明确测试。所有接口在生产者任务定义，文件名与迁移编号对应当前源码；没有实现占位或新增正文副本。Task 1、2可独立验证，但Task 3依赖两者，Task 4依赖Task 3，因此沿用由当前助手逐项实现，完成后独立全分支评审的执行方式。

本计划已于 2026-10-03 经用户确认，使用 superpowers:executing-plans 在当前会话逐项实施；未另建用户聊天任务。跨会话 Web 入口、附件内容索引和完整 M4 留后续。

执行调整：上下文构造器采用 Service 方法以使用注入时钟；实际策略发布先锁 tenant FOR UPDATE，搜索持有 tenant FOR SHARE，因此首版发布测试验证发布等待当前页提交、下一页应用 hard_deny。保留最终策略指针复核。

最终实施源码 472 个顶层 PASS／0 FAIL／1 辅助 SKIP，独立评审无 Critical／Important；一个组合边界测试 Minor 记录为后续补强，详见验收记录。已创建并附加实现草稿 PR #67，最终远端 SHA／状态核对后结束本轮。
