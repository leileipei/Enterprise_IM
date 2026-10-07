# P4-30 最终独立只读评审原始结论

评审代理：/root/review_p4_30_final；gpt-6-astra xhigh，fresh fork none。范围 7f5868aa11a3d4d8b758f0e332813502efe7eb6e..ac8bef72f68ee834cdaa728e5378abc5fcc6fa47。无修改工作树/index/HEAD/数据库；仅在 /private/tmp/p430-review-ac8bef7-2369c5qs 固定源码副本做补充验证。

## Strengths

BEGIN 前 session 锁、完整 UUID 回执查找、解锁失败关闭连接；六表参数化追加、保存点回滚、提交前授权、严格回执编码；旧只读 profile 独立。14门禁日志无 FAIL/SKIP，具备真实 SQLSTATE、TCP COMMIT 断应答、进程退出和 HTTP 边界证据。

## Critical

无。

## Important

1. access/import_authorization.go:90 和 importapply/get.go:59 审计 INSERT 未检查 RowsAffected。BEFORE INSERT RETURN NULL 可无错误零行，违反业务/回执/审计一致性。隔离命令标签 INSERT 0 0 复现为 nil；必须检查一行并补真实触发器测试。
2. migration22 down:2 直接 DROP，migration_test.go:77 在已提交回执后要求 down 成功。清掉去重键却保留主数据与审计，重新 up 后旧编号可换正文重用。必须终态非空拒绝、空表允许并核对旧回执不变；不能当外部 DBA 改库排除。
3. import_admin.go:163/175 公开 IMPORT_KEY_CONFLICT/IMPORT_COMMIT_UNKNOWN 与已确认 BATCH_KEY_CONFLICT/COMMIT_OUTCOME_UNKNOWN 不一致，影响客户恢复分支。采用规格固定码，内部 sentinel 可保留。
4. service.go:132 丢弃 EvaluateDocument report，incomplete/TIMEOUT/CANCELED 均报422。隔离测试确认合法文件取消后 nil doc/incomplete；须区分运行中断503可重试与真正文档422，补取消/截止测试。
5. import_admin.go:34/148 认证/输入/数据库拒绝没有固定安全事件或关联号。401捕获零日志；现有 NoSecrets 允许零日志通过。增加仅固定码+服务生成安全关联号的记录，并证明存在且不泄密。
6. 测试/证据缺口：没有实际 COMMIT 已发送期间到期、回执 INSERT 失败、非完整性触发器错误注入。现有 token到期在上传或回执INSERT之前，不替代 COMMIT期间。补具名场景及必需门禁名，核对三者原子性及同编号恢复；未测不等于已确认产品错误。

## Minor

无。

## Review Focus

RF1锁/UUID代码正确，未实际制造64位哈希碰撞。RF2原字节/actor/GET语义正确，membership变化有纯绑定测试。RF3COMMIT到期缺证据。RF4万层拓扑纯测试、HTTP300层和库存/CHECK有证据，其余故障见6。RF5认证前正文/忙碌正确，安全事件缺失见5。

## Declined to judge

- Task8尚未完成交付docs/计划状态/草稿PR，属后续收尾。
- 客户导入、生产迁移/部署/性能，明确未执行。
- IdP已签JWT即时撤销，规格排除 introspection。
- DBA绕过服务改库和触发器非事务外部副作用，规格排除；本次down不属此排除。

## Assessment

独立草稿交付 With fixes；不构成可合并或生产验收结论。全仓库套件尚在执行时评审结束，不引用中间计数。补充复现日志 review-reproductions.log。
