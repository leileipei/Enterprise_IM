import 'dart:async';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/auth/account_selector.dart';
import 'package:enterprise_im/core/auth/client_session.dart';
import 'package:enterprise_im/core/auth/native_auth.dart';
import 'package:enterprise_im/core/auth/session_store.dart';
import 'package:enterprise_im/core/storage/secure_vault.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'auth_test.dart' as oidc;
import 'client_session_test.dart' as setup;
import 'session_bootstrap_test.dart' as bootstrap;

final class SelectorFault implements SecureVault {
  final SecureVault delegate;
  bool fail = false;
  Completer<void>? gate, entered;
  SelectorFault(this.delegate);
  @override
  Future<String?> read(String ns, String key) => delegate.read(ns, key);
  @override
  Future<void> delete(String ns, String key) => delegate.delete(ns, key);
  @override
  Future<void> write(String ns, String key, String value) async {
    if (key == 'account') {
      if (fail) throw const VaultFailure();
      if (entered?.isCompleted == false) entered!.complete();
      await gate?.future;
    }
    await delegate.write(ns, key, value);
  }
}

NativeAuth authentication({
  Completer<ApiResponse>? me,
  Completer<void>? entered,
}) => bootstrap.authentication(
  oidc.FakeTransport((request, _) async {
    if (request.uri.path.endsWith('/config'))
      return oidc.response(oidc.config());
    if (request.uri.path.endsWith('/refresh'))
      return oidc.response(oidc.tokens('new-refresh'));
    if (entered?.isCompleted == false) entered!.complete();
    return me?.future ?? oidc.response(oidc.self);
  }),
);

void main() {
  test(
    'admitted login remembers last account for a new owner and logout removes it',
    () async {
      final (_, vault, store) = await setup.fixture();
      final owner = ClientSession(authentication(), store, platform: 'macos');
      addTearDown(owner.logout);
      await owner.restore(bootstrap.account());
      await owner.close();
      final saved = await AccountSelector(vault).load();
      expect(saved, isNotNull);
      expect(saved!.userId, bootstrap.user);
      final next = ClientSession(
        authentication(),
        SessionStore(vault, directory: store.directory),
        platform: 'macos',
      );
      await next.restore(saved);
      await next.logout();
      expect(vault.values, isEmpty);
    },
  );
  test('selector write failure cannot publish authenticated client', () async {
    final (_, vault, store) = await setup.fixture();
    final failing = SelectorFault(vault)..fail = true;
    final auth = authentication();
    final owner = ClientSession(
      auth,
      SessionStore(failing, directory: store.directory),
      platform: 'macos',
    );
    addTearDown(() async {
      failing.fail = false;
      await owner.logout();
    });
    await expectLater(
      owner.restore(bootstrap.account()),
      throwsA(isA<VaultFailure>()),
    );
    expect(owner.current, isNull);
    expect(auth.session, isNull);
    failing.fail = false;
    await owner.logout();
  });
  test(
    'logout while selector write is delayed rejects admission and erases late reference',
    () async {
      final (_, vault, store) = await setup.fixture();
      final delayed = SelectorFault(vault)
        ..gate = Completer<void>()
        ..entered = Completer<void>();
      final owner = ClientSession(
        authentication(),
        SessionStore(delayed, directory: store.directory),
        platform: 'macos',
      );
      addTearDown(() async {
        if (delayed.gate?.isCompleted == false) delayed.gate!.complete();
        await owner.logout();
      });
      final restoring = owner.restore(bootstrap.account());
      final checked = expectLater(restoring, throwsA(isA<AuthCancelled>()));
      // Bound fixture wait so a missing handoff is an assertion, never a hang.
      await Future.any([
        delayed.entered!.future,
        Future<void>.delayed(const Duration(milliseconds: 500)),
      ]);
      expect(delayed.entered!.isCompleted, isTrue);
      final loggingOut = owner.logout();
      delayed.gate!.complete();
      await loggingOut;
      await checked;
      expect(vault.values.isEmpty, isTrue);
    },
  );
  test(
    'logout before restored identity arrives deletes the selected account without opening DB',
    () async {
      final (root, vault, store) = await setup.fixture();
      final me = Completer<ApiResponse>(), entered = Completer<void>();
      final owner = ClientSession(
        authentication(me: me, entered: entered),
        store,
        platform: 'macos',
      );
      addTearDown(owner.logout);
      final restoring = owner.restore(bootstrap.account());
      final checked = expectLater(restoring, throwsA(isA<AuthCancelled>()));
      await entered.future;
      await owner.logout();
      me.complete(oidc.response(oidc.self));
      await checked;
      expect(vault.values.isEmpty, isTrue);
      expect(root.listSync(), isEmpty);
    },
  );
  test(
    'failed logout remains retryable and removes account reference after vault recovers',
    () async {
      final (_, vault, store) = await setup.fixture();
      final owner = ClientSession(authentication(), store, platform: 'macos');
      addTearDown(() async {
        vault.unavailable = false;
        await owner.logout();
      });
      await owner.restore(bootstrap.account());
      vault.unavailable = true;
      await expectLater(owner.logout(), throwsA(isA<VaultFailure>()));
      expect(owner.current, isNull);
      vault.unavailable = false;
      await owner.logout();
      expect(vault.values.isEmpty, isTrue);
    },
  );
}
