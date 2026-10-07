# P4-31 固定源码完整联调验收实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付可重复执行的本机联调入口，用同一固定源码完成既有消息、文件、浏览器、导入门禁及完整/race套件，并提供可核对证据和资源清理结果。

**Architecture:** 一个薄 Python 入口先固定源码，再加载归档中的编排模块；复用现有 Go/脚本门禁和真实服务。资源登记、命令监管、环境白名单与结果判定有独立接口；业务成功来自正式程序，失败/取消也执行归属校验和清理。

**Tech Stack:** 锁定 Go/pgx/Redis/AWS SDK；Python3.9+ 标准库和 unittest；已有 Docker、OpenSSL、固定 mc、qpdf/ClamAV；已有 Node/Playwright/Chrome。不升级产品依赖。

**Spec:** [已确认 P4-31 规格](../specs/2026-10-07-p4-31-local-integration-design.md)。用户于2026-10-07回复“继续”确认书面规格，设计提交 `e90e43a035cf5d9a4fa82672f2a5d0b6dcefcceb`。用户于2026-10-07回复“确认”批准本计划；保留此前选定的执行方式：当前助手逐项实现，最终一次独立整体评审。

本计划只开发一个统一验证系统，复用的消息、数据库、文件等已有业务不拆成新产品子项目。以下九项各有独立测试周期；未批准本计划前不写实现、安装运行依赖或启动夹具。

## Global Constraints

- 工作树 `/Users/leo.cui/.codex/worktrees/p4-31-local-integration-design/企业IM系统`，分支 `codex/p4-31-local-integration-design`；产品基线 `3c786f09c50248a10e780c0e7d86a38803409431`。保护原工作区及旧草稿，不合并、部署或运行客户导入。
- 入口只公开 `--source-commit <完整40位SHA>`、`--output-dir <新绝对目录>`；本轮只支持已选择的本机隔离方案。所有构建/测试/脚本来自该归档，入口哈希与源中同名文件一致；归档拒绝链接/穿越，旧证据不覆盖。
- 不升级 `go.mod/go.sum`；`GOWORK=off`、`GOFLAGS=-mod=readonly -buildvcs=false`，Go缓存路径显式指定。编排模块从归档加载，不能执行未提交模块；二进制来源以归档、构建参数与哈希共同证明。
- PG16/MinIO/Alpine/qpdf/ClamAV/mc 按现有 `testdata/file-runtime/versions.lock`；Redis固定 `redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499`；执行前核对平台/ImageID/版本/工具哈希。锁定清单缺失或不匹配为预检失败，不能浮动升级。
- 文件进程数据库名 `enterprise_im_files`、环回随机端口；测试桶两个不同 `p426-p431-<owner>` 前缀名称、版本化且初始/结束私有。新owner32位小写hex；无业务卷或客户数据。
- 管理连接只准备/注入/清理；API、repair、导入writer为普通角色；比较工具实际只读profile仍保留。四个不同IAM主体上传/扫描/下载/精确版本清理，另管理/引导主体；正式子进程不接收管理凭据。
- 私密根0700、秘密文件0600、scanner manifest/config0400。父测试环境不继承 `PG*`、`HOME`、`USERPROFILE`、子进程开关或其他未知变量；用白名单明确传递，不改比较驱动守卫。
- 本轮原生正/负向clamd各自PID/socket/config/manifest；定义真实签名/构建时间，产品24小时新鲜度规则不变。正向stream26214400、child0、global262144000、recursion16、files10000；负向保留原子文件限制反例；Linux512MiB/1CPU与1MiB tmpfs资源门禁实际运行。
- Go测试包 `-p 1`，服务内部真实并发不减；单包30分钟、单门禁外层60分钟、每轮含准备清理6小时；等待工具单次不超过60秒，工作期间持续进展。无自动SQL/业务重试，失败尝试保留后使用新目录。
- 规格17门禁固定名见任务7；测试门禁具名PASS>0、FAIL=0、普通SKIP=0、必要名/子例齐全。full/race只能例外记录精确 `internal/policystore::TestRealtimeAPIChild`，对应调用方PASS且两个真实子进程证据成立；原始skip仍显示。
- 同一最终SHA重跑全部必需门禁及full/race；不复用历史完整套件。四字段 required_gates_passed/full_suite_passed/race_suite_passed/cleanup.removed 均true才总体成功；缺包/测试、日志破损、竞争报告、来源/哈希或清理失败均非零。
- 只修复本轮实际复现的夹具、工具或既有产品缺陷；契约变更另行确认。所有修正保留失败证据、最小改动及回归，不移除测试或放宽正式规则。
- 成功/失败/取消先保存脱敏证据，再清理本轮登记资源。清理失败保留最少私密恢复登记；正常收尾不保留可用凭据。分享产物不含DSN/token/IAM key/private key/cookie/env值。

## Review Focus

- RF1：入口与归档一致但本机模块被编辑、子脚本从归档找错Git库——实际执行只来自固定源，外部工作区不污染结果；任务2/6测试。
- RF2：Go返回0却缺包/测试，截断日志或辅助SKIP被滥用——完整清单和真实调用方证据决定通过，不能只数FAIL；任务1/7测试。
- RF3：创建资源后登记前取消、PID重用、容器删除中——按预留owner和实际指纹恢复，只清理可证明归属的资源；任务3/7测试。
- RF4：套件继承HOME/PG密码/子进程开关或IAM管理身份——白名单不泄漏，正式账户真实拒绝未授权操作；任务2/4/6测试。
- RF5：病毒库/配置在长轮次中失效、readiness失败被重试掩盖、错误日志含秘密——继续拒绝、保存原失败、脱敏后分享；任务5/7/8测试。

## 文件归属和共享模型

| 文件 | 责任/任务 |
| --- | --- |
| `scripts/test-project-integration.py` | 标准库bootstrap与CLI，Task2/7；源码固定前不导入项目模块 |
| `scripts/integration/__init__.py`、`model.py`、`results.py` | 数据结构、事件解析、门禁判定，Task1 |
| `scripts/integration/source.py`、`tools.py`、`environment.py` | 来源/工具/环境及测试清单，Task2 |
| `scripts/integration/registry.py`、`commands.py` | 私密登记、执行/取消/清理，Task3 |
| `scripts/integration/services.py`、`iam.py` | PG/TLS、Redis、MinIO/IAM与实际预检，Task4 |
| `scripts/integration/scanner.py`、`browser_probe.cjs` | 正负扫描器、manifest、浏览器预检，Task5 |
| `scripts/integration/gates.py` | 既有门禁适配、选择清单，Task6 |
| `scripts/integration/run.py`、`evidence.py` | 生命周期、公开结果与秘密处置，Task7 |
| `scripts/tests/test_project_integration_<results/source/environment/registry/services/scanner/gates/run>.py`、`support.py` | unittest反例、独立临时目录/临时Git库与合成协议日志，随所属任务 |
| `internal/testfixtures/integration_registry.go`、`integration_registry_test.go`、`integration_iam_test.go` | 仅被测试夹具引用的登记桥与真实IAM预检，Task3/4；正式程序不导入 |
| `internal/policystore/realtime_process_integration_test.go`、`file_business_process_helpers_test.go` | 开启本轮登记时记录实际子进程生命周期，Task3/6 |
| `scripts/test-import-apply.py`、`test_import_apply_gates.py`、`test-file-runtime.sh` | 新可选登记/仅必需门禁及资源标签，Task6；默认验证契约保留 |
| `testdata/project-integration/versions.lock`、`README.md`、`iam/{upload,scan,download,cleanup,bootstrap}.json` | 无秘密锁定/说明/IAM模板，Task2/4/7 |
| `docs/开发增量-P4-31-验收记录.md`、`docs/verification/p4-31-results.json` | 最终中文记录，Task9 |

model.py定义下列可序列化类型，秘密字段不得出现在repr/公开转换中：

- `SourceSnapshot(commit:str, repository_root:Path, root:Path, archive:Path, archive_sha256:str)`。
- `Toolchain(paths:Dict[str,Path], versions:Dict[str,str], hashes:Dict[str,str], images:Dict[str,Dict[str,str]], goos:str, goarch:str)`，paths固定键go/python/node/node_modules/chrome/qpdf/clamd/sigtool/freshclam/mc/openssl/docker。
- `Inventory(packages:Set[str], tests:Dict[str,Set[str]], required_subtests:Set[str])`；完整名称格式 `package::TestName[/sub]`。无测试包仍在packages中且tests为空；仅清单确认无测试、实际Go输出为`[no test files]`且命令成功时接受该包终态，不把包级无测试记录当具名测试SKIP或PASS。
- `GateEvent(name:str, kind:str, source_commit:str, exit_code:int, log:Path, counts:Dict[str,int], executed:Set[str], failures:List[str], skips:List[str], checks:List[str], helper_proofs:List[dict], passed:Set[str], package_status:Dict[str,str], started:Set[str], inventory:Optional[Inventory])`；kind为test/check，counts分top/sub/package的pass/fail/skip及ordinary_skip/helper_skip。
- `ResourceRef(kind:str, owner:str, identity:str, fingerprint:dict, state:str)`；kind为container/process/directory，fingerprint为私密登记结构，公开转换只返回安全归属元数据。
- `CleanupResult(removed:bool, failures:List[str])`、`FixtureBundle(environment:Dict[str,str], secrets:Set[str], registry_path:Path, owners:Set[str], metadata:dict)`，FixtureBundle不允许公开自动序列化。
- `Verdict(required_gates_passed:bool, full_suite_passed:bool, race_suite_passed:bool, failures:List[str])`。

---

### Task 1：可信事件解析与门禁判定

**Files:** 新建model/results、tests/results及support（临时目录、合成Go JSON/verbose日志生成器）；不运行真实依赖。

**Interfaces:** `parse_go(log:Path,name:str,source_commit:str,exit_code:int)->GateEvent`、`parse_verbose(log:Path,name:str,source_commit:str,exit_code:int)->GateEvent`、`parse_unittest(log:Path,name:str,source_commit:str,exit_code:int)->GateEvent`（返回kind=test，命令元数据由调用方显式传入）；`validate_gate(event:GateEvent, inventory:Inventory, allowed_helper:Optional[str])->List[str]`；`evaluate(events:List[GateEvent], cleanup:CleanupResult, commit:str)->Verdict`。常量 `REQUIRED_GATES`与任务7的17名完全一致，检查型门禁需非空checks且exit0，测试型门禁需具名PASS。

- [x] **Step 1：写反例与正向测试。** 测试名 `test_zero_missing_subtest_nonzero_exit`、`test_truncated_json_and_missing_package`、`test_duplicate_source_hash_failure`、`test_helper_requires_parent_and_two_processes`、`test_full_and_race_are_independent`、`test_unittest_missing_fail_skip_and_zero_match`。support生成真实协议形状的合成事件，不标作集成证明。

```python
# event_without_one_test、complete_inventory由本任务support建立；删除一条顶层PASS仍必须失败。
assert validate_gate(event_without_one_test, complete_inventory, None)
assert not evaluate(all_green_except_full, CleanupResult(True, []), commit).full_suite_passed
```

- [x] **Step 2：验证RED。** `python3 -m unittest discover -s scripts/tests -p 'test_project_integration_results.py' -v`。Expected：缺接口或指定反例断言失败；保存实际输出，不能把测试语法错误当RED。
- [x] **Step 3：实现解析/判定。** 严格区分包和测试终态，合法Go输出可含构建诊断但缺失/截断JSON事件不能成为PASS；完整清单要求每项终态，普通SKIP失败，helper例外仅full/race及固定包名。两个子进程都须registered/ready/exit证据，来源相同且对应真实调用方PASS，原始SKIP保留。全结果遇重复门禁、缺门禁、来源错误或清理失败拒绝。unittest解析具名ok/FAIL/ERROR/skipped及Ran总数，核对固定源unittest发现清单，失败/跳过/零匹配/截断不能作为orchestrator_contract通过。
- [x] **Step 4：验证GREEN。** 同Step2，Expected：所有判定正/反例PASS，完整/race失败不被其他绿色门禁覆盖。
- [x] **Step 5：提交。** 显式add本任务文件；`test: enforce complete integration gate evidence`。

### Task 2：固定源码、工具锁定、环境与测试清单

**Files:** 新建入口bootstrap、source/tools/environment及source/environment所属测试文件；新建versions.lock。消费Task1模型，不加载或启动夹具。

**Interfaces:** 入口 `bootstrap(repository_root:Path,commit:str,out:Path)->Dict[str,str]`（仅标准库）；`load_archived_runner(data:Dict[str,str])->Callable[[Dict[str,str],Path],int]`（返回归档中的execute_bootstrap）；source `verify_snapshot(data:Dict[str,str])->SourceSnapshot`、`collect_inventory(snapshot,tools,env,selection:Optional[Dict[str,str]])->Inventory`；tools `discover_toolchain(snapshot,private:Path)->Toolchain`；environment `test_environment(tools:Toolchain,private:Path,values:Dict[str,str])->Dict[str,str]`。

- [ ] **Step 1：写测试。** `test_dirty_module_never_executes`（临时Git库含已提交模块，编辑本机模块加入探针，执行只能看到归档探针）、`test_archive_link_traversal_and_output_reuse`、`test_entry_digest_and_wrong_repository`、`test_environment_excludes_home_pg_and_child_flags`、`test_inventory_go_list_and_test_list`、`test_tool_digest_mismatch_and_missing_mc`。Go缓存和工具路径为测试参数，不修改系统变量/共用安装。

```python
assert 'HOME' not in test_environment(tools, private, values)
assert 'IM_APPEND_CHILD_SCHEMA' not in test_environment(tools, private, values)
assert archive_module_marker == 'committed'
```

- [ ] **Step 2：验证RED。** `python3 -m unittest discover -s scripts/tests -p 'test_project_integration_source.py' -v`及 `python3 -m unittest discover -s scripts/tests -p 'test_project_integration_environment.py' -v`。Expected：缺接口或目标断言失败。
- [ ] **Step 3：实现固定源与白名单。** 入口在解包、校验自身哈希之前不import integration；之后从归档scripts目录加载，核验模块来源。归档有repository_root只供git archive读取；测试cwd为snapshot.root。工具路径从已知安装/捆绑路径发现并固定，mc缺失时从锁定官方release取得darwin-arm64二进制，必须匹配既有SHA `e745d9866fc40ff7cf876abeb28e05e153a8cfeba601bcc8daa6e124b81384c5`，获取失败为toolchain失败。标准库环境仅放PATH/TMPDIR/LANG/TZ、明确Go缓存/GOFLAGS/GOWORK/NODE_PATH及本轮必须IM_TEST变量，管理与产品变量按用途分组；禁继承未知env、GODEBUG和子进程开关。按 `go list -json ./...` 与 `go test -json -list . -p 1 ./...`记录当前平台每包和顶层清单，不用源码正则代替build-tag解析。
- [ ] **Step 4：验证GREEN/实际只读发现。** 运行Step2的两个source/environment命令；对本机工具/镜像只读发现，记录发现状态和真实版本/哈希；mc获取在本任务执行时按锁定值进行；此步不启动服务。Expected：反例PASS，当前平台清单非空，未知版本拒绝。
- [ ] **Step 5：提交。** `feat: pin integration source tools and isolated environments`，只add本任务文件和无秘密lock。

### Task 3：资源登记、命令监管与测试进程桥

**Files:** 新建registry/commands及registry tests；新建internal/testfixtures登记桥及Go单测；修改两个已有进程测试helper以可选启用登记。

**Interfaces:** `Registry(path:Path,owner:str,commit:str)`；`reserve(child_owner:str)->None`、`add(ref:ResourceRef)->None`、`lifecycle(identity:str,event:str,detail:dict)->None`、`cleanup(deadline:float)->CleanupResult`；`run_command(name:str,kind:str,source_commit:str,argv:List[str],cwd:Path,env:Dict[str,str],log:Path,deadline:float,registry:Registry)->GateEvent`；Go桥 `RegisterIntegrationProcess(cmd *exec.Cmd,gate,test string)(*IntegrationProcess,error)`、`Ready() error`、`Exited(expected,actual string) error`，未配置 `IM_TEST_INTEGRATION_REGISTRY` 时保持旧测试路径。桥只由*_test.go调用。

- [ ] **Step 1：写测试。** `test_cancel_between_create_and_register`、`test_pid_reuse_and_foreign_owner`、`test_auto_remove_in_progress`、`test_deadline_preserves_exit_and_runs_cleanup`、Go `TestIntegrationRegistryProof`。Python测试使用独立短命子进程/合成Docker协议响应，真实Docker证明留任务4；不能用合成协议声称实际资源验收。

```python
assert registry.cleanup(deadline).removed is False  # 外部owner不得被停止
assert foreign_process_is_alive
assert verdict_for_pending_auto_remove != 'removed'
```

- [ ] **Step 2：验证RED。** `python3 -m unittest discover -s scripts/tests -p 'test_project_integration_registry.py' -v`；`go test ./internal/testfixtures -run '^TestIntegrationRegistryProof$' -count=1`。Expected：接口/行为缺失失败。
- [ ] **Step 3：实现安全生命周期。** 私密JSONL schema_version1，写入用flock、O_APPEND和fsync；reserve先于创建并登记子owner，create后立即记录ID。容器验证实际ID/精确owner标签，进程验证UID/开始时间/程序身份/本轮私密路径，不单凭PID或名称。命令新建本轮进程组、记录开始/退出，deadline与signal停新任务并清理已证明子进程；自动rm等待absence，不能重删删除中容器。Go桥向同一登记写registered/ready/exited，保存两个API子进程实际状态，登记开关为IM_TEST_INTEGRATION_REGISTRY、IM_TEST_INTEGRATION_OWNER、IM_TEST_INTEGRATION_SOURCE_SHA、IM_TEST_INTEGRATION_GATE；启用registry时其他三项缺失/格式错误即拒绝，source校验40位。Ready在实际readiness后、Exited在实际cmd.Wait后记录，不记录argv中的秘密。
- [ ] **Step 4：验证GREEN。** 上述Python/Go命令全PASS；`go list -deps ./cmd/...`不含internal/testfixtures，关闭登记时旧两个进程测试仍可按原夹具运行；不得新增正式测试hook。
- [ ] **Step 5：提交。** `feat: track owned integration resources and process lifecycles`。

### Task 4：真实数据服务、TLS和IAM夹具

**Files:** 新建services/iam、services tests、IAM模板、internal/testfixtures/integration_iam_test.go；完善lock/README准备段。消费Task2工具/环境及Task3登记。

**Interfaces:** `prepare_services(snapshot:SourceSnapshot,tools:Toolchain,registry:Registry,private:Path,deadline:float)->FixtureBundle`；`probe_services(bundle:FixtureBundle,tools:Toolchain,deadline:float)->GateEvent`；`prepare_iam(bundle:FixtureBundle,tools:Toolchain,private:Path,deadline:float)->FixtureBundle`。所有秘密只存FixtureBundle/0600文件，不公开序列化。

- [ ] **Step 1：写测试。** `test_service_labels_loopback_db_and_roles`、`test_four_iam_principals_and_no_admin_child_env`、`test_failed_preflight_cleans_created_resources`、`test_probe_version_not_created_by_api`，及真实 `TestIntegrationFixtureIAM`：匿名拒绝、上传/扫描/下载/cleanup允许矩阵、cleanup nil/empty/null versionid拒绝。策略模板仅替换本轮两个bucket名和主体，读探针固定 `_im_runtime/read-probe/v1`、内容 `enterprise-im-file-read-probe-v1`+LF。
- [ ] **Step 2：验证RED。** `python3 -m unittest discover -s scripts/tests -p 'test_project_integration_services.py' -v`。Expected：准备接口或权限矩阵断言缺失失败；真实IAM测试待Step4具备服务后执行，环境缺失/SKIP不当RED/PASS。
- [ ] **Step 3：准备及最小实现。** 新建带 `im.integration.owner=<owner>` 和 `im.integration.source=<commit>` 的PG/Redis/MinIO，环回随机端口，tmpfs/本轮数据目录，无客户卷。PG建enterprise_im_files、btree_gist、TLS CA/错误CA/SAN127.0.0.1证书；实际TLS连接验证。mc使用私密config-dir，输出只记录模板，创建版本主桶/策略桶、四独立权限主体与管理/引导主体；cleanup模板按锁定MinIO支持的非空非null `s3:versionid`条件实际测，不笼统给DeleteObject。管理员准备与普通API/repair角色分离，导入/只读独立profile留子门禁。故障时按registry清理。
- [ ] **Step 4：验证GREEN。** Step2 unittest全PASS；真实probe_services及私密bundle环境中的 `go test ./internal/testfixtures -run '^TestIntegrationFixtureIAM$' -count=1 -v` exit0且必要名PASS、0SKIP；再读bucket/version/匿名/角色/label/TLS库存确认，清理本任务真实夹具。记录真实端口和资源ID的安全元数据，绝不打印凭据。
- [ ] **Step 5：提交。** `feat: provision owned database redis and versioned IAM fixtures`，大型数据/秘密不入Git。

### Task 5：扫描运行绑定与浏览器预检

**Files:** 新建scanner、browser_probe.cjs、scanner tests，完善准备说明；复用现有clamd.conf及scanner公开行为。

**Interfaces:** `prepare_scanner(bundle:FixtureBundle,tools:Toolchain,registry:Registry,private:Path,deadline:float)->FixtureBundle`；`probe_scanner(bundle:FixtureBundle,tools:Toolchain,deadline:float)->GateEvent`；`probe_browser(bundle:FixtureBundle,tools:Toolchain,registry:Registry,private:Path,deadline:float)->GateEvent`。工具paths补齐sigtool/freshclam；manifest字段严格沿runtime_binding.go（kind=local-process、pid及三个CVD的hash），不添加产品未识别字段。

- [ ] **Step 1：写测试。** `test_stale_future_definition_refused`、`test_manifest_pid_command_socket_hash_and_modes`、`test_definition_changes_after_start_refused`、`test_browser_closes_private_profile_without_host_home`；用合成签名信息测拒绝路径，真实签名/扫描由Step4证明。
- [ ] **Step 2：验证RED。** `python3 -m unittest discover -s scripts/tests -p 'test_project_integration_scanner.py' -v`。Expected：接口或拒绝断言失败。
- [ ] **Step 3：实现准备。** 复制真实main/daily/bytecode.cvd到私密目录；sigtool验证签名/构建信息，需更新时freshclam只针对本轮目录。正/负向clamd各自配置0400、私有socket600、真实native argv=`<binary> --config-file=<config>`，先定义后启动再记录manifest；负向MaxFileSize26214400，其余正向合同不放宽。Node probe require锁定已安装Playwright、启动实际Chrome私密profile并关闭；保持Go父环境无HOME，确需配置只作用于受监管浏览器子进程。准备/运行每阶段重新校验定义仍在24小时内，失效中止保存失败，不更新mtime伪造有效性。
- [ ] **Step 4：验证GREEN与真实运行。** unittest全PASS；在本轮bundle运行 `go test ./internal/filescanner -run '^TestScannerReal(CleanEICARProtocol|NoIncompleteReady|Attestation|TrustedRuntime|NearStreamLimit)$' -count=1 -v`，0FAIL/0SKIP；Node probe输出launch/close和版本检查结果，registry确认本任务创建的扫描/浏览器实例退出。Linux资源由Task6/8独立证明。
- [ ] **Step 5：提交。** `feat: prepare bound scanner and isolated browser runtimes`。

### Task 6：既有门禁适配和子脚本契约

**Files:** 新建gates/gates tests；修改test-import-apply.py/test_import_apply_gates.py及test-file-runtime.sh的可选登记/资源标签；必要进程fixture仅补生命周期证据，不改业务断言。

**Interfaces:** `gate_specs(snapshot:SourceSnapshot,inventory:Inventory)->List[dict]`（dict固定键name/kind/argv/cwd/env_group/required）；`run_stage(spec:dict,snapshot:SourceSnapshot,tools:Toolchain,bundle:FixtureBundle,registry:Registry,deadline:float)->GateEvent`；`validate_import_child(report:Path,source_commit:str)->GateEvent`。import子脚本新增 `--required-only`、`--resource-registry <私密路径>`、`--resource-owner <预留32hex>`，相互校验；旧默认仍执行旧完整套件。

- [ ] **Step 1：写测试。** `test_archive_script_with_readonly_git_root`、`test_import_required_only_is_not_full_pass`、`test_child_source_and_hash_mismatch`、`test_all_existing_required_names_preserved`、`test_linux_resource_names_not_zero_match`。现有ApplyGateTests再验证缺新参数时原默认/独立全套判定不变。

```python
assert child['full_suite_status'] == 'not_executed'
assert child['full_suite_passed'] is False
assert child['source_commit'] == snapshot.commit
```

- [ ] **Step 2：验证RED。** `python3 -m unittest discover -s scripts/tests -p 'test_project_integration_gates.py' -v`和 `python3 scripts/test_import_apply_gates.py`的新子模式反例。Expected：新接口/模式尚不存在，相关断言失败；原测试通过不冒称RED。
- [ ] **Step 3：接入门禁。** import脚本必须从snapshot.root调用其归档文件，cwd仅为snapshot.repository_root供git archive读取，内部测试仍自身固定归档；父预留owner被所有新容器登记，finally仍独立清理。required-only不运行/reuse完整套件，完整来源字段为空/not_executed，不能伪造full结果。父验证十四名和所有必要名、源码、归档/日志/二进制hash、子cleanup。文件脚本run-all接口不变，可从明确IM_TEST_INTEGRATION_REGISTRY/OWNER变量登记资源；Linux容器加标签/cid登记，原512MiB/1CPU/1MiB tmpfs不变。
- [ ] **Step 4：验证GREEN/完整清单。** 两Python命令全PASS；核对file_messages8、download16、web11、RP01–14及slow_client_SIGTERM的现有集合完全保留。file_components按其现有包+regex的编译inventory要求每个选中顶层名；file_resources须具名TestScannerRealResourceBoundary、TestFileTransferRealDiskFull及其既有子例、0SKIP。message_realtime包含完整access/oidcauth包、P4-30完整ACK regex和规格四个实际调用方，不缩减选取绿色测试。
- [ ] **Step 5：提交。** `feat: compose strict existing gates with owned child resources`。

### Task 7：统一生命周期、完整套件与分享证据

**Files:** 新建run/evidence、run tests，完善入口main/README；消费前六任务接口。

**Interfaces:** `execute_bootstrap(data:Dict[str,str],output:Path)->int`（归档run.py中调用verify_snapshot后执行execute）；`execute(snapshot:SourceSnapshot,output:Path)->int`；`write_evidence(output:Path,snapshot:SourceSnapshot,tools:Toolchain,events:List[GateEvent],cleanup:CleanupResult,inventory:Inventory,secrets:Set[str])->Verdict`；`validate_delivery(source_commit:str,report:Path,repository_root:Path)->None`（evidence.py，验证成功无返回，失败抛安全错误）；入口主函数 `main(argv:Optional[List[str]])->int`。结果JSON schema_version=`project_integration_v1`；17个门禁名必须为orchestrator_contract/toolchain/fixture_preflight/build_all/vet_all/message_realtime/file_components/file_resources/file_messages/file_download_retention/web_files/file_business_process/import_append/full_repository/race_repository/evidence_integrity/resource_cleanup。

- [ ] **Step 1：写测试。** `test_missing_fixture_and_partial_green_never_success`、`test_cancelled_setup_preserves_report_and_cleanup`、`test_full_inventory_early_child_exit`、`test_signal_stops_new_gates`、`test_raw_secret_is_not_published_or_retained`、`test_changed_hash_and_false_cleanup_override_green`。通过受控短命命令产生真实exit/signal/截断JSON，不伪造实际业务集成结果。
- [ ] **Step 2：验证RED。** `python3 -m unittest discover -s scripts/tests -p 'test_project_integration_run.py' -v`。Expected：尚缺统一状态或指定拒绝行为失败。
- [ ] **Step 3：实现统一执行。** bootstrap→orchestrator_contract（固定源 `python3 -m unittest discover -s scripts/tests -p 'test_project_integration_*.py' -v`，合并stdout/stderr以parse_unittest核对具名结果）→toolchain→专属资源/预检→build/vet→各stage→full/race→hash校验→finally清理→最终判断。full/race分别在同源完整白名单环境执行 `go test -json -p 1 -timeout=30m -count=1 ./...` 与 `go test -race -json -p 1 -timeout=30m -count=1 ./...`；开始前记录各自清单，终态核对所有包/顶层，helper仅在真实caller与两个实际生命周期符合时豁免普通SKIP。每gate有60分钟期限，全轮6小时，收到取消停止新gate。分享JSON记录源码、工具/平台/argv模板、原始/普通/helper计数和完整名字、hash、耗时及customer/production未执行；原日志保持0600，分享副本精确秘密替换并检查token/私钥格式，不能安全发布则不发布且报告处置，正常收尾不保留可用凭据。validate_delivery校验17门禁、四成功字段、源/程序/存留分享日志哈希、入口一致和当前HEAD产品差异；被处置原日志只能通过原始哈希及脱敏/删除记录追溯，不能谎称再次读取验证。
- [ ] **Step 4：验证GREEN与CLI契约。** `python3 -m unittest discover -s scripts/tests -p 'test_project_integration_*.py' -v`全PASS；临时已提交小型Git测试源通过入口路径证明加载的是归档runner，缺source/out/工具错误非零且不泄密。只将此叫编排契约验证，不叫完整联调；真实17项在Task8执行。
- [ ] **Step 5：提交。** `feat: run fixed-source integration with complete evidence and cleanup`。后续验证只能采用包含此入口的完整提交，不用未提交源码。

### Task 8：真实固定源码验收和具体问题修正

**Files:** 按真实失败记录确定最少工具/夹具/已有产品文件；先在本轮证据目录保留原失败、源码SHA与具体复现。代码修正名单须在修改前写入执行账本，不混入其他编辑。

**Interfaces:** 正式入口为Task7 `main`；失败记录每项 `case_id/source_commit/gate/package_test/kind/reproduction_command/safe_error/evidence_hash`，kind=fixture/tool/product/contract。这一任务不预造未知产品缺陷；实际复现后才能确定对应文件及测试。

- [ ] **Step 1：确定固定源和新目录。** 提交前七任务后记录完整SHA；用 `python3 scripts/test-project-integration.py --source-commit <该完整SHA> --output-dir <新绝对目录>`。所有本轮真实资源自动准备，源外工具获取只按锁定值，失败保留。Expected：进入实际门禁或给出具体非零原因，不能空跑PASS。
- [ ] **Step 2：读取全部输出并归类。** 检查源、17门禁、每包/测试清单、完整/race普通FAIL/SKIP、helper子进程证据、扫描/IAM实测及cleanup；结合历史125失败/48SKIP排查但以本轮为准。Expected：真实状态和全部未通过名字齐全；readiness/定义/权限不得简单归为环境已解决。
- [ ] **Step 3：对已复现问题执行TDD。** 每个case先固定复现命令和对应失败断言；夹具只修本轮准备、工具补反例、产品只按既有合同最小修正。观看有效RED→GREEN及相关套件，再逐项提交最小修正（有实改才用 `fix: resolve verified integration failures`）；初次即PASS的补证明确为覆盖，不假称产品修正。契约类输出证据等待范围确认，不擅改。
- [ ] **Step 4：最终同源重跑。** 任一代码/工具改动均新提交、新证据目录，重跑准确正式入口、全部17门禁/full/race；不使用reuse参数、不拼接尝试。Expected：exit0，四成功字段true，所有必需名/清单/hashes一致，普通FAIL/SKIP0、唯一helper有两实际进程证据、race无报告、cleanup成功。若无法满足，任务未完成，仅交付事实与剩余问题。
- [ ] **Step 5：核对验收收据。** 使用Task7的validate_delivery核对Step4实际报告和固定SHA；本步不再修改已验证产品/工具。task-done引用最后绿色入口命令及收据校验，日志按当次目录保存；没有缺陷只保存证明，不为凑commit改代码。

### Task 9：唯一整体评审、交付记录和远端核对

**Files:** README、规格/本计划状态、中文验收记录、verification/p4-31-results.json及评审/裁决记录；不得包含运行秘密或大型样本。

**Interfaces:** 来源为Task8最终完整验证报告；产品差异范围 `3c786f09c50248a10e780c0e7d86a38803409431..<最终源>`；独立draft base=`codex/p4-30-controlled-import-design`。完成检查使用 Task7的 `validate_delivery(source_commit:str,report:Path,repository_root:Path)->None` 核对存留产物hash、已处置日志的追溯记录、入口一致、产品diff为空及清理结果。

- [ ] **Step 1：准备唯一整体只读评审包。** Task8全部绿色后，对整个分支源码差异评审，提供本规格、计划、RF1–RF5和真实非秘密证据；不派实现代理或逐任务review。评审安排在本任务最终完成前，避免交付依赖评审的循环；除非用户改变执行方式，只派这一位最终reviewer。
- [ ] **Step 2：逐项核实并集中修正。** 作者按实际用户影响复核severity；Critical/Important一次集中修正，先有效RED后GREEN；产品/工具变化重新固定SHA并重跑全部17门禁/full/race，不派第二次评审。Minor明确记为deferred，所有不采纳项作裁决并写代价，不顺带扩展未批准范围。
- [ ] **Step 3：整理验收材料。** 记录固定源/归档/工具/平台/程序/日志hash、完整包/顶层/子测试和helper原始计数、各门禁及实际耗时、失败尝试、资源清理、原评审与修正及所有裁决；明确客户/生产未执行。本期成功须Task8/评审修正后的完整条件成立，不冒称生产可用。
- [ ] **Step 4：提交并核对文档差异。** `docs: deliver P4-31 local integration acceptance`；`git diff <固定最终源> HEAD -- cmd internal scripts db go.mod go.sum testdata`为空（否则重新验证新源）。运行validate_delivery并确认原工作区及旧PR提交不变、专属树干净；文档提交后不重复无变化的6小时门禁，但要校验固定证据和产品差异。
- [ ] **Step 5：交付独立draft。** 按批准方式只推送P4-31分支、创建以P4-30为base的新draft并立即attach_artifact；核对远端head、旧PR不变。不merge/部署。task-done保存最终检查；备份全部非秘密证据后仅清理本计划scratch，保留worktree和失败尝试目录。网络/授权阻断时如实交付本地状态，不声称PR已创建。

## 自检和规格覆盖

| 规格 | 任务与验证 |
| --- | --- |
| §1–3目标/范围/历史与当前 | 全局约束、Task8/9 |
| §4固定源/工具/统一入口 | Task2、6、7，RF1 |
| §5DB/Redis/IAM/环境 | Task2–4、6，RF4 |
| §6扫描/浏览器/正式进程 | Task3–6，RF3/5 |
| §7门禁/helper/完整清单 | Task1、2、6–8，RF2 |
| §8工具自身反例/期限 | Task1–3、6、7 |
| §9修正分类/同源重跑 | Task8/9 |
| §10归属/取消/秘密清理 | Task3、4、5、7、9，RF3/5 |
| §11证据/计数 | Task1、7、9 |
| §12 I01–I11 | I01→2/7；I02→4/5/7；I03→4；I04→5/6/8；I05→3/6/8；I06→6/8；I07→6/8；I08→1/2/7/8；I09→8/9；I10→3/7/9；I11→7/9 |
| §13交付/执行方式 | Task9、当前助手逐项实现及一次最终评审 |

接口自检：SourceSnapshot/Toolchain/Inventory/FixtureBundle/Registry/GateEvent/Verdict在生产者与调用方一致；修正先提交后固定源重跑，收据校验由Task7提供并由Task8/9消费；17名称和唯一helper例外一致；每项均有独立验收命令、RED/GREEN或真实执行条件；未知产品修正由真实case绑定，不假定已知问题。用户已确认计划；Task1已按RED→GREEN实现结果判定，其他任务与完整联调尚未完成。
