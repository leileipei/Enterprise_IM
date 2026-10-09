# Enterprise IM 原生客户端

共享 Flutter 客户端开发中。当前已生成 Windows、macOS、Android runner；iOS runner 等待最低系统版本决定。尚未交付四平台安装包。

## 当前功能

中文 HTTPS 服务配置、系统浏览器登录、PKCE、动态本地回调、移动链接订阅、刷新和退出。访问令牌与人员资料只保存在内存。支持严格消息 DTO、64 位序号、同步证明和缓存期限解析；持久化、消息列表、原生设备送达及已读仍按后续计划实施。

## 固定工具链

Flutter 3.47.3、Dart 3.13.3，精确源码和插件归档摘要记录在 `../../docs/verification/p5-02-toolchain-lock.json`。保留 `pubspec.lock`，禁止静默升级。SDK、pub cache 与平台 SDK 位于工程外。

在本目录执行 `flutter test --timeout 30s`。固定 Flutter 的分析器在包含中文的目录名中会产生 LSP 字节长度错误；静态分析使用指向本目录同一源码的英文符号链接，执行 `flutter analyze --fatal-infos <英文链接绝对路径>`，不修改 SDK 或复制源码。

## 验证边界

共享测试和实际 TCP 本地回调测试已通过。macOS 实际构建退出69，需用户本人完成 Xcode/Apple SDK 许可；iOS13 基线与固定引擎最低15冲突，等待决定。Windows、Android 构建、安全存储插件写读删、真机、签名和安装验收尚未执行。共享测试不能替代这些平台门禁。
