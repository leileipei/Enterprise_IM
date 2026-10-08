## Strengths

- 已逐批读取 **76 个变更文件的完整差异（新增文件全文）**、完整规格/计划、执行账本和安全失败摘要；核对 F 的公开报告、来源归档及相关分享日志。没有读取凭据、私钥、原始登记或 live 环境文件。
- 独立确认 F 报告 SHA256 为 `5b829b5bb71e34d4adf0e93d47803a14be6af1f1470222ef37fbd97ed0b20e6a`；301 份分享日志哈希一致，归档与固定提交一致，`validate_delivery` PASS。
- 独立重解析 full/race：各 **1044 顶层 PASS、1150 子测试 PASS、28 包 PASS、FAIL 0、普通 SKIP 0、唯一 helper SKIP 1**，对应两个实际子进程证据成立。未重跑完整套件。
- 来源固定、普通数据库角色、IAM 权限收紧、环境白名单和清理后记录刷新实现可靠；未发现扩大 helper 例外或放松产品 guard。A023 的原失败和失败修正均保留，最终夹具保留实际授权、到期和阻塞写断言。

## Critical

无已确认问题。

## Important

### 1. Linux 门禁没有强制检查既有必要子例

**位置：** `scripts/integration/gates.py:131`

`required_subtests` 只加入两个顶层 mandatory 名，未加入以下四个既有子例：

- `TestFileSpoolCrashRecovery/upload`
- `TestFileSpoolCrashRecovery/scan`
- `TestFileSpoolStartupSafety/symlink`
- `TestFileSpoolStartupSafety/public_permissions`

**实际依据：** 我用隔离合成日志删除全部四个子例、保留七个顶层 PASS 和正确限额 proof，`parse_resource_logs` 仍返回零失败、`sub_pass=0`。这违反计划 Task6 明确要求“及其既有子例”。**F 实际四个子例都通过，不是本轮漏跑。**

**修正：** 固定四个完整包名/子例名为必需集合，加入删除任一子例事件但保留顶层 PASS 的拒绝测试。

### 2. 正常业务浏览器进程缺少本轮生命周期记录

**位置：** `internal/policystore/web_file_test_helpers_test.go:325`；`internal/policystore/realtime_browser_integration_test.go:232`

这些真实业务 Node/Chrome 路径执行 `CombinedOutput` 或 `Start/Wait`，没有接入本轮 registry。清理时 recovery 只能发现仍存活的进程，不能补回正常退出历史。

**实际依据：** F 公开资源记录只有浏览器预检的一个 Node、一个 Chrome 身份；多组实际业务浏览器测试已通过，但没有对应正常实例的登记、等待和退出记录。规格 §10 要求测试子进程纳入登记或提供可核对记录。

**修正：** 在共享浏览器夹具加入实际 Node/Chrome 身份及生命周期收据，覆盖正常完成和取消路径，保留 owner/source、PID/UID/start/hash 和实际退出证明。**这是追溯缺口；没有证据表明 F 遗留浏览器。**

### 3. Linux 限额 proof 和测试二进制哈希未进入交付证据

**位置：** `scripts/integration/gates.py:107`、`:155`；`scripts/test-file-runtime.sh:31`

解析器读取真实 inspect 限额 proof、两个编译清单和两份 Linux 日志，但未把它们加入 `related_logs`；提前返回绕过后续日志收集。`structure.test`、`transfer.test` 的 SHA256 也没有进入最终二进制清单。

**实际依据：** F 分享产物没有 `disk-full`、`resource-list` 或 `.proof.json` 独立条目；容器公开 fingerprint 仅有 ID。组件命令日志保留了合并测试输出，但私密目录清理后无法独立核对本轮实际限额、清单和两个二进制身份。

**修正：** 精确列入安全日志/proof 收集白名单，保存两个二进制哈希、编译参数及容器实际 memory/CPU/tmpfs/image/source/owner/exit/cleanup 收据，并由 `validate_delivery` 校验。不要收集整个环境或私密目录。

## Minor

### 1. 最终扫描器定义和绑定信息未保存为安全收据

**位置：** `scripts/integration/scanner.py:140`、`:152`；`scripts/integration/evidence.py:121`

真实版本、签名构建时间、定义哈希、配置/manifest 绑定保存在运行 metadata；公开预检日志只有 `bound_live_scanner_<pid>`。清理后无法检查本轮具体定义及检查时刻。

建议保存经过字段白名单处理的 source、定义版本/哈希/签名构建时间、新鲜度检查时刻、binary/config/manifest 哈希和绑定身份。当前源码确实进行了真实检查，**未发现绕过新鲜度验证**；问题是交付追溯粒度不足。

### 2. 最后两个门禁缺少真实时间字段

**位置：** `scripts/integration/run.py:34`

F 的 `evidence_integrity`、`resource_cleanup` 开始/结束/耗时均为 `null`，与规格 §11 不符。建议记录真实时间；同时区分 Linux 执行时间与 `file_resources` 的解析时间 `0.001s`。

## Declined to judge

- **C 的两个删除案例内部根因：** 原失败保留；后续通过不能证明某个产品根因已修复，不据此认定产品修复。
- **F 存在遗留或误杀进程：** 归属检查包含实际身份、私有目录和精确 owner/source，未找到成立证据；浏览器记录缺失不等于清理失败。
- **任意登记顺序导致目录提前删除：** 所考察的现有实际清理单测 PASS，当前执行顺序未复现该问题，不列确认缺陷。
- **Task9 文档当前状态：** 这是已明确的评审后收尾工作，不将待更新状态认定为虚假交付。
- **客户/生产验收、容量、HA/DR：** 本次范围之外，报告明确 `not_executed`，本机 PASS 不支持这些结论。

## Assessment

**With fixes，尚不 ready。** 三项 Important 影响必需覆盖判定及最终证据完整性，应先修正。F 的真实通过结果可信，但不足以消除这些源码缺口。

本次工作树、index、HEAD 均未修改，最终 HEAD 仍为 `b2f4e5a758a61c17b142c824dd6333169742bf92`，工作树干净；没有启动第二 reviewer。本结论不授权合并。
