# P5-02 四平台 Flutter 客户端规格

日期：2026-10-08。状态：Windows＋macOS＋Android＋iOS／Flutter细化规格与实施计划已获用户确认，待P5-01完成后实施。

## 1. 目标与依赖

使用 `clients/enterprise_im` 一个Flutter工程，接入P5-01回执协议和P4-31真实API。成功标准是每个平台可安装／启动、完成OIDC登录和集团任职选择、单聊／群聊文本与附件、历史补拉、送达／已读及设备管理，并提供该平台独立证据。使用共享核心或在模拟API运行不能替代平台验收。

依赖回执规格 `2026-10-08-p5-01-receipts-design.md`。最小首批系统版本作为产品基线：Windows 10 x64、macOS 12 ARM64、Android API24、iOS13；如精确SDK或插件需要更高版本，先用构建证据提出基线修订，不静默抬升。Windows ARM64、macOS Intel、Linux不计本轮平台完成范围。

## 2. 工具链与精确依赖

官方当前安装文档给出Flutter 3.47.3；客户端固定目标SDK 3.47.3、Dart>=3.12.0 <4.0.0。官方Git标签3.47.3已只读核对到commit `e8113bf45620cbeb8aff64947ee4c93e16adb4cf`；实施首个门禁核对实际附带Dart版本及SDK安装/归档摘要；验证不满足即停止该SDK安装／生成项目，报告证据。元数据查询不等于依赖可构建。

2026-10-08已从pub.dev API核对：sqlite3 3.7.0、cryptography 2.9.0、flutter_secure_storage 11.2.0、app_links 7.2.2、path_provider 2.1.6、url_launcher 6.3.3、file_selector 1.1.0、flutter_local_notifications 22.3.1。使用精确版本，不使用caret或latest，提交pubspec.lock。仅使用Dart SDK HttpClient/WebSocket/Random.secure，不再引入HTTP、UUID、状态管理包；业务状态使用ChangeNotifier/ValueNotifier。

其中app_links要求Dart ^3.12.0、Flutter>=3.44.0；各插件四平台支持和归档摘要见 `docs/verification/p5-01-toolchain-metadata-20261008.json`。Flutter下载清单当前查询404，GitHub API标签查询限流；已通过官方Git引用查询核对固定源commit，SDK安装、引擎工件及归档摘要仍是准备门禁，不能写“工具链已验证”。实施须先解决固定来源再安装，SDK置于独立可写工具目录，通过任务环境变量调用，不改系统PATH或全局xcode-select。

Android build使用所选SDK模板要求的Gradle/AGP/JDK，生成后固定Gradle分发checksum、wrapper及AGP版本，先完成原生最小构建。Xcode当前27.0可通过本任务DEVELOPER_DIR使用；iOS minVersion若与所选SDK不兼容，同样停在工具链门禁提出修订。

## 3. 组件边界

- `lib/core/protocol/`：严格DTO、canonical decimal seq、typed消息、错误码、API与安全地址限制；共享JSON夹具和Web/Go保持一致。
- `lib/core/auth/`：外部浏览器PKCE、state/nonce、平台回调、token会话代次与刷新。
- `lib/core/storage/`：密钥安全存储、加密本地事务、pending发送及同步记录，单一后台isolate访问SQLite。
- `lib/core/sync/`：连续补拉、持久化后确认、回执队列、用户read合并、WS通知与轮询恢复。
- `lib/features/`：任职、目录、会话、群动作、文件传输、设备管理。
- `lib/platform/`：回调入口、前后台、窗口焦点、通知、文件选择和导出适配。
- `lib/ui/`：中文界面、桌面双栏／移动路由、错误与可访问性反馈，不展示内部数据库或证据术语。

服务地址必须绝对HTTPS、无userinfo/query/fragment，API跟随同一源；不跟随把Authorization带到另一源的重定向。集团私有CA依赖系统信任，不设置badCertificateCallback接受所有证书。Fixture只使用隔离测试CA／测试构建配置，Release不含关闭TLS校验的选项。

## 4. 原生OIDC与后端适配

新增受配置控制的 `GET /api/v1/native/config?platform=windows|macos|android|ios`，返回固定issuer、authorization_url、client_id、scope、redirect方式及server_time。只返回非秘密配置。`IM_NATIVE_ENABLED=false`默认；开启要求OIDC、receipts启用且四个平台显式配置。各平台独立client_id加入现有AllowedClientIDs；不默认扩大允许列表。

新增 `POST /api/v1/native/oauth/token`：严格JSON `{platform,code,code_verifier,redirect_uri,nonce}`，最大4KiB；只向运营配置的HTTPS token endpoint兑换。redirect必须符合该平台预配置规则；IdP public client，不发送client secret。服务端复用已配置JWK/JWT校验ID token签名、issuer、audience、过期、nonce，再验证API access token适用现有audience／client及本地身份映射。失败返回闭集401/503，不转发IdP原始正文或令牌日志。公共兑换/refresh每IP每分钟20次、每进程最多20并发、5秒总预算；平台端点以环境固定允许集配置，无由请求指定的外联URL。

刷新 `POST /api/v1/native/oauth/refresh` 仅在平台启用刷新能力时可用，输入platform＋refresh_token（4KiB上限），固定IdP地址，限制同兑换。服务端校验新access token及包含时的新ID token；轮转token由客户端原子替换。响应不含新refresh token表示保留原值，invalid_grant删除原值并重新登录；IdP不支持则页面明确提示到期重登，不能模拟刷新。

客户端desktop在127.0.0.1随机端口监听 `/oauth/callback`，不绑定外部地址；IdP与broker允许该client固定loopback host/path的动态port，实际token兑换使用当次精确redirect。收到请求先验证method/path/state/重复回调/当次代次，完成即关闭监听。登录超时120秒，取消立即关闭。系统浏览器由url_launcher externalApplication打开。

Android/iOS使用精确自定义scheme `com.enterpriseim.client:/oauth/callback` 和各自不同client_id，以PKCE防截获；app_links在冷启动前初始化，回调state不匹配即丢弃。若客户拥有受控HTTPS域名，可另行配置App/Universal Links和域名关联；不虚构域名所有权。

PKCE verifier为32～64bytes安全随机编码，S256挑战；state、nonce分别32bytes随机。并发登录只允许一个活跃会话；IdP拒绝、取消、token不适用、expired、issuer变化均不给已登录UI。不得保存state/verifier到日志或普通偏好文件。

## 5. 本地存储与阅读期限

本地库按SHA256(service origin＋tenant＋user)命名，存于应用支持目录。SQLite只保存opaque记录键、随机nonce、AES256-GCM ciphertext和tag；正文、文件名、账号资料、业务标识及队列内容放在加密payload。记录AAD包含schemaVersion／账号命名空间／记录类型／opaque key，防跨账号移植和调换；密钥32bytes，每账号随机，存于操作系统安全存储。nonce每次加密随机96bit，不复用。用单一isolate串行事务，正文与页游标／pending receipt同事务提交。

JWT access token只在内存；refresh token、device凭据与数据库密钥在安全存储中按origin/tenant/user隔离。设备ID不是硬件ID；新安装或凭据丢失重新登记。密钥丢失不能降级保存明文，应删除不可读库并重新同步。认证前不打开旧账号库。

同步页cache_until由服务端给出；客户端使用收到server_time后的单调计时计算剩余期限，不能因系统时钟回拨延长。应用重启或长时间挂起后的首次阅读必须联网重新授权；离线且租约已过期锁定内容，仅保留待确认发送所需加密队列。默认短期离线阅读300秒，租户可设0；无法承诺离线期间即时收到策略撤销。

收到hard_deny／过期遮蔽、退出、切账号／服务或设备撤销，取消请求、停止同步并清除受影响正文和私有附件缓存；账号退出删除密钥、数据库及临时文件。文件导出仅用户显式操作、当时通过下载授权；导出到用户选择位置后由用户管理，不宣称应用可回收外部副本。

## 6. 同步与发送状态机

当前身份由origin/tenant/user/actingMembership/generation确定。账号或任职切换递增generation；所有API、数据库结果及回执提交必须匹配generation。尚未确认的旧发送沿用Web规则，先重试或明确放弃，再切换任职／会话。

状态顺序：登录→任职→设备恢复/登记→同步→允许操作。消息补拉从本地完整已提交页开始；验证连续seq、内容结构和next_after_seq；完整本地事务提交后提交delivered；提交失败保留pending proof重试，proof过期重新拉同页。只有delivered成功后才将页面可见消息加入read队列。read合批300ms、最大500条，前台/激活会话/窗口焦点、至少50%进入可见区域持续500ms；后台、预览、搜索命中不触发read。

发送先在本地事务保存UUIDv7 client_msg_id、origin/identity/会话和正文，再调用现有API。ACK到达才显示已发送；超时保持“发送结果待确认”，重试沿用同ID及内容；409/410明确提示用户，不重造ID伪装重试。生成UUIDv7使用已登录server_time和单调偏移；无有效时间基线时先联网核对，不盲从设备时钟。

实时仍使用既有短期ticket＋WS sync_required；通知只触发合并刷新。连接失败时每5秒补拉当前会话，健康连接约30～40秒兜底核对；随机错峰。切前台立即验证设备及身份后补拉，后台停止read并按系统约束减少轮询。重连退避1/2/4/8/16/30秒加抖动，401先刷新或重登，403停止非法任职，429尊重Retry-After，503保留状态重试。

群回执采用P5-01可见接收者统计与默认owner/admin门槛；普通成员不得把不可查询状态显示成“无人已读”。未读exact=false按下界文案显示。seq使用Dart int64并严格字符串解析，JS共享夹具使用BigInt，不接收浮点数或超界值。

## 7. 页面、附件与平台适配

核心页面：服务配置、登录、任职选择、通讯录、单聊／群列表、消息历史与输入、群创建及现有群动作、设备管理。界面支持屏幕阅读器语义、字体放大、键盘导航、中文输入法；发送键不在IME composition期间触发。

文件模块复用既有reservation→content→扫描poll→绑定消息，PDF/PNG/JPEG/TXT、25MiB与类型、caption、保留和审计规则不变；旧reservation失败按原错误码处理，不能自动创建多个已绑定文件。file_selector只获取用户明确选择的文件。授权下载流式写入私有临时文件，失败清除；打开/导出前复核当前期限和generation，不把下载完成当已读。

本地通知通过flutter_local_notifications，进程活跃收到新消息提示“收到新消息”；通知payload只含本账号opaque会话引用，点击需再授权。拒绝通知权限不影响消息及回执。系统终止后的推送需客户APNs／FCM／私有网关，属于独立配置适配和真机验收，不计本期进程内通知完成证据。

Windows安装包及URI/file路径不得把秘密放命令行；macOS启用网络／回调及安全存储所需entitlements；Android禁止云备份客户端私有数据，明文网络默认禁用；iOS Keychain不跨App共享，要求设备解锁。具体插件原生配置依照锁定版本官方说明，四个平台都测试可写、可读、可删除及重启恢复。

## 8. 平台完成与交付

| 平台 | 构建与运行证据 | 可交付测试产物 |
| --- | --- | --- |
| macOS ARM64 | 本机原生build、真实后端登录/文本/附件/回执及两次重启 | .app归档；签名／公证另列 |
| Windows x64 | Windows runner构建、Windows实际启动及真实后端用例 | .exe＋运行目录归档；正式安装器与签名另列 |
| Android | APK构建、模拟器及至少一台真机生命周期/回执/安全存储 | 测试APK；正式签名另列 |
| iOS | simulator构建与运行、真机签名安装及同组用例 | simulator .app；可安装IPA须Apple团队与签名 |

构建任务可以写GitHub Actions，但没有真实成功执行日志不能标通过；不得推送密钥／签名材料。保存SDK源码commit/归档摘要、pubspec.lock摘要、平台、架构、build模式、应用hash、测试命令、实际退出、后端固定源及资源清理。

当前本机Xcode27.0，默认DEVELOPER_DIR指向CommandLineTools；未在默认路径发现Android SDK或Flutter。Windows构建/运行环境与iOS真机签名尚待准备。环境缺项不阻止Go/Web或共享核心的独立进展，但该平台保持“未验收”。最终报告按共享逻辑、模拟器、原生运行、客户签名与生产分别报告。

## 9. 官方核对来源

SDK：[Flutter安装](https://docs.flutter.dev/install/manual)、[SDK归档](https://docs.flutter.dev/install/archive)。OAuth：[RFC8252](https://datatracker.ietf.org/doc/html/rfc8252)。依赖：[安全存储](https://pub.dev/packages/flutter_secure_storage)、[app_links](https://pub.dev/packages/app_links)、[SQLite](https://pub.dev/packages/sqlite3)、[cryptography](https://pub.dev/packages/cryptography)、[path_provider](https://pub.dev/packages/path_provider)、[url_launcher](https://pub.dev/packages/url_launcher)、[file_selector](https://pub.dev/packages/file_selector)、[本地通知](https://pub.dev/packages/flutter_local_notifications)。版本及archive_sha256的公开读取记录保存在本期metadata JSON；还未运行pub resolve或平台构建。

## P5-01评审后的协议补充

配置离线缓存0时，sync正文带可选online_until（最多5秒并受设备／JWT／内容期限约束），cache_until仍是server_time。Native只在内存展示该实时授权，不将online_until作为本地持久阅读租约；离线、挂起、失焦或授权失败立即清除正文。在线到期先清正文，再重新同步授权；页游标与待确认证明仍依本地事务规则提交。
