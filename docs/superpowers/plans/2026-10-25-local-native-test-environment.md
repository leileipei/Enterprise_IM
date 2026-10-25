# 本地原生客户端手工联测环境 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 用独立测试OIDC、严格HTTPS和仅新建自有模拟器的CA信任，使原普通应用产物完成浏览器登录及消息／回执联测。
**Architecture:** Go测试专用包提供有界、一次性授权码／PKCE／nonce与旋转refresh、合成人员选择页、独立短期CA及同源HTTPS代理；Python复用Registry监督固定源生产API/Worker和PG/Redis，管理租期与状态／停止。模拟器仅使用私有镜像副本和独立ADB；普通APK与旧证据不改。
**Tech Stack:** 固定Go1.27.1、现有jwt/v5、Python标准库、既有PG16/Redis7 digest镜像、固定Android/iOS工具链；不新增第三方库。
**Spec:** `docs/superpowers/specs/2026-10-24-local-native-test-environment-design.md`（2026-10-25用户继续批准A）。

## Global Constraints

- 真机暂缓；不得改Mac信任、已有模拟器、SDK基础镜像、客户环境；guest root/remount仅新建归属AVD。
- 测试认证不得链接进生产API；原Release APK不重建／重签／关闭TLS校验。所有结果按源码／产物／平台记录。
- 所有服务回环绑定；新资源owner/source与租期；秘密0700/0600私有存储且不进日志或证据包。
- 保留所有历史报告和失败；原Tasks5—9 BASE/in_progress；本地提交前说明精确范围，不push/CI/deploy。

## Review Focus

1. 重复参数、错误client/redirect/nonce/PKCE、code重放／过期、refresh跨client复用必须拒绝（Task1）。
2. 授权请求／code／refresh状态达到上限、过期与并发兑换必须有界和单次成功（Task1）。
3. 监听地址、代理目标或私有路径非本机／链接／权限宽松必须拒绝；TLS错误不得放行（Task2/3）。
4. 启动失败／停止／租约过期／PID复用不得误杀外部资源，无法证明归属时保留恢复记录（Task3/4）。
5. Android可写CA不污染基础镜像；无真实浏览器／无法证明信任或清理不得报通过（Task4/5）。

### Task1：有界本地合成OIDC与交互选择页

**Files:** Create `internal/testfixtures/localoidc/{provider.go,tokens.go,provider_test.go}`。
**Interfaces:** `Config{Issuer,Audience string; Clients map[string]Client; Users []User; Now func()time.Time; Random io.Reader; MaxPending int}`；`Client{ID,RedirectURI string; Loopback bool}`；`User{Subject,Name string}`；`New(Config)(*Provider,error)`；Provider实现`http.Handler`，路径`/oidc/authorize`,`/oidc/token`,`/oidc/keys`,`/oidc/.well-known/openid-configuration`。
- [x] 先写真实httptest端点测试：`TestBrowserCodePKCENonceAndRotatingRefresh`、`TestRejectMalformedDuplicateAndWrongBinding`、`TestExpiryReplayAndCapacity`、`TestConcurrentCodeSingleUse`；缺实现RED。
- [x] 实现HTML标注本地合成身份源，POST一次性选择令牌；code60秒、选择请求300秒、refresh3600秒、access900秒；默认各状态上限256；state/nonce/challenge均43字符base64url，verifier43—128合法字符；严格重复参数和4KiB请求预算。
- [x] 现有RS256 JWT库；access typ at+jwt/aud API/client_id；ID token aud client/nonce；token响应no-store，无日志令牌；wrong binding/expired/used拒绝。
- [x] 运行`go test ./internal/testfixtures/localoidc -count=1`与同包race，观察GREEN；只提交本任务包。

### Task2：独立CA、严格TLS与HTTPS代理命令

**Files:** Create `internal/testfixtures/localoidc/{tls.go,tls_test.go,frontend.go,frontend_test.go}`、`cmd/im-local-test-idp/{main.go,tls.go,main_test.go}`。
**Interfaces:** `GenerateTLS(privateDir,owner string,now time.Time)(TLSFiles,error)`返回CA/服务器cert/key路径及指纹；`Frontend(provider http.Handler,upstream *url.URL)(http.Handler,error)`；命令读取0600私有JSON配置，拒绝非回环监听／目标，启动后只写非秘密readyJSON；租期及SIGTERM关闭。
- [x] RED：新CA彼此不同、SAN仅127.0.0.1/localhost、证书验证成功／错误CA失败、私有路径链接或宽权限拒绝；代理严格回环且WS/Authorization仍交真实API，错误路径无泄密。
- [x] Go标准crypto/x509生成独立CA、服务器key与JWT签名key；测试包不含私钥。TLS1.2最低，身份源与API同一HTTPS前端。
- [x] GREEN：真实HTTPS Client使用精确CA；跨CA拒绝；前端路由、命令输入正反例通过。
- [x] 只提交该独立测试命令和组件。

### Task3：隔离后端运行工具与合成人员初始化

**Files:** Create `scripts/run-local-native-env.py`、`scripts/integration/{local_env.py,local_env_state.py}`、`scripts/tests/test_local_env.py`、`testdata/local-native-env/seed.sql`。
**Interfaces:** CLI `start --state-dir NEW_ABS --lease-seconds 1800 --go ABS`前台监督；`status --state-dir ABS`只读；`stop --state-dir ABS`写owner/source绑定停止请求；状态只含公开origin/CA指纹/资源标识，不含秘密。
- [x] RED：目录链接／复用／宽权限／外部目标、停止PID复用与非归属、租期过期／失败部分回收、状态泄密拒绝。
- [x] 固定源archive建四个二进制；新PG/Redis随机凭据与loopback端口；数值顺序迁移、合成Alice/Bob及双组织任职／通信策略、issuer绑定；不连接既有数据库。
- [x] 启动独立HTTPS/IdP、生产Worker/API；进程指纹／ready／实际退出都登记；停止/过期只回收本轮且失败留恢复记录。
- [x] GREEN：Python合同与实际新环境health/config、合成OAuth/me/refresh验证；源／工具／秘密及资源独立核验后本地提交。

### Task4：仅自有模拟器的CA与原普通包

**Files:** Create `scripts/integration/local_env_simulators.py`、`scripts/tests/test_local_env_simulators.py`；仅必要时增加既有AndroidSimulator可选私有镜像入口，默认行为不变。
**Interfaces:** `prepare_android(env,owner,source,apk,ca)->OwnedAndroid`；`prepare_ios(...archive,ca)->OwnedIOS`；全程精确AVD/UUID/端口绑定。
- [ ] RED：拒绝已有AVD/UUID、USB/默认ADB、未证明私有镜像路径／CA／APK摘要；CA失败与清理失败不通过。
- [ ] 先验证独立可写镜像／覆盖层，基础镜像前后SHA不变；独立ADB且关USB发现；仅本次AVD root/remount/reboot，加指定CA；iOS只新UUID keychain add-root-cert。
- [ ] 安装既有同摘要APK／iOS归档，保存CA指纹、前后源镜像摘要与真实浏览器解析结果；不加任意证书回调。
- [ ] 实际严格HTTPS通过与错误CA拒绝；记录全部失败、原生退出和所属资源回收。没有浏览器则保留缺项，不能将内部WebView夹具称系统浏览器。

### Task5：真实系统浏览器登录与双进程消息验收

**Files:** Create对应`docs/verification/p5-02-local-manual-environment-results.json`；更新本地运行指南及总览；必要的UI驱动仅在本项目新建模拟器。
- [ ] 真实默认浏览器打开本地合成身份页，选择Alice/Bob，OS回调普通应用及任职／会话。
- [ ] 两个普通原生进程完成发送／设备送达／可见已读与断线恢复；SQL独立核对，无测试入口、私有配置或CA绕过。
- [ ] 固定源、原产物、CA、实际命令／退出、失败、独立完整性与资源清理核对；不累计历史用例或补真机／客户SSO。
- [ ] 提交运行说明及限定验收；原Task5—9保持进行中，未完成的平台门禁分别记录。

## 执行约定

已确认Native方式与方案A授权；按任务顺序在当前活动工作树执行，不重复询问同一授权。常规细节由实现者决定并记录；只有真实超出本设计的操作才需新授权。计划自查覆盖设计所有条款；不扩大成新产品认证、生产部署或客户IdP。


## 2026-10-25 实施检查点

Task1—3 已实现并完成限定后端验证；Task4 / Task5 保持未完成。固定实现 `206605a7f61e022471013060cd882184a95207db`：相关 Python37项、测试身份包正常/竞态各41项检查通过；原生协议探针46项通过，不能相加成平台用例数。

Task4 已有私有镜像和精确 CA 准入、iOS 新 UUID 安装入口；通用 `prepare_android` 尚未实现。两种 Android 镜像只含 WebView Shell，未据此验收系统浏览器。iOS 前三轮实际 Safari 失败/停留保留；最新 Origin/CSP 修正测试通过，但新一轮 UI 被 Mac 锁屏挡住。双普通进程消息/回执/恢复没有通过。原 Tasks5—9 BASE/in_progress 和真机暂缓不变。

本轮已停止全部临时服务和新建模拟器，保留脱敏证据与恢复点。人工解锁后从新 owner、CA、UUID 继续，不复用已清理状态；不重复申请方案 A 已授权的范围。


## 2026-10-25 解锁后 iOS 分项检查点

用户回复“已解锁”后，使用新owner/CA/UUID和原普通归档完成真实Safari双账号回调、任职选择、3条双向消息及设备送达/可见已读。Bob真实Home后台时TCP连接消失、第三条已提交未回执；恢复同一进程自动补拉后形成回执，独立SQL核实。全部运行资源已回收。

这是Task5的iOS分项证据，不将Task4/5整体改为完成：Android独立浏览器/原包等价验证、原生错误CA负例、正文到期后的刷新体验和其他外部门禁仍未完成。普通刷新未立即恢复旧正文，重进会话可重新取得正文；原包及源码均保持，本轮不改包替换已有验证来源。


## 2026-10-25 正文手动刷新源码修正检查点

修正 `be50ec8a728d5cc7553e28c20dae7878cf5395e6` 使手动刷新先完成待确认页面/相同凭据重试，再重授权历史正文；普通轮询和发送后补拉保持增量。新增5项回归，Flutter全量265项通过，格式和固定SDK的Dart全包分析通过；Flutter分析入口中文路径LSP崩溃及最初TMPDIR准备失败保留。见[正文刷新修正验证](../../P5-02-正文刷新修正验证.md)。

仅源码回归通过，没有新包构建或本轮原生UI联测；旧f3d产物与历史证据不改。Task4/5整体仍未完成；新版本普通包的真实后端到期刷新、Android真实浏览器、原生错误CA仍待验证，真机继续暂缓。
