# P4-28 消息 ACK 时间一致性修复与验证记录

日期：2026-10-06。
状态：ACK 缺陷修复验证通过；完整测试门禁已执行但未通过；交付独立草稿 PR。

## 1. 范围与基线

- 基线：主分支 `80424dbd5a537023c33e56654f4a12b522885a21`。
- 生产逻辑修复：`2019c240ccf03357d52c713dcf742c21ddeb41cb`。
- 最终代码及测试：`7148b5ec787745784a74bb11f98e866c8048f529`。后一个提交仅修正测试时区比较。
- 分支：`codex/p4-28-message-ack-time`，与 P4-27 导入预检草稿分支独立。
- 本次涉及单聊文本、群聊文本、单聊附件、群聊附件的持久化 ACK。

## 2. 原因与修复

首次发送曾以 Go 应用时钟的纳秒值返回 `server_time`，幂等重试和历史拉取则读取 PostgreSQL `messages.accepted_at`，该字段存储微秒精度。例如首次 `...00.123456789Z`、重试 `...00.123456Z`，消息 ID 和序号相同但响应时间不一致。

在单聊、群聊两处 `INSERT INTO messages` 中，使用 `RETURNING id::text, accepted_at` 扫描消息 ID 与 ACK 时间；首次 ACK 的时间因此直接来自持久化记录。附件发送复用这两条路径。HTTP ACK 和历史投影继续按已有规则输出 UTC。

授权校验仍使用完整精度的应用时钟，包括数据库锁等待后的提交前复查。序号、幂等、限流、Outbox、审计与回滚事务不变；没有修改数据库结构、API 字段或依赖。

## 3. 回归验收

新增 `TestMessageACKUsesPersistedTime`：四种消息路径 × `123456789` / `999999999` 两种纳秒值，共八子用例。使用真实 PostgreSQL；比较首次 ACK、推进一秒时钟后的幂等 ACK、历史拉取、消息行、幂等行及 Outbox 时间，并检查重复请求只写一次。期望来自数据库行，不硬编码截断算法。

| 验证 | 结果 |
| --- | --- |
| 旧主分支 + 新增测试，UTC | 八子用例全部按预期失败，首次 ACK 与数据库时间不同 |
| 最小生产修复，UTC | 八子用例通过；原始 OIDC→HTTP→消息 ACK 用例通过 |
| 测试时区问题，Asia/Shanghai | 复现八子用例误报；统一 UTC 比较后八子用例及 HTTP 用例通过 |
| 最终固定提交，消息相关数据库回归 | 46 个顶层用例、84 个子用例通过，0 失败、0 跳过 |
| 最终固定提交，完整 OIDC 包 | 18 个顶层用例、12 个子用例通过，0 失败、0 跳过 |
| 仓库构建及 vet | 全部通过 |
| 捆绑 Node 补验 | 原先因 PATH 无 Node 失败的六项前端单元测试通过 |

消息回归覆盖发送、冲突重试、成员退出后的重放、历史拉取、并发序号／同键发送、审计回滚，以及数据库锁等待后任职或策略跨越时间边界的拒绝路径。

环境：Go 1.27.1，Linux arm64 测试二进制；PostgreSQL 16.14。临时数据库容器无外部网络、无映射端口，数据目录为 tmpfs，冻结源码和二进制只读挂载。所有测试阶段均核对并删除本次拥有的容器。

## 4. 完整套件结果及限制

已经执行 `go test -json -count=1 ./...`，退出码 1：388 顶层用例与 374 子用例通过，46 顶层与 65 子用例失败，464 顶层与 328 子用例跳过。两个失败包为 `internal/policystore`、`internal/webclient`。该运行是生产修复提交的冻结快照；与最终固定提交相比仅新增测试的 UTC 比较有差别，生产文件逐字节相同。

也执行了整个 `internal/policystore` 的真实 PostgreSQL 回归，退出码 1：393 顶层与 435 子用例通过，40 顶层与 50 子用例失败，16 顶层跳过。失败涉及未配置的 Redis、S3 专用角色／扫描器／浏览器进程夹具，以及 PostgreSQL 测试容器未带 Go 工具；与本次 ACK 精度路径无关。该整包阶段失败后 OIDC 没有继续执行，之后在最终固定提交单独运行完整 OIDC 包并通过。

六项前端 Node 缺失失败已用捆绑 Node 24.19.0 补验通过；没有把其他失败或跳过项计为通过，也没有声称完整套件全绿。客户身份源联调、完整附件端到端、M4 和生产放行仍待相应环境验收。

### 完整仓库运行中的失败用例（顶层）

- `internal/webclient:TestWebFileDownload`
- `internal/webclient:TestWebFileMessages`
- `internal/webclient:TestWebFilePolicy`
- `internal/webclient:TestWebFileSearch`
- `internal/webclient:TestWebFileTransfer`
- `internal/webclient:TestWebFileTransport`
- `internal/policystore:TestFileBusinessProcessRP14`
- `internal/policystore:TestFileBusinessProcessRP11`
- `internal/policystore:TestFileBusinessProcessRP01`
- `internal/policystore:TestFileBusinessProcessRP02`
- `internal/policystore:TestFileBusinessProcessRP12`
- `internal/policystore:TestFileBusinessProcessRP13`
- `internal/policystore:TestFileBusinessProcessRP03`
- `internal/policystore:TestFileBusinessProcessRP04`
- `internal/policystore:TestFileBusinessProcessRP05`
- `internal/policystore:TestFileBusinessProcessRP06`
- `internal/policystore:TestFileBusinessProcessRP10`
- `internal/policystore:TestFileBusinessProcessRP07`
- `internal/policystore:TestFileBusinessProcessRP08`
- `internal/policystore:TestFileBusinessProcessRP09`
- `internal/policystore:TestFileBusinessRuntimeSchema`
- `internal/policystore:TestFileBusinessRuntimePrivileges`
- `internal/policystore:TestFileBusinessRuntimeFunctionPrivilege`
- `internal/policystore:TestFileDownloadAuditRuntimeSchema`
- `internal/policystore:TestFileDeleteRealVersionsIAM`
- `internal/policystore:TestFileDeleteRealUnknownDelete`
- `internal/policystore:TestFileDeleteRealHoldOrdering`
- `internal/policystore:TestFileDeleteRealOrphanQuarantine`
- `internal/policystore:TestFileDeleteRealMarker403`
- `internal/policystore:TestFileDownloadRealOIDCScan`
- `internal/policystore:TestFileDownloadRealTokenExpiryBlockedWrite`
- `internal/policystore:TestFileDownloadRealRevocation`
- `internal/policystore:TestFileDownloadRealAuditRepair`
- `internal/policystore:TestFileDownloadRealTotalDeadline`
- `internal/policystore:TestFileDownloadRealTCPRevocation`
- `internal/policystore:TestWebFileRealLifecycle`
- `internal/policystore:TestWebFileRealRejectedScan`
- `internal/policystore:TestWebFileRealSettings`
- `internal/policystore:TestWebFileRealProductionClosed`
- `internal/policystore:TestWebFileRealSearch`
- `internal/policystore:TestWebFileRealSearchFinalBoundary`
- `internal/policystore:TestWebFileRealContextIsolation`
- `internal/policystore:TestWebFileRealUnknownUploadSend`
- `internal/policystore:TestWebFileRealDownloadFaults`
- `internal/policystore:TestWebFileRealRevocation`
- `internal/policystore:TestWebFileRealPolicyConflict`

### 完整 PostgreSQL 包的失败用例及首个原因

- `TestFileBusinessProcessRP14`：file_business_process_browser_test.go:163: required process fixture missing IM_TEST_REDIS_URL
- `TestFileBusinessProcessRP11`：file_business_process_budget_test.go:17: required process fixture missing IM_TEST_REDIS_URL
- `TestFileBusinessProcessRP01`：file_business_process_configuration_test.go:21: required process fixture missing IM_TEST_REDIS_URL
- `TestFileBusinessProcessRP02`：file_business_process_configuration_test.go:114: required process fixture missing IM_TEST_REDIS_URL
- `TestFileBusinessProcessRP12`：file_business_process_configuration_test.go:182: required process fixture missing IM_TEST_REDIS_URL
- `TestFileBusinessProcessRP13`：file_business_process_configuration_test.go:236: required process fixture missing IM_TEST_REDIS_URL
- `TestFileBusinessProcessRP03`：file_business_process_lifecycle_test.go:133: required process fixture missing IM_TEST_REDIS_URL
- `TestFileBusinessProcessRP04`：file_business_process_lifecycle_test.go:178: required process fixture missing IM_TEST_REDIS_URL
- `TestFileBusinessProcessRP05`：file_business_process_lifecycle_test.go:222: required process fixture missing IM_TEST_REDIS_URL
- `TestFileBusinessProcessRP06`：file_business_process_lifecycle_test.go:258: required process fixture missing IM_TEST_REDIS_URL
- `TestFileBusinessProcessRP10`：file_business_process_lifecycle_test.go:283: required process fixture missing IM_TEST_REDIS_URL
- `TestFileBusinessProcessRP07`：file_business_process_recovery_test.go:150: required process fixture missing IM_TEST_REDIS_URL
- `TestFileBusinessProcessRP08`：file_business_process_recovery_test.go:270: required process fixture missing IM_TEST_REDIS_URL
- `TestFileBusinessProcessRP09`：file_business_process_recovery_test.go:423: required process fixture missing IM_TEST_REDIS_URL
- `TestFileDeleteRealVersionsIAM`：file_delete_real_integration_test.go:102: dedicated upload role required
- `TestFileDeleteRealUnknownDelete`：file_delete_real_integration_test.go:212: dedicated upload role required
- `TestFileDeleteRealHoldOrdering`：file_delete_real_integration_test.go:264: dedicated upload role required
- `TestFileDeleteRealOrphanQuarantine`：file_delete_real_integration_test.go:318: dedicated upload role required
- `TestFileDeleteRealMarker403`：file_delete_real_integration_test.go:362: dedicated upload role required
- `TestFileDownloadRealOIDCScan`：file_download_real_integration_test.go:207: dedicated upload role required
- `TestFileDownloadRealTokenExpiryBlockedWrite`：file_download_real_integration_test.go:379: dedicated upload role required
- `TestFileDownloadRealRevocation`：file_download_real_integration_test.go:487: dedicated upload role required
- `TestFileDownloadRealAuditRepair`：file_download_real_integration_test.go:545: dedicated upload role required
- `TestFileDownloadRealTotalDeadline`：file_download_real_integration_test.go:584: dedicated upload role required
- `TestFileDownloadProductionClosed`：file_download_real_integration_test.go:616: build im-api: exec: "go": executable file not found in $PATH:
- `TestFileDownloadRealTCPRevocation`：file_download_real_integration_test.go:639: dedicated upload role required
- `TestFileMessageRealScanSendPull`：file_message_real_integration_test.go:328: build im-api: exec: "go": executable file not found in $PATH:
- `TestFileMessageProductionClosed`：file_message_real_integration_test.go:453: build im-api: exec: "go": executable file not found in $PATH:
- `TestFileMessageRetiredHTTP`：file_message_real_integration_test.go:479: build im-api: exec: "go": executable file not found in $PATH:
- `TestWebFileRealLifecycle`：web_file_real_integration_test.go:13: required runtime missing: IM_TEST_REDIS_URL
- `TestWebFileRealRejectedScan`：web_file_real_integration_test.go:37: required runtime missing: IM_TEST_REDIS_URL
- `TestWebFileRealSettings`：web_file_real_integration_test.go:46: required runtime missing: IM_TEST_REDIS_URL
- `TestWebFileRealProductionClosed`：web_file_real_integration_test.go:55: required runtime missing: IM_TEST_REDIS_URL
- `TestWebFileRealSearch`：web_file_search_integration_test.go:14: required runtime missing: IM_TEST_REDIS_URL
- `TestWebFileRealSearchFinalBoundary`：web_file_search_integration_test.go:86: required runtime missing: IM_TEST_REDIS_URL
- `TestWebFileRealContextIsolation`：web_file_test_helpers_test.go:659: required runtime missing: IM_TEST_REDIS_URL
- `TestWebFileRealUnknownUploadSend`：web_file_security_integration_test.go:19: required runtime missing: IM_TEST_REDIS_URL
- `TestWebFileRealDownloadFaults`：web_file_security_integration_test.go:36: required runtime missing: IM_TEST_REDIS_URL
- `TestWebFileRealRevocation`：web_file_security_integration_test.go:44: required runtime missing: IM_TEST_REDIS_URL
- `TestWebFileRealPolicyConflict`：web_file_security_integration_test.go:52: required runtime missing: IM_TEST_REDIS_URL

完整日志保留了所有失败子用例名称及跳过项。上述列表用于说明已执行的完整门禁状态，不是排除测试后报告全绿。

## 5. 只读评审与处置

独立只读评审对生产修复未发现 Critical / Important 问题，提出一项 Minor：测试实际时间格式化应同样归一化 UTC。已在 `Asia/Shanghai` 真实复现并修正；最终 UTC 回归通过，时区 GREEN 快照与最终代码逐字节一致。没有追加第二次独立评审。

评审建议的合入前置条件是完整数据库回归及全仓库测试通过，目前该条件尚未满足。修复以 draft PR 交付，尚未合并或部署。

## 6. 复现与证据

对已配置数据库及独立测试 schema 的开发环境，可执行：

```sh
TZ=UTC go test -count=1 ./internal/policystore -run '^TestMessageACKUsesPersistedTime$'
TZ=Asia/Shanghai go test -count=1 ./internal/policystore -run '^TestMessageACKUsesPersistedTime$'
go test -count=1 ./internal/oidcauth
go build ./...
go vet ./...
```

数据库用例需要 `IM_TEST_DATABASE_URL`；缺失时会跳过，跳过不构成数据库验收。完整端到端还需要仓库规定的 Redis、S3、扫描器、浏览器及专用测试角色夹具。

本地证据目录（不进入仓库）：`/Users/leo.cui/Documents/Codex/Enterprise_IM/p4-28-ack-time-20261006`。

- `red/`：旧主分支测试覆盖及预期失败日志。
- `green-focused/`：初始 GREEN、完整仓库测试、构建／vet、Node 补验。
- `green-fixed/`：初始修复提交的整包 PostgreSQL 运行，保留失败状态。
- `timezone-red/`、`timezone-green/`：测试时区问题的 RED／GREEN。
- `regression-fixed/`：最终固定提交的消息相关及完整 OIDC 包。
- `snapshot-comparison.json`：源码快照比对和顶层／子用例计数。
- `full-host-summary.json`、`full-postgres-failures.json`：失败名称清单。
- `fixed-static-verification.json`、`review.md`：最终构建／vet 与评审记录。
- `verify_ack.py`：冻结源码、交叉编译、隔离容器及清理的复现脚本，各阶段记录源码／二进制／日志 SHA-256。

P4-27 当时的旧缺陷排除及其交付包保持历史记录，本次通过结果只归属于本次修复提交。
