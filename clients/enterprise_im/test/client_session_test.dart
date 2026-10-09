import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/auth/auth_session.dart';
import 'package:enterprise_im/core/auth/account_reference.dart';
import 'package:enterprise_im/core/auth/client_session.dart';
import 'package:enterprise_im/core/auth/session_store.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/storage/secure_vault.dart';
import 'auth_test.dart' as oidc;
import 'session_bootstrap_test.dart' as bootstrap;
import 'session_store_test.dart' as disk;

Future<(Directory, disk.MemoryVault, SessionStore)> fixture() async {
  final root = await Directory.systemTemp.createTemp('im-client-session-test-');
  addTearDown(() => root.delete(recursive: true));
  final vault = disk.MemoryVault();
  // Seed using the same principal as the real NativeAuth/me response.
  final seeded = SessionStore(vault, directory: root);
  final auth = AuthSession(
    origin: bootstrap.account().origin,
    tenantId: bootstrap.tenant,
    userId: bootstrap.user,
    accessToken: 'old-memory-only',
    expiresAt: DateTime.utc(2026, 10, 9, 1),
    generation: 1,
    budget: const Duration(hours: 1),
  );
  await seeded.open(auth);
  await seeded.saveCredentials(auth, refreshToken: 'saved-refresh');
  await seeded.close();
  // No stale database owner survives preparation.
  return (root, vault, SessionStore(vault, directory: root));
}

void main() {
  test(
    'client restoration persists rotated refresh before exposing database and logout erases both',
    () async {
      final (root, vault, store) = await fixture();
      final entered = Completer<void>(), me = Completer<ApiResponse>();
      final native = bootstrap.authentication(
        oidc.FakeTransport((request, _) async {
          if (request.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (request.uri.path.endsWith('/refresh'))
            return oidc.response(oidc.tokens('rotated-refresh'));
          entered.complete();
          return me.future;
        }),
      );
      final owner = ClientSession(native, store, platform: 'macos');
      final restoring = owner.restore(bootstrap.account());
      await entered.future;
      expect(owner.current, isNull);
      me.complete(oidc.response(oidc.self));
      final current = await restoring;
      expect(owner.current, same(current));
      expect(current.session.userId, bootstrap.user);
      final bundle =
          jsonDecode(
                vault.values.entries
                    .singleWhere((e) => e.key.endsWith('/session'))
                    .value,
              )
              as Map;
      expect(bundle['refresh_token'], 'rotated-refresh');
      expect(vault.values.values.join(), isNot(contains('private-access')));
      await owner.logout();
      expect(owner.current, isNull);
      expect(vault.values, isEmpty);
      expect(root.listSync(), isEmpty);
    },
  );
  test(
    'vault write failure prevents authenticated client admission and preserves old bundle',
    () async {
      final (_, vault, store) = await fixture();
      final prior = Map<String, String>.from(vault.values);
      final native = bootstrap.authentication(
        oidc.FakeTransport((request, _) async {
          if (request.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (request.uri.path.endsWith('/refresh'))
            return oidc.response(oidc.tokens('rotated-but-not-saved'));
          vault.failWrites = true;
          return oidc.response(oidc.self);
        }),
      );
      final owner = ClientSession(native, store, platform: 'macos');
      await expectLater(
        owner.restore(bootstrap.account()),
        throwsA(isA<VaultFailure>()),
      );
      expect(owner.current, isNull);
      expect(native.session, isNull);
      expect(vault.values, prior);
      vault.failWrites = false;
      // The failed handoff must have released the database owner.
      final fresh = SessionStore(vault, directory: store.directory);
      final auth = AuthSession(
        origin: bootstrap.account().origin,
        tenantId: bootstrap.tenant,
        userId: bootstrap.user,
        accessToken: 'fresh',
        expiresAt: DateTime.utc(2026, 10, 9, 1),
        generation: 2,
        budget: const Duration(hours: 1),
      );
      await fresh.open(auth);
      await fresh.logout();
    },
  );
  test('logout during credential write cannot admit a late client', () async {
    final (_, vault, store) = await fixture();
    final native = bootstrap.authentication(
      oidc.FakeTransport((request, _) async {
        if (request.uri.path.endsWith('/config'))
          return oidc.response(oidc.config());
        if (request.uri.path.endsWith('/refresh'))
          return oidc.response(oidc.tokens('late-refresh'));
        return oidc.response(oidc.self);
      }),
    );
    vault.writeGate = Completer<void>();
    vault.entered = Completer<void>();
    final owner = ClientSession(native, store, platform: 'macos');
    final restoring = owner.restore(bootstrap.account());
    final checked = expectLater(restoring, throwsA(isA<AuthCancelled>()));
    await vault.entered!.future;
    final logout = owner.logout();
    expect(owner.current, isNull);
    vault.writeGate!.complete();
    await logout;
    await checked;
    expect(owner.current, isNull);
    expect(vault.values, isEmpty);
    expect(store.directory.listSync(), isEmpty);
  });
  test(
    'suspension closes current client but preserves saved refresh for restart',
    () async {
      final (_, vault, store) = await fixture();
      final native = bootstrap.authentication(
        oidc.FakeTransport((request, _) async {
          if (request.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (request.uri.path.endsWith('/refresh'))
            return oidc.response(oidc.tokens('rotated-refresh'));
          return oidc.response(oidc.self);
        }),
      );
      final owner = ClientSession(native, store, platform: 'macos');
      final current = await owner.restore(bootstrap.account());
      await owner.close();
      expect(owner.current, isNull);
      expect(vault.values, isNotEmpty);
      await expectLater(current.store.pendingSends(), throwsA(isA<Object>()));
      final restarted = ClientSession(
        bootstrap.authentication(
          oidc.FakeTransport((request, _) async {
            if (request.uri.path.endsWith('/config'))
              return oidc.response(oidc.config());
            if (request.uri.path.endsWith('/refresh'))
              return oidc.response(oidc.tokens('restart-refresh'));
            return oidc.response(oidc.self);
          }),
        ),
        SessionStore(vault, directory: store.directory),
        platform: 'macos',
      );
      await restarted.restore(AccountReference.fromSession(current.session));
      await restarted.logout();
    },
  );
  test(
    'invalid saved refresh deletes only that credential without opening the database',
    () async {
      final (_, vault, store) = await fixture();
      final previous =
          jsonDecode(
                vault.values.entries
                    .singleWhere((e) => e.key.endsWith('/session'))
                    .value,
              )
              as Map;
      final native = bootstrap.authentication(
        oidc.FakeTransport((request, _) async {
          if (request.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          return ApiResponse(
            401,
            utf8.encode('{"error_code":"reauthenticate"}'),
            {},
          );
        }),
      );
      final owner = ClientSession(native, store, platform: 'macos');
      await expectLater(
        owner.restore(bootstrap.account()),
        throwsA(isA<AuthFailure>()),
      );
      final current =
          jsonDecode(
                vault.values.entries
                    .singleWhere((e) => e.key.endsWith('/session'))
                    .value,
              )
              as Map;
      expect(current['refresh_token'], isNull);
      expect(current['key'], previous['key']);
      expect(owner.current, isNull);
      final cleanup = SessionStore(vault, directory: store.directory);
      final auth = AuthSession(
        origin: bootstrap.account().origin,
        tenantId: bootstrap.tenant,
        userId: bootstrap.user,
        accessToken: 'fresh',
        expiresAt: DateTime.utc(2026, 10, 9, 1),
        generation: 3,
        budget: const Duration(hours: 1),
      );
      await cleanup.open(auth);
      await cleanup.logout();
    },
  );
}
