# P2-05 实时连接身份与握手设计

## 目标与阶段边界

为未来 Redis Stream 在线推送建立浏览器可用的 WebSocket 身份入口。现有 Bearer 令牌只能用于普通 HTTP 请求，浏览器 WebSocket 构造器不能设置自定义认证头，因此本增量增加短时一次性票据。本增量的连接只发送一次 `ready` 事件，要求客户端立即使用 P2-04 的 `after_seq` 接口补拉；尚不消费 Stream 或推送新消息，不宣称 M02 在线投递通过。

## 方案

显式配置 `IM_REALTIME_REDIS_URL` 时，在现有 OIDC API 进程启用两个路由。`POST /api/v1/realtime/tickets` 使用现有 Bearer 令牌与 `X-Acting-Membership-ID`，不接受查询或正文。数据库重新检查租户、账户、组织及选定任职当前有效，写入 `realtime_ticket` 审计。通过后用 32 字节密码学随机数生成票据，Redis 保存其 SHA-256 摘要键及可信身份，TTL 为 30 秒；正文只返回原始票据与 `expires_in_seconds:30`，并设置 `Cache-Control:no-store`。Redis 不可用时返回 503，不回退到本机内存。

浏览器通过 `new WebSocket(url, ["enterprise-im.v1", "ticket.<opaque>"])` 连接 `GET /api/v1/realtime`。服务只接受这两个协议项和严格票据格式；用 Redis `GETDEL` 原子消费票据，因此同一票据最多成功建立一个连接。消费后再次核查当前任职并写 `realtime_connect` 审计，再执行升级。未认证或过期票据返回 401，失效身份 403，存储或审计故障 503。票据不出现在 URL、响应子协议或应用日志里。WebSocket 库使用默认同源 Origin 校验，不开放跨源，也不启用压缩。

连接升级后只发送 `{"type":"ready","resync_required":true}`；客户端从自身已连续确认的 `seq` 继续 HTTP 补拉。服务每 5 秒复核账户和选定任职，失效时关闭连接；每 30 秒发送 Ping 检测死连接。客户端消息暂不支持，连接仅接收控制帧。关闭、服务终止和请求取消均释放连接，不保留任何在线消息队列。

## 数据与安全边界

Redis 票据键只含固定前缀与 token 摘要，值仅含规范化的 tenant/user/membership UUID。票据创建不依赖会话，不能越过 P2-04 的逐条历史权限；未来推送仍须在发送前重新授权。Redis 丢失只使尚未消费的票据失效，不影响 PostgreSQL 中已 ACK 的消息。部署时应只使用 TLS 后的 `wss` 与 HTTPS。每个进程对同时连接数设上限，并在关闭时释放额度。

## 验证

测试覆盖一次性/过期/并发消费、Redis 故障、冻结或结束任职、审计故障、无效请求、错误 Origin、协议回显、ready 帧、连接存活期间撤权及停机释放。真实 Redis 和 PostgreSQL 集成测试与原有完整测试都须通过。
