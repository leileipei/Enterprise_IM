import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/auth/auth_session.dart';
import 'package:enterprise_im/core/auth/session_store.dart';
import 'package:enterprise_im/core/storage/secure_vault.dart';
import 'package:enterprise_im/core/storage/records.dart';
import 'package:enterprise_im/core/storage/encrypted_store.dart';

const tenant = '00000000-0000-4000-8000-000000000001';
const user = '00000000-0000-4000-8000-000000000002';
AuthSession authenticated({
  int generation = 1,
  Duration budget = const Duration(hours: 1),
}) => AuthSession(
  origin: Uri.parse('https://im.example'),
  tenantId: tenant,
  userId: user,
  accessToken: 'DO_NOT_PERSIST_ACCESS',
  expiresAt: DateTime.utc(2026, 10, 9, 1),
  generation: generation,
  budget: budget,
);

final class MemoryVault implements SecureVault {
  final values = <String, String>{};
  bool unavailable = false, failWrites = false;
  int reads = 0;
  Completer<void>? writeGate;
  Completer<void>? entered;
  @override
  Future<String?> read(String ns, String key) async {
    reads++;
    if (unavailable) throw const VaultFailure();
    return values['$ns/$key'];
  }

  @override
  Future<void> write(String ns, String key, String value) async {
    if (unavailable || failWrites) throw const VaultFailure();
    if (entered?.isCompleted == false) entered?.complete();
    await writeGate?.future;
    values['$ns/$key'] = value;
  }

  @override
  Future<void> delete(String ns, String key) async {
    if (unavailable) throw const VaultFailure();
    values.remove('$ns/$key');
  }
}

void main() {
  test('testRefreshRotationCrashDoesNotMixAccounts', () async {
    final root = await Directory.systemTemp.createTemp('im-session-test-');
    addTearDown(() => root.delete(recursive: true));
    final vault = MemoryVault();
    // Use one namespace store with the observable fake OS vault.
    final owner = SessionStore(vault, directory: root);
    final auth = authenticated();
    await owner.open(auth);
    await owner.saveCredentials(auth, refreshToken: 'refresh-old');
    final old = Map<String, String>.from(vault.values);
    vault.unavailable = true;
    await expectLater(
      owner.saveCredentials(auth, refreshToken: 'refresh-new'),
      throwsA(isA<VaultFailure>()),
    );
    expect(vault.values, old);
    expect(owner.refreshToken, 'refresh-old');
    vault.unavailable = false;
    final other = AuthSession(
      origin: auth.origin,
      tenantId: tenant,
      userId: tenant,
      accessToken: 'other',
      expiresAt: auth.expiresAt,
      generation: 1,
      budget: const Duration(hours: 1),
    );
    await expectLater(
      owner.saveCredentials(other, refreshToken: 'other-account'),
      throwsA(isA<AuthFailure>()),
    );
    expect(vault.values, old);
    await owner.saveCredentials(auth, refreshToken: 'refresh-new');
    final oldKey = jsonDecode(old.values.single)['key'];
    expect(jsonDecode(vault.values.values.single)['key'], oldKey);
    await owner.close();
    final restarted = SessionStore(vault, directory: root);
    await restarted.open(authenticated(generation: 2));
    expect(restarted.refreshToken, 'refresh-new');
    await restarted.logout();
  });
  test(
    'logout blocks database results immediately during a pending credential rotation',
    () async {
      final root = await Directory.systemTemp.createTemp('im-session-test-');
      addTearDown(() => root.delete(recursive: true));
      final vault = MemoryVault(),
          sessions = SessionStore(vault, directory: root);
      final auth = authenticated();
      final store = await sessions.open(auth);
      vault.writeGate = Completer<void>();
      vault.entered = Completer<void>();
      final rotating = sessions.saveCredentials(
        auth,
        refreshToken: 'late-refresh',
      );
      final rotationCheck = expectLater(
        rotating,
        throwsA(isA<AuthCancelled>()),
      );
      await vault.entered!.future;
      final pending = store.putPendingSend(
        PendingSend(
          clientMsgId: '019a0c00-0000-7000-8000-000000000001',
          conversationId: tenant,
          actingMembershipId: user,
          text: 'late',
          generation: 1,
          createdAt: DateTime.utc(2026, 10, 9),
        ),
      );
      final outcome = pending.then<Object?>(
        (_) => null,
        onError: (Object error) => error,
      );
      final logout = sessions.logout();
      final result = await outcome;
      vault.writeGate!.complete();
      await logout;
      await rotationCheck;
      expect(result, isA<StoreFailure>());
      expect(vault.values, isEmpty);
      expect(root.listSync(), isEmpty);
    },
  );

  test('testRestartRequiresFreshAuthBeforeDatabaseOpen', () async {
    final root = await Directory.systemTemp.createTemp('im-session-test-');
    addTearDown(() => root.delete(recursive: true));
    final vault = MemoryVault();
    final sessions = SessionStore(vault, directory: root);
    await expectLater(
      sessions.open(authenticated(budget: Duration.zero)),
      throwsA(isA<AuthFailure>()),
    );
    expect(vault.reads, 0);
    expect(root.listSync(), isEmpty);
    final first = await sessions.open(authenticated());
    await first.putPendingSend(
      PendingSend(
        clientMsgId: '019a0c00-0000-7000-8000-000000000001',
        conversationId: tenant,
        actingMembershipId: user,
        text: 'restart payload',
        generation: 1,
        createdAt: DateTime.utc(2026, 10, 9),
      ),
    );
    final saved = Map<String, String>.from(vault.values);
    expect(saved.values.join(), isNot(contains('DO_NOT_PERSIST_ACCESS')));
    await sessions.close();
    final restarted = SessionStore(vault, directory: root);
    final second = await restarted.open(authenticated(generation: 2));
    addTearDown(restarted.logout);
    expect(
      (await second.pendingSends()).single.clientMsgId,
      '019a0c00-0000-7000-8000-000000000001',
    );
    expect(
      await second.readAuthorizedMessages(LeaseClock.unauthorized()),
      isEmpty,
    );
    expect(vault.values, saved);
  });
  test(
    'missing key erases unreadable ciphertext while vault failure preserves it',
    () async {
      final root = await Directory.systemTemp.createTemp('im-session-test-');
      addTearDown(() => root.delete(recursive: true));
      final vault = MemoryVault();
      final sessions = SessionStore(vault, directory: root);
      final first = await sessions.open(authenticated());
      await first.putPendingSend(
        PendingSend(
          clientMsgId: '019a0c00-0000-7000-8000-000000000001',
          conversationId: tenant,
          actingMembershipId: user,
          text: 'old',
          generation: 1,
          createdAt: DateTime.utc(2026, 10, 9),
        ),
      );
      await sessions.close();
      final ns = await accountNamespace(
        Uri.parse('https://im.example'),
        tenant,
        user,
      );
      final database = File('${root.path}/$ns.sqlite');
      final before = await database.readAsBytes();
      vault.unavailable = true;
      await expectLater(
        SessionStore(vault, directory: root).open(authenticated(generation: 2)),
        throwsA(isA<VaultFailure>()),
      );
      expect(await database.readAsBytes(), before);
      vault.unavailable = false;
      vault.values.clear();
      // Corrupt SQLite still must be removable without opening it with a new key.
      await database.writeAsBytes([1, 2, 3, 4]);
      final restarted = SessionStore(vault, directory: root);
      final second = await restarted.open(authenticated(generation: 2));
      addTearDown(restarted.logout);
      expect(await second.pendingSends(), isEmpty);
    },
  );
  test(
    'logout during secure write cannot leave late credentials or a live database',
    () async {
      final root = await Directory.systemTemp.createTemp('im-session-test-');
      addTearDown(() => root.delete(recursive: true));
      final vault = MemoryVault()
        ..writeGate = Completer<void>()
        ..entered = Completer<void>();
      final sessions = SessionStore(vault, directory: root);
      final opening = sessions.open(authenticated());
      final check = expectLater(opening, throwsA(isA<AuthCancelled>()));
      await vault.entered!.future;
      final logout = sessions.logout();
      vault.writeGate!.complete();
      await logout;
      await check;
      expect(vault.values, isEmpty);
      expect(root.listSync(), isEmpty);
    },
  );
  test(
    'a corrupt secure bundle never becomes plaintext fallback or a replacement key',
    () async {
      final root = await Directory.systemTemp.createTemp('im-session-test-');
      addTearDown(() => root.delete(recursive: true));
      final vault = MemoryVault();
      final ns = await accountNamespace(
        Uri.parse('https://im.example'),
        tenant,
        user,
      );
      vault.values['$ns/session'] = jsonEncode({'key': 'bad'});
      await expectLater(
        SessionStore(vault, directory: root).open(authenticated()),
        throwsA(isA<VaultFailure>()),
      );
      expect(root.listSync(), isEmpty);
      expect(vault.values['$ns/session'], jsonEncode({'key': 'bad'}));
    },
  );
}
