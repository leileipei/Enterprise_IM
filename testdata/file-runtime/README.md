# 文件运行依赖夹具

仅用于P4-22专用本地联调；MinIO为固定历史构建，不作为生产发行版可用性证明。容器和bucket必须独立，凭据写本机0600文件，不提交、不输出。

compose.yaml固定镜像digest并使用随机localhost端口；启动后由夹具管理员创建专用私有bucket、启用versioning。产品适配器不创建bucket、不修改ACL或策略。

环境：IM_TEST_DATABASE_URL、IM_TEST_S3_ENDPOINT、IM_TEST_S3_BUCKET、IM_FILE_S3_ACCESS_KEY、IM_FILE_S3_SECRET_KEY。运行 `scripts/test-file-runtime.sh run-s3`；缺环境非零退出。测试验证匿名GET拒绝、私有ACL/策略、versioning和旧固定版本回读。无环境单独go test的Skip不计组件验收。

CredentialSource首版仅接受environment，并读取IM_FILE_S3_ACCESS_KEY/SECRET_KEY；HTTPS或loopback HTTP，禁重定向。运行需要PutObject/GetObjectVersion/ListBucketVersions及只读GetBucketVersioning/GetBucketAcl/GetBucketPolicy预检权限；Worker省略PutObject。ETag不作为SHA证据。

官方依据：[AWS重试](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-retries-timeouts.html)、[GetObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html)、[ListObjectVersions](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectVersions.html)。PUT显式NopRetryer，精确versionId读取并完整实测回验。

ClamAV/qpdf配置和运行来源证明由Task 8补全；当前占位项不能启用扫描。生产容量、客户IDP、DR及发行许可另行验收。
