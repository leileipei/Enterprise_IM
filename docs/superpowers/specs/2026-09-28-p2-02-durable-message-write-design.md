# P2-02 文本消息可靠写入设计

## 范围

基于 P2-01 的单聊会话，增加受保护的文本消息写入、幂等 ACK、连续会话序号和事务性 Outbox。消息提交后才返回 ACK。本增量不发布 Outbox、不推送 WebSocket，也不提供离线补拉；后续增量分别实现这些能力。

## 写入契约

`POST /api/v1/conversations/{id}/messages` 使用 Bearer 令牌和 `X-Acting-Membership-ID`，JSON 仅接受 `client_msg_id` 与 `text`。`client_msg_id` 必须是规范 UUIDv7，内含毫秒时间戳处于服务端当前时间之前 7 天至之后 5 分钟的窗口。过期返回 410，未来越界或格式错误返回 400。正文必须是有效 UTF-8、非空白且 UTF-8 字节数不超过 16 KiB；原样保存，SHA-256 摘要取 JSON 解码后正文的 UTF-8 字节。不同 JSON 转义但正文相同视为同一内容。

服务端先验证本人账号、租户和选定任职，再验证 UUIDv7 时间窗口。幂等键为 `(tenant_id, conversation_id, sender_user_id, client_msg_id)`。已有键与摘要相同时返回原始 `message_id`、`conversation_id`、`seq`、`accepted_at`；摘要不同返回 409 `idempotency_conflict`。重复请求仍须经过当前本人身份校验，但不再增加序号、限流计数或 Outbox 事件。原始 ACK 不随策略变动而改写；账号冻结或任职失效的重复请求仍拒绝。

新消息必须持有有效本人任职、有效目标任职、活跃单聊会话，且发送时双方仍为会话记录中的用户组合。执行当前发布的 `send_message` 策略；没有权限或会话不可用时不返回消息或会话元数据。会话记录的选定任职若在请求中途被重选，拒绝本次写入并要求客户端重试。新消息按租户和发送用户执行数据库事务内的固定一秒窗口限流，默认每用户每秒 10 条，可用 `IM_MESSAGE_RATE_PER_SECOND` 设置正整数。超过返回 429；幂等重试不占新额度。

## 数据与事务

新增 `messages`、`message_idempotency`、`outbox_events` 和 `message_rate_windows`。消息的 `(tenant_id, conversation_id, seq)` 唯一；发送幂等键唯一。幂等记录引用确切消息并至少保存 30 天，消息保存策略仍按产品基线单独处理。Outbox 事件唯一关联消息，初始状态 `pending`，有 `next_retry_at` 与重试次数，供后续 Worker 使用。所有表用复合外键锁定租户、会话、用户和任职归属。

在单一 PostgreSQL 事务中，按 ID 顺序锁定双方任职，检查本人身份并读取已存在的幂等记录；新消息再锁会话行、确认会话所选任职未变、执行实时策略和限流，递增 `last_seq`，插入消息、幂等记录和 Outbox，写策略决策与请求审计并提交。任一失败回滚序号、消息、限流和 Outbox。并发发送由会话行锁串行化；同键并发只生成一条消息。响应 200 的 ACK 含 `message_id`、`conversation_id`、`seq`、`server_time`。

## 错误与验证

400 表示请求或 UUIDv7 无效；403 表示本人身份失效；404 隐藏会话、目标或当前通信边界；409 表示相同幂等键正文不同或会话任职上下文变化；410 表示重试窗口过期；429 表示发送频率上限；503 表示数据库、审计或锁不可用。所有拒绝仅返回稳定错误码，不泄漏正文或 SQL 细节。

PostgreSQL 测试覆盖 M01、M04 和 M05 的事务语义：同键重试、不同正文冲突、多连接并发、连续 seq、故障回滚无缺口、Outbox 与消息一一对应，以及冻结、离职、策略撤权、跨租户、限流和会话重选。HTTP/JWT 测试覆盖严格输入、状态码及真实持久化。M02 的最终投递和 M03 的重连补拉属于后续增量，不因 Outbox 入库而宣称通过。
