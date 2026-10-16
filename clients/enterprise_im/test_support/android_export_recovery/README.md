# Android 清理票据恢复验证

仅用于全新、自有的 API36 AOSP ARM64 模拟器和合成文件。普通产品包不包含本目录 instrumentation 或独立故障 DocumentsProvider。

## 真实生命周期用例

先按 `../android_export_provider/README.md` 构建、安装独立提供程序。在固定 Flutter SDK、私有 HOME／ADB 和锁定依赖环境内编译：

```text
flutter pub get --offline --enforce-lockfile
flutter build apk --debug --no-pub --target=integration_test/android_export_recovery_test.dart --dart-define=IM_ANDROID_EXPORT_PROBE_ID=<本轮32hex>
```

安装后持续使用同一 APK／签名；实际 force-stop 或 SIGKILL 不卸载／清数据，不重建 APK。测试启动日志会给出 `control` 前缀；本轮为应用 code_cache 下 `im-recovery-<32hex>`。主机只写测试阶段文件 `<control>-stage`，不写 `no_backup/export-cleanup.json`，不注入目标 URI 或系统回调。

| 阶段 | 操作及实际断言 |
|---|---|
| prepare | 真实系统选择器进入 IM Export Faults，Save 后写入4096字节并EIO，delete仍受阻；新 FileExport 实例也必须返回 cleanup_failed、源零次 |
| prepare-activity | 与prepare相同，写出阶段结果后等待 close-activity；通过公开 SystemNavigator.pop 请求真实 Activity.finish |
| prepare-active | 真实选择后取得票据；正常active再次begin必须 export_busy；两次20000字节原生write ACK，目标仍为空，随后等待主机强杀 |
| blocked | 销毁后启动同一APK，新FileExport必须在选择器／源打开前返回cleanup_failed，源零次 |
| recover | 仅解除已独立观察的所属 UUID.blocked 夹具门禁；新导出先清理旧目标，再通过真实选择器写出精确7/11/13 |
| prepare-same | prepare后等待recover-same；解除门禁后同一Activity、同一PID用新FileExport先清理旧目标，再导出7/11/13 |
| idle | 不进行导出，用于成功文件重启保护检查 |

Activity测试必须等待 Android `wm_on_destroy_called`，确认旧 Activity 已销毁而 native PID 未变，再写 blocked 并启动替换 Activity。进程测试分别用所属包 force-stop，或先检查 `/proc/<pid>/cmdline` 等于应用包名，再由 run-as SIGKILL；确认旧 PID 消失、新启动 PID 不同。

每一阶段读取对应 `<control>-<阶段>-result.json`（prepare-same后另有recover-same结果），核对status、实际PID和源次数。prepare-active直接调用真实原生桥，结果中的source_calls=1为该阶段计数；业务源回调零次的结论来自blocked阶段。不能只依据CLI退出0。

主机独立检查提供程序文件实际长度／哈希、事件顺序、4字节sentinel、票据PENDING及persisted权限；恢复应先删旧UUID再创建新UUID。成功后再次终止并cold-launch idle，完成文件哈希必须不变，票据及本次持久授权必须已释放。为隔离后续同名试验，可在归档保护证据后重置合成成功文件；主机重置不算应用清理成功。

## 设备 instrumentation

`build.py` 需要固定 SDK36、JDK21、已编译的生产 ExportJournal／ExportLedger classes目录和目标APK的临时Debug keystore。生产类只作为编译classpath，不能进入测试dex；运行时从已安装目标APK加载。先核验目标APK哈希与sourceDir，期间不要让其它driver替换目标APK。

```text
python3 test_support/android_export_recovery/build.py --sdk <SDK> --java-home <JDK> --target-classes <生产classes> --debug-keystore <所属Debug-key> --out <独立输出>
adb -s <所属serial> install <输出>/recovery-tests.apk
adb -s <所属serial> shell am instrument -w -e owner <本轮32hex> com.enterpriseim.exportrecoverytests/com.enterpriseim.client.tests.RecoveryInstrumentation
```

必须观察 `RECOVERY_ALL_PASSED 15`，而不是只看ADB退出0；失败会带failed及堆栈。检查实际Android AtomicFile、真实文件副作用，资源边界注入删除／授权失败；不宣称15个真实云盘故障。测试目录成功后自行删除。

范围从原生票据ACK后开始。选择器创建目标到回调／ACK之前的销毁、断电／OS重启、真实最低API24、物理设备及云盘异步提交未覆盖。票据恢复在下一次导出前重试，不自动重放业务正文；无法确认目标不存在时保留票据并阻止新导出。
