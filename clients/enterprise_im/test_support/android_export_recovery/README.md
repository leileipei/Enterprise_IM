# Android 清理票据恢复验证

仅用于全新、自有的 API24 Google APIs／API36 AOSP ARM64 模拟器和合成文件。普通产品包不包含本目录 instrumentation 或独立故障 DocumentsProvider。

## 真实生命周期用例

先按 `../android_export_provider/README.md` 构建、安装独立提供程序。在固定 Flutter SDK、私有 HOME／ADB 和锁定依赖环境内编译：

```text
flutter pub get --offline --enforce-lockfile
flutter build apk --debug --no-pub --target=integration_test/android_export_recovery_test.dart --dart-define=IM_ANDROID_EXPORT_PROBE_ID=<本轮32hex>
```

安装后持续使用同一 APK／签名；实际 force-stop 或 SIGKILL 不卸载／清数据，不重建 APK。测试启动日志会给出 `control` 前缀；本轮为应用 code_cache 下 `im-recovery-<32hex>`。主机只写测试阶段文件 `<control>-stage`，不写 `no_backup/export-cleanup.json`，不注入目标 URI 或系统回调。

| 阶段 | 操作及实际断言 |
|---|---|
| prepare | 真实系统选择器进入 IM Export Faults，使用Use this folder／Allow后写入4096字节并EIO，delete仍受阻；新 FileExport 实例也必须返回 cleanup_failed、源零次 |
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

`build.py` 需要固定 SDK36、JDK21、已编译的生产 ExportJournal／ExportLedger／ExportDirectory classes目录和目标APK的临时Debug keystore。生产类只作为编译classpath，不能进入测试dex；运行时从已安装目标APK加载。先核验目标APK哈希与sourceDir，期间不要让其它driver替换目标APK。

```text
python3 test_support/android_export_recovery/build.py --sdk <SDK> --java-home <JDK> --target-classes <生产classes> --debug-keystore <所属Debug-key> --out <独立输出>
adb -s <所属serial> install <输出>/recovery-tests.apk
adb -s <所属serial> shell am instrument -w -e owner <本轮32hex> com.enterpriseim.exportrecoverytests/com.enterpriseim.client.tests.RecoveryInstrumentation
```

必须观察 `RECOVERY_ALL_PASSED 15` 和 `TREE_ALLOCATION_ALL_PASSED 25` 和 `MANAGER_RECOVERY_ALL_PASSED 3`、`RELEASE_RECEIPTS_ALL_PASSED 20`，而不是只看ADB退出0；失败会带failed及堆栈。检查实际Android AtomicFile、真实文件副作用，资源边界注入删除／授权失败；不宣称63个真实云盘故障。测试目录成功后自行删除。

本节旧版生命周期入口从原生票据ACK后开始；当前目录模式的创建后、ACK前验证见 `../android_export_handoff/README.md`。本次最低API24 ARM64模拟器结果见仓库docs/verification/p5-02-task8-android-api24-results.json；该旧检查点未覆盖断电／OS重启、物理设备及云盘异步提交；最新系统重启范围见下文。票据恢复在下一次导出前重试，不自动重放业务正文；无法确认目标不存在时保留票据并阻止新导出。

目录契约使用真实 MatrixCursor、生产 ExportDirectory 解析和真实磁盘文件。16项覆盖：创建后返回丢失、延迟创建、delete返回false／抛错、既有目录权限、前缀碰撞、多匹配、COMMITTED保护、DELETED重试、部分取权失败、不支持创建目录、UTF8文件名、错误URI记录、loading空列表、loading单行部分列表和error结果。查询不完整时文件、票据和授权必须全部保留；不能通过空列表或单行推断不存在／唯一。旧版15项仍读写v1并检查未知版本、损坏及AtomicFile中断。

API24注意：不要在测试中使用API26才引入的File.toPath／java.nio.file.Files；使用流IO。Android7 AtomicFile.delete只处理base和bak，本产品在已确认清理／提交并释放授权后还核验并删除保留的.new侧文件；未知或孤立元数据仍保留并阻断。旧版15项包含此真实AtomicFile中断回归。

## 原目录重新授权与系统重启（2026-10-16）

当前增加9项授权契约：ALLOCATING／PENDING恢复、错目录、旧票据、仅read／仅write取权后失败、原有read权限保护、终态拒绝重新取权及不完整授权offer。额外3项Manager检查在目标APK实际Manager上覆盖普通销毁、取权前关闭及取权期间关闭；后两者保留票据，下一次明确请求可恢复。资源边界受控，磁盘和Android AtomicFile真实。

原目录重新授权检查点在API24／API36各通过15＋25＋3状态与管理契约，以及8项资源契约。真实系统重启测试入口为`integration_test/android_export_reboot_test.dart`，先前已批准仅对原目录URI精确匹配重新授权；取消、错选、Activity重建不读取新正文。原先实际系统重启发现的授权回现缺陷已采用下述收据机制修复；保留原始失败记录。最新实际测试见仓库`docs/P5-02-Android系统重启恢复验证.md`。

## 持久授权释放收据（2026-10-16）

新增20项ReleaseReceiptChecks，API24／API36各合计15＋25＋3＋20＋8＝71项通过。它们使用目标APK的生产类、真实Android AtomicFile及磁盘；授权／启动代次／失败边界可受控注入，不表示71个真实提供程序故障。Instrumentation还在真实目标应用UID下读取BOOT_COUNT，两个API均可读。

生产no_backup/export-grant-releases.json版本1包含entries，每项仅uri、owned、lastReleaseAttemptBoot，不含正文。最多32个不同目录、98304字节，目录标识最多2048 UTF8字节。容量在take和reauthorize前核对，新增owned在take前原子持久记录，保留既有权限位。同一次系统启动即使release后为0仍保留；每次重新release前更新启动代次，在后续启动代次确认owned位无才核销。未知代次不核销；损坏、未知版本、孤立.new或无法持久确认时保留责任并阻断新导出。

应用前台启动和下一次导出前在同一worker核对；启动只处理释放责任，不删除文档，并避开磁盘未完成journal所用目录。COMMITTED文件永不因释放收据删除。旧版本已清空journal且未保存owned收据的遗留授权无法自动认定归属。该机制不是开机广播服务，不能保证用户启动应用前授权已释放。

独立实际系统重启验证在同一040-final产品APK上完成：创建后ACK前、RAM ACK40000、API36实际4096字节EIO；两平台均重现完成后快速重启的授权回现，并验证启动重释放、同代次保留、后代次核销、完成文件哈希不变。证据为仓库docs/verification/p5-02-task8-android-release-receipts-results.json。原子中断等20项资源契约与这些实际系统场景分别计数。
