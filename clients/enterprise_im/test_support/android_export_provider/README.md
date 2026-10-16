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

../android_export_session 的 ExportSessionChecks 用真实文件加测试专用 OutputStream 包装器，在写入／close 处抛同步 IOException；同一组6项契约可在宿主JVM与Android instrumentation运行，覆盖描述符关闭、同时write／close异常、delete抛错或返回false、原目标重试及成功复制。

ExportSessionInstrumentation 还运行真实Android可靠管道：接收端读取并独立保存70000字节，然后 closeWithError；测试夹具确保该状态已报告才执行关闭。生产 ProviderOutput.checkError 必须捕获该状态，删除目标且关闭描述符。instrumentation APK链接目标应用的生产类，不能将 ExportSession／ProviderOutput 编入测试dex；build.py需传入其编译classpath、所属Debug keystore和私有输出目录。

```text
adb -s <自有序列号> shell am instrument -w -r -e owner <新32hex> com.enterpriseim.exportsessiontests/com.enterpriseim.client.tests.ExportSessionInstrumentation
```

必须核对结果中的 SESSION_ALL_PASSED 7 和 INSTRUMENTATION_CODE: -1；adb命令退出0本身不代表测试通过。试验后卸载两个所属测试APK并清理所属模拟器／临时根，保留日志。此验证包括受控提供程序及同步故障，未证明任意云盘关闭后的异步提交、远端持久化、进程死亡后恢复或真机表现。

固定SDK的DDS启动若失败，可用同一测试的标准driver入口：

```text
flutter drive --driver=test_driver/integration_test.dart --target=integration_test/android_export_provider_test.dart -d <自有序列号> --no-pub --no-dds --dart-define=IM_ANDROID_EXPORT_PROBE_ID=<新32hex> --dart-define=IM_ANDROID_PROVIDER_CASE=<write|delete|write-delete>
```

必须同时核对CLI退出码、实际用例结果与独立提供程序文件事实。driver输出中的tearDownAll会增加运行器计数，不应计入业务场景数。

## 创建返回前的销毁窗口

夹具还支持 `../android_export_handoff/README.md`：含 `-handoff-` 的合成文件在真实文件及name映射落地后建立 `<UUID>.return-wait`，阻塞createDocument返回（上限120秒）。主机独立确认文件和门禁后才销毁所属Activity／进程，再仅删除已观察到的该门禁，使创建RPC返回；不注入URI、回调或原生票据。delete门禁仍保留，验证清理失败时禁止新选择器／正文，然后解除delete门禁由应用恢复删除。`files/directory-loading` 是显式查询故障夹具，返回带EXTRA_LOADING=true的空MatrixCursor；在成功验证前必须移除此测试开关。
