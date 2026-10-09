import 'package:enterprise_im/core/auth/account_selector.dart';
import 'package:enterprise_im/core/auth/account_reference.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/storage/secure_vault.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  test(
    'account namespace canonicalizes service and isolates tenant and person',
    () async {
      const tenant = '00000000-0000-4000-8000-000000000001';
      const user = '00000000-0000-4000-8000-000000000002';
      final a = await accountNamespace(
        Uri.parse('https://im.example.test'),
        tenant,
        user,
      );
      expect(
        a,
        await accountNamespace(
          Uri.parse('https://im.example.test/'),
          tenant,
          user,
        ),
      );
      expect(a, matches(RegExp(r'^[a-f0-9]{64}$')));
      expect(
        a,
        isNot(
          await accountNamespace(
            Uri.parse('https://im.example.test:8443'),
            tenant,
            user,
          ),
        ),
      );
      expect(
        a,
        isNot(
          await accountNamespace(
            Uri.parse('https://im.example.test'),
            user,
            tenant,
          ),
        ),
      );
      await expectLater(
        accountNamespace(Uri.parse('http://im.example.test'), tenant, user),
        throwsFormatException,
      );
    },
  );
  test(
    'vault refuses reset fallback and preserves other account keys',
    () async {
      final stored = <String, String>{};
      final requests = <MethodCall>[];
      const channel = MethodChannel(
        'plugins.it_nomads.com/flutter_secure_storage',
      );
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(channel, (call) async {
            requests.add(call);
            final args = call.arguments as Map;
            final key = args['key'] as String;
            if (call.method == 'write') stored[key] = args['value'] as String;
            if (call.method == 'delete') stored.remove(key);
            if (call.method == 'read') return stored[key];
            return null;
          });
      addTearDown(
        () => TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
            .setMockMethodCallHandler(channel, null),
      );
      final vault = OsSecureVault();
      final a = 'a' * 64, b = 'b' * 64;
      await vault.write(a, 'session', 'secret-a');
      await vault.write(b, 'session', 'secret-b');
      await vault.delete(a, 'session');
      expect(await vault.read(a, 'session'), isNull);
      expect(await vault.read(b, 'session'), 'secret-b');
      final account = AccountReference(
        origin: Uri.parse('https://im.example'),
        tenantId: '00000000-0000-4000-8000-000000000001',
        userId: '00000000-0000-4000-8000-000000000002',
      );
      await AccountSelector(vault).save(account);
      expect(
        (await AccountSelector(OsSecureVault()).load())!.userId,
        '00000000-0000-4000-8000-000000000002',
      );
      await expectLater(
        vault.write(b, 'account', 'never'),
        throwsFormatException,
      );
      await AccountSelector(vault).forget(account);
      expect(stored.keys.toList(), ['enterprise-im-v1/$b/session']);
      for (final call in requests) {
        final args = call.arguments as Map;
        final options = args['options'] as Map;
        expect(options['resetOnError'], 'false');
      }
      await expectLater(
        vault.write('../other', 'session', 'never'),
        throwsFormatException,
      );
    },
  );
  test(
    'vault plugin failures are closed and never converted into missing credentials',
    () async {
      const channel = MethodChannel(
        'plugins.it_nomads.com/flutter_secure_storage',
      );
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(
            channel,
            (call) async => throw PlatformException(
              code: 'unavailable',
              message: 'private-key-detail',
            ),
          );
      addTearDown(
        () => TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
            .setMockMethodCallHandler(channel, null),
      );
      await expectLater(
        OsSecureVault().read('a' * 64, 'session'),
        throwsA(
          isA<VaultFailure>().having(
            (e) => e.toString(),
            'closed message',
            isNot(contains('private-key-detail')),
          ),
        ),
      );
    },
  );
}
