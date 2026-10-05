# 文件运行依赖夹具

仅用于P4-22专用本地联调；MinIO为固定历史构建，不作为生产发行版可用性证明。容器和bucket必须独立，凭据写本机0600文件，不提交、不输出。

compose.yaml固定镜像digest并使用随机localhost端口；启动后由夹具管理员创建专用私有bucket、启用versioning。产品适配器不创建bucket、不修改ACL或策略。

环境：IM_TEST_DATABASE_URL、IM_TEST_S3_ENDPOINT、IM_TEST_S3_BUCKET、IM_FILE_S3_ACCESS_KEY、IM_FILE_S3_SECRET_KEY。运行 `scripts/test-file-runtime.sh run-s3`；缺环境非零退出。测试验证匿名GET拒绝、私有ACL/策略、versioning和旧固定版本回读。无环境单独go test的Skip不计组件验收。

CredentialSource首版仅接受environment，并读取IM_FILE_S3_ACCESS_KEY/SECRET_KEY；HTTPS或loopback HTTP，禁重定向。运行需要PutObject/GetObjectVersion/ListBucketVersions及只读GetBucketVersioning/GetBucketAcl/GetBucketPolicy预检权限；Worker省略PutObject。ETag不作为SHA证据。

官方依据：[AWS重试](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-retries-timeouts.html)、[GetObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html)、[ListObjectVersions](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectVersions.html)。PUT显式NopRetryer，精确versionId读取并完整实测回验。

qpdf12.4.2/ClamAV1.5.4已用于专用本地真实试验。`run-scan`还要求IM_TEST_QPDF_PATH、IM_TEST_CLAMD_SOCKET和IM_TEST_SCANNER_MANIFEST；缺依赖非零退出，包含必须通过的正向ready验收。

manifest为只读0400、同运行用户拥有的JSON，kind=local-process，包含PID、实际clamd_binary_path及SHA256、config_path及SHA256、clamd_socket_path、qpdf_path及SHA256、definition_directory与main.cvd/daily.cvd/bytecode.cvd的完整SHA256。config同样0400；按锁定配置用单独进程启动，实际argv须为`<clamd路径> --config-file=<配置路径>`。只接受私有Unix socket。运行时核实进程启动早于后续配置变更、数据库先安装后启动以及当前CVD构建版本/时间；不使用文件mtime冒充病毒库构建时间。

原25MiB子文件限制会静默跳过嵌套ZIP末尾测试条目，保留为必须不能ready的负向夹具。用户已批准MaxFileSize=0、MaxScanSize=250MiB、StreamMaxLength=25MiB及相同内部展开限额；修正后的真实正向clean／EICAR与嵌套／展开限额检测已通过。两个进程使用独立socket及manifest，不能混用负向证据与正向验收。详细证据见[限额验证记录](../../docs/P4-22-ClamAV限额验证记录.md)。生产容器镜像/容量、客户IDP、DR及发行许可另行验收。

## 完整门禁

`run-scan`同时要求原配置负向进程IM_TEST_UNPROVEN_CLAMD_SOCKET／IM_TEST_UNPROVEN_SCANNER_MANIFEST。`run-all`另要求IM_TEST_S3_POLICY_BUCKET（与主bucket不同的私有versioned负向策略测试bucket）、IM_TEST_FILE_WORKER_ACCESS_KEY／SECRET_KEY（实际只读角色）、IM_TEST_FILE_RUNTIME_OUTPUT_DIR（私有绝对路径）。JSON回归输出assembly.json和components.json，任何组件SKIP即失败。

完整旧功能回归还需现有IM_TEST_REDIS_URL、IM_TEST_BROWSER_NODE、NODE_PATH及CHROMIUM_EXECUTABLE；这些由本机私有环境夹具提供，不能提交凭据。主bucket始终保持私有；公开策略负向测试仅操作独立负向bucket。

资源门禁按当前Docker实际架构交叉编译当前源，使用versions.lock固定Alpine镜像。`samples/generate_structure.py <私有目录>`流式生成40M像素16位RGBA及40M+1超限PNG；512MiB／1CPU容器验证完整解析和超限拒绝。上传磁盘满使用独立1MiB tmpfs，显式IM_TEST_SPOOL_FULL_DIR=/limited；原主机磁盘不会被填满。大型样本、构建产物和凭据不提交。

真实OIDC发行者仅是专用TLS测试夹具；被测试的是实际cmd/im-api及cmd/im-file-worker可执行进程。实际只读账号写入被拒绝、Worker强制终止后新job接管、正常SIGINT清理均在组件JSON门禁中。命令及结论见[中文验收记录](../../docs/开发增量-P4-22-验收记录.md)。

每个API／Worker须使用不同的本地私有0700目录，且目录归运行UID所有。NewService生命周期持有目录flock；启动取得独占锁后仅回收可证明为本组件创建的孤儿文件。第二个实例不能共享活跃目录。不明链接、权限或尺寸使启动拒绝，正常Close停止新上传并等待已有上传清理。Linux门禁增加真实SIGKILL上传／扫描回收及活跃实例保护场景。

## P4-23 文件消息集成门禁

执行 `scripts/test-file-messages.sh run-all`。除 P4-22 的真实 PG、Redis、versioned 私有 S3、qpdf、正常与限额反例 clamd、浏览器变量外，可设置 `IM_TEST_FILE_MESSAGE_OUTPUT_DIR` 为独立私有绝对目录。脚本记录 Go JSON，要求具名测试实际通过且无 SKIP；依赖缺失、陈旧病毒库或绑定失效均退出非零。

集成夹具使用生产 API 子进程的 TLS OIDC/JWKS 验证和数据库身份映射；显式文件消息 Handler 的测试认证桥仅转发该进程 `/api/v1/me` 返回的已验证身份。上传与扫描使用真实 S3、qpdf、ClamAV；不得手写 ready 代替扫描。旧 Web 使用实际生产 API 和 PKCE 登录，验证固定附件占位、文本发送及重新加载补拉。生产子进程在上传开启或未识别 `IM_FILE_MESSAGE_ENABLED=true` 时仍拒绝文件发送。

测试自建进程、数据库 schema、Redis stream 和浏览器均由夹具清理。外部专用容器/扫描器由本轮操作者清理；S3 对象只写入该轮专用 bucket，销毁该轮无持久卷容器时一起清理。不得删除正式绑定证据或修改正式生命周期。P4-23 不提供下载授权和文件收发 UI，ACK 仅证明入库。

## P4-24 下载与保留期清理门禁

执行 `scripts/test-file-download-retention.sh run-all`，需要本轮私有 PostgreSQL、Redis、版本化 S3、新鲜 ClamAV/qpdf manifest 和真实浏览器运行时。独立角色变量：`IM_TEST_FILE_UPLOAD_ACCESS_KEY/SECRET_KEY`、`IM_TEST_FILE_WORKER_ACCESS_KEY/SECRET_KEY`、`IM_FILE_CLEANUP_S3_ACCESS_KEY/SECRET_KEY`。仅夹具管理使用 `IM_TEST_S3_ADMIN_ACCESS_KEY/SECRET_KEY` 创建自建对象的 delete marker。不得指向客户桶或业务库。

脚本严格检查 16 项具名 PASS，所有所选用例 0 FAIL/0 SKIP；`IM_TEST_FILE_DOWNLOAD_OUTPUT_DIR` 必须绝对私有目录，日志不打印凭据。固定版本 GET、慢 TCP、短 JWT、撤权后零正文、55 秒对象读延迟加 7 秒真实数据库审计等待的 60 秒总截止、审计未知恢复、固定版本 DELETE 的丢响应和迟到发送，以及保全/暂停/额度/IAM 均有独立断言。

下载身份桥只在真实生产 `/api/v1/me` 已校验完整 token 后，从同一已验证 token 提取 `exp`；身份始终来自验证器返回值。此桥只用于显式测试下载装配，生产 GET/content 与文件发送继续关闭，客户端没有下载入口。

清理夹具的过期预约及上传历史由数据库边界构造，始终启用所有触发器；对象 PUT/版本、只读扫描以及 clean/ready 判断都是真实本轮运行。逻辑 TTL 用例仅构造旧消息接受时刻，不宣称经过真实 48 小时；故障用例临时隐藏自建 schema 的配置表，仅证明依赖查询失败时拒绝正文。真实网络字节、版本权限和新鲜扫描与这些历史前提分别核验。文件 ID 随机，避免同一专用桶多轮遗留版本污染来源清单。

下载 writer 接收字节不代表客户端收妥，也不能撤回内核/客户端已有缓冲。终结持久化失败保留原 session 闸门；等待不可变 deadline 真正到期后，机器恢复登记 unknown，不能凭完整 HTTP 响应补成 completed。
