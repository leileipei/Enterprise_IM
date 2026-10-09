import 'dart:async';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/auth/auth_session.dart';
import 'package:enterprise_im/core/auth/account_selector.dart';
import 'package:enterprise_im/core/auth/session_store.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/storage/secure_vault.dart';
import 'package:enterprise_im/ui/login_page.dart';
import 'auth_test.dart' as oidc;
import 'login_session_test.dart' show pumpUntil;
import 'session_bootstrap_test.dart' as bootstrap;
import 'session_store_test.dart' as disk;

Future<(Directory, disk.MemoryVault, SessionStore)> seed() async {
  final root = await Directory.systemTemp.createTemp('im-login-startup-test-');
  final vault = disk.MemoryVault();
  final store = SessionStore(vault, directory: root);
  final identity = AuthSession(
    origin: bootstrap.account().origin,
    tenantId: bootstrap.tenant,
    userId: bootstrap.user,
    accessToken: 'seed-memory-only',
    expiresAt: DateTime.utc(2026, 10, 9, 1),
    generation: 1,
    budget: const Duration(hours: 1),
  );
  await store.open(identity);
  await store.saveCredentials(identity, refreshToken: 'restart-refresh');
  await AccountSelector(vault).save(bootstrap.account());
  await store.close();
  return (root, vault, SessionStore(vault, directory: root));
}

LoginPage login(SessionStore store, ApiTransport transport) => LoginPage(
  platform: 'macos',
  callbackFactory: (_, __) =>
      throw StateError('restore must not open callback'),
  browser: oidc.FakeBrowser(
    (_) => throw StateError('restore must not launch browser'),
  ),
  storeFactory: () async => store,
  apiFactory: (origin) => ApiClient(origin, transport),
);
void cleanup(
  WidgetTester tester,
  Directory root,
  SessionStore store, {
  Completer<ApiResponse>? me,
}) {
  addTearDown(() async {
    if (me?.isCompleted == false) me!.complete(oidc.response(oidc.self));
    await tester.pumpWidget(const SizedBox());
    var closed = false;
    final closing = store.close().whenComplete(() => closed = true);
    await pumpUntil(tester, () => closed);
    await closing;
    await tester.runAsync(() => root.delete(recursive: true));
  });
}

void main() {
  testWidgets(
    'cold start displays login only after refreshed identity and vault handoff',
    (tester) async {
      final (root, vault, store) = (await tester.runAsync(seed))!;
      final me = Completer<ApiResponse>(), entered = Completer<void>();
      cleanup(tester, root, store, me: me);
      await tester.pumpWidget(
        MaterialApp(
          home: login(
            store,
            oidc.FakeTransport((request, _) async {
              if (request.uri.path.endsWith('/config'))
                return oidc.response(oidc.config());
              if (request.uri.path.endsWith('/refresh'))
                return oidc.response(oidc.tokens('rotated-cold-refresh'));
              expect(request.uri.path, '/api/v1/me');
              entered.complete();
              return me.future;
            }),
          ),
        ),
      );
      await pumpUntil(tester, () => entered.isCompleted);
      expect(find.text('登录成功'), findsNothing);
      expect(
        tester.widget<TextField>(find.byType(TextField)).controller!.text,
        'https://im.example',
      );
      me.complete(oidc.response(oidc.self));
      await pumpUntil(tester, () => find.text('登录成功').evaluate().isNotEmpty);
      expect(vault.values.values.join(), contains('rotated-cold-refresh'));
      expect(vault.values.values.join(), isNot(contains('private-access')));
      await tester.tap(find.text('退出登录'));
      await pumpUntil(tester, () => find.text('已退出登录。').evaluate().isNotEmpty);
      expect(vault.values.isEmpty, isTrue);
      expect(root.listSync().isEmpty, isTrue);
    },
  );
  testWidgets(
    'unavailable startup vault never performs authentication or opens DB',
    (tester) async {
      final (root, vault, store) = (await tester.runAsync(seed))!;
      cleanup(tester, root, store);
      vault.unavailable = true;
      var requests = 0;
      await tester.pumpWidget(
        MaterialApp(
          home: login(
            store,
            oidc.FakeTransport((_, __) async {
              requests++;
              return oidc.response(oidc.self);
            }),
          ),
        ),
      );
      await pumpUntil(
        tester,
        () => find.text('无法读取上次账号，请检查系统安全存储后重试。').evaluate().isNotEmpty,
      );
      expect(find.text('登录成功'), findsNothing);
      expect(requests, 0);
      vault.unavailable = false;
    },
  );
  testWidgets(
    'cancelled cold-start identity cannot log in or retain the selected account',
    (tester) async {
      final (root, vault, store) = (await tester.runAsync(seed))!;
      final me = Completer<ApiResponse>(), entered = Completer<void>();
      cleanup(tester, root, store, me: me);
      await tester.pumpWidget(
        MaterialApp(
          home: login(
            store,
            oidc.FakeTransport((request, _) async {
              if (request.uri.path.endsWith('/config'))
                return oidc.response(oidc.config());
              if (request.uri.path.endsWith('/refresh'))
                return oidc.response(oidc.tokens('late-cold-refresh'));
              entered.complete();
              return me.future;
            }),
          ),
        ),
      );
      await pumpUntil(tester, () => entered.isCompleted);
      await tester.tap(find.text('取消登录'));
      me.complete(oidc.response(oidc.self));
      await pumpUntil(tester, () => vault.values.isEmpty);
      expect(find.text('登录成功'), findsNothing);
      expect(root.listSync().isEmpty, isTrue);
    },
  );
  testWidgets(
    'failed cold-start recovery is retryable only for its selected service',
    (tester) async {
      final (root, _, store) = (await tester.runAsync(seed))!;
      cleanup(tester, root, store);
      var fail = true;
      await tester.pumpWidget(
        MaterialApp(
          home: login(
            store,
            oidc.FakeTransport((request, _) async {
              if (request.uri.path.endsWith('/config'))
                return oidc.response(oidc.config());
              if (request.uri.path.endsWith('/refresh')) {
                if (fail) return ApiResponse(503, [], {});
                return oidc.response(oidc.tokens('retried-refresh'));
              }
              return oidc.response(oidc.self);
            }),
          ),
        ),
      );
      await pumpUntil(
        tester,
        () =>
            find.text('恢复登录').evaluate().isNotEmpty &&
            tester
                    .widget<TextButton>(find.widgetWithText(TextButton, '恢复登录'))
                    .onPressed !=
                null,
      );
      expect(find.text('登录成功'), findsNothing);
      await tester.enterText(find.byType(TextField), 'https://other.example');
      await tester.pump();
      expect(
        tester
            .widget<TextButton>(find.widgetWithText(TextButton, '恢复登录'))
            .onPressed,
        isNull,
      );
      await tester.enterText(find.byType(TextField), 'https://im.example');
      await tester.pump();
      fail = false;
      await tester.tap(find.text('恢复登录'));
      await pumpUntil(tester, () => find.text('登录成功').evaluate().isNotEmpty);
    },
  );
  testWidgets(
    'temporary store factory failure can retry startup and authenticate',
    (tester) async {
      final (root, _, store) = (await tester.runAsync(seed))!;
      cleanup(tester, root, store);
      var attempts = 0;
      final transport = oidc.FakeTransport((request, _) async {
        if (request.uri.path.endsWith('/config'))
          return oidc.response(oidc.config());
        if (request.uri.path.endsWith('/refresh'))
          return oidc.response(oidc.tokens('factory-retry-refresh'));
        return oidc.response(oidc.self);
      });
      await tester.pumpWidget(
        MaterialApp(
          home: LoginPage(
            platform: 'macos',
            callbackFactory: (_, __) => throw StateError('no browser callback'),
            apiFactory: (origin) => ApiClient(origin, transport),
            storeFactory: () async {
              attempts++;
              if (attempts == 1) throw const VaultFailure();
              return store;
            },
          ),
        ),
      );
      await pumpUntil(
        tester,
        () => find.text('重试读取上次账号').evaluate().isNotEmpty,
      );
      await tester.tap(find.text('重试读取上次账号'));
      await pumpUntil(tester, () => find.text('登录成功').evaluate().isNotEmpty);
      expect(attempts, 2);
    },
  );
  for (final action in ['fresh login', 'refresh']) {
    testWidgets(
      'reauthentication clears successful UI while $action waits for new authorization',
      (tester) async {
        final (root, _, store) = (await tester.runAsync(seed))!;
        final blocked = Completer<ApiResponse>(), entered = Completer<void>();
        cleanup(tester, root, store, me: blocked);
        var reauthenticating = false;
        await tester.pumpWidget(
          MaterialApp(
            home: login(
              store,
              oidc.FakeTransport((request, _) async {
                if (reauthenticating &&
                    (action == 'fresh login' &&
                            request.uri.path.endsWith('/config') ||
                        action == 'refresh' &&
                            request.uri.path.endsWith('/refresh'))) {
                  entered.complete();
                  return blocked.future;
                }
                if (request.uri.path.endsWith('/config'))
                  return oidc.response(oidc.config());
                if (request.uri.path.endsWith('/refresh'))
                  return oidc.response(oidc.tokens('fresh-ui-refresh'));
                return oidc.response(oidc.self);
              }),
            ),
          ),
        );
        await pumpUntil(tester, () => find.text('登录成功').evaluate().isNotEmpty);
        reauthenticating = true;
        await tester.tap(
          find.text(action == 'fresh login' ? '使用企业账号登录' : '恢复登录'),
        );
        await pumpUntil(tester, () => entered.isCompleted);
        expect(find.text('登录成功'), findsNothing);
      },
    );
  }
}
