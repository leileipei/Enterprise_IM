import 'package:flutter/material.dart';
import '../core/protocol/models.dart';

class ReceiptBadge extends StatelessWidget {
  final ReceiptSummary? summary;
  final String kind;
  const ReceiptBadge({super.key, required this.summary, required this.kind});
  @override
  Widget build(BuildContext context) {
    final value = summary;
    final label = value == null
        ? '回执状态暂不可用'
        : !value.summaryAvailable
        ? '送达及已读状态不可查询'
        : kind == 'group'
        ? '可见接收者：已送达 ${value.visibleDeliveredCount}/${value.visibleRecipientCount} · 已读 ${value.visibleReadCount}/${value.visibleRecipientCount}'
        : switch (value.status) {
            'read' => '已读',
            'delivered' => '已送达',
            _ => '已发送',
          };
    return Semantics(
      liveRegion: true,
      child: Text(label, style: Theme.of(context).textTheme.labelSmall),
    );
  }
}

class UnreadBadge extends StatelessWidget {
  final UnreadState? state;
  const UnreadBadge({super.key, required this.state});
  @override
  Widget build(BuildContext context) {
    final value = state;
    return Semantics(
      liveRegion: true,
      child: Text(
        value == null
            ? '未读数量暂不可用'
            : value.exact
            ? '未读 ${value.count} 条'
            : '未读至少 ${value.count} 条',
      ),
    );
  }
}
