# Enterprise IM 原生客户端

共享Flutter客户端开发中，已生成Windows、macOS、Android、iOS runner。2026-10-15用户批准iOS最低15；已完成iOS27专用模拟器Debug构建／启动、前台活动边界及两进程Keychain专项，尚未交付四平台验收安装包。

## 当前功能与证据

已实现中文服务配置／原生OIDC登录、任职与设备准入、加密SQLite与原请求恢复、消息／送达／精确已读、通讯录／跨组织群操作、本人设备管理、附件扫描状态及私有下载／导出。共享逻辑和正式HTTPS／PostgreSQL／Redis后端已分别验证；对应证据与范围见仓库`docs/verification/p5-02-task5-real-messaging-results.json`及`p5-02-task6-real-files-results.json`。这些记录不代表已安装的四平台系统插件或窗口事件验收。

桌面当前会话通知须用户显式启用，仅在进程运行且前台授权页面提示“收到新消息”；拒绝通知不阻止消息，不支持应用终止后的推送或冷启动通知导航。Windows启动只允许无参数，令牌、URL、文件路径和未知参数均在Flutter引擎初始化前被拒绝；OAuth仍使用本机随机端口回调。

## 固定工具链

Flutter3.47.3、Dart3.13.3，源码固定为`e8113bf45620cbeb8aff64947ee4c93e16adb4cf`，记录在仓库`docs/verification/p5-02-toolchain-lock.json`。保留pubspec.lock并使用`flutter pub get --enforce-lockfile`，禁止静默升级。SDK、pub cache和平台SDK位于工程外。Xcode许可／工具可用状态改变后需要重新生成插件清单；已有缓存可执行`flutter pub get --offline --enforce-lockfile`。

本目录执行`flutter test --no-pub`。固定Flutter分析器在包含中文目录名时曾出现LSP字节长度错误；可创建指向同一目录的英文符号链接，再执行`flutter analyze --fatal-infos <英文链接绝对路径>`，不修改SDK或复制源码。

## 桌面门禁

Windows启动防护的宿主C++测试编译真实wWinMain，但Win32／Flutter SDK边界为夹具；不能替代Windows真实`.exe`运行。原生启动门禁、两进程安全存储及桌面回调测试的命令和边界见仓库`docs/P5-02-桌面验证操作.md`。`.github/workflows/client-desktop.yml`仅提供手动触发的构建／专项验证准备，不把工作流文件存在当作PASS。

## 当前未完成项

Xcode许可已经解除。macOS原生编译检查通过，但仅为命令级禁用签名的编译产物，链接器隐式ad-hoc签名不代表开发证书签名；本机开发证书数量仍为0，真实签名／安装、OS通知／文件选择／窗口生命周期及后端验收尚未完成。Windows原生构建／运行及CI未执行，Windows10基线仍需实际验证。iOS基线已批准为15，当前只在iOS27模拟器运行，未验证iOS15实际设备兼容性。Android已完成独立SDK／JDK准备、API36 ARM64模拟器Release构建与登录首屏、原生导出边界3项、前台活动及保留安装的两进程Keystore专项；API36本地AOSP提供程序的真实系统取消、70000字节完整导出、同名保留及首次写入前授权回调失败清理已通过；API36本地提供程序的部分接收后源异常／取消／25MiB超限清理及恢复导出已通过，完整25MiB内容独立核验正确；原生以有上限RAM缓冲接收、完整后才复制到目标，增加内存和最后复制开销；API36受控提供程序的最终复制write失败／持续delete失败与重试、原生同步close异常及已报告可靠描述符终态错误已通过；删除失败明确返回export_cleanup_failed并保留原票据，清理成功前不打开新选择器／源；API36已确认票据的Activity同PID／进程强停／SIGKILL／仅RAM写入后终止的持久清理恢复，以及同Activity新导出对象重试已通过，失败清理前不打开新源，完成文件重启不误删；Android已按用户批准改为先选目录、应用创建带唯一操作前缀的文件；API36已通过创建文件后至票据ACK前的Activity重建／force-stop／SIGKILL恢复及加载中目录保护，兼容旧版v1票据；最低API24的Android7.0 ARM64模拟器兼容性与创建前后销毁恢复已通过，原生31项状态／目录和8项资源契约、13项导出及活动／Keystore均已验证，修复旧AtomicFile侧文件清理和48MiB堆下25MiB缓冲OOM；云盘关闭后异步提交、断电、真机与发行签名仍待验证（OS重启专项见下文）；iOS已接入先选文件夹、再授权写入的导出桥，同名文件拒绝覆盖，真实文件系统与原生参数校验已通过，iOS27本地文件提供程序的真实取消／完整导出／同名保护／授权回调失败清理已验收，iCloud及真机仍待验收；移动冷／热回调、真实挂起／恢复准入、通知／附件／回执完整链路、Android／iOS真机和正式签名／公证／客户生产验收仍未完成。移动复现条件见仓库docs/P5-02-移动端验证操作.md。

最低版本专项证据：仓库docs/verification/p5-02-task8-android-api24-results.json。保持min24／compile36／target36、固定Flutter与依赖；真实API24物理设备、其它ABI／OEM、断电、云盘异步提交、正式发行签名及移动后端完整链路仍未验收。

系统重启专项（2026-10-16）：已按批准实现原目录精确匹配重新授权和持久授权释放收据。API24／API36各71项原生检查、共享242项、分析零问题及普通Release构建通过。5个恢复场景执行12次真实系统重启；两平台另各1次快速重启，均实际观察到已释放授权回现，应用启动后再释放，完成文件不变；各1次后续系统重启确认收据核销，共16次合格重启转换。收据最多32个目录，只保存URI／本次新增权限位／最近释放尝试的启动代次；同代次保留，后续代次确认权限消失后核销。启动核对不删除文件，未完成journal所用权限受到保护。此结论限本地受控提供程序、API24／API36 ARM64模拟器，不能外推到断电／任意云盘／真机。旧版本已丢失所有权记录的授权不盲目释放，应用启动前也不保证系统已清除授权。当前结果见仓库docs/P5-02-Android系统重启恢复验证.md及docs/verification/p5-02-task8-android-release-receipts-results.json；此前失败证据保留。
