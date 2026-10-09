import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/storage/records.dart';

void main() {
  final valid = <String, Object?>{
    'client_msg_id': '019a0c00-0000-7000-8000-000000000001',
    'conversation_id': '00000000-0000-4000-8000-000000000001',
    'acting_membership_id': '00000000-0000-4000-8000-000000000002',
    'text': '已批准正文',
    'generation': 1,
    'created_at': '2026-10-09T00:00:00.000Z',
    'state': 'pending',
  };
  test(
    'typed pending send preserves exact retry identity and rejects malformed context',
    () {
      final send = PendingSend.fromJson(valid);
      expect(send.clientMsgId, valid['client_msg_id']);
      expect(send.toJson(), valid);
      for (final change in [
        {'client_msg_id': 'not-a-uuid'},
        {'client_msg_id': '019a0c00-0000-4000-8000-000000000001'},
        {'conversation_id': 'other'},
        {'acting_membership_id': 'other'},
        {'generation': -1},
        {'text': '\u0000'},
        {'text': '中' * 6000},
        {'state': 'sent'},
        {'created_at': 'local clock'},
      ]) {
        expect(
          () => PendingSend.fromJson({...valid, ...change}),
          throwsFormatException,
        );
      }
    },
  );
}
