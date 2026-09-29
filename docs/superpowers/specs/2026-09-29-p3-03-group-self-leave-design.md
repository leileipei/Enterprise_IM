# P3-03 群成员资格查询与主动退群设计

## 范围

为已建群开放本人当前成员区间查询和主动退群。邀请、移除、转让群主、群消息收发另行实现。本增量不增加数据库表：沿用 `conversation_membership_intervals` 的不可变历史与活跃成员唯一约束。

## API

- `GET /api/v1/groups/{group_id}/membership`：仅返回当前调用人的活跃区间 `interval_id`、`role`、`join_seq`、`group_status`。非成员、其他租户或非群均返回 404。
- `POST /api/v1/groups/{group_id}/leave`：JSON `{"interval_id":"<uuid>"}`。成功返回 `interval_id`、`status=left`、`leave_seq`。必须指定区间 ID；重复提交已退出的同一区间返回原结果，不触碰此后重新入群的新区间。
- 仅接受已验证身份与有效当前任职。无效身份 403，畸形请求 400，群或本人区间不可用 404，群主尚未转让时 409 `owner_transfer_required`，审计不可用时 503。

## 事务和边界

退群先锁当前任职，再锁群会话和指定区间；锁序与后续群消息及邀请保持一致。退群将 `leave_seq` 记为事务中群的 `last_seq`，不增加消息序号；`left_at` 不早于 `joined_at`。`policy_blocked` 群仍允许退出。成功审计与成员更新在同一事务，审计失败则回滚。重试只读取已关闭区间，不再产生审计事件。

当前群主不能自退；后续转让群主后可以退出。一次退群只关闭指定的活跃区间。调用者即使另有任职，也按集团统一 `user_id` 找本人群成员资格。查询已退出的群返回 404，历史读取权限由后续群消息 API 按区间序号执行。
