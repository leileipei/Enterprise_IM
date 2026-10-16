import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:sqlite3/sqlite3.dart';
import 'package:enterprise_im/core/protocol/models.dart';
import 'package:enterprise_im/core/protocol/seq.dart';
import 'package:enterprise_im/core/storage/encrypted_store.dart';
import 'package:enterprise_im/core/storage/records.dart';

const conversation = '00000000-0000-4000-8000-000000000001';
const membership = '00000000-0000-4000-8000-000000000002';
final server = DateTime.utc(2026, 10, 9);
SyncPage page({
  int seq = 1,
  bool zero = false,
  bool redacted = false,
  int revision = 1,
}) => SyncPage.fromJson({
  'conversation_id': conversation,
  'messages': [
    if (redacted)
      {'seq': '$seq', 'redacted': true}
    else
      {
        'seq': '$seq',
        'message_id': conversation,
        'sender_user_id': membership,
        'message_type': 'text',
        'text': 'PAGE_PRIVATE_PAYLOAD',
        'server_time': server.toIso8601String(),
        'content_expires_at': server
            .add(const Duration(hours: 1))
            .toIso8601String(),
        'cache_until': server
            .add(Duration(seconds: zero ? 0 : 300))
            .toIso8601String(),
      },
  ],
  'next_after_seq': '$seq',
  'receipt_revision': '$revision',
  'has_more': false,
  'server_time': server.toIso8601String(),
  'sync_proof': base64Url.encode(List<int>.filled(32, seq)).replaceAll('=', ''),
  'proof_expires_at': server
      .add(const Duration(seconds: 300))
      .toIso8601String(),
}, after: Seq.parse('${seq - 1}'));
PendingReceipt receipt(SyncPage p, {int generation = 1}) => PendingReceipt(
  conversationId: conversation,
  syncProof: p.syncProof!,
  nextAfterSeq: p.nextAfterSeq,
  proofExpiresAt: p.proofExpiresAt!,
  generation: generation,
);
LeaseClock grant() => LeaseClock.start(
  server,
  Duration.zero,
  server.add(const Duration(seconds: 300)),
  now: () => Duration.zero,
);
void main() {
  test(
    'redaction revokes old reading even when its disk transaction fails',
    () async {
      final root = await Directory.systemTemp.createTemp('im-page-test-');
      addTearDown(() => root.delete(recursive: true));
      final store = await EncryptedStore.open(
        'a' * 64,
        List<int>.filled(32, 5),
        directory: root,
        generation: 1,
      );
      addTearDown(store.close);
      final first = page(), clock = grant();
      await store.commitPage(
        first,
        receipt(first),
        actingMembershipId: membership,
        clock: clock,
      );
      final db = sqlite3.open('${root.path}/${'a' * 64}.sqlite');
      addTearDown(db.close);
      db.execute(
        "CREATE TRIGGER fail_receipt BEFORE INSERT ON records WHEN NEW.kind='receipt' BEGIN SELECT RAISE(ABORT,'controlled failure'); END",
      );
      final denied = page(redacted: true, revision: 2);
      await expectLater(
        store.commitPage(
          denied,
          receipt(denied),
          actingMembershipId: membership,
          clock: clock,
        ),
        throwsA(isA<StoreFailure>()),
      );
      expect(await store.readAuthorizedMessages(clock), isEmpty);
      expect((await store.committedCursor(conversation)).toString(), '1');
    },
  );

  test('testStoreCommitFailureDoesNotAdvanceCursor', () async {
    final root = await Directory.systemTemp.createTemp('im-page-test-');
    addTearDown(() => root.delete(recursive: true));
    final store = await EncryptedStore.open(
      'a' * 64,
      List<int>.filled(32, 5),
      directory: root,
      generation: 1,
    );
    addTearDown(store.close);
    final db = sqlite3.open('${root.path}/${'a' * 64}.sqlite');
    addTearDown(db.close);
    db.execute(
      "CREATE TRIGGER fail_receipt BEFORE INSERT ON records WHEN NEW.kind='receipt' BEGIN SELECT RAISE(ABORT,'controlled failure'); END",
    );
    final p = page(), clock = grant();
    await expectLater(
      store.commitPage(
        p,
        receipt(p),
        actingMembershipId: membership,
        clock: clock,
      ),
      throwsA(isA<StoreFailure>()),
    );
    expect(await store.committedCursor(conversation), isNull);
    expect(await store.pendingReceipts(), isEmpty);
    expect(await store.readAuthorizedMessages(clock), isEmpty);
    expect(db.select('SELECT count(*) AS n FROM records').single['n'], 0);
    db.execute('DROP TRIGGER fail_receipt');
    await store.commitPage(
      p,
      receipt(p),
      actingMembershipId: membership,
      clock: clock,
    );
    expect((await store.committedCursor(conversation)).toString(), '1');
    expect(await store.pendingReceipts(), hasLength(1));
    expect(
      (await store.readAuthorizedMessages(clock)).single.content['text'],
      'PAGE_PRIVATE_PAYLOAD',
    );
  });
  test(
    'all committed pages retain proofs and forged or restarted reading grants are refused',
    () async {
      final root = await Directory.systemTemp.createTemp('im-page-test-');
      addTearDown(() => root.delete(recursive: true));
      final key = List<int>.filled(32, 5);
      final store = await EncryptedStore.open(
        'a' * 64,
        key,
        directory: root,
        generation: 1,
      );
      final p = page(), clock = grant();
      await store.commitPage(
        p,
        receipt(p),
        actingMembershipId: membership,
        clock: clock,
      );
      final second = page(seq: 2);
      await store.commitPage(
        second,
        receipt(second),
        actingMembershipId: membership,
        clock: clock,
      );
      expect(await store.pendingReceipts(), hasLength(2));
      expect(await store.readAuthorizedMessages(grant()), isEmpty);
      expect(await store.readAuthorizedMessages(clock), hasLength(2));
      clock.invalidate();
      expect(await store.readAuthorizedMessages(clock), isEmpty);
      await store.close();
      final reopened = await EncryptedStore.open(
        'a' * 64,
        key,
        directory: root,
        generation: 2,
      );
      addTearDown(reopened.close);
      expect(await reopened.readAuthorizedMessages(grant()), isEmpty);
      expect((await reopened.committedCursor(conversation)).toString(), '2');
    },
  );
  test(
    'zero cache stores no body and a redacted reauthorization removes earlier ciphertext',
    () async {
      final root = await Directory.systemTemp.createTemp('im-page-test-');
      addTearDown(() => root.delete(recursive: true));
      final store = await EncryptedStore.open(
        'a' * 64,
        List<int>.filled(32, 5),
        directory: root,
        generation: 1,
      );
      addTearDown(store.close);
      final p = page(), clock = grant();
      await store.commitPage(
        p,
        receipt(p),
        actingMembershipId: membership,
        clock: clock,
      );
      final denied = page(redacted: true, revision: 2);
      await store.commitPage(
        denied,
        receipt(denied),
        actingMembershipId: membership,
        clock: clock,
      );
      expect(await store.readAuthorizedMessages(clock), isEmpty);
      final zero = page(seq: 2, zero: true, revision: 2);
      await store.commitPage(
        zero,
        receipt(zero),
        actingMembershipId: membership,
        clock: clock,
      );
      final db = sqlite3.open('${root.path}/${'a' * 64}.sqlite');
      addTearDown(db.close);
      expect(
        db
            .select("SELECT count(*) AS n FROM records WHERE kind='message'")
            .single['n'],
        0,
      );
      expect(await store.pendingReceipts(), hasLength(2));
    },
  );
  test(
    'a mismatched proof and noncontiguous page cannot overwrite a committed cursor',
    () async {
      final root = await Directory.systemTemp.createTemp('im-page-test-');
      addTearDown(() => root.delete(recursive: true));
      final store = await EncryptedStore.open(
        'a' * 64,
        List<int>.filled(32, 5),
        directory: root,
        generation: 1,
      );
      addTearDown(store.close);
      final p = page(), clock = grant();
      await expectLater(
        store.commitPage(
          p,
          receipt(p, generation: 2),
          actingMembershipId: membership,
          clock: clock,
        ),
        throwsA(isA<StoreFailure>()),
      );
      await store.commitPage(
        p,
        receipt(p),
        actingMembershipId: membership,
        clock: clock,
      );
      final gap = page(seq: 3);
      await expectLater(
        store.commitPage(
          gap,
          receipt(gap),
          actingMembershipId: membership,
          clock: clock,
        ),
        throwsA(isA<StoreFailure>()),
      );
      expect((await store.committedCursor(conversation)).toString(), '1');
    },
  );
  test(
    'historical replay still rejects membership and revision changes',
    () async {
      final root = await Directory.systemTemp.createTemp('im-page-scope-');
      addTearDown(() => root.delete(recursive: true));
      final store = await EncryptedStore.open(
        'a' * 64,
        List<int>.filled(32, 5),
        directory: root,
        generation: 1,
      );
      addTearDown(store.close);
      final first = page(revision: 5), clock = grant();
      await store.commitPage(
        first,
        receipt(first),
        actingMembershipId: membership,
        clock: clock,
      );
      await expectLater(
        store.commitPage(
          first,
          receipt(first),
          actingMembershipId: conversation,
          clock: clock,
        ),
        throwsA(isA<StoreFailure>()),
      );
      final older = page(revision: 4);
      await expectLater(
        store.commitPage(
          older,
          receipt(older),
          actingMembershipId: membership,
          clock: clock,
        ),
        throwsA(isA<StoreFailure>()),
      );
      expect(await store.committedCursor(conversation), Seq.parse('1'));
    },
  );
}
