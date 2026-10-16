# Android 目录创建后、票据确认前的恢复验证

仅用于全新、自有API36 AOSP ARM64模拟器和合成文件。用户已批准先选目录、由应用创建文件。产品使用真实ACTION_OPEN_DOCUMENT_TREE；实际文件为`im-<操作UUID>-<请求名>`，UTF8叶名最多255字节，保留常见扩展名，不重命名、不覆盖原文件。

## 顺序与保护

TREE_PREPARED先记录目录、操作前缀和本次新增授权位；取得持久授权并确认目录可创建、无此前缀后，持久写ALLOCATING，再调用createDocument。唯一匹配结果写成PENDING后才返回票据ACK。下一次导出先恢复旧操作；ALLOCATING零匹配可能是提供程序仍在创建，必须保留并阻断。多匹配、目录类型、身份不符、加载中／报错列表都不能删除。COMMITTED完成文件和DELETED清理收据沿用保护；v1文件URI票据继续可恢复。只释放本次取得的授权位，不持久化正文或自动重放下载。

## 构建与真实操作

使用固定工具链、独立HOME／ADB／AVD及锁定依赖；先安装`../android_export_provider`夹具。编译并全程使用同一测试APK，不卸载／清数据：

```text
flutter pub get --offline --enforce-lockfile
flutter build apk --debug --no-pub --target=integration_test/android_export_handoff_test.dart --dart-define=IM_ANDROID_EXPORT_PROBE_ID=<新32hex>
```

应用code_cache下的`im-handoff-<32hex>-stage`仅控制测试阶段，主机不写原生journal或URI。阶段为choose、observe、observe-loading、idle；结果文件同前缀。

1. choose前设置阶段，通过真实DocumentsUI选择IM Export Faults目录，Use this folder／Allow。独立确认真实空文件、name和return-wait门禁，journal必须是v2 ALLOCATING、uri为空、目录persisted授权存在，尚无choose-returned ACK。
2. 预先写observe，再实际force-stop所属包，或核验/proc/PID/cmdline后run-as SIGKILL；确认旧PID消失。仅移除已观察的return-wait，让提供程序RPC继续，启动同一APK，核对新PID不同。
3. Activity场景用本目录独立instrumentation：Application.ActivityLifecycleCallbacks捕获真实MainActivity，主机touch`cache/im-handoff-<32hex>-recreate`后调用公开Activity.recreate。必须观察同PID内旧Activity销毁、新Activity身份改变；不是am repeat模拟销毁。
4. observe-ready后touch`-assert-return`；blocked-result必须cleanup_failed且source_calls=0，原文件仍在，票据／授权仍在。observe-loading还需先打开夹具directory-loading，再touch`-check-loading`，loading-result同样源0且不丢票据；随后关闭该查询开关。
5. 只解除已观察到的所属UUID.blocked门禁，touch`-recover`，真实选择目录。旧文件应由应用删除，再导出精确7／11／13；不得主机代删后算通过。
6. 写idle，再终止／重启。完成文件长度及哈希不变，journal不存在、本次持久授权释放，sentinel不变。Activity测试touch`cache/im-handoff-<32hex>-finish`结束instrumentation，须有ACTIVITY_HANDOFF_PASSED及实际退出0。

独立生命周期APK由build.py编译，参数为--sdk、--java-home、--target-classes、--debug-keystore、--out；同目标测试签名。运行包为`com.enterpriseim.exporthandofftests/com.enterpriseim.client.tests.HandoffLifecycleInstrumentation`，传`-e owner <32hex>`。测试类不能进入产品dex。

## 本地目录回归与范围

本地系统目录选择应进入内部存储Download下的专用子目录；Android限制不能将Download根直接作为树授权目录。取消时按观察到的实际DocumentsUI逐级Back，直到应用收到取消。界面驱动在Allow后停止访问应用可访问性树，避免框架SemanticsHandle竞态；产品断言及原生错误检查保持开启。

本轮结果见仓库`docs/verification/p5-02-task8-android-directory-handoff-results.json`。仅证明受控API36 ARM64模拟器及本地提供程序；未验证任意云盘、提供程序改写所有权前缀或长期不提供完整查询、断电／OS重启、实际API24、真机、发行签名、CI或后端全链路。未确定归属时保留票据并阻断新导出，不自动清空元数据。
