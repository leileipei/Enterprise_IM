# 文件运行依赖夹具

仅用于P4-22专用本地联调；MinIO为固定历史构建，不作为生产发行版可用性证明。容器和bucket必须独立，凭据写本机0600文件，不提交、不输出。

compose.yaml固定镜像digest并使用随机localhost端口；启动后由夹具管理员创建专用私有bucket、启用versioning。产品适配器不创建bucket、不修改ACL或策略。

环境：IM_TEST_DATABASE_URL、IM_TEST_S3_ENDPOINT、IM_TEST_S3_BUCKET、IM_FILE_S3_ACCESS_KEY、IM_FILE_S3_SECRET_KEY。运行 `scripts/test-file-runtime.sh run-s3`；缺环境非零退出。测试验证匿名GET拒绝、私有ACL/策略、versioning和旧固定版本回读。无环境单独go test的Skip不计组件验收。

CredentialSource首版仅接受environment，并读取IM_FILE_S3_ACCESS_KEY/SECRET_KEY；HTTPS或loopback HTTP，禁重定向。运行需要PutObject/GetObjectVersion/ListBucketVersions及只读GetBucketVersioning/GetBucketAcl/GetBucketPolicy预检权限；Worker省略PutObject。ETag不作为SHA证据。

官方依据：[AWS重试](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-retries-timeouts.html)、[GetObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html)、[ListObjectVersions](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectVersions.html)。PUT显式NopRetryer，精确versionId读取并完整实测回验。

qpdf12.4.2/ClamAV1.5.4已用于专用本地真实试验。`run-scan`还要求IM_TEST_QPDF_PATH、IM_TEST_CLAMD_SOCKET和IM_TEST_SCANNER_MANIFEST；缺依赖非零退出，包含必须通过的正向ready验收。

manifest为只读0400、同运行用户拥有的JSON，kind=local-process，包含PID、实际clamd_binary_path及SHA256、config_path及SHA256、clamd_socket_path、qpdf_path及SHA256、definition_directory与main.cvd/daily.cvd/bytecode.cvd的完整SHA256。config同样0400；按锁定配置用单独进程启动，实际argv须为`<clamd路径> --config-file=<配置路径>`。只接受私有Unix socket。运行时核实进程启动早于后续配置变更、数据库先安装后启动以及当前CVD构建版本/时间；不使用文件mtime冒充病毒库构建时间。

目前原25MiB子文件限制会静默跳过嵌套ZIP末尾测试条目，正向run-scan必须失败。禁止将负向失败关闭通过视为组件可用。候选配置尚待确认，不能启用扫描。详细证据见[限额验证记录](../../docs/P4-22-ClamAV限额验证记录.md)。生产容器镜像/容量、客户IDP、DR及发行许可另行验收。
