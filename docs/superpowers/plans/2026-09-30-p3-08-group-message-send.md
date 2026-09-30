# P3-08 群文本消息可靠写入实施计划

**目标：**实现与 P3-07 群补拉相接的群消息写入，并保持 P2 单聊可靠链路的事务和幂等语义。

**规格：**`docs/superpowers/specs/2026-09-30-p3-08-group-message-send-design.md`

## 文件职责

- `internal/policystore/group_message_send.go`：群发送事务、区间与全员策略复核，复用 `messages.go` 的 ACK、幂等、限流和审计辅助函数。
- `internal/policystore/group_message_send_test.go`：真实 PostgreSQL 写入、策略、区间、并发和回滚测试。
- `internal/policystore/realtime_recipients.go` 及测试：Outbox 群事件按消息序号解析历史收件人，拒绝伪造发送任职。
- `internal/httpserver/groups.go`、`internal/httpserver/conversations.go`：群 POST 路由与复用的消息请求/ACK 格式。
- `internal/httpserver/group_message_send_test.go`：认证后路由、严格输入和错误映射。
- `README.md`：群发送示例、策略阻断和仍未覆盖的能力。

## 步骤

1. [x] 写群发送 PostgreSQL 集成测试，先确认缺少接口或写入行为导致失败；实现首次写入、原 ACK 重试和群补拉可见性。
2. [x] 用失败测试覆盖退群、错误任职、跨租户和失效身份；实现群行与成员区间的串行授权。
3. [x] 用失败测试覆盖全员双向策略、`policy_blocked`、限流和时间边界；实现拒绝审计与阻断状态事务提交。
4. [x] 用失败测试覆盖 Outbox/审计回滚及并发连续序号；完成可靠写入事务和群事件收件人解析。
5. [x] 用失败的 HTTP 测试驱动 POST 路由、严格 JSON、错误映射与 `duplicate` ACK；更新 README。
6. [x] 运行 PostgreSQL 全量测试、定向竞态测试、`go vet`、构建和差异检查；独立审阅后提交并创建以 P3-07 为基线的草稿 PR。

## 复审重点

- 幂等重试不能重复消耗限流、序号或 Outbox，也不能让冻结账号取回 ACK。
- 退群与发送并发必须有唯一顺序；消息发送任职必须等于区间来源任职。
- 策略拒绝或成员任职失效必须阻断整群新增消息，且不能留下部分写入。
- 策略生效或任职到期跨越事务时，最终检查不能使用过期快照。
- `policy_blocked` 已存在或新产生时不得返回新消息 ACK；旧 ACK 可按幂等规则重放。
