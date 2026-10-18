# 全量 Go、Python 失败与跳过修复计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans inline. Steps use checkbox syntax.

**Goal:** 修复91df68d后全量Go／Python的失败和普通跳过，输出同源实际执行、证据及清理结论。
**Architecture:** 保留现有业务断言和版本／身份／安全门禁，先定位失败，再最小修复。使用既有固定提交导出和独立PostgreSQL／Redis／MinIO／扫描器／浏览器夹具，原始失败不可改写。
**Tech Stack:** Go1.27.1、Python3.9+、固定Node／Playwright／Chrome、Docker、ClamAV、qpdf。
**Spec:** 用户2026-10-17继续修复全量Go／Python及跳过验证；docs/verification/p5-02-task8-mobile-backend-results.json；testdata/project-integration/README.md。

## Global Constraints

- 复用codex/p5-02-native-clients隔离工作树；主目录提案和既有报告、743份上轮移动恢复原证据、783份后端原证据保留。
- 不放宽断言、跳过、24小时签名库新鲜度、工具锁或资源所有权限制；不修改其他会话服务。
- 不推送、合并、部署；本地检查点前告知；Task5—8原BASE与in_progress保持。
- 必要辅助进程型helper须通过父测试证明实际执行及预期退出；不伪报为普通业务跳过通过。

## Review Focus

- 进程Popen返回后尚未稳定或已经退出：登记前须证明存活和完整指纹，拒绝未知／外部进程。
- 浏览器版本漂移：精确固定版本验证，隔离工具或显式受控锁更新；不能自动接受任意版本。
- 专用Append／IAM与文件业务依赖：实际执行，独立清单不丢测试。
- 取消／超时／失败：清理仅本轮资源，保留失败原件并脱敏秘密。
- 测试时序：等待可证明事件，不用扩大超时掩盖产品错误。

## Task 1: 失败复现及测试工具修复
**Files:** scripts/tests/test_project_integration_registry.py；scripts/tests/test_project_integration_scanner.py；scripts/integration/registry.py、tools.py、scanner.py（仅根因需要时修改）；必要新增针对性测试。
- [ ] 记录起始SHA、工作树、主目录和既有报告hash；复现Python两失败与下载期限问题并收集退出证据。
- [ ] 对根因补RED测试，最小修复，跑受影响测试GREEN；不得删除断言或把失败改为skip。

## Task 2: 完整夹具与跳过验证
**Files:** scripts/integration/run.py、environment.py、gates.py及现有夹具；必要增加独立全量入口测试。
- [ ] 探测已固定工具、镜像、签名库；配置完整本轮自有TLS／DB／IAM／对象存储／扫描器／浏览器。
- [ ] 正确传递Append专用夹具和Node；真实执行先前失败／跳过测试，检查完整清单和实际终态。
- [ ] 产品失败按systematic-debugging与TDD修复，不降低现有门禁。

## Task 3: 同源最终全量与交付
**Files:** docs/verification/p5-02-full-go-python-results.json；既有实施记录及验证操作文档追加；本计划。
- [ ] 冻结修复后源码并保存明确本地运行检查点；独立归档该SHA。
- [ ] 全量Python、Go ./...及相关完整集成门禁执行并核对无普通skip／缺失终态；已存在race门禁同源执行。
- [ ] 独立审计登记进程、容器、端口、临时目录与秘密清理；核对旧证据未改。
- [ ] 最后一次fresh-context只读复核；修复P1/P2；证据、改动范围、提交不可变blob及主目录状态复核。


## 执行中诊断补充：下载证据与搜索预算

第二轮出现两项真实删除ErrIncomplete（隔离10次纯PG、原两项S3各3次未复现，唯一根因未建立）和500候选／20会话搜索5秒超时（隔离4.964秒，limit1仍4.201秒）。保留失败，已失败轮次停止并清理。继续既有Task2故障修复范围：

- 搜索：保留完整500候选／20会话预算与原锁顺序；锁后批读名称按原Go匹配语义预筛，只对潜在匹配加载原完整证明；临时时间仅计数，最终统一数据库时间复评，截断必须保留未扫描位置及后续会话。以指定真实PG正常就绪夹具的实际事务往返<200补确定性RED，不称所有输入统一上界，原5秒／权限／审计／游标门禁保持。
- 删除时间：先证明宿主取时依赖的确定性缺陷，再用同Repository数据库权威clock_timestamp作探测后的证明时间；原精确版本／承诺／时序区间／租期／审计守卫保持，取时错误和非法时间不得结算。不将历史两项失败唯一归因此缺陷。
- 上述候选均须先RED后GREEN、真实PG／S3和权限／并发回归、只读复核及新固定提交的完整17门禁／Go／race／Python重跑，禁止拼接前源最终结论。


## 第三轮全仓新增浏览器失败与最小修复

固定ce251第三轮已通过前置专项、追加导入和同源Python193次，但全仓实际出现TestTwoRealBrowsersDeliveryReadAndRecovery在重新登录后设备列表数量失败，保存原始partial日志后主动取消，整轮actual1、非全量完成、race未执行；671个登记PID及19个容器独立核对已清理，私有根删除。不得用此前专项通过覆盖此RED。

设备注册由selectMembership异步启动，list可先读取旧快照。新增受控Promise回归在原实现稳定0!=1；list仅等待既有pending，不主动创建新设备；普通注册失败后允许列旧设备释放配额，账号或代数变化拒绝旧请求。原双浏览器送达/已读/恢复断言及超时保持。单位GREEN、真实后端/race与只读复核后，须以新固定提交重新执行完整Python、17门禁、全仓和竞态。


## 2026-10-18 第五轮签名库跨期及证据一致性修复

第五轮固定6515源码同源Python193次及前置专项通过，原未插桩RP14长下载通过；全仓执行中daily28155越过24小时有效期，多个Worker按原规则拒绝启动。原始失败、部分全仓日志和主动取消实际退出1保留；574个登记PID、15个容器独立核对清理，私有根删除，full/race未完成，不拼接为通过。

准备扫描器时以剩余全轮期限计算固定required_until，签名库必须覆盖该时点。仅更新本轮副本，更新后仍不足则在启动daemon前失败；签名验证、24小时新鲜度、运行绑定和逐阶段复核保持。期限按sigtool现有分钟级Build time计算，未声称秒级精度。

起始／最终窗口状态、最后验证的daily期限、更新退出码及freshclam原日志进入原脱敏／哈希／处置链；超时和取消仍留证。诊断采集异常也必须在evidence_integrity记固定失败码，绝不能在真实非零退出时留下成功总回执；嵌套finally确保该路径仍清理。新增全绿协议后采集失败回归已先RED，实际拥有子进程的异常清理回归亦保留RED。协议测试只证明编排，不替代业务验收。

修复和只读评审后冻结新源码，顺序重跑同源Python、完整17门禁、Go全仓及race，独立复解析与资源审计后再输出最终结果。
