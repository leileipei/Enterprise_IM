# P2-03 Outbox 发布 Worker 设计

## 目标与边界

P2-02 已在消息事务中写入 `outbox_events`，但没有进程发布这些事件。本增量增加独立 Worker，把待发布的 `message_created` 事件送入 Redis Stream，供后续 WebSocket Gateway 消费。PostgreSQL 继续保存消息事实；Redis 事件仅作为在线通知与加速通道，不承载消息正文，也不是唯一持久副本。

本增量不实现 Gateway、设备确认、客户端推送或补拉，因此不宣称消息已送达。用户继续通过 P2-02 的 ACK 判断服务端是否已持久化接收。

## 发布语义

Worker 从 PostgreSQL 按 `next_retry_at, created_at, id` 读取一条到期的 `pending` 事件，使用 `FOR UPDATE SKIP LOCKED` 在事务内锁定它。多个 Worker 可并行处理不同事件；单条事件同时只由一个事务处理。Worker 在持有该行锁时调用 Redis `XADD`，成功后把事件更新为 `published`、记录 `published_at` 并增加 `attempt_count`，提交事务。

Redis 发布失败或超时：仍在同一事务中增加 `attempt_count`，保持 `pending`，把 `next_retry_at` 延后。失败后按 1、2、4、8 秒指数退避，上限 5 分钟；事务更新失败则整个事务回滚，事件保持原状。没有到期事件时不写数据库。

Redis `XADD` 成功而 PostgreSQL 提交失败时，事件会在下次处理时再次发布。**语义是至少一次，不是恰好一次。**事件载荷包含稳定的 `event_id`、`tenant_id`、`conversation_id`、`message_id`、`seq`、`event_type`，后续消费者必须按 `event_id` 去重并按会话 `seq` 发现乱序/缺口。多 Worker 不承诺 Redis 中同会话事件的严格顺序。事件不含消息正文、工号或姓名。

Redis Stream 不在生产者侧自动裁剪，以免在消费端上线前丢弃尚未处理的通知。上线前需与 Gateway 一起确定消费组、确认、积压容量和清理策略。Redis 数据丢失不删除 PostgreSQL 消息，后续补拉按数据库会话序号恢复。

## 进程与配置

新增 `cmd/im-outbox-worker`。必需配置 `IM_DATABASE_URL` 与 `IM_OUTBOX_REDIS_URL`（`redis://` 或 `rediss://`），可选 `IM_OUTBOX_STREAM`，默认 `enterprise-im:message-created:v1`。进程启动时检查数据库与 Redis 连通性；运行中每次处理一条事件，队列为空时短暂等待，错误写结构化日志后继续重试；收到 SIGTERM/SIGINT 后停止领取新事件并关闭连接。每次发布有 5 秒超时。

## 验收

PostgreSQL 测试覆盖单事件发布、无事件、多 Worker 互斥、Redis 发布失败后的退避与恢复、数据库更新失败不错误标记 `published`。Redis 集成测试验证 Stream 中字段与事件 ID，且不包含正文。进程配置测试覆盖必需参数和无效 URL。全量测试、静态检查和构建通过。文档明确 ACK 与在线投递的区别及本增量的运维限制。
