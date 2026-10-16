# Android 导出故障验证夹具

仅用于新建空模拟器。独立 `com.enterpriseim.exportfixture` APK，禁止安装到用户已有设备或选择业务目录；不会打入客户端 Release。用现有固定 SDK36／JDK21 构建，不下载依赖或接受许可：

```text
python3 test_support/android_export_provider/build.py --sdk <SDK绝对路径> --java-home <JDK绝对路径> --out <私有输出目录>
```

安装 provider.apk 后，在真实系统选择器的侧栏选择 **IM Export Faults**。当前产品先选目录，请在夹具根目录按Use this folder／Allow；文件名增加随机操作前缀。客户端入口为 integration_test/android_export_provider_test.dart，传入新32hex IM_ANDROID_EXPORT_PROBE_ID，IM_ANDROID_PROVIDER_CASE 为 write／delete／write-delete。

- write：ProxyFileDescriptorCallback 将真实文件的前4096字节写入后返回 EIO；要求应用报错、目标被删、后续导出成功。
- delete：源拒绝读取，提供程序持续拒绝删除空文档；要求 export_cleanup_failed、原票据重试仍失败且不打开新选择器／源。
- write-delete：先部分写入失败，再持续删除失败；独立检查4096字节残留与重复删除事件，解除所属 .blocked 门禁后由应用重试删除原文档，再导出。

提供程序文件、UUID映射和事件位于该夹具自己的 files/documents 与 files/events.log，使用该空模拟器的 run-as 独立核验。收到 ANDROID_PROVIDER_CONTROL 后先核验再触碰日志中严格校验归属的客户端 code_cache 标记。release 阶段只删除已观察到的所属 UUID.blocked 门禁；残留文档应由应用删除，不能由主机代删后算成功。成功恢复内容精确为7／11／13；各试验间可归档并重置已核验成功的夹具输出。哨兵31／41／59／26始终不变。

# 同步关闭错误和可靠描述符终态

../android_export_session 的 ExportSessionChecks 用真实文件加测试专用 OutputStream 包装器，在写入／close 处抛同步 IOException；同一组7项契约可在宿主JVM与Android instrumentation运行，覆盖描述符关闭、同时write／close异常、delete抛错或返回false、原目标重试及成功复制；新增25MiB完整内容、输入缓冲复用与完成前不打开目标检查，宿主可用java -Xmx48m复现连续缓冲OOM并验证修正。

ExportSessionInstrumentation 还运行真实Android可靠管道：接收端读取并独立保存70000字节，然后 closeWithError；测试夹具确保该状态已报告才执行关闭。生产 ProviderOutput.checkError 必须捕获该状态，删除目标且关闭描述符。instrumentation APK链接目标应用的生产类，不能将 ExportSession／ProviderOutput 编入测试dex；build.py需传入其编译classpath、所属Debug keystore和私有输出目录。

```text
adb -s <自有序列号> shell am instrument -w -r -e owner <新32hex> com.enterpriseim.exportsessiontests/com.enterpriseim.client.tests.ExportSessionInstrumentation
```

必须核对结果中的 SESSION_ALL_PASSED 8 和 INSTRUMENTATION_CODE: -1；adb命令退出0本身不代表测试通过。试验后卸载两个所属测试APK并清理所属模拟器／临时根，保留日志。此验证包括受控提供程序及同步故障，未证明任意云盘关闭后的异步提交、远端持久化、进程死亡后恢复或真机表现。

固定SDK的DDS启动若失败，可用同一测试的标准driver入口：

```text
flutter drive --driver=test_driver/integration_test.dart --target=integration_test/android_export_provider_test.dart -d <自有序列号> --no-pub --no-dds --dart-define=IM_ANDROID_EXPORT_PROBE_ID=<新32hex> --dart-define=IM_ANDROID_PROVIDER_CASE=<write|delete|write-delete>
```

必须同时核对CLI退出码、实际用例结果与独立提供程序文件事实。driver输出中的tearDownAll会增加运行器计数，不应计入业务场景数。

## 创建返回前的销毁窗口

夹具还支持 `../android_export_handoff/README.md`：含 `-handoff-` 的合成文件在真实文件及name映射落地后建立 `<UUID>.return-wait`，阻塞createDocument返回（上限120秒）。主机独立确认文件和门禁后才销毁所属Activity／进程，再仅删除已观察到的该门禁，使创建RPC返回；不注入URI、回调或原生票据。delete门禁仍保留，验证清理失败时禁止新选择器／正文，然后解除delete门禁由应用恢复删除。`files/directory-loading` 是显式查询故障夹具，返回带EXTRA_LOADING=true的空MatrixCursor；在成功验证前必须移除此测试开关。

## 最低 API24 兼容验证

夹具minSdk24，仍用固定SDK36／JDK21编译。API24使用真实普通ParcelFileDescriptor和关闭监听；API26+才加载独立嵌套类中的ProxyFileDescriptorCallback。名称含-write的代理EIO夹具在API24明确不支持，不能把API36的代理写入故障验收套用到API24。API24的同步write／close故障与可靠管道终态检查来自单独instrumentation，共7项文件／25MiB契约加1项可靠管道，生产类从目标APK加载。

API24真实目录创建前返回阻塞、拒绝删除、加载中查询与RAM ACK后SIGKILL恢复均可使用；先选目录界面的确认文案是SELECT。最终结果及边界见仓库docs/verification/p5-02-task8-android-api24-results.json。

重授权验证新增同一独立提供程序中的IM Export Foreign目录，真实系统选择器可选但产品应因URI与旧票据不一致而拒绝；其中3字节sentinel独立核验不变，不得为错目录保存持久授权。该目录不提供导出创建能力，仅用于真实错选检验。

## 释放收据快速重启观察门禁（2026-10-16）

仅独立测试提供程序增加files/open-wait：write-open记录open_held后最多等待120秒，主机解除该门禁才返回真实文件描述符，记录open_unblocked。API24为普通FD，API36为ProxyFD；超时抛FileNotFound，应用按真实失败路径清理。此门禁用于在实际目录授权已落入OS持久存储时才允许完成复制，再立即系统重启。release后不等待OS持久化；主机不写生产journal、不注入URI／系统回调／授权。产品APK不包含此夹具或门禁。

API36的系统urigrants.xml可能为ABX。测试root只读诊断使用abx2xml INPUT UNIQUE_OUTPUT，将输出写入本轮唯一/data/local/tmp文件、读取后删除；禁止-i和修改输入。该诊断用于核实重现前置条件，不是产品能力或正式权限依赖。085两平台真实观察到授权回现，再由同一040-final产品启动核对释放；091在独立后续重启验证核销。原始超时、ABX误读及宿主失败均保留。精确URI＋targetPkg的全部persisted位已独立复核95份快照，不能只匹配0x3就宣称无残留。
