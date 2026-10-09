import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/auth/account_reference.dart';
import 'package:enterprise_im/core/auth/native_auth.dart';
import 'package:enterprise_im/core/auth/session_store.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'auth_test.dart' as oidc;
import 'session_store_test.dart' as disk;

const tenant = '11111111-1111-4111-8111-111111111111';
const user = '22222222-2222-4222-8222-222222222222';
AccountReference account() => AccountReference(
  origin: Uri.parse('https://im.example'),
  tenantId: tenant,
  userId: user,
);
NativeAuth authentication(ApiTransport transport) => NativeAuth(
  ApiClient(Uri.parse('https://im.example'), transport),
  callbackFactory: (_, __) => throw StateError('restore cannot open callback'),
  browser: oidc.FakeBrowser(
    (_) => throw StateError('restore cannot launch browser'),
  ),
);

void main() {
  test(
    'restart refresh admits no session before fresh me and exposes rotated credential',
    () async {
      final entered = Completer<void>(), me = Completer<ApiResponse>();
      final transport = oidc.FakeTransport((request, _) async {
        if (request.uri.path.endsWith('/config'))
          return oidc.response(oidc.config());
        if (request.uri.path.endsWith('/refresh')) {
          final body = jsonDecode(utf8.decode(request.body!)) as Map;
          expect(body, {'platform': 'macos', 'refresh_token': 'saved-refresh'});
          return oidc.response(oidc.tokens('rotated-refresh'));
        }
        if (request.uri.path == '/api/v1/me') {
          expect(request.headers['Authorization'], 'Bearer private-access');
          entered.complete();
          return me.future;
        }
        throw StateError('unexpected endpoint');
      });
      final auth = authentication(transport);
      addTearDown(auth.cancel);
      final restoring = auth.restore(account(), 'macos', 'saved-refresh');
      await entered.future;
      expect(auth.session, isNull);
      me.complete(oidc.response(oidc.self));
      final verified = await restoring;
      expect(verified.userId, user);
      expect(verified.tenantId, tenant);
      expect(auth.session, same(verified));
      expect(auth.refreshCredential, 'rotated-refresh');
      expect(verified.toString(), isNot(contains('access')));
    },
  );
  test(
    'restart rejects changed principal and never exposes refreshed credentials',
    () async {
      final auth = authentication(
        oidc.FakeTransport((request, _) async {
          if (request.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (request.uri.path.endsWith('/refresh'))
            return oidc.response(oidc.tokens('other-secret'));
          return oidc.response({...oidc.self, 'user_id': tenant});
        }),
      );
      await expectLater(
        auth.restore(account(), 'macos', 'saved-refresh'),
        throwsA(
          isA<AuthFailure>().having((e) => e.code, 'code', 'identity_changed'),
        ),
      );
      expect(auth.session, isNull);
      expect(auth.refreshCredential, isNull);
      expect(auth.canRefresh, isFalse);
    },
  );
  test(
    'invalid saved refresh requires login and cancellation rejects late me',
    () async {
      var revoked = true;
      final entered = Completer<void>(), late = Completer<ApiResponse>();
      final auth = authentication(
        oidc.FakeTransport((request, _) async {
          if (request.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (request.uri.path.endsWith('/refresh')) {
            if (revoked)
              return ApiResponse(
                401,
                utf8.encode('{"error_code":"reauthenticate"}'),
                {},
              );
            return oidc.response(oidc.tokens('rotated'));
          }
          entered.complete();
          return late.future;
        }),
      );
      await expectLater(
        auth.restore(account(), 'macos', 'saved'),
        throwsA(
          isA<AuthFailure>().having((e) => e.code, 'code', 'reauthenticate'),
        ),
      );
      expect(auth.session, isNull);
      revoked = false;
      final restoring = auth.restore(account(), 'macos', 'saved');
      final checked = expectLater(restoring, throwsA(isA<AuthCancelled>()));
      await entered.future;
      await auth.cancel();
      late.complete(oidc.response(oidc.self));
      await checked;
      expect(auth.session, isNull);
      expect(auth.refreshCredential, isNull);
    },
  );
  test(
    'disabled refresh and another service cannot restore a saved account',
    () async {
      final auth = authentication(
        oidc.FakeTransport(
          (request, _) async =>
              oidc.response({...oidc.config(), 'refresh_enabled': false}),
        ),
      );
      await expectLater(
        auth.restore(account(), 'macos', 'saved'),
        throwsA(isA<AuthFailure>()),
      );
      expect(auth.canRefresh, isFalse);
      await expectLater(
        auth.restore(
          AccountReference(
            origin: Uri.parse('https://another.example'),
            tenantId: tenant,
            userId: user,
          ),
          'macos',
          'saved',
        ),
        throwsA(isA<AuthFailure>()),
      );
      expect(auth.session, isNull);
    },
  );
  test(
    'reading saved refresh never opens the account database before authentication',
    () async {
      final root = await Directory.systemTemp.createTemp('im-bootstrap-test-');
      addTearDown(() => root.delete(recursive: true));
      final vault = disk.MemoryVault();
      final seeded = SessionStore(vault, directory: root);
      final verified = disk.authenticated();
      await seeded.open(verified);
      await seeded.saveCredentials(verified, refreshToken: 'saved-only');
      await seeded.close();
      final loader = SessionStore(vault, directory: root);
      final reference = AccountReference.fromSession(verified);
      expect(await loader.loadRefresh(reference), 'saved-only');
      // A separate freshly authenticated owner still obtains the exclusive worker.
      final fresh = SessionStore(vault, directory: root);
      await fresh.open(disk.authenticated(generation: 2));
      expect(fresh.refreshToken, 'saved-only');
      await fresh.close();
      vault.unavailable = true;
      await expectLater(loader.loadRefresh(reference), throwsA(isA<Object>()));
      vault.unavailable = false;
      final erased = SessionStore(vault, directory: root);
      await erased.open(disk.authenticated(generation: 3));
      await erased.logout();
      expect(root.listSync(), isEmpty);
      expect(vault.values, isEmpty);
    },
  );
}
