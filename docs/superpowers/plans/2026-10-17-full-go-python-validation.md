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
