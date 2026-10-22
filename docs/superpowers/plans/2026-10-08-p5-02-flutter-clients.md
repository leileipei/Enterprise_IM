# P5-02 四平台 Flutter 客户端实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. 当前会话由同一实施者逐项实现，不默认派代理。

**Goal:** 交付Windows、macOS、Android、iOS可安装并连接真实IM后端的客户端，完成文本／附件、设备送达、用户已读和多端恢复。

**Architecture:** 一个Flutter工程共享协议、身份、加密事务存储和同步核心，UI依屏幕尺寸适配。系统浏览器登录通过固定配置的原生OIDC broker，平台层只负责回调、生命周期、通知与文件选择／导出。

**Tech Stack:** Flutter3.47.3固定源码 `e8113bf45620cbeb8aff64947ee4c93e16adb4cf`，Dart>=3.12.0 <4.0.0，SDK HttpClient/WebSocket，SQLite、AES256-GCM及规格中精确插件；后端Go1.27.1及既有OIDC。

**Spec:** `docs/superpowers/specs/2026-10-08-p5-02-flutter-clients-design.md`，依赖P5-01规格／计划。

**Status:** 细化规格与实施计划已获用户确认，P5-01 完成后逐项实施；SDK已核对来源，尚未安装、resolve或构建。

## Global Constraints

- 首批Windows10 x64、macOS12 ARM64、AndroidAPI24、iOS15（2026-10-15用户批准由13调整）；依赖不兼容须用证据修订，不静默提高门槛。
- 固定SDK版本/源码、pubspec.lock及插件精确版本；无caret/latest；不升级Go依赖。
- sqlite3 3.7.0、cryptography 2.9.0、flutter_secure_storage 11.2.0、app_links 7.2.2、path_provider 2.1.6、url_launcher 6.3.3、file_selector 1.1.0、flutter_local_notifications 22.3.1。
- JWT access token只在内存；refresh token、device凭据与数据库密钥在操作系统安全存储。
- 本地正文及业务标识AES256-GCM加密；seq为规范十进制字符串，逐消息事实决定状态。
- 默认短期离线阅读300秒，租户可设0；重启后的首次阅读必须联网授权；不承诺离线即时撤销。
- ≥50%可见500ms，前台／焦点／激活会话且本人device已送达才确认read；read合批300ms，最大500。
- 现有25MiB文件、PDF/PNG/JPEG/TXT、授权、保留及扫描规则不变。
- 本地通知不代表应用被终止后的推送；APNs/FCM/私有网关另有配置与真机验收。
- 独立工具目录及任务级DEVELOPER_DIR；不改全局PATH、xcode-select或其它项目配置。
- 模拟API、共享测试、模拟器、实际平台运行与客户签名/生产状态分别报告；环境缺项不得标该平台完成。

## Review Focus

- 冷启动回调、并发登录、state/nonce/redirect mismatch或异常IdP响应：Task1、2、8。
- 账号/服务/任职切换及晚到请求不能串库、泄漏或发错人：Task3、4、5。
- 明文SQLite/WAL、密钥失效、系统时钟回拨、崩溃/重启不能扩大阅读期限：Task3。
- 超时/断页/取消/本地事务失败不能重造client_msg_id或误报送达read：Task4、5。
- 真实各平台的安全存储、外部浏览器、IME、文件导出、挂起不能由Widget测试替代：Task6、7、8、9。

## 执行准备与证据

由当前实施者在已批准分支工作。每任务 RED→最小实现→GREEN→相关回归→明确文件提交；不要把缺Flutter/平台工具当有效RED。先完成P5-01，Task1的backend native broker可与不依赖Flutter的准备独立执行，之后核心和平台任务顺序执行。

Task2将SDK安装到独立任务工具目录，产生 `docs/verification/p5-02-toolchain-lock.json`，保存实际SDK/Dart/engine commit、归档及锁文件hash。命令中的 `flutter` 指该锁定SDK绝对路径，通过本任务PATH临时设置；执行目录为 `clients/enterprise_im`。Go命令在仓库根，沿用GOWORK=off、-mod=readonly。真机/Windows尚缺时继续不依赖它们的步骤，最终逐平台报告未验收条件。

### Task 1：原生OIDC broker与严格运行配置

**Files:** Create `internal/nativeauth/config.go`、`exchange.go`、`tokens.go`、`config_test.go`、`exchange_test.go`、`tokens_test.go`、`internal/httpserver/native_auth.go`、`native_auth_test.go`、`cmd/im-api/native_runtime.go`、`native_runtime_test.go`；Modify `cmd/im-api/main.go`、`internal/oidcauth/auth.go`（共享经过配置的JWK provider，不能放宽既有access-token校验）。

**Interfaces:** `nativeauth.PlatformConfig{Platform,ClientID,RedirectURI string;Loopback bool;RefreshEnabled bool}`；`nativeauth.Config{Issuer,AuthorizationURL,TokenURL,Audience,Scope string;Platforms map[string]PlatformConfig}`；`ExchangeRequest{Platform,Code,CodeVerifier,RedirectURI,Nonce string}`；`RefreshRequest{Platform,RefreshToken string}`；`Tokens{AccessToken,RefreshToken,TokenType string;ExpiresAt,ServerTime time.Time}`。`Broker.Exchange(ctx,req)(Tokens,error)`、`Refresh(ctx,req)(Tokens,error)`；configured JWK validator检查ID token，现有Authenticator验证access token及本地用户映射。`HandlerWithNativeAuth(base,broker)(http.Handler,error)`；loadNativeRuntime返回严格验证config，默认false。

- [ ] 写 `TestNativeOAuthRejectsWrongIssuerNonceAudienceAndRedirect`、`TestNativeOAuthNoArbitraryTokenDestination`、`TestNativeRuntimeDisabledOrMissingPrerequisites`：PKCE/nonce、127.0.0.1动态port精确path、scheme、错误平台、4KiB、开关及客户端允许列表。
- [ ] `go test -count=1 ./internal/nativeauth ./internal/httpserver ./cmd/im-api -run '^TestNative'`，缺方法/路由FAIL；先准备签名测试IdP，不能将无网络环境当预期失败。
- [ ] 实现5秒总预算、20并发、20/min/IP、运营固定tokenURL、签名/azp/nonce/expiry验证及安全错误；首次兑换含nonce校验，refresh遵守IdP轮转、不要求refresh一定返回ID token。
- [ ] 写并通过 `TestNativeOAuthRefreshRotationAndInvalidGrant`、`TestNativeOAuthTimeoutAndSensitiveLogRedaction`、`TestNativeOAuthExchangeThroughRealAPIIdentityMapping`；运行既有OIDC audience/client回归PASS。
- [ ] 提交 `feat: add configured native oidc token broker`。

### Task 2：可运行Flutter登录、平台回调与协议解析

**Files:** Create `clients/enterprise_im/{pubspec.yaml,pubspec.lock,analysis_options.yaml,lib/main.dart}`、`lib/core/protocol/{seq.dart,api_error.dart,api_client.dart,models.dart}`、`lib/core/auth/{native_auth.dart,auth_session.dart,pkce.dart}`、`lib/platform/{browser_launcher.dart,login_callback.dart}`、`lib/ui/login_page.dart`、`test/{protocol_test.dart,auth_test.dart}`；生成四平台runner，冻结测试bundle标识com.enterpriseim.client；Create `scripts/prepare-client-toolchain.py`、`docs/verification/p5-02-toolchain-lock.json`。

**Interfaces:** `Seq.parse(String canonical)`/`toString()`封装BigInt，0～2^63-1；`ApiClient(origin,transport)`返回严格DTO且拒绝跨源重定向；`AuthSession{origin,tenantId,userId,accessToken,expiresAt,generation}`；`NativeAuth.signIn(String platform)`与`refresh()`返回Future<AuthSession>，`cancel()`返回Future<void>。`LoginCallback.start()`返回本次redirectUri和callback Future；`close()`退出listener/subscription。Task1为token broker。Dart DTO在models.dart定义：`DeviceGrant{String deviceId,deviceToken;DateTime expiresAt}`、`SyncPage{String conversationId;List<MessageDTO> messages;Seq nextAfterSeq,receiptRevision;bool hasMore;String? syncProof;DateTime? proofExpiresAt;DateTime serverTime}`、`MessageDTO{Seq seq;bool redacted;String? messageId,senderUserId,messageType,text;Map<String,Object?>? attachment;DateTime? contentExpiresAt,cacheUntil,onlineUntil}`。`MessageACK{String messageId,conversationId;Seq seq;DateTime serverTime;bool duplicate}`；`ConversationSummary{String id,kind,name;Seq lastSeq}`；P5-01其它DTO按细化规格JSON字段强类型定义。

- [ ] 先准备并验证固定源SDK/附带Dart及隔离工具环境，验证失败停止下载/生成；保存SDK/engine锁定记录。下一步生成项目后设置精确pubspec，运行pub get核对归档hash并生成pubspec.lock；解析不兼容先保存证据，不静默升级。
- [ ] `flutter create --no-pub --org com.enterpriseim --project-name enterprise_im --platforms windows,macos,android,ios .`，仅生成本期客户端并设置测试标识；删除模板默认cupertino_icons/flutter_lints，仅保留Flutter、flutter_test、integration_test SDK依赖与规格8个精确包；定义本地analysis规则，pub get冻结lock后写 `testProtocolInt64AndBadDTO`、`testAuthStateNonceCallbackAndCancellation`，运行 `flutter test test/protocol_test.dart test/auth_test.dart`确认方法缺失FAIL。
- [ ] 实现HTTPS配置页/登录页、PKCE与精确回调、Jwt内存、当前generation；桌面127.0.0.1监听／mobile app_links在冷启动前初始化；显式外部浏览器。
- [ ] 测试PASS且 `flutter analyze --fatal-infos` 0 issues；运行锁定平台插件最小实际build，记录成功/环境缺项；插件可加载并完成一次安全存储写读删，不仅看pub解析成功。
- [ ] 提交 `feat: add shared flutter protocol and native sign in`，排除SDK/cache/build产物与秘密。

### Task 3：加密本地事务、会话恢复与退出清理

**Files:** Create `lib/core/storage/{secure_vault.dart,encrypted_store.dart,store_worker.dart,records.dart}`、`lib/core/auth/session_store.dart`、`test/{storage_test.dart,session_restore_test.dart}`、`integration_test/secure_storage_test.dart`。

**Interfaces:** `SecureVault.read/write/delete(namespace,key)`；`EncryptedStore.open(namespace,key)`、`commitPage(SyncPage page,PendingReceipt pending)`、`putPendingSend(PendingSend send)`、`pendingSends()`、`readAuthorizedMessages(LeaseClock clock)`、`closeAndErase()`；全部Future。`LeaseClock.start(serverTime,receivedMonotonic,cacheUntil)`，restart必须重新授权。记录payload包含conversation/membership/generation/seq/content，SQLite外层只有opaque key/type/nonce/ciphertext/tag。模型在本任务`records.dart`定义：`PendingSend{String clientMsgId,conversationId,actingMembershipId,text;int generation;DateTime createdAt;String state}`、`PendingReceipt{String conversationId,syncProof;Seq nextAfterSeq;DateTime proofExpiresAt;int generation}`、`StoredMessage{Seq seq;String conversationId;Map<String,Object?> content;DateTime cacheUntil,contentExpiresAt}`。

- [ ] 写 `testEncryptedStoreHasNoPlaintextInDatabaseOrWal`、`testStoreCommitFailureDoesNotAdvanceCursor`、`testNamespaceSwapAndTamperRejected`、`testRestartAndClockRollbackCannotExtendLease`；运行 `flutter test test/storage_test.dart test/session_restore_test.dart`确认未实现FAIL。
- [ ] 实现AES256-GCM/随机96bit nonce/AAD、单isolate事务、每账号密钥、refresh/device凭据隔离及本地记录；密钥丢失拒绝降级，删除不可读缓存。
- [ ] 编写退出/换服务/账号代次原子销毁；`testLogoutCancelsLateWritesAndErasesKeyDatabase`、`testRefreshRotationCrashDoesNotMixAccounts`通过，保留尚未确认发送的幂等编号直到显式放弃。
- [ ] 在每可用平台运行 `flutter test integration_test/secure_storage_test.dart -d <device>`，验证重启后密钥读取和注销删除；无法运行平台不记PASS。
- [ ] 提交 `feat: persist encrypted client state and safe session restore`。

### Task 4：同步、送达、阅读和恢复核心

**Files:** Create `lib/core/sync/{sync_engine.dart,receipt_queue.dart,realtime_channel.dart,retry_policy.dart}`、`lib/platform/app_activity.dart`、`test/{sync_test.dart,receipt_queue_test.dart,recovery_test.dart}`。

**Interfaces:** `SyncEngine.syncConversation(context)`、`resume()`、`stop()`；输入 `ClientContext{AuthSession session;String actingMembershipId;DeviceGrant device;int generation}`、Task3存储、P5-01SyncPage。`ReceiptQueue.confirmVisible(List<Seq> seqs,ClientContext context)`，`contextChanged()`；`AppActivity{foreground,focused,activeConversation}`为平台注入；输出逐消息send/receive/read状态及UnreadState，不自行构造用户身份。

- [ ] 写 `testGapOrTransactionFailureDoesNotConfirmDelivery`、`testExpiredProofRefetchKeepsCursor`、`testBackgroundAndStaleGenerationNeverRead`、`testTwoDevicesMergeUserReadWithoutOwnDeliveryForgery`；`flutter test test/sync_test.dart test/receipt_queue_test.dart test/recovery_test.dart`确认FAIL。
- [ ] 实现完整页验证→本地提交→delivered→可见read；采用300ms合批、500条、精确seq；WS sync_required合并刷新，5秒/30～40秒兜底及1～30秒抖动重连。
- [ ] `testRevokedDeviceAndIdentityStopQueues`、`testRepeatedOutOfOrderReceiptAndPollingConverge`、`testClientLeaseExpiryRedactsCachedContent`通过；网络错误按401/403/409/410/429/503明确分流。
- [ ] 真实后端协议测试验证sender/Bob两device、read变更、可见区间；保留生产无mock分支，测试通过注入transport/activity实现。
- [ ] 提交 `feat: implement durable multi device sync and receipts`。

### Task 5：目录、会话、群动作及可靠发送界面

**Files:** Create `lib/features/{memberships,directory,conversations,groups}/`中的controller/page/model，`lib/ui/{client_shell.dart,message_list.dart,message_composer.dart,receipt_badge.dart}`、`test/{navigation_test.dart,message_send_test.dart,message_view_test.dart}`；Modify `lib/main.dart`。

**Interfaces:** `ConversationController.select(ClientContext context,ConversationSummary conversation):Future<void>`、`sendText(String text):Future<MessageACK>`、`retryPending(String clientMsgId):Future<MessageACK>`、`abandonPending(String clientMsgId):Future<void>`；消费Task4引擎和现有P4-31 API。UUIDv7由 `ClientMessageId.create(serverClock,secureRandom)` 生成，服务端时间/单调偏移为基线；先putPendingSend后网络，ACK后更新。

- [ ] 写 `testSendTimeoutAndRestartReuseOriginalClientMsgId`、`testAccountOrMembershipSwitchBlocksPendingSend`、`testOnlyVisibleDeliveredMessagesProduceRead`、`testImeCompositionDoesNotSend`；运行 `flutter test test/navigation_test.dart test/message_send_test.dart test/message_view_test.dart`确认FAIL。
- [ ] 实现中文桌面双栏/移动列表路由、任职选择、可见人员搜索、已有单聊/群列表及已有group动作；成功ACK标已发送，结果未确认显式重试，409/410不伪造成功。
- [ ] 新回执UI显示sent/delivered/read；群summary_available=false用规定文案，visible_read/count标签准确，未读下界不可冒充精确值。
- [ ] Widget测试PASS：群跨组织创建/授权拒绝、退群状态、键盘/字号/语义、50%/500ms可见性；真实后端 `integration_test/messaging_test.dart`验证收发与群动作。
- [ ] 提交 `feat: add group directory and reliable messaging client UI`。

### Task 6：附件、本人设备管理及安全文件导出

**Files:** Create `lib/features/files/{file_transfer.dart,file_download.dart,file_message_card.dart}`、`lib/features/devices/{device_controller.dart,device_page.dart}`、`lib/platform/{file_picker.dart,file_export.dart}`、`test/{file_transfer_test.dart,file_download_test.dart,device_test.dart}`；Modify Android `android/app/src/main/kotlin/com/enterpriseim/client/MainActivity.kt`、iOS `ios/Runner/AppDelegate.swift`（仅移动导出桥）。

**Interfaces:** `FileTransfer.reserveAndUpload(context,SelectedFile)`、`poll()`、`cancel()`、`bindToMessage()`；`FileDownload.downloadAuthorized(context,fileId)`、`exportPrivateFile(context,artifact)`；`DeviceController.list()`、`revoke(id)`、`renew()`；file_selector只做支持的平台文件选择，desktop保存选择使用其原生API，mobile使用MethodChannel `enterprise_im/file_export` 调用系统CREATE_DOCUMENT/导出选择器。

- [ ] 写 `testFileTransferDoesNotBindBeforeReadyOrAcrossContext`、`testInterruptedDownloadErasesTemporaryFile`、`testExpiredFileOrLateResponseCannotExport`、`testDeviceQuotaAndRevocationStopOwnClient`；运行三个对应 `flutter test` 文件确认FAIL。
- [ ] 实现既有reservation/content/poll/bind、25MiB/类型/错误码；stream写私有临时文件，取消/失败删除；读卡片、下载与文件内容打开三种状态分开。
- [ ] 实现本人设备额度满入口、孤立登记撤销/新ID登记、当前设备撤销退出；移动导出用用户明确系统选择，不把私密业务路径传递为外链。
- [ ] `integration_test/files_and_devices_test.dart`真实后端验证clean/EICAR/扫描失败、下载失败/到期与撤销；各平台实际picker/export运行单独记录，不用mock替代平台验收。
- [ ] 提交 `feat: add authorized files and own device management`。

### Task 7：桌面平台、生命周期与测试包

**Files:** Modify `macos/Runner/{DebugProfile.entitlements,Release.entitlements,Info.plist}`、`macos/Runner.xcodeproj/project.pbxproj`、`windows/runner/{main.cpp,flutter_window.cpp}`；Create `lib/platform/{desktop_activity.dart,notifications.dart}`、`integration_test/{desktop_lifecycle_test.dart,real_backend_test.dart}`、`scripts/build-client-macos.sh`、`scripts/build-client-windows.ps1`、`.github/workflows/client-desktop.yml`。

**Interfaces:** Task4 AppActivity接窗口焦点/最小化、Task2desktop loopback、Task3系统安全存储、Task6picker/export；Notifications仅进程活跃且经授权状态提示“收到新消息”，点击再复核context。

- [ ] 写 `testDesktopMinimizeStopsReadAndRefocusResyncs`、`testNotificationDeniedDoesNotBlockMessaging`、`testDesktopLoopbackCancelClosesListener`；实际desktop integration RED确认行为未实现。
- [ ] 实现平台焦点、通知、网络及Keychain entitlements；不扩大文件读取权限。Windows启动参数拒绝秘密/任意外部业务URL。
- [ ] macOS执行 `flutter build macos --release`、`flutter test integration_test/desktop_lifecycle_test.dart -d macos`，实际启动release与固定后端跑real_backend_test；记录应用hash及原生退出。
- [ ] Windows runner执行 `flutter build windows --release`、同组 `-d windows` 用例；打包运行目录，实际Windows启动/登录/文本/文件/回执/重启。CI不存在或未执行成功时保存“未运行”，不能用workflow存在代替PASS。
- [ ] 提交 `feat: deliver desktop platform adapters and test packages`。签名/公证状态单独报告。

### Task 8：Android／iOS平台、真机与安装包

**Files:** Modify Android `AndroidManifest.xml`、`build.gradle.kts`、`MainActivity.kt`及iOS `Runner/{Info.plist,Runner.entitlements,AppDelegate.swift}`、`Runner.xcodeproj/project.pbxproj`；Create `lib/platform/mobile_activity.dart`、`integration_test/mobile_lifecycle_test.dart`、`scripts/build-client-mobile.py`、`.github/workflows/client-mobile.yml`。

**Interfaces:** AppLinks精确scheme/path、Task4前后台代次、Task3安全存储、Task6文件选择/导出；bundle测试标识com.enterpriseim.client，正式分发由客户签名团队核定，不声称拥有域名。

- [ ] 写 `testMobileColdAndWarmCallbackUsesCurrentLoginState`、`testSuspendNeverReadsAndResumeRevalidatesDevice`、`testBackupRestoreWithoutKeyErasesUnreadableStore`；模拟器native integration RED，缺设备不算有效RED。
- [ ] 实现Android明文网络禁用/私有数据不备份、iOS解锁Keychain/非共享访问、移动通知授权及deep-link启动；系统挂起后按前台恢复补拉，不能宣称WS保证被杀应用推送。
- [ ] Android `flutter build apk --release` 和 `flutter test integration_test/mobile_lifecycle_test.dart -d <android-device>`，至少一次模拟器及真机完整用例；记录APKhash/签名性质。
- [ ] iOS `flutter build ios --simulator` 和相同用例 `-d <ios-device>`；有Apple团队/真机才执行签名安装及 `flutter build ipa --release`，无材料保持“真机分发未验收”。两端分别验证附件、reconnect、多device/read一致性。
- [ ] 提交 `feat: deliver mobile platform adapters and test builds`。

### Task 9：跨端最终验收与交付

**Files:** Create `scripts/test-native-clients.py`、`scripts/tests/test_native_client_results.py`、`clients/enterprise_im/integration_test/cross_device_test.dart`、`docs/P5-02-四平台客户端验收.md`、`docs/verification/p5-02-results.json`、`docs/客户端测试指南.md`；Modify `README.md`。

**Interfaces:** 门禁结果逐platform、architecture、fixed source、SDK/lock/binary hashes、commands actual exit、customer signing、tests/cleanup分别记录；缺项blocked不映射passed。测试包只收已实际生成可复核文件，禁止发布私钥/凭据。

- [x] 编排RED `python3 -m unittest discover -s scripts/tests -p 'test_native_client_results.py' -v`：只有共享逻辑PASS、平台未跑、实际exit非零、hash错配、真机缺项或清理失败均不能overall“四平台完成”。
- [ ] 实现真实后端资源复用及native platform runner协调，确保证据归属与secret脱敏；不影响其它项目/客户数据。
- [ ] `flutter analyze --fatal-infos`、`flutter test`、各实际平台integration与四平台build门禁；跨端至少Alice与Bob、Bob两device、跨组织群、断网/重启/丢ACK/receipt重试、退出/撤销/权限/保留变化。
- [ ] 固定交付源后核对后端相关普通/race回归、旧Web兼容、客户端binary hash及无秘密包，实际进程/资源清理；分别标本地、模拟器、真机、签名与客户SSO状态。
- [ ] 提交测试指南及证据；按既有已授权交付流程推送Draft PR、立即attach_artifact；客户签名/生产发布需要实际材料及对应授权，不以本期代码PR冒充上线。

## Handoff

用户联合评审后保持Native执行方式。先P5-01，后本计划；可独立完成的工作持续推进，平台环境缺项只阻塞对应平台验收。到交付时按真实证据声明已完成范围，不给未经验证的四平台完成结论。


## 2026-10-18 Task9阶段推进

用户明确暂缓双真机验证，继续不依赖硬件的工作。本轮完成离线证据合同核验入口、36项合同／CLI测试及测试指南；历史结果只核验原件完整性，不补当前平台用例。真机暂缓仍保持缺项，四平台完成为false。统一runner适配、cross_device实际协调、全部平台最终运行、客户／签名／CI仍未完成，Task9保持进行中。Task5—8原BASE和状态不变。


### Task9移动runner接入增量（2026-10-18）

已实现 `--export-acceptance` 和只读 `--runner-output`：同一固定提交的Android build／普通Release launch及iOS Debug模拟器build可导出；其他原生业务专项仍需各自实际产物与证据。源码／工具／摘要／参数／清理不满足时拒绝，旧报告不重写或换源。固定源码 `f4041320241232f775a24fcd25633074e3b28777` 上Python277执行无失败／跳过；Android API36及iOS构建结果见新增runner-adapter报告。Debug安全存储场景未映射到Release产物。首轮iOS元数据附带文件导致导出失败，保留后通过真实归档RED→GREEN及新源重跑修复。

上方“真实后端资源复用及native platform runner协调”尚未整体勾选；本增量仅完成移动构建／启动适配。跨端独立进程／账号／设备协调、全部平台门禁、真机／Windows／客户条件及最终交付继续保持未完成。Tasks5—8原BASE／状态保持，本地检查点不触发远端动作。


### Task9 iOS同归档普通启动增量（2026-10-18）

固定源 `9be0ccc09eb7dd9e4d832a089633e1af82b295fd 的已有 `--verify-simulator` 在专项后构建／归档正常main应用，安装时核对文件清单及可执行文件摘要，验证原生PID、UTC进程观察、首屏、停止后的消失及所属模拟器删除，并接入同产物launch证据。iOS26.5／iPhone17Pro实际runner0、共享248／分析0／活动与两进程安全存储／普通包build+launch通过；Python287执行零失败／跳过，新增10项覆盖启动合同与评审问题。原生leaf OS退出码unknown，真机及业务／跨端门禁不补填。Task9协调子项仍不整体勾选；Tasks5—8原BASE不变，旧证据保留，无远端动作。


### Task9 跨端独立进程消息增量（2026-10-18）

固定源 `60fe84b2eaf19b424d76c7450380fe74d527f1bd` 的三台自有模拟器（Android API36两台、iOS26.5一台）通过独立PID／设备身份核验、双向消息及ACK、逐设备送达／可见已读、UID级真实断网、同进程自动补拉、用户已读去重和发送端回执显示。11个独立数据库检查点通过，最终3消息／3设备／5设备送达与已读／3用户已读事实；实际后端Go夹具退出0。共享248／分析0、完整Python298次零失败／错误／跳过。此前4轮失败均保留，资源与旧证据独立核实。详情见[专项结果](../../verification/p5-02-cross-process-messaging-results.json)。

此增量未整体勾选Task9协调／交付：完整群组、文件、吊销、保留期与重启矩阵，Windows／macOS业务验收，双真机、客户交互SSO、分发签名、通知及远端CI仍有独立门禁。Tasks5—8原BASE／状态不变；原生框架测试数未采集、OS leaf退出码unknown；无远端动作。


### Task9 跨端独立进程群聊、附件及设备失效增量（2026-10-19）

固定源 `c61194d9fe07d10ae6c6e24f04c6e8af5d63ca6b`，两台Android API36及一台iOS26.5自有模拟器，三个独立PID／设备连接真实产品API／Outbox／Receipt／File Worker及官方下载审计修复进程，配套PG／Redis／MinIO／真实绑定ClamAV。跨两个组织建群，群主名单2成员2组织、普通成员完整名单403；双设备分别送达／可见已读，iOS回复，真实上传扫描ready附件、两端按原字节和SHA256下载并完成审计；iOS撤销同用户Android后，后者自动终态并删除UI拥有的私有缓存，六个旧凭据入口实际401，静置后无请求／重新登记，有效iOS继续接收seq4。14个独立DB检查点通过，最终4消息／3设备／1撤销、6设备送达与已读、4用户已读、1群／2成员／2组织／1附件／2已审计下载。

共享Flutter248／分析0，同源完整Python305执行／298唯一测试零失败／错误／跳过；受影响HTTP／文件下载服务普通及竞态轮次分别通过307／307项测试事件（包含子用例，每轮唯一标识数单列在报告），均退出0且无失败／跳过。后端夹具实际0，五产品进程及两扫描进程生命周期通过；修复实测发现的撤销设备仍可读附件缺陷，下载各授权检查事务验证设备，旧无设备头Web协议保留。一次独立评审的重要问题（产品日志随Go临时目录消失）已RED→GREEN修正。七轮失败与局部回归原件保留；群回执检查修正为指定消息行的群人数格式，下载工厂显式恢复实时绑定下的本轮测试CA信任区，局部探针RED→GREEN；最终轮实际加载签名库及独立验证收据保留；第二轮更新库原字节未保留，该失败轮仅诊断。独立核实自有资源清理、工具不变、69旧JSON与前轮507原件不变。原生框架数量未采集、OS leaf退出码unknown；全量Go／race本增量未重跑。详情见[专项结果](../../verification/p5-02-cross-process-group-files-devices-results.json)。

本增量不整体勾选Task9，Tasks5—8原BASE／状态不变；双真机暂缓，桌面业务、剩余保留期与重启矩阵、客户交互SSO、外部系统选择器／导出、分发签名、通知、四平台／客户／生产／远端CI仍有独立门禁。无推送、合并、部署或CI。


### Task9 下载中途设备撤销增量（2026-10-19）

测试源 `b70634dfad9d4476f3eb4a40e7870645e47d7729` 只增加回归，产品代码仍为c61194d。真实8MiB附件、TLS处理器／Repository与生产OIDC／设备API，在Prepare后首字节前及首段32768字节后的下一检查前提交设备撤销。前者401／正文0，后者已有200响应UnexpectedEOF截断；两者终态interrupted／authorization_revoked。原设备新请求401，另一有效设备8MiB完整摘要一致；各窗口2终态均已审计、正文暂存清理。移除设备校验的独立归档反证两个窗口均泄露完整正文并实际退出1；固定源码普通／race各1顶层2子例通过，完整Python305／298唯一零失败／错误／跳过。12产品进程生命周期、扫描器／容器／端口／私有根清理、70旧JSON与1675原件不变均独立核实。详情见[专项结果](../../verification/p5-02-download-device-stream-results.json)。

属于有调度边界的真实服务端并发验证，原生UI／真机／内核已接受字节回收、任意慢网络时点不由本项证明；旧无设备头Web协议保留。Task9与Tasks5—8原BASE／进行中状态不变；全仓Go／race和其余平台／客户门禁未补填，真机暂缓，无远端动作。


### Task9 保留期与移动进程恢复增量（2026-10-20）

测试源 `abb6edebd2c1fdecb0e7ec1f200aff1d3ff110c1`；产品实现未改。真实Android API36×2＋iOS26.5三独立进程及生产后端，附件30→1→30日与实际404／重新下载、双接收进程终止期间正文30→1日及新消息4、同安装系统安全存储／原设备／游标3恢复、设备renew先于sync、残留附件启动清理均通过。已有历史正文延长保持409且审计，单独延长附件不能复活过期正文。7个独立DB阶段，最终4消息／3设备／7设备D+R／4用户read；4次70字节摘要一致的审计下载、6次内容404、策略历史2／5及拒绝审计1。Flutter248、analyzer0、Python310／303唯一零失败／错误／跳过；71旧JSON／1742旧原件不变及所有自有资源清理独立核实。详情见[专项结果](../../verification/p5-02-retention-restart-results.json)。

时间戳老化仅隔离夹具，无系统时钟修改；未开启物理清理，不承诺物理删除恢复或即时广播失效。旧授权read404先安全清空，再真实返回／重选获得当前授权。原始失败、Xcode用户初始化、Chrome版本基线与异步测试完成等待修正保留。Task9及Tasks5—8原BASE／进行中状态不变，真机继续暂缓，无远端发布动作；全仓Go／race及其余平台和客户门禁未补填。


### Task9 丢ACK及发送进程恢复增量（2026-10-20）

测试源 `6a2b69ec07ab950ab7333bb49fde9a5cdfc06125`；产品实现未改。真实Android API36×2＋iOS26.5三独立进程，生产API／Outbox／Receipt及PG／Redis：Android原消息seq1和iOS原消息seq2分别已落库、上游200后下游TLS中断，客户端无ACK／状态并保存pending；force-stop／SIGKILL后原账号vault、设备ID、数据库及client_msg_id恢复，授权前隐藏正文、设备renew早于sync。各观察3秒无自动消息POST，显式UI重试返回同message_id／seq的duplicate=true并保存确认、移除pending；新seq3正常且发送端已读。7SQL阶段最终3消息／3设备／5设备D+R／3用户read；5真实上游200含2丢确认、2重复重试、1新发送。Flutter248／analyzer0／Python312执行305唯一零失败错误跳过，聚焦Go2测试普通及race通过。72旧JSON／2648旧原件未变，原失败、截图、DB和传输轨迹留存并独立清理核实。详情见[专项结果](../../verification/p5-02-ack-restart-results.json)。

范围为受控ACK返回窗口和应用进程重启，非任意崩溃时点或额外系统重启；回执失败重试仍待专项验证，全仓Go／race未补跑。Task9及Tasks5—8原BASE／进行中状态保持，真机继续暂缓，无远端发布或完整四平台声明。


### Task9 回执自动重试增量（2026-10-21）

测试源 `45107705e8ec2be2039bab5f9a834f5fb8dcd3de`；后端实现未改；客户端成功已读只清除自身旧警告，并通知界面，较新的其它错误保留。真实Android API36×2＋iOS26.5三独立进程及生产API／Outbox／Receipt、PG／Redis：Bob双设备delivered／read四流分别至少两次受控503后自动恢复，失败与首个成功请求摘要一致，同PID／设备，无手动刷新或重选。送达失败时加密SQLite cursor1/pending保留，消息未获显示及读取授权；实际送达200后pending清除。已读503时设备read及用户read均0；先放行iOS时设备read1/0、用户read1，再放行Android后设备read1/1、用户仍1。发送端已送达→已读及后续seq2/3通过，10SQL阶段最终3消息／3设备／5设备D+R／3用户read。Flutter250／analyzer0／Python314执行307唯一零失败错误跳过，聚焦Go2项普通及race通过；73旧JSON／2870旧原件不变，证据及资源清理独立核实。详情见[专项结果](../../verification/p5-02-receipt-retry-results.json)。

故障仅为服务端转发前503，送达既有指数退避、已读Retry-After2秒；不覆盖所有状态、回执提交后丢ACK或死亡后已读曝光恢复。Task9及Tasks5—8原BASE／进行中状态保持，真机继续暂缓，无全仓Go／race重跑或远端发布声明。


### Task9 任职结束权限隔离增量（2026-10-21）

固定源 `e926330325c95ebbc15ce46ff7d93fc0ccae3b06`，本轮测试扩展、产品实现未改。Android API36×2＋iOS26.5三独立原生进程，真实生产API／Outbox／Receipt和PG／Redis：变更前seq1双Bob设备已读、seq2 Alice已读，随后实际管理接口204结束Bob当前任职。双端自动观测403 invalid_identity、消息正文／上下文／本地已读／加密接收游标及pending清空；实际旧sync、history、真实旧proof送达、read、send各403。集团账号、另一任职、原设备仍active，/me和本人设备列表仍200；3秒静默窗无后续失效设备请求或新注册，Alice账号有效。5SQL阶段最终2消息／3设备／0撤销／3设备D+R／2用户read，服务器历史回执保持。Flutter250／analyzer0／Python316执行309唯一零失败错误跳过；聚焦HTTP Go2项普通及race通过。74旧JSON／3510原件／主工作区不变，证据及自有资源独立核实；首轮缺测试导入导致analyzer失败，失败原件保留，最终运行通过。详情见[专项结果](../../verification/p5-02-permission-change-results.json)。

本轮仅在线任职结束；另一任职在本人接口仍可选，未切换再发消息；退出竞态、所有通信策略、离线／死亡窗口仍待验收。Task9及Tasks5—8原BASE／进行中状态保留，真机继续暂缓，无全仓Go／race或远端发布声明。


### Task9 退出在途响应隔离增量（2026-10-21）

固定源 `28ebe98b795f71f97859d55bf274988ce817591a`，测试扩展，产品实现未改。Android API36×2＋iOS26.5三原生进程及真实生产API／Outbox／Receipt、PG／Redis：Bob iOS在真实next_seq2同步200、Android在next_seq4同步200生成后，下游响应头前暂缓；按产品方法顺序关闭view／membership并ClientSession.logout，系统安全存储、账号选择及DB文件清空，消息页正文／已读清空、发送关闭。完成清理后原字节放行，两条请求均观测取消、摘要相同，双端不恢复旧账号或正文。同PID退出，iOS退出后Android读seq2、发送seq3被Alice读；双端退出后Alice发送seq5，3秒无其设备请求／新注册／新增回执。7SQL阶段最终5消息／3设备／0撤销／4设备D+R／3用户read；Flutter250／analyzer0／Python318执行311唯一零失败错误跳过；Go夹具2顶层＋2子场景普通／race通过。75旧JSON／3707原件及主工作区保持，自有资源独立清理，截图已复核。详情见[专项结果](../../verification/p5-02-logout-race-results.json)。

本轮为原生控制器／账号退出边界，不含完整退出按钮导航、再次登录、所有异步回调或OS死亡。夹具放行为向已取消连接写入原成功字节的尝试，不证明客户端实际接收。Task9及Tasks5—8原BASE／进行中、真机暂缓保持；无全仓Go／race重跑或远端发布声明。


### Task9 实际退出按钮与冷启动增量（2026-10-21）

固定源 `e60fa443b306ca83171e83fc2c5129a6cd145243`，本轮测试扩展、产品实现未改。Android API36×2＋iOS26.5三原生进程及生产API／Outbox／Receipt、PG／Redis：Bob双端实际LoginPage恢复、点击进入工作空间／任职／会话，原设备renew200、seq1已读；从消息页返回并点实际Shell退出按钮，回LoginPage且安全存储／账号选择／DB文件清空，无工作空间或恢复登录入口。同进程重建页面无新API；Androidforce-stop／iOSSIGKILL后同安装同私有目录、新PID且旧进程消失，重启无seedrefresh，实际LoginPage保持未登录，无OAuth／新注册。iOS退出后Android回复seq2、Alice读；seq3只有存活Android读；双端退出冷启动后seq4不增加回执，3秒静默无设备请求／新认证。8SQL阶段最终4消息／3设备／0撤销／4设备D+R／3用户read。Flutter250／analyzer0／Python320执行313唯一零失败错误跳过，HTTP Go2项普通及race通过；76旧JSON／3894原件及主工作区不变，截图与自有资源清理核实。详情见[专项结果](../../verification/p5-02-logout-navigation-results.json)。

真实UI使用保存refresh和合成IdP恢复，不替代客户交互式SSO或退出后重新登录；退出中途崩溃／存储故障及原生客户回调仍另验。Task9及Tasks5—8原BASE／进行中和真机暂缓保持；无全仓Go／race重跑、完整四平台或远端发布声明。


### Task9 新登录及真实移动端回调增量（2026-10-22）

固定测试源 `af128f1c6a223a0f4c48b36af0d7ca497aa7a290`，仅测试扩展，产品实现未改。Android API36×2＋iOS26.5三独立进程，私有配置均无seed refresh；真实LoginPage输入服务地址并点击企业账号登录，生产NativeAuth PKCE／state／nonce、真实AppLinks／MobileLoginCallback和生产token broker串联。合成授权适配器用自有CA获取授权结果，再经生产ExternalBrowserLauncher实际交付OS：错误state被收到但200ms内不完成attempt，有效回调完成一次；每端1token／0refresh，系统安全存储与生产loadRefresh核对、真实任职／会话UI入场通过。Alice两条消息由Bob双端分别送达／已读，iOS回复由Alice送达／已读，5SQL阶段最终3消息／3设备／5设备D+R／3用户read。Flutter250／analyzer0／Python322执行315唯一零失败错误跳过，相关Go普通与race通过。77旧JSON／4333原件及主工作区保持，失败轮次原件保留，截图与自有资源清理核实。详情见[专项结果](../../verification/p5-02-fresh-login-callback-results.json)。

授权适配器不含系统浏览器HTTPS授权页面，不替代客户SSO／MFA／企业身份源；无OS信任修改。退出后重新登录、OAuth进程销毁恢复、真机／桌面签名／通知／完整四平台和远端CI另验；Task9及Tasks5—8原BASE／进行中及真机暂缓保持。无全仓Go／race重跑或远端发布声明。
