import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';
import 'package:sqlite3/sqlite3.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/storage/encrypted_store.dart';
import 'package:enterprise_im/core/storage/records.dart';

// Deterministic UUIDv7 fixtures only; the product generator belongs to Task5.
PendingSend pendingFixture(Map<String, Object?> value) {
  var number = 0;
  for (final byte in utf8.encode(value['client_msg_id']! as String)) {
    number = (number * 31 + byte) & 0xffffffffffff;
  }
  return PendingSend.fromJson({
    ...value,
    'client_msg_id':
        '019a0c00-0000-7000-8000-${number.toRadixString(16).padLeft(12, '0')}',
    'conversation_id': '00000000-0000-4000-8000-000000000001',
    'acting_membership_id': '00000000-0000-4000-8000-000000000002',
    'created_at': '2026-10-09T00:00:00.000Z',
    'state': 'pending',
  });
}

void main() {
  test('testEncryptedStoreHasNoPlaintextInDatabaseOrWal', () async {
    final root = await Directory.systemTemp.createTemp('im-encryption-test-');
    addTearDown(() => root.delete(recursive: true));
    final store = await EncryptedStore.open(
      'a' * 64,
      List<int>.generate(32, (i) => i),
      directory: root,
      generation: 7,
    );
    addTearDown(store.close);
    await store.putPendingSend(
      pendingFixture({
        'client_msg_id': 'known-sensitive-id',
        'text': 'VERY_PRIVATE_UNICODE_企业正文',
        'generation': 7,
      }),
    );
    final sends = await store.pendingSends();
    expect(sends.single.text, 'VERY_PRIVATE_UNICODE_企业正文');
    for (final entity in root.listSync()) {
      if (entity is File) {
        final bytes = await entity.readAsBytes();
        final raw = latin1.decode(bytes);
        expect(raw, isNot(contains('VERY_PRIVATE')));
        expect(raw, isNot(contains(sends.single.clientMsgId)));
        expect(raw, isNot(contains(sends.single.actingMembershipId)));
      }
    }
  });
  test('testNamespaceSwapAndTamperRejected', () async {
    final root = await Directory.systemTemp.createTemp('im-encryption-test-');
    addTearDown(() => root.delete(recursive: true));
    final key = List<int>.generate(32, (i) => i);
    final store = await EncryptedStore.open(
      'a' * 64,
      key,
      directory: root,
      generation: 1,
    );
    await store.putPendingSend(
      pendingFixture({
        'client_msg_id': 'pending-original',
        'text': 'secret',
        'generation': 1,
      }),
    );
    await store.close();
    final old = File('${root.path}/${'a' * 64}.sqlite');
    await old.copy('${root.path}/${'b' * 64}.sqlite');
    final swapped = await EncryptedStore.open(
      'b' * 64,
      key,
      directory: root,
      generation: 1,
    );
    addTearDown(swapped.close);
    await expectLater(swapped.pendingSends(), throwsA(isA<StoreFailure>()));
  });
  test('testLogoutCancelsLateWritesAndErasesKeyDatabase', () async {
    final root = await Directory.systemTemp.createTemp('im-encryption-test-');
    addTearDown(() => root.delete(recursive: true));
    final store = await EncryptedStore.open(
      'a' * 64,
      List<int>.filled(32, 1),
      directory: root,
      generation: 1,
    );
    final pending = store.putPendingSend(
      pendingFixture({'client_msg_id': 'pending', 'text': 'body', 'generation': 1}),
    );
    final check = expectLater(pending, throwsA(isA<StoreFailure>()));
    await store.closeAndErase();
    await check;
    expect(root.listSync(), isEmpty);
    await expectLater(store.pendingSends(), throwsA(isA<StoreFailure>()));
  });
  test('pending IDs are immutable and stale generation cannot write', () async {
    final root = await Directory.systemTemp.createTemp('im-encryption-test-');
    addTearDown(() => root.delete(recursive: true));
    final store = await EncryptedStore.open(
      'a' * 64,
      List<int>.filled(32, 1),
      directory: root,
      generation: 1,
    );
    addTearDown(store.close);
    await store.putPendingSend(
      pendingFixture({'client_msg_id': 'same-id', 'text': 'first', 'generation': 1}),
    );
    await expectLater(
      store.putPendingSend(
        pendingFixture({
          'client_msg_id': 'same-id',
          'text': 'changed',
          'generation': 1,
        }),
      ),
      throwsA(isA<StoreFailure>()),
    );
    await expectLater(
      store.putPendingSend(
        pendingFixture({'client_msg_id': 'late', 'text': 'body', 'generation': 0}),
      ),
      throwsA(isA<StoreFailure>()),
    );
    expect((await store.pendingSends()).single.text, 'first');
  });
  test('ciphertext tamper cannot become a readable pending message', () async {
    final root = await Directory.systemTemp.createTemp('im-encryption-test-');
    addTearDown(() => root.delete(recursive: true));
    final key = List<int>.filled(32, 7);
    final namespace = 'a' * 64;
    final store = await EncryptedStore.open(
      namespace,
      key,
      directory: root,
      generation: 1,
    );
    await store.putPendingSend(
      pendingFixture({
        'client_msg_id': 'tamper-target',
        'text': 'secret',
        'generation': 1,
      }),
    );
    await store.close();
    final db = sqlite3.open('${root.path}/$namespace.sqlite');
    final cipher = Uint8List.fromList(
      db.select('SELECT ciphertext FROM records').single['ciphertext']
          as Uint8List,
    );
    cipher[0] ^= 1;
    db.execute('UPDATE records SET ciphertext=?', [cipher]);
    db.close();
    final reopened = await EncryptedStore.open(
      namespace,
      key,
      directory: root,
      generation: 1,
    );
    addTearDown(reopened.close);
    await expectLater(reopened.pendingSends(), throwsA(isA<StoreFailure>()));
  });
  test('logout erases a repository even after ordinary close', () async {
    final root = await Directory.systemTemp.createTemp('im-encryption-test-');
    addTearDown(() => root.delete(recursive: true));
    final store = await EncryptedStore.open(
      'a' * 64,
      List<int>.filled(32, 9),
      directory: root,
      generation: 1,
    );
    await store.putPendingSend(
      pendingFixture({'client_msg_id': 'pending', 'text': 'body', 'generation': 1}),
    );
    await store.close();
    await store.closeAndErase();
    expect(root.listSync(), isEmpty);
  });

  test(
    'one active account database and old logout cannot erase a newer owner',
    () async {
      final root = await Directory.systemTemp.createTemp('im-encryption-test-');
      addTearDown(() => root.delete(recursive: true));
      final key = List<int>.filled(32, 9);
      final namespace = 'a' * 64;
      final old = await EncryptedStore.open(
        namespace,
        key,
        directory: root,
        generation: 1,
      );
      await expectLater(
        EncryptedStore.open(namespace, key, directory: root, generation: 2),
        throwsA(isA<StoreFailure>()),
      );
      await old.close();
      final fresh = await EncryptedStore.open(
        namespace,
        key,
        directory: root,
        generation: 2,
      );
      addTearDown(fresh.closeAndErase);
      await fresh.putPendingSend(
        pendingFixture({
          'client_msg_id': 'new-pending',
          'text': 'new body',
          'generation': 2,
        }),
      );
      await expectLater(old.closeAndErase(), throwsA(isA<StoreFailure>()));
      expect((await fresh.pendingSends()).single.text, 'new body');
    },
  );
  test(
    'an equivalent pending send is idempotent regardless of field order',
    () async {
      final root = await Directory.systemTemp.createTemp('im-encryption-test-');
      addTearDown(() => root.delete(recursive: true));
      final store = await EncryptedStore.open(
        'a' * 64,
        List<int>.filled(32, 1),
        directory: root,
        generation: 1,
      );
      addTearDown(store.closeAndErase);
      await store.putPendingSend(
        pendingFixture({
          'client_msg_id': 'same',
          'text': 'unchanged',
          'generation': 1,
        }),
      );
      await store.putPendingSend(
        pendingFixture({
          'generation': 1,
          'text': 'unchanged',
          'client_msg_id': 'same',
        }),
      );
      expect(await store.pendingSends(), hasLength(1));
    },
  );
  test(
    'concurrent logout requests erase one closed repository idempotently',
    () async {
      final root = await Directory.systemTemp.createTemp('im-encryption-test-');
      addTearDown(() => root.delete(recursive: true));
      final store = await EncryptedStore.open(
        'a' * 64,
        List<int>.filled(32, 2),
        directory: root,
        generation: 1,
      );
      await store.putPendingSend(
        pendingFixture({'client_msg_id': 'same', 'text': 'private', 'generation': 1}),
      );
      await store.close();
      await Future.wait([store.closeAndErase(), store.closeAndErase()]);
      expect(root.listSync(), isEmpty);
    },
  );
}
