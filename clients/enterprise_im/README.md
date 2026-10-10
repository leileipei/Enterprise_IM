# Enterprise IM 原生客户端

共享Flutter客户端开发中，已生成Windows、macOS、Android runner；iOS runner等待最低系统版本决定，尚未交付四平台验收安装包。

## 当前功能与证据

已实现中文服务配置／原生OIDC登录、任职与设备准入、加密SQLite与原请求恢复、消息／送达／精确已读、通讯录／跨组织群操作、本人设备管理、附件扫描状态及私有下载／导出。共享逻辑和正式HTTPS／PostgreSQL／Redis后端已分别验证；对应证据与范围见仓库`docs/verification/p5-02-task5-real-messaging-results.json`及`p5-02-task6-real-files-results.json`。这些记录不代表已安装的四平台系统插件或窗口事件验收。

桌面当前会话通知须用户显式启用，仅在进程运行且前台授权页面提示“收到新消息”；拒绝通知不阻止消息，不支持应用终止后的推送或冷启动通知导航。Windows启动只允许无参数，令牌、URL、文件路径和未知参数均在Flutter引擎初始化前被拒绝；OAuth仍使用本机随机端口回调。

## 固定工具链

Flutter3.47.3、Dart3.13.3，源码固定为`e8113bf45620cbeb8aff64947ee4c93e16adb4cf`，记录在仓库`docs/verification/p5-02-toolchain-lock.json`。保留pubspec.lock并使用`flutter pub get --enforce-lockfile`，禁止静默升级。SDK、pub cache和平台SDK位于工程外。Xcode许可／工具可用状态改变后需要重新生成插件清单；已有缓存可执行`flutter pub get --offline --enforce-lockfile`。

本目录执行`flutter test --no-pub`。固定Flutter分析器在包含中文目录名时曾出现LSP字节长度错误；可创建指向同一目录的英文符号链接，再执行`flutter analyze --fatal-infos <英文链接绝对路径>`，不修改SDK或复制源码。

## 桌面门禁

Windows启动防护的宿主C++测试编译真实wWinMain，但Win32／Flutter SDK边界为夹具；不能替代Windows真实`.exe`运行。原生启动门禁、两进程安全存储及桌面回调测试的命令和边界见仓库`docs/P5-02-桌面验证操作.md`。`.github/workflows/client-desktop.yml`仅提供手动触发的构建／专项验证准备，不把工作流文件存在当作PASS。

## 当前未完成项

Xcode许可已经解除。macOS原生编译检查通过，但仅为命令级禁用签名的编译产物，链接器隐式ad-hoc签名不代表开发证书签名；本机开发证书数量仍为0，真实签名／安装、OS通知／文件选择／窗口生命周期及后端验收尚未完成。Windows原生构建／运行及CI未执行，Windows10基线仍需实际验证。iOS13与固定引擎最低15冲突等待决定；Android／iOS真机和正式签名／公证／客户生产验收均未完成。
