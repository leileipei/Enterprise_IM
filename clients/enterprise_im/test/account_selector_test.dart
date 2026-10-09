import 'dart:async';
import 'dart:convert';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/auth/account_reference.dart';
import 'package:enterprise_im/core/auth/account_selector.dart';
import 'package:enterprise_im/core/storage/secure_vault.dart';
import 'session_store_test.dart' as disk;

AccountReference reference({String user = disk.user}) => AccountReference(
  origin: Uri.parse('https://im.example'),
  tenantId: disk.tenant,
  userId: user,
);

void main() {
  test(
    'restart reads only account metadata from vault and preserves other bundles',
    () async {
      final vault = disk.MemoryVault();
      vault.values['${'a' * 64}/session'] = 'unrelated-bundle';
      await AccountSelector(vault).save(reference());
      final restarted = await AccountSelector(vault).load();
      expect(restarted!.origin.origin, 'https://im.example');
      expect(restarted.tenantId, disk.tenant);
      expect(restarted.userId, disk.user);
      expect(vault.values['${'a' * 64}/session'], 'unrelated-bundle');
      final payload =
          jsonDecode(
                vault.values.entries
                    .singleWhere((e) => e.key.endsWith('/account'))
                    .value,
              )
              as Map;
      expect(payload.keys.toSet(), {
        'version',
        'origin',
        'tenant_id',
        'user_id',
      });
    },
  );
  test(
    'malformed selector is a closed error and is never treated as missing',
    () async {
      final vault = disk.MemoryVault();
      final selector = AccountSelector(vault);
      await selector.save(reference());
      final key = vault.values.keys.single;
      for (final raw in [
        '{',
        '{"version":1,"origin":"http://im.example"}',
        jsonEncode({
          'version': 1,
          'origin': 'https://im.example',
          'tenant_id': disk.tenant,
          'user_id': disk.user,
          'access_token': 'must-reject',
        }),
        'x' * 4097,
      ]) {
        vault.values[key] = raw;
        await expectLater(selector.load(), throwsA(isA<VaultFailure>()));
        expect(vault.values[key], raw);
      }
    },
  );
  test(
    'unavailable vault never supplies an account or deletes its bundle',
    () async {
      final vault = disk.MemoryVault();
      final selector = AccountSelector(vault);
      await selector.save(reference());
      final prior = Map<String, String>.from(vault.values);
      vault.unavailable = true;
      await expectLater(selector.load(), throwsA(isA<VaultFailure>()));
      expect(vault.values, prior);
    },
  );
  test(
    'logout for prior account cannot remove a later selected account',
    () async {
      final vault = disk.MemoryVault();
      final first = AccountSelector(vault), second = AccountSelector(vault);
      await first.save(reference());
      const other = '00000000-0000-4000-8000-000000000003';
      await second.save(reference(user: other));
      await first.forget(reference());
      expect((await second.load())!.userId, other);
      await second.forget(reference(user: other));
      expect(await second.load(), isNull);
    },
  );
  test('late selector write is serialized before logout removes it', () async {
    final vault = disk.MemoryVault()
      ..writeGate = Completer<void>()
      ..entered = Completer<void>();
    final first = AccountSelector(vault), second = AccountSelector(vault);
    final saving = first.save(reference());
    await vault.entered!.future;
    final forgetting = second.forget(reference());
    vault.writeGate!.complete();
    await saving;
    await forgetting;
    expect(await second.load(), isNull);
  });
  test('unrelated vaults do not share a blocked selection queue', () async {
    final firstVault = disk.MemoryVault(), secondVault = disk.MemoryVault();
    final first = AccountSelector(firstVault),
        second = AccountSelector(secondVault);
    await second.save(reference());
    firstVault.writeGate = Completer<void>();
    firstVault.entered = Completer<void>();
    final blocked = first.save(reference());
    await firstVault.entered!.future;
    var completed = false;
    final loading = second.load().then((account) {
      completed = true;
      return account;
    });
    try {
      await Future<void>.delayed(const Duration(milliseconds: 100));
      expect(completed, isTrue);
      expect((await loading)!.userId, disk.user);
    } finally {
      firstVault.writeGate!.complete();
      await blocked;
      await loading;
    }
  });
}
