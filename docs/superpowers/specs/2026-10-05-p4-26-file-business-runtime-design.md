# P4-26 正式服务装配与附件进程验收设计

日期：2026-10-05。基线：[P4-25 草稿 PR #73](https://github.com/leileipei/Enterprise_IM/pull/73)，交付提交 `6cee53c03e7e83d83690a7788c45c8a52ff13bf2`。设计分支：`codex/p4-26-file-runtime-design`。

状态：用户于2026-10-05确认本书面规格（确认时提交d0d6d5157e225d03663fa3c2c654b4109f9e8d02），已形成[逐项实施计划](../plans/2026-10-05-p4-26-file-business-runtime.md)，计划待审阅。以下新增行为均为实施要求；尚未实现产品代码、安装依赖、执行迁移、启动服务或部署。

## 1 项目意图与完成标准

让已获准的集团员工使用正式构建的 im-api 和独立 Worker，完成上传、扫描、单聊／群聊附件发送、名称搜索及授权下载。运维明确选择业务启用状态，启动配置错误显式失败；关闭上传后已有合格附件仍按原权限下载。

P4-25 已验证领域服务、HTTP 合约和真实浏览器，但完整 Web 文件夹具使用测试专用 Handler 装配。当前正式 im-api 的附件发送、下载与文件名搜索关闭；上传和扫描 Worker 已有独立开关。本阶段补正式进程的装配与运行证据，沿用原文件生命周期及授权模型。

v2.2 原始 F01 条目为“无权限用户持有文件链接”，预期“下载拒绝并记审计”。本阶段必须在真实正式 API 进程复现，核对有效登录但无文件参与资格的拒绝审计。登录失败与文件权限拒绝分开计数，不能把只返回401当成F01通过。附件功能矩阵另列成功、故障、撤权、保全及恢复证据。

成功标准：四种开关组合一致；请求只进入真实正式服务；能力与路由相符；对象及不可变证据匹配；未知结果保留原幂等编号；故障不输出未授权内容或伪造成功；原文字、搜索和实时消息回归通过。结果仅认定本阶段本地进程验收，客户联调、M4、HA／容量／备份DR及生产放行另验。

## 2 范围与方案选择

采用一个默认关闭的附件业务总开关，一次控制附件发送、授权下载及文件名搜索。上传保持原开关；扫描、Outbox、审计修复与删除保持独立进程，业务开关不启动任何 Worker。

| 方案 | 取舍 |
| --- | --- |
| 采用统一附件业务开关 | 三项能力与装配一致，配置组合少；首版不能独立关闭其中一项 |
| 三项独立业务开关 | 可分别启用，但扩大路由、依赖和恢复组合，暂不采用 |
| 延续测试装配或改做身份源 | 无法补齐本阶段正式进程证据；LDAP／AD另行设计 |

新增范围：API配置及生命周期协调、独立下载凭据与稳定节点归属、只读存储探针、严格路由装配、文件就绪检查、数据库审计修复专用模式、真实多进程门禁及运维说明。

保持：迁移000001～000021、授权／保全／动态保留语义、PDF／PNG／JPEG／UTF-8 TXT、最大25MiB、绑定唯一性、ACK／seq／Outbox原子性、原下载及查询期限、候选预算、中文及FEFF名称契约。无新迁移、名称副本、索引、预签名链接、外链、预览、OCR、Range、移动端或自动对象删除。

## 3 当前代码与衔接点

| 当前代码 | 实施衔接 |
| --- | --- |
| productionFileCapabilities仅返回UploadEnabled | 从全部成功装配的不可变配置产生四布尔能力 |
| HandlerWithConversations传入nil文件服务 | 开启业务时使用现有HandlerWithFileMessages，保持文字与群管理 |
| productionFileDownloadHandler无条件关闭下载 | 按开关安装真实服务或关闭Handler，协调PUT方法分派 |
| productionFileSearchHandler关闭搜索 | 按开关安装已有单／跨会话名称搜索 |
| filedownload.NewService验证目录、owner和锁 | 配置专属目录、稳定owner，按原完整来源检查恢复 |
| NewS3读取上传凭据 | 增加只读构造入口，禁止回退至上传／清理凭据 |
| 基础ready检查数据库，实时包装器检查Redis／Outbox | 合并文件运行检查，保持原总预算 |
| im-file-cleaner同循环修复审计并删除 | 增加repair-only模式，不构造删除适配器 |
| Web文件夹具在_test.go补挂Handler | 新门禁编译、启动正式可执行文件，不补挂业务Handler |

对应文件包括cmd/im-api/main.go、internal/httpserver/conversations.go、file_download.go、file_search.go、file_capabilities.go、server.go、internal/objectstore、internal/filedownload、cmd/im-file-cleaner及真实进程测试。扫描与Outbox Worker沿用现有正式实现。

## 4 配置与能力

新增`IM_FILE_BUSINESS_ENABLED`，只接受空、false、true；空及false为关闭，其他值启动失败。启动时冻结，修改后需重启，无热切换。U代表原`IM_FILE_UPLOAD_ENABLED`，B代表新业务开关。

| U | B | upload_enabled | message_send_enabled | download_enabled | filename_search_enabled |
| --- | --- | --- | --- | --- | --- |
| false | false | false | false | false | false |
| true | false | true | false | false | false |
| false | true | false | true | true | true |
| true | true | true | true | true | true |

能力表示服务器装配，租户策略、身份、任职、会话、扫描及期限仍逐次复核。B=true/U=false可以下载及搜索已有合格附件；附件绑定仍受原租户上传策略等规则限制，不绕过租户禁用策略。

能力接口仅在既有OIDC装配启用时提供，沿用四字段、认证、有效任职、no-store／nosniff及严格请求。仅在全部Handler装配成功后产生能力；初始化失败不监听。临时依赖故障不把能力改成false，能力不是逐文件许可。

`typed_v1.download_available`保持false，Web沿用全局能力与卡片available的判断，每次点击仍执行真实授权GET。租户上传策略／历史及本人文件状态在U或B开启时装配；预约和PUT只由U控制。两者关闭保持原未启用行为。

| B=true必需配置 | 校验及用途 |
| --- | --- |
| 原OIDC配置、IM_DATABASE_URL | 验签、生产身份映射及数据库操作 |
| IM_FILE_S3_ENDPOINT／REGION／BUCKET／PATH_STYLE | 与上传／扫描使用同一私有文件存储，沿原端点及布尔校验 |
| IM_FILE_DOWNLOAD_S3_ACCESS_KEY／SECRET_KEY | 下载独立凭据，无回退／共享默认值 |
| IM_FILE_DOWNLOAD_SPOOL_DIR | 专属绝对目录，0700、当前用户所有，内容文件0600，拒绝符号链接及未知来源 |
| IM_FILE_DOWNLOAD_OWNER_ID | 非零、小写规范UUID；同节点及目录重启保持一致，各节点分别配置 |
| IM_FILE_READ_PROBE_VERSION_ID | 预先准备探针的固定版本，非空、非null，沿原版本长度及字符约束 |

凭据来源由构造入口固定：上传／扫描沿用environment，下载使用download_environment，删除沿用cleanup_environment；浏览器及任意环境值不能切换角色。下载访问键与本进程已配置的上传或清理访问键相同则拒绝启动，错误不输出键值。

B=false不解析或使用新增下载配置，不建下载目录／客户端、不发读取探针，仍严格校验B布尔值。U=true原必需配置保持。B=true/U=false不要求上传凭据、上传目录、qpdf或ClamAV配置，API不创建扫描器。

## 5 存储角色与读取探针

上传、扫描、下载、删除采用四个独立IAM主体。下载只允许目标私有桶的原元数据检查和固定版本读取，不授予PUT、DeleteObject、DeleteObjectVersion或业务版本枚举。匿名和跨桶拒绝保持。成功启动不单独证明最小权限，部署验收必须实际验证各主体允许及拒绝动作。

新增`objectstore.NewS3ReadOnly`，返回与既有Store兼容的适配器：PutVersion及FindAttemptVersions明确拒绝且零网络请求；ReadVersion沿用固定版本／完整性契约并使用独立凭据。下载Repository及授权规则保持。

探针对象键固定为`_im_runtime/read-probe/v1`，内容为UTF-8的`enterprise-im-file-read-probe-v1`后跟一个LF。管理员在测试／部署准备时用独立引导权限创建并记录VersionID。API和健康检查绝不创建、覆盖或删除对象；探针无业务元数据，不经文件API或搜索返回。

启动检查桶版本启用、ACL／策略私有、探针指定版本实际可读；响应版本、完整字节和固定内容必须匹配，最多读取1KiB。缺失、错版、null、访问拒绝、截断、额外内容、重定向和超时均失败。探针key／VersionID不进入普通日志或API响应。

引导权限不授予业务主体。存储发行版、IAM及许可仍须部署环境核验，固定测试版本结果不推断其他发行版可用。

## 6 启动检查与装配

全部文件初始化在监听前完成。B=true共享至多30秒初始化上下文，U／B同时开启也不逐项刷新预算；原上传专属检查仍不超过15秒，文件Worker原90秒校验保持。

顺序：严格配置→数据库契约及权限→私有版本化存储／只读探针→稳定owner及目录锁／来源回收→下载及需要的上传服务→完整业务路由→能力、实时及Web→监听。失败释放已创建的服务、锁和连接，非零退出，不部分开放三项业务。

数据库检查覆盖文件来源、上传策略、扫描作业、附件绑定、下载会话／终态、审计、保留配置及保全关系。依据已提交000018～000021和实际业务SQL，检查实际列类型、必要外键／CHECK有效状态、关键来源／证据／保全触发器正常会话启用、终态成对约束可延迟。关键外键／CHECK关联定义和触发器实际函数关联应匹配本基线；不能仅凭表名或约束名称存在通过。缺业务写入或审计权限也拒绝。启动不执行DDL或修复数据。

日志只用invalid_file_business_configuration、file_schema_unavailable、file_read_probe_unavailable、file_spool_unavailable等固定类别，不附原始异常、凭据、连接地址或目录内容。

API不要求扫描Worker在线才能读取原ready对象，不要求Redis才能持久化附件消息；若启用实时，原Redis／Outbox就绪条件继续生效。

## 7 路由顺序与HTTP契约

B=true只装配一次HandlerWithFileMessages，覆盖文本／附件、单聊／群聊及群管理；B=false沿HandlerWithConversations。禁止两个通用会话包装器抢先拒绝附件。

能力及三个名称搜索精确路径优先于通用files/{id}、groups/{id}。同一content路径按实际开关分派：

| U | B | GET | PUT | 其他方法Allow |
| --- | --- | --- | --- | --- |
| false | false | 原关闭503 | 405 | GET，沿原关闭契约 |
| true | false | 原关闭503 | 真实上传 | GET, PUT |
| false | true | 真实下载 | 405 | GET |
| true | true | 真实下载 | 真实上传 | GET, PUT |

真实GET／PUT保持原严格参数及身份校验。B=true/U=false时不得写死Allow: PUT或把PUT送入错误分支。关闭GET保持原503，发送及名称搜索保持原关闭错误；文字和群管理正常。开启后的错误沿原业务映射。

包装器保持ResponseController截止时间及Flush可达，不压缩／缓存二进制。普通API的10秒WriteTimeout、名称搜索5秒总／4秒statement／1秒lock、500候选／20会话、下载60秒及在途复核窗口保持。

## 8 就绪与运行故障

B=true扩展API就绪，合并原数据库及已启用实时检查，整个请求共享原2秒总上下文。新增检查为私有版本化桶／只读探针、下载服务未关闭或因清理错误失效、所持目录及manifest仍合法。就绪不修复审计、不扫描客户文件、不创建下载会话、不枚举或删除业务对象。

失败ready返回原503／unavailable，全部恢复后返回200；live保持自身存活含义。不增Worker心跳表、后台健康缓存或迁移。探针有界关闭响应体，复用传输层，不产生临时文件。

就绪用于路由与运维，不替代授权，也不保证检查与下一请求间状态不变。能力仍为装配事实；每个请求保持当前身份、策略、来源、审计及完整性检查，故障拒绝或中止，不回退凭据或延长期限。

扫描停机时未ready文件保持原状态，页面在原120秒窗口显示未完成，不伪造clean／已发送。已有ready只按原证据／期限判定；Worker恢复继续原租约及隔离规则。

## 9 目录归属与退出恢复

每个API节点使用独立下载目录及稳定owner。拒绝共用上传／扫描目录、重叠目录、两节点共用目录、同目录不同owner；使用实际路径及inode防符号链接／替换。单进程无法获知全部部署目录，因此部署预检与双进程门禁共同验证。

SIGTERM先停止新连接，原10秒预算等待请求；超时必须关闭剩余HTTP连接、取消请求上下文，再关闭服务及释放锁。filedownload.Close等待活动请求，必须先中止连接。服务关闭另设10秒收尾上限；超时或错误非零退出并保留未完成证据，不声称已回收。禁止Shutdown失败时立即os.Exit而跳过服务关闭。上述退出预算不延长任何业务请求期限。

强制停止后仅回收原来源完整的孤儿文件；未知文件、来源不完整或错误owner使启动失败并保留。重启不能换owner覆盖目录，不能宽泛rm清理。未结算下载由原process_lost／unknown审计恢复，未确认前保持访问门禁。

内核已接受字节、完整Blob及用户已保存副本无法事后召回；本阶段不改变该边界。

## 10 下载审计专用修复模式

新增`im-file-cleaner --execute --repair-only`，允许原--once做一次有界批次；未给--execute仍关闭。未知参数、重复参数和冲突组合严格拒绝。原删除模式及租户cleanup_enabled保持。

repair-only只建数据库连接及运行期机器身份，调用既有RepairFileDownloadAudit；不建S3删除适配器、不要求删除凭据、不调用Step／枚举／删除、不消费清理作业。工厂与参数解析分开两种模式，避免提前构造删除能力。

每批至多20条／5秒上下文；连续模式批次后等1秒，失败按1、2、4、8、16、30秒封顶退避，父取消停止。缺必需结构、证据约束或修复权限启动失败。既有租约、终态及机器审计成对事务保持。

业务开关不自动启动该进程。停止修复不伪audit_acked，同文件未结算访问按原门禁拒绝；实际重启核对后恢复。只修复审计不取得对象删除权限，删除仍由原明确执行模式及租户策略允许。

## 11 真实进程验收矩阵

新门禁从同一不可变源码编译并启动正式im-api、im-file-worker、im-outbox-worker及repair-only命令。至少两个API进程使用独立owner／spool。测试提供TLS／OIDC签发器、schema、私有桶、四IAM主体、ClamAV／qpdf、Redis和Chrome，不补挂业务Handler、不替代Repo、不直接把实际生命周期文件置ready。

Chrome经同源HTTPS代理连接真实API。代理仅终止TLS／路由及注入可观测网络故障，不能代替正式服务产生能力、预约、状态、消息、搜索或下载成功结果。注入故障须与已核验的真实请求处理对应，不能用完全模拟响应宣称正式进程链路通过。记录构建SHA、进程归属、实际数据库／对象／审计证据；不保存token、凭据、名称搜索q、文件内容或完整业务响应。

| 编号 | 必选场景 | 证据要求 |
| --- | --- | --- |
| RP01 | 四组合及非法配置 | 实际能力／路由／方法一致；B关闭零探针／下载目录，文字群管理正常 |
| RP02 | 启动依赖及结构错误 | 缺OIDC／凭据、公开桶／禁版本、探针错版／拒绝、目录／owner／锁错误、缺列／外键／禁关键触发器非零退出且不监听 |
| RP03 | 四类型单群完整链路 | 正式预约→PUT→真实扫描→ACK→真实通知→typed补拉→名称搜索→显式保存；字节及中文／FEFF名称一致 |
| RP04 | F01无权限持链接 | 有效未参与员工、跨租户、无参与权限管理员零内容并有拒绝审计 |
| RP05 | 恶意及无效内容 | 实际EICAR、坏／加密PDF、MIME伪装、超限拒绝；未ready不可发 |
| RP06 | 关闭上传仍可读 | 先形成附件，再重启U=false/B=true；合格GET／搜索正常，预约／PUT拒绝，租户规则保持 |
| RP07 | 在途撤权及期限 | JWT、hard_deny、任职／上传者停用、退群再入gap、TTL／计划策略沿原窗口 |
| RP08 | 正常退出及强制重启 | 活跃下载中SIGTERM／失联，连接中止、锁释放、同owner重启；仅来源完整回收，未知目录保留 |
| RP09 | 审计故障与修复 | 停修复→真实待结算→拒绝→restart／once恢复；机器审计恰一次，repair-only零S3调用／清理作业 |
| RP10 | 双API幂等与补拉 | 原UUID／body跨节点只一绑定／消息／Outbox，duplicate ACK一致；真实Redis通知及离线恢复 |
| RP11 | 正常500及21会话前进 | 有效完整来源／扫描事实，正式API原5秒内有界续查，数据库工作量与扫描量分列 |
| RP12 | 运行故障与恢复 | 实际S3／DB故障ready503、live正常，恢复ready200；业务失败封闭，扫描停机不伪ready |
| RP13 | IAM与目录权限 | 四真实主体允许／拒绝、匿名／跨桶拒绝，读角色PUT／DELETE实际拒绝，同spool竞争拒绝 |
| RP14 | Web上下文／未知操作 | 七阶段取消、原冻结UUID／body核对、ACK／补拉分离、大整数CAS、通知与typed共同等待、四次传输故障 |

RP03／05／10的ready必须来自实际扫描Worker，不能从测试进程直接运行ScanWorker.RunOnce替代正式Worker进程。RP11允许正常服务建立501个有效数据库绑定及扫描事实，另以RP03／05证明实际扫描；不声称扫描了501个对象。旧500损坏候选、Unicode及真实门禁全部保留。

验收记录分列原F01、附件成功矩阵和未验收生产条件。本地签发器不算客户IdP证据，正式证书／代理、组织数据、支持浏览器和存储发行版仍须客户联调。

## 12 验证与交付

新必选门禁为`scripts/test-file-business-runtime.sh run-all`，覆盖RP01～14，缺真实必需依赖不得SKIP。实施计划按失败→实现→通过逐项规定，定向单测不替代最终门禁。

产品SHA固定，git archive创建独立源码，以下七命令同源码运行并记录实际退出、顶层／子例／包计数与测量。原脚本时限保持，TestRealtimeAPIChild辅助入口可单列SKIP，真实调用方和新必选场景须PASS。

~~~sh
go test -json -timeout=30m ./... -count=1
go test -json -race -timeout=30m ./internal/policystore ./internal/access ./internal/httpserver ./internal/filedownload ./internal/filetransfer ./internal/objectstore ./internal/realtime ./internal/outbox ./internal/webclient ./cmd/im-api ./cmd/im-file-worker ./cmd/im-file-cleaner -count=1
scripts/test-web-files.sh run-all
scripts/test-file-download-retention.sh run-all
scripts/test-file-messages.sh run-all
scripts/test-file-runtime.sh run-all
scripts/test-file-business-runtime.sh run-all
~~~

保持旧默认关闭断言，另补开启状态；不减弱P4-25测试专用装配证明。全仓、race及进程重门禁顺序执行，专属数据库／桶／目录隔离。失败／取消尝试保留，未完整退出不计通过。

沿用当前助手逐任务实施，最后一次整体独立评审；Critical／Important经一轮具名失败→通过修正，重新固定并重跑七门禁，不派第二轮reviewer。Minor单独记录；P4-24既有审计分类后续项不混入本轮。

交付中文验收及部署配置／启停／探针／权限说明，专属分支草稿PR的base为P4-25分支，创建后关联任务；主工作区与前项PR保留，不合并／部署。资源按PID／label／路径核验只退出本轮；证据目录0700、文件0600，数据保留与删除分开记录，敏感配置不入库。

## 13 运维顺序与验收边界

先验证迁移、角色权限及私有版本化桶，建立探针，为节点配置稳定owner和专属目录；启动需要的scanner／文件Worker、Outbox及审计修复，最后显式配置API并核对ready。完整联调使用U=true/B=true。

暂停新上传：U=false并重启，B保持true。关闭附件业务：B=false并重启，按退出流程处理中途请求；已有本机副本不撤回。禁止借关开关回滚迁移、删除元数据或审计。

物理删除仍由清理进程及租户cleanup_enabled控制，repair-only独立继续。未决删除承诺与登记保全沿原冲突／对账流程，重启不规避承诺。精确在线版本删除不证明备份／WAL／介质／用户副本清除。

正式可执行文件可被显式装配不构成客户环境启用批准。F01本地进程结果按实测记录，M4、客户联调、生产发行版、安全评审、HA／容量及RPO／RTO仍单独退出验收。

## 14 规格自检与下一步

自检覆盖：四组合与方法一致；B关闭零新依赖，B开启与U独立；探针只读且固定版本；稳定owner与来源回收一致；ready共享2秒且不代替授权；repair-only无删除能力；进程门禁不补挂Handler；原期限／关闭默认值保持；功能与生产放行分别陈述。

本规格已完成文档自检并获用户确认，逐项实施计划已编写。按writing-plans交接规则，计划待用户审阅确认后沿用当前助手逐项实施，不再次选择执行方式。本规格的测试为实施要求，不是已执行结果。
