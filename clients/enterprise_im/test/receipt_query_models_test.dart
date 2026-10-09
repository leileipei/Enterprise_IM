import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/models.dart';

void main() {
  test('unread lower bound preserves exact flag and canonical head', () {
    final value = UnreadState.fromJson({
      'unread_count': 4,
      'exact': false,
      'head_seq': '9007199254740993',
      'receipt_revision': '3',
      'policy_version': '2',
    });
    expect(value.count, 4);
    expect(value.exact, isFalse);
    expect(value.headSeq.toString(), '9007199254740993');
    for (final count in [-1, 4.0, '4', 11]) {
      expect(
        () => UnreadState.fromJson({
          'unread_count': count,
          'exact': true,
          'head_seq': '10',
          'receipt_revision': '3',
          'policy_version': '2',
        }),
        throwsFormatException,
      );
    }
  });
  test('group summaries preserve visible counts without inventing facts', () {
    final raw = <String, Object?>{
      'seq': '1',
      'status': 'sent',
      'summary_available': true,
      'visible_recipient_count': 3,
      'visible_delivered_count': 2,
      'visible_read_count': 1,
    };
    final value = ReceiptSummary.fromJson(raw);
    expect(value.visibleReadCount, 1);
    expect(value.visibleRecipientCount, 3);
    expect(value.status, 'sent');
    for (final changes in [
      <String, Object?>{'visible_read_count': 3},
      {'status': 'read'},
      {'visible_delivered_count': 4},
      {'visible_read_count': -1},
    ]) {
      expect(
        () => ReceiptSummary.fromJson({...raw, ...changes}),
        throwsFormatException,
      );
    }
    final hidden = ReceiptSummary.fromJson({
      ...raw,
      'summary_available': false,
      'visible_recipient_count': 0,
      'visible_delivered_count': 0,
      'visible_read_count': 0,
    });
    expect(hidden.summaryAvailable, isFalse);
    expect(hidden.status, 'sent');
    expect(
      () => ReceiptSummary.fromJson({...raw, 'summary_available': false}),
      throwsFormatException,
    );
  });
}
