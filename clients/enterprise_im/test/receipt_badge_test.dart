import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/models.dart';
import 'package:enterprise_im/ui/receipt_badge.dart';

void main() {
  testWidgets(
    'group unavailable never implies zero readers and visible counts name their scope',
    (tester) async {
      ReceiptSummary summary(bool available) => ReceiptSummary.fromJson({
        'seq': '1',
        'status': available ? 'delivered' : 'sent',
        'summary_available': available,
        'visible_recipient_count': available ? 3 : 0,
        'visible_delivered_count': available ? 3 : 0,
        'visible_read_count': available ? 1 : 0,
        'delivered_at': null,
        'read_at': null,
      });
      await tester.pumpWidget(
        MaterialApp(
          home: Scaffold(
            body: ReceiptBadge(summary: summary(false), kind: 'group'),
          ),
        ),
      );
      expect(find.textContaining('不可查询'), findsOneWidget);
      expect(find.textContaining('0'), findsNothing);
      await tester.pumpWidget(
        MaterialApp(
          home: Scaffold(
            body: ReceiptBadge(summary: summary(true), kind: 'group'),
          ),
        ),
      );
      expect(find.textContaining('可见接收者'), findsOneWidget);
      expect(find.textContaining('已读 1/3'), findsOneWidget);
    },
  );
  testWidgets(
    'unread lower bound is distinguished from exact and unknown receipt is not sent',
    (tester) async {
      UnreadState unread(bool exact) => UnreadState.fromJson({
        'unread_count': 2,
        'exact': exact,
        'head_seq': '3',
        'receipt_revision': '1',
        'policy_version': '1',
      });
      await tester.pumpWidget(
        MaterialApp(
          home: Scaffold(
            body: Column(
              children: [
                const ReceiptBadge(summary: null, kind: 'direct'),
                UnreadBadge(state: unread(false)),
              ],
            ),
          ),
        ),
      );
      expect(find.text('已发送'), findsNothing);
      expect(find.textContaining('至少 2'), findsOneWidget);
      await tester.pumpWidget(
        MaterialApp(
          home: Scaffold(body: UnreadBadge(state: unread(true))),
        ),
      );
      expect(find.textContaining('至少'), findsNothing);
      expect(find.textContaining('未读 2'), findsOneWidget);
    },
  );
}
