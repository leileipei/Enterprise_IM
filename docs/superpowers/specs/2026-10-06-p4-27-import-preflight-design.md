# P4-27 离线组织与身份数据预检设计

日期：2026-10-06。状态：设计规格，待书面审阅；产品代码及实施计划尚未开始。

基线：main `80424dbd5a537023c33e56654f4a12b522885a21`。用户已选择首个身份源暂未确定，并确认先实现 JSON 命令行预检。执行方式沿用当前助手逐项实施。

## 1. 目标与范围

为集团组织和身份接入提供可重复的离线预检：输入结构化 JSON，发现文件内部的结构、标识、关系和区间冲突，输出可定位且不回显人员内容的报告。业务人员可以先核对脱敏样本，后续再对接真实主数据来源。

本增量提供一个独立 Go 命令和内部预检模块，不连接数据库、Redis、身份源或对象存储。只读取显式指定的本地文件，并将报告写至标准输出。数据库存量比较、执行导入、人员更新／停用、身份重绑定、管理员授权变更、SQL／CSV 输入和自动同步在后续增量设计。

预检通过只说明文件符合本规格，不能证明与目标库无冲突、身份平台兼容、实际人员映射正确或操作获得授权。报告始终声明这些边界，不能作为写入许可。

## 2. 方案选择

| 方案 | 优点 | 本阶段约束 |
| --- | --- | --- |
| 离线 JSON 命令行预检，已选 | 可直接使用已验证的虚构样本；不依赖客户环境 | 只判断文件内部关系 |
| 后台上传与批次导入 | 可供业务人员在浏览器操作 | 需要独立身份授权、批次保存和写入审批流程 |
| 身份源实时同步 | 能持续应用外部变更 | 身份源、稳定外部主键及生命周期语义尚未确定 |

后续适配器可以生成同一 JSON 数据契约。首版不会根据姓名、邮箱或相似工号猜测用户关系。

## 3. 命令与运行契约

命令目录：`cmd/im-import-preflight`。内部模块：`internal/importpreflight`，按解析、结构、关系／区间、报告四项职责拆分文件。

```sh
im-import-preflight --input /absolute/path/sample_data.json
```

仅支持 `--input`、`--help`、`--version`。正常预检只输出一份 JSON 报告；帮助和版本是独立模式，不能与 `--input` 混用。参数缺失、重复、未知或混用返回固定用法提示与退出码 2，不回显参数值。

输入必须是普通本地文件；拒绝符号链接、目录、FIFO、设备及套接字。读取前后检查文件类型，打开过程须防止路径替换造成阻塞或跟随符号链接。平台实现允许 Linux／macOS，其他平台若不能提供同等打开保证，明确拒绝运行，不降级为不受保护的打开。首版不接受标准输入、网络 URL 或自动发现文件。

预检执行的固定上限：

| 项目 | 上限／规则 |
| --- | --- |
| 原始输入 | 10 MiB（10 × 1024 × 1024 字节），最多读取上限加一字节判断超限 |
| JSON 嵌套深度 | 16，包含未知或错误类型的值 |
| 九张表总记录数 | 10,000，空数组允许 |
| 每个字符串值 | 解码后 UTF-8 最多 4,096 字节；标识和元数据另按本规格校验 |
| 最多展示的问题 | 200；继续检查并统计其余问题，不保留超出上限的明细 |
| 最长预检时间 | 从处理输入开始共享 10 秒；取消／超时停止处理并返回 incomplete |
| 报告序列化大小 | 最多 256 KiB；超出时作为运行失败，不输出成功报告 |

限制是本工具的数据契约，不是新增生产数据库业务规则。大小／深度／记录数等限制触发后停止后续扫描，不能以部分扫描结果报告 valid。时间限制与输入上限同时约束读取与校验；任何等待不可绕过父取消。

报告先在有界内存中完整序列化再输出。输出失败返回 2；管道中不完整的 JSON 不能被当作成功结果。日志／标准错误只输出固定原因码或用法，不输出文件绝对路径、文件名、原始异常、记录值或未知字段名。

校验与 SHA-256 必须使用同一次有界读取的原始字节，不能在校验后重新打开文件计算哈希。命令不读取 `IM_*` 连接配置，也不因这些环境变量存在而尝试连接服务。

## 4. JSON 输入格式

采用现有 `sample_data.json` 的格式，顶层只允许下列字段：

| 字段 | 契约 |
| --- | --- |
| `format_version` | 必填，JSON 整数字面量 1；`1.0`、字符串、null 和其他版本拒绝 |
| `baseline_commit` | 必填，40 位十六进制字符串，仅作来源声明，不据此授权或证明相同表结构 |
| `data_origin` | 必填，1～64 字节字符串；内容是来源说明，不是信任等级 |
| `reference_time` | 必填，带明确时区的 RFC3339 时间，精度最多微秒 |
| `identity_source_selected` | 必填，布尔值，仅作来源说明 |
| `tables` | 必填，包含以下九个固定表名及各自记录数组，九个表都必须出现 |

允许多租户文件，但全部引用必须在文件内部闭合。即使某 UUID 已存在于某客户数据库，缺失的文件引用仍是错误；后续增量再处理数据库存量与增量文件。

严格拒绝重复 JSON 对象成员名（按解码后的名字判断）、未知顶层字段、未知表、未知记录字段、多个 JSON 文档、尾随内容、非法 UTF-8、UTF-8 BOM、未配对的 Unicode surrogate 转义和非法 JSON。不能依赖解码器静默替换字符或覆盖重复键。

### 4.1 记录字段

以下列表完整限定首版可接受字段。`?` 表示可省略且缺失等同 null；`=值` 表示可省略且采用对应数据库默认值。无标记字段必须显式提供且不能为 null。`created_at` 等数据库生成列不在输入范围。

| 表 | 字段 |
| --- | --- |
| `tenants` | `id`, `code`, `name`, `status=active` |
| `legal_entities` | `id`, `tenant_id`, `code`, `name`, `status=active` |
| `organizations` | `id`, `tenant_id`, `parent_id?`, `legal_entity_id?`, `org_type`, `code`, `name`, `status=active` |
| `departments` | `id`, `tenant_id`, `organization_id`, `parent_id?`, `code`, `name`, `status=active` |
| `users` | `id`, `tenant_id`, `global_employee_no`, `display_name`, `status=active` |
| `user_organizations` | `id`, `tenant_id`, `user_id`, `organization_id`, `employee_no?`, `title?`, `effective_from`, `effective_to?`, `is_primary=false`, `status=active` |
| `user_departments` | `id`, `tenant_id`, `user_organization_id`, `organization_id`, `department_id`, `effective_from`, `effective_to?`, `is_primary=false`, `status=active` |
| `external_identities` | `issuer`, `subject`, `tenant_id`, `user_id`, `status=active` |
| `admin_grants` | `id`, `tenant_id`, `membership_id`, `membership_organization_id`, `role`, `scope_organization_id?`, `status=active`, `effective_from`, `effective_to?` |

显式 null 只允许用于 `?` 字段，不能触发默认值。ID 与外键采用标准带连字符 UUID 字符串；内部比较统一 UUID 值，大小写表示同一 UUID，不限定版本，兼容样本 UUIDv5。

所有文本拒绝 U+0000。业务文本保留原值，不自动 trim、大小写折叠或 Unicode 规范化。代码／工号／主体的唯一性按原字符串匹配；这不证明与使用非确定性 collation 的目标库等价。`issuer`／`subject` 必须满足去掉两端 U+0020 后仍有字符，与现有 SQL btrim 检查一致；不把所有 Unicode 空白都视为数据库禁止值。

时间采用有效 Gregorian 日期的 RFC3339 字符串，带 `Z` 或数值时区偏移，年份 0001～9999，小数最多六位；内部按 UTC 时刻比较，保留微秒精度，不接受本地无时区时间或隐式精度舍入。空结束时间表示正无穷。

## 5. 校验规则

来源是基线中的 `000001_group_foundation`、`000002_admin_access` 和 `000004_external_identities`。本工具不解析／执行任意输入 SQL，也不把样本中的管理员角色或来源声明视为权限。

### 5.1 标量与唯一性

- 九张表按上述格式检查类型、必填、UUID、时间及大小限制。
- 各表 `id` 唯一；`external_identities` 的 `(issuer, subject)` 为复合主键。UUID 主键在对应表全文件范围唯一，不因租户不同而允许重复。
- 租户 `code` 全文件唯一；法人、组织 `code` 在租户内唯一；部门 `code` 在租户与组织内唯一；集团工号在租户内唯一。
- 外部身份的 `(tenant_id, user_id, issuer)` 唯一；不能给同 issuer 下的同用户同时绑定多个主体。不同 issuer 可绑定同一用户，预检并不证明当前单 issuer API 配置支持同时认证。
- 状态枚举：租户 active／suspended；法人／组织／部门 active／disabled；用户 active／frozen／departed；组织与部门任职 active／suspended／ended；外部身份 active／disabled；授权 active／revoked。

### 5.2 引用与组织图

- 法人、组织、部门、用户、任职、身份、授权的租户必须存在；所有关系使用现有复合外键所限定的同租户／同组织条件。
- 组织类型只接受 virtual_group／headquarters／company／branch／division／overseas。virtual_group 的法人为空，其他类型的法人必填且在同租户存在。
- 组织父节点在同租户存在；部门父节点在同租户同组织存在。拒绝自身作父节点及任意长度环，使用迭代图算法，不能用与组织深度相关的无限递归。
- 虚拟组织不能被任职引用。跨法人事业部的组织父节点与本组织法人不同不构成错误，组织层级不等于法人归属。
- 用户与组织任职、组织与部门、部门任职与组织任职的关联必须完整匹配。遇到缺失或歧义主键时报告对应问题，不任选其中一行继续形成有效关系。

### 5.3 任职及授权区间

- `effective_to` 若有值，必须严格晚于 `effective_from`；区间为左闭右开 `[from, to)`，相邻区间合法。
- 同租户同用户同组织的组织任职不能重叠；同租户同用户的 `is_primary=true` 组织任职不能重叠。
- 同租户同组织任职同部门的部门任职不能重叠；同租户同组织任职的 `is_primary=true` 部门任职不能重叠。
- 所有这些重叠检查都包含 ended／suspended 记录，与现有排斥约束一致，不能只检查 active 记录。按分组排序扫描，避免全部记录两两比较。
- 部门任职完整区间必须包含于其所属组织任职；组织结束而部门未结束会报告越界。首版仅检测，不自动缩短或补齐区间。
- 管理授权只接受 group_admin／organization_admin。group_admin 的 scope 为空；organization_admin 必须指定同租户组织。授权必须引用同租户匹配组织的任职，授权区间满足首末时间关系。
- 现有数据库没有要求授权区间必须包含于任职区间，首版不新增该要求。撤销授权、冻结人员、已结束任职及 inactive 上级可以作为历史数据存在，不能因此擅自改变记录状态。

### 5.4 本阶段不能判断的内容

没有数据库存量，不能判断组织改法人保护触发器、同 ID 已存在但内容不同、写入期间并发冲突或真实更新顺序。样本 DB10 的“缩短组织任职导致部门区间越界”在离线快照中通过父子区间检查呈现；数据库 UPDATE 保护本身仍留在数据库验证中。

不能判断 issuer 是否来自真实客户、sub 是否真的代表此人、JWT／PKCE 兼容性、管理员是否获真实授权、离职数据是否已及时同步、历史会话权限或附件可访问性。时间参考值不授权历史写入，也不拿系统当前时间将历史记录判为非法。

## 6. 报告与错误处理

报告固定含以下字段：`report_version=1`、`validation_profile=group_identity_v1`、`status`、`checks_complete`、`input_sha256`、`counts`、`errors_total`、`issues`、`issues_truncated`、`scope=offline_file`、`database_checked=false`、`identity_provider_checked=false`、`import_authorized=false`。

`counts` 固定包含九张表的已解析数量及 `total`。未完成结构解析时数量可为 null，不能把未扫描记录当作零。SHA-256 只在完整原始文件读取成功后提供，否则为 null；用于关联本次输入，不作为认证或授权。

每个 issue 只包含固定表名 `entity`（文档级为 document）、从 1 开始的输入数组位置 `row`（文档级为 null）、固定已知字段名或 `_unknown`／`_record`／`_document` 标记 `field`、固定原因码 `code` 和可选的同表 `related_row`。不输出 UUID、姓名、工号、issuer、subject、路径、原文、动态错误消息或未知字段名。

报告顺序固定：文档问题优先，之后按上文表顺序、输入行号、字段声明顺序、原因码、related_row 排序；三个特殊字段标记依次排在已知字段后，空 related_row 排在数值前。按 `(entity, row, field, code, related_row)` 完全相同的项去重。关联冲突以输入位置定位，不通过排序重排输入。主键缺失／歧义造成的关系检查不能产生虚假的通过，也应避免为同一根本错误无限生成派生问题。

解析／文件结构无法成立时停止模型校验，checks_complete=false。模型校验中，非法或重复主键使该目标不可用于关系判断：该目标的主键问题只报告一次对应位置，引用者报告一次 REF_NOT_FOUND 或 REF_SCOPE_MISMATCH，不再为这条无效关系生成时间、组织类型等派生问题。除此以外，checks_complete=true 表示全部适用检查已经完成，并不表示有问题的文件满足约束。超过明细上限时保留按上述排序最靠前的 200 个 issue，不能随 map 迭代顺序随机截取。

固定原因码分组：

| 类别 | 原因码 |
| --- | --- |
| 输入／结构 | INVALID_ENCODING, JSON_INVALID, DUPLICATE_JSON_KEY, FORMAT_VERSION_UNSUPPORTED, UNKNOWN_FIELD, UNKNOWN_TABLE |
| 资源限制 | INPUT_TOO_LARGE, DEPTH_LIMIT, ROW_LIMIT, FIELD_LIMIT |
| 标量 | FIELD_REQUIRED, TYPE_INVALID, UUID_INVALID, TIME_INVALID, ENUM_INVALID, TEXT_NUL, TEXT_BLANK |
| 唯一性／关系 | PK_DUPLICATE, UNIQUE_DUPLICATE, REF_NOT_FOUND, REF_SCOPE_MISMATCH, SELF_PARENT, TREE_CYCLE, VIRTUAL_LEGAL_MISMATCH, VIRTUAL_MEMBERSHIP |
| 时间／授权形状 | INTERVAL_INVALID, INTERVAL_OVERLAP, PRIMARY_OVERLAP, DEPARTMENT_INTERVAL_OUTSIDE, GRANT_SCOPE_INVALID |
| 运行失败 | INPUT_READ_FAILED, INPUT_TYPE_UNSUPPORTED, TIMEOUT, CANCELED, OUTPUT_WRITE_FAILED |

退出语义：

| 退出码 | 报告语义 |
| --- | --- |
| 0 | status=valid，完整检查完成且无错误；或独立 help／version 模式 |
| 1 | status=invalid，发现输入／模型问题。若结构或资源限制阻止后续检查，checks_complete=false |
| 2 | 参数、读取、平台保证、取消、超时或输出失败；可输出报告时 status=incomplete，checks_complete=false |

检查完整但错误明细超过 200 时继续统计 `errors_total`，`issues_truncated=true`，status=invalid、checks_complete=true。取消或超时必须返回 incomplete，不能只因暂时已发现某些问题而假装整份文件检查结束。任何路径都不得返回有错误的 valid。

## 7. 验收与实施 Review Focus

以下是待编写及执行的验收，现有样本数据库日志不算预检命令已经通过。

1. **样本闭环：**现有固定样本 74 条、九表、双租户、UUIDv5、跨法人事业部、相邻调动和历史状态通过；零错误、counts 完整，input_sha256 与原文件一致。空数组齐全且无引用的文件也通过。
2. **十一类模型冲突：**把原数据库用例转换成 JSON 快照变异，覆盖复合身份重复、同用户同 issuer 多主体、跨租户身份、组织环、部门跨组织、虚拟组织任职、同组织任职重叠、主任职重叠、部门区间越界、父区间缩短及重复工号；要求报告位置与原因符合本规格。数据库 SQL 不作为工具输入。
3. **解析不可被绕过：**重复键及转义等价键、未知字段／表、缺字段、错误 null、浮点版本、尾随文档、非法 UTF-8／surrogate／BOM、JSON 非字符串键均拒绝；UTF-8 多字节按字节上限计数。
4. **关系与区间边界：**主键大小写等价、不同租户相同主键、缺失父节点、深组织链与环、部门环、ended 行重叠、相邻微秒区间、开区间、不同偏移但同一时刻、部门主任职、授权 scope 及默认值逐项覆盖。
5. **隐私与确定性：**在姓名、subject、issuer、未知字段名和输入路径放置标记，stdout／stderr 无这些标记；同一原文件重复运行的报告字节一致；以 raw bytes 计算哈希，合法 JSON 空白变化可改变哈希但不改变判定。
6. **资源与 I/O：**10 MiB 边界及超一字节、10,000 条边界及超一条、深度／字段长度、200 项明细截断而统计完整、超时／取消、符号链接／FIFO／路径替换、输出失败均有可重复测试。最大允许输入须在共享 10 秒预算内完成或诚实返回 incomplete；不能将超时称为合法数据不通过模型校验。
7. **数据库对照：**在独立临时 PostgreSQL 中用固定迁移对照正常样本及对应约束变异，确认模型判定一致；仅测试夹具访问数据库，命令及内部预检包不含数据库客户端。记录差异和测试环境，不扩大为客户集成证明。
8. **交付核验：**从最终固定源码执行预检专项及相关回归、构建／vet，保存报告、资源边界和隐私检查证据；不重复启动无关附件依赖，也不借用之前的全仓 PASS 冒充本次新增测试。

RF1：重复键／Unicode／默认值不能让预检与数据库语义悄悄分歧。RF2：关系查找必须处理复合 Scope、重复主键和依赖错误。RF3：区间排斥不按状态过滤，相邻时刻合法。RF4：组织图和区间算法在最大输入内有界，检查取消贯穿全流程。RF5：错误报告不回显人员内容或原始 I/O 错误。RF6：文件类型与路径替换、输出失败不能绕过完整性或产生成功退出。

## 8. 交付边界与后续

本增量不增加生产依赖、数据库迁移、HTTP 路由、功能开关或凭据。样本的 `admin_grants` 只参与文件结构校验，报告明确没有授予任何权限。

设计获书面确认后再形成逐项实施计划；计划审阅后，沿用当前助手逐项实现。后续受控写入需要独立设计认证／租户范围、数据库存量对比、并发复核、批次幂等、审计及回滚语义；预检报告不自动进入写入步骤。客户身份源接入、M4 签收及生产验收仍待执行。
