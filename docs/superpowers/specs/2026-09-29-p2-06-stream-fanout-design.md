# P2-06 Redis Stream 在线补拉通知设计

## 目标与边界

在 P2-05 已认证的 WebSocket 上，向在线单聊双方发送新消息补拉信号。信号为 `{"type":"sync_required"}`，不含租户、会话、序号或正文；客户端收到后用 P2-04 的 HTTP 接口按本地连续 `seq` 补拉。`ready` 仍要求首次连接时补拉。信号可以合并、重复或丢失，不能当作设备送达 ACK。本阶段不实现聊天 UI、消息正文直推、设备已读/送达或群聊。

## 流与路由

每个 API 实例独立以 `XREAD` 读取 `enterprise-im:message-created:v1`，不用消费组：消费组会把同一事件只分配给一个实例，无法通知连在其他实例的用户。Worker 在健康处理循环中通常每 3 秒刷新按 Stream 名称派生的 10 秒 Redis 存活标记；处理受阻或持续失败则让标记过期。API 启动和读流期间要求同一 Redis、同一 Stream 上有该标记，批次处理期间也约每秒复核。因此需要先启动 Worker，再启动开启通知的 API。标记过期、读流故障或数据库核验超过 3 秒时，终止本实例的通知能力并关闭现有 WebSocket；恢复需要进程重启。启动时读取 Stream 尾 ID 后开始读新事件；空流从 `0-0` 开始。重连的 `ready` 使客户端从 PostgreSQL 补拉，Stream 不承担消息恢复。

每条 `message_created` 事件必须具备合法 `event_id`、`tenant_id`、`conversation_id`、`message_id` 和正数 `seq`。数据库用所有标识与 Outbox、消息记录交叉验证，只提取当时的发送与接收用户 ID；缺失或不匹配的记录不通知任何人，也不占用去重 ID。每个实例维护最多 10000 个已验证事件 ID 的去重窗口。通知按 `tenant_id:user_id` 投递到本实例的所有在线连接，连接内一个槽位合并突发信号；每次写信号前重新核验当前身份。任何数据库读取或 Stream 读取错误使通知能力失效，阻止新票据、关闭现有连接并使 `/health/ready` 返回 503，避免无提示地漏发。

## 配置与限制

沿用 `IM_REALTIME_REDIS_URL`；`IM_REALTIME_STREAM` 可覆盖默认 Stream 名称，须与 Worker 的 `IM_OUTBOX_STREAM` 一致。连接与 Stream 均使用现有 Redis 客户端。多实例各自读取同一流，因此每个实例都能通知本机连接。Stream 暂不自动裁剪，生产部署前需按容量监控积压并设计可检测缺口的保留策略；本增量不宣称完成容量、高可用或设备送达验收。

## 验证

真实 Redis 测试覆盖两实例都收到事件、同事件 ID 去重、无关用户与跨租户隔离、突发合并、Worker 标记过期与 Redis 故障；真实 PostgreSQL 测试覆盖事件标识交叉验证、旧记录缺失接收人、伪造事件及数据库故障。WebSocket 测试覆盖通知帧、发送前撤权、故障关闭和就绪探针。完整 Go 测试、竞争检测、静态检查及构建须通过。
