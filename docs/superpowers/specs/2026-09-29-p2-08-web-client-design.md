# P2-08 基础 Web 单聊设计

## 目标与范围

基于已完成的 P2-07 本人身份接口，交付同源浏览器页面，让集团用户通过现有 OIDC 身份源登录、选择有效任职、搜索可见人员并完成单聊文本收发。页面直接使用已审计的 API；本增量不增加会话列表、未读数、送达/已读确认、群聊或跨刷新会话恢复。客户身份源与生产入口的联调仍是独立验收事项。

## 登录和令牌边界

仅显式设置 `IM_WEB_ENABLED=true` 且 OIDC 启用时暴露 `/web/`。需配置 HTTPS 授权地址、令牌地址和精确的 `https://.../web/` 回调地址，以及 OIDC 白名单中的公共客户端 ID。浏览器产生随机 `state` 和至少 256 位 PKCE verifier，在 sessionStorage 暂存登录事务，使用 S256 发起授权码请求。回调时先验证唯一 `state`，清除事务并从地址栏移除 `code`；错误或过期事务不兑换。

`POST /web/oauth/token` 仅接受同源 Origin、JSON 中的授权码和合法 PKCE verifier，服务器只向固定配置的 HTTPS 令牌地址发送 `authorization_code` 兑换请求，禁止重定向、设置短超时和响应大小上限。只接收 Bearer 访问令牌，并用现有 OIDC Authenticator 验证签名、发行方、受众、客户端 ID 与本地身份绑定后才返回给浏览器；不转发 ID Token 或刷新令牌。访问令牌仅存于页面内存，刷新页面需重新走 SSO；页面不提供持久化登录。所有 Web 资源及兑换响应禁止缓存，页面设置严格 CSP、不发送 Referer。

## 聊天操作

登录后先调用 `GET /api/v1/me`，展示当前有效任职供用户选择；未选择时不能调用聊天业务接口。选择后在所有目录、会话、消息和票据请求中携带选定 `X-Acting-Membership-ID`。可按姓名搜索人员并选择其可见任职，调用 `POST /api/v1/conversations` 发起单聊。打开会话后从 `after_seq=0` 分页补拉，逐条推进游标；`redacted` 只显示不可见占位。发送文本时生成 UUIDv7 `client_msg_id`，失败重试保留原 ID 和原正文；ACK 仅表明服务端持久化。页面以纯文本渲染用户和消息内容。

配置 Redis 实时能力时，页面申请一次性票据、用现有 WebSocket 子协议连接，在 `ready` / `sync_required` 时补拉；实时不可用时以定时补拉继续工作。切换任职或退出时关闭连接、清除当前会话与内存令牌。401/403 令用户重新登录或重选任职，其他错误显示可理解的提示，不能暗示消息已送达。

## 验证

Go 测试覆盖关闭时不暴露 Web、配置校验、静态资源安全头、固定令牌端点与回调地址、输入/Origin/体积限制、兑换故障、令牌验证失败和正常返回。浏览器脚本要通过语法检查和本地页面检查。完整现有测试、竞争检测、静态检查和构建通过。身份源真实授权码联调、浏览器兼容性及生产 HTTPS 需另行验证。

协议依据：[RFC 9700](https://www.rfc-editor.org/rfc/rfc9700.html)、[RFC 7636](https://www.rfc-editor.org/rfc/rfc7636.html)、[RFC 10017](https://www.rfc-editor.org/rfc/rfc10017.html)。
