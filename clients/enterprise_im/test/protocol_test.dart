import 'dart:convert';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/seq.dart';
import 'package:enterprise_im/core/protocol/models.dart';

const id = '11111111-1111-4111-8111-111111111111';
Map<String, Object?> message(String seq) => {
  'seq': seq,
  'message_id': id,
  'sender_user_id': id,
  'message_type': 'text',
  'text': '消息',
  'server_time': '2026-10-09T00:00:00Z',
  'content_expires_at': '2026-10-09T01:00:00Z',
  'cache_until': '2026-10-09T00:05:00Z',
};
Map<String, Object?> page() => {
  'conversation_id': id,
  'messages': [message('9007199254740993')],
  'next_after_seq': '9007199254740993',
  'has_more': false,
  'receipt_revision': '2',
  'sync_proof': base64Url.encode(List<int>.filled(32, 7)).replaceAll('=', ''),
  'proof_expires_at': '2026-10-09T00:05:00Z',
  'server_time': '2026-10-09T00:00:00Z',
};
void main() {
  test('testProtocolInt64AndBadDTO', () {
    for (final value in ['0', '9007199254740993', '9223372036854775807']) {
      expect(Seq.parse(value).toString(), value);
    }
    for (final value in [
      '',
      '00',
      '01',
      '+1',
      '-1',
      '1.0',
      '1e3',
      ' 1',
      '9223372036854775808',
    ]) {
      expect(() => Seq.parse(value), throwsFormatException);
    }
    final parsed = SyncPage.fromJson(
      page(),
      after: Seq.parse('9007199254740992'),
    );
    expect(parsed.messages.single.seq.toString(), '9007199254740993');
    for (final bad in <Map<String, Object?>>[
      {...page(), 'next_after_seq': '9007199254740994'},
      {
        ...page(),
        'messages': [message('9007199254740994')],
      },
      {
        ...page(),
        'messages': [
          {...message('9007199254740993'), 'seq': 9007199254740993},
        ],
      },
      {...page(), 'sync_proof': null},
      {...page(), 'proof_expires_at': '2026-10-08T23:59:59Z'},
      {
        ...page(),
        'messages': [
          {
            ...message('9007199254740993'),
            'cache_until': '2026-10-09T02:00:00Z',
          },
        ],
      },
      {
        ...page(),
        'messages': [
          {
            ...message('9007199254740993'),
            'server_time': '2026-02-31T00:00:00Z',
          },
        ],
      },
    ]) {
      expect(
        () => SyncPage.fromJson(bad, after: Seq.parse('9007199254740992')),
        throwsFormatException,
      );
    }
    final ack =
        jsonDecode(
              '{"message_id":"$id","conversation_id":"$id","seq":9007199254740993,"server_time":"2026-10-09T00:00:00Z","duplicate":false}',
            )
            as Map<String, Object?>;
    expect(MessageACK.fromJson(ack).seq.toString(), '9007199254740993');
    expect(
      () => MessageACK.fromJson({...ack, 'seq': 9007199254740993.0}),
      throwsFormatException,
    );
    expect(
      () => MessageACK.fromJson({...ack, 'seq': -1}),
      throwsFormatException,
    );
    expect(
      () => MessageDTO.fromJson({'seq': '1', 'redacted': true, 'text': '泄漏'}),
      throwsFormatException,
    );
    final online = {
      ...message('1'),
      'cache_until': '2026-10-09T00:00:00Z',
      'online_until': '2026-10-09T00:00:05Z',
    };
    final live = SyncPage.fromJson({
      ...page(),
      'messages': [online],
      'next_after_seq': '1',
    }, after: Seq.parse('0'));
    expect(
      live.messages.single.onlineUntil,
      DateTime.utc(2026, 10, 9, 0, 0, 5),
    );
    expect(
      () => SyncPage.fromJson({
        ...page(),
        'messages': [
          {...online, 'online_until': '2026-10-09T00:00:06Z'},
        ],
        'next_after_seq': '1',
      }, after: Seq.parse('0')),
      throwsFormatException,
    );
  });
  test(
    'maximum valid server text uses UTF8 bytes rather than an invented shorter limit',
    () {
      final full = '&' * 16384;
      expect(MessageDTO.fromJson({...message('1'), 'text': full}).text, full);
      expect(
        () => MessageDTO.fromJson({...message('1'), 'text': '中' * 5462}),
        throwsFormatException,
      );
    },
  );
}
