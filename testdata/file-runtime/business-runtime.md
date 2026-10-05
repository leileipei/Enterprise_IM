# P4-26 正式附件进程夹具

本门禁只启动同一源码构建的 im-api、im-file-worker、im-outbox-worker、im-file-cleaner。每次 Go 测试调用构建一次，记录四个二进制的 SHA256。TLS 同源代理只转发正式服务的响应；对象 HTTP 代理和 PostgreSQL TCP 中继仅转发实际协议并注入网络中断，不提供业务成功响应。

## 专属资源

必须新建专用本地 PostgreSQL 数据库 `enterprise_im_files`，连接地址为 localhost／127.0.0.1；测试按本轮唯一 schema 隔离并创建临时 API／repair 角色。使用新建、私有且已开启版本控制的 `p426-` 前缀两个测试桶，两者不得相同。不要指向客户库或客户桶。资源创建后登记实际容器 ID／标签、PID 及目录，仅清理本轮登记资源。

四个 IAM 主体分别负责上传、扫描、下载、清理；访问键不得相同。独立引导主体在测试前创建固定探针 `_im_runtime/read-probe/v1`，内容为 `enterprise-im-file-read-probe-v1` 加一个 LF，返回的不可变 VersionID 供两 API 使用。下载主体只允许固定版本读取及桶元数据检查，不允许 PUT／DELETE／版本枚举；API 不自行创建探针。精确版本清理与无版本删除分别实测，不能用适配器在本地拒绝替代 IAM 拒绝证明。

本轮固定历史 MinIO 的删除认证先检查 DeleteObject，再检查版本删除拒绝规则。清理策略需采用该版本实际支持的 `s3:versionid` 条件键，仅允许存在、非空且非 null 的版本；无版本删除仍须真实拒绝。[锁定认证实现](https://github.com/minio/minio/blob/RELEASE.2024-11-07T00-52-20Z/cmd/auth-handler.go#L431)。生产供应商的 IAM 策略需在客户环境另验。

## 必需变量与执行

沿用 README.md 的真实 PG、Redis、私有版本桶、新鲜 qpdf／ClamAV manifest、Node／Chrome 变量；增加 `IM_TEST_FILE_DOWNLOAD_ACCESS_KEY/SECRET_KEY` 和 `IM_TEST_FILE_BOOTSTRAP_ACCESS_KEY/SECRET_KEY`。扫描 Worker 使用扫描角色，API 只收到当前开关需要的正式变量，repair-only 子进程只收到数据库变量；子进程不继承测试管理凭据。

执行 `scripts/test-file-business-runtime.sh run-all`。输入缺失／归属或权限错误退出2；用例失败、缺少、SKIP或 Go 非零退出均失败。`IM_TEST_FILE_BUSINESS_OUTPUT_DIR` 可指定新建的绝对私有0700目录，含链接的路径拒绝；证据文件0600，已有日志不覆盖。未指定时创建独立目录。直接从 Git 执行要求干净的固定源码；固定 SHA 导出目录中运行时显式设置 `IM_TEST_FILE_BUSINESS_BUILD_SHA`。

最终门禁要求 RP01～RP14 全部实际 PASS，0FAIL／0SKIP。Task9 的四项通过不代表完整十四项门禁已通过。源码和二进制哈希、节点稳定 owner、正式进程日志分别记录；不记录 token、凭据、文件名搜索 q、正文或完整业务响应。真实扫描与 RP11 的数据库候选边界夹具分别记载，不把数据库构造的501项称为501次对象扫描。

本夹具用于本地正式进程验收；不代表客户 IdP、M4、生产发行版、安全评审、HA／容量、备份DR或生产放行通过。
