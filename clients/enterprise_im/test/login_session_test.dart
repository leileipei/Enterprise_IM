import 'dart:async';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/auth/session_store.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/ui/login_page.dart';
import 'auth_test.dart' as oidc;
import 'session_store_test.dart' as disk;

Future<void> pumpUntil(WidgetTester tester, bool Function() done) async {
  for (var i = 0; i < 100 && !done(); i++) {
    await tester.runAsync(
      () => Future<void>.delayed(const Duration(milliseconds: 10)),
    );
    await tester.pump();
  }
  expect(done(), isTrue);
}

LoginPage page(SessionStore sessions) {
  final callback = oidc.FakeCallback();
  return LoginPage(
    platform: 'macos',
    callbackFactory: (_, __) => callback,
    apiFactory: (origin) => ApiClient(
      origin,
      oidc.FakeTransport((request, _) async {
        if (request.uri.path.endsWith('/config'))
          return oidc.response(oidc.config());
        if (request.uri.path.endsWith('/token'))
          return oidc.response(oidc.tokens('stored-refresh'));
        return oidc.response(oidc.self);
      }),
    ),
    browser: oidc.FakeBrowser((uri) async {
      callback.result.complete(
        Uri.parse('http://127.0.0.1:49152/oauth/callback').replace(
          queryParameters: {
            'state': uri.queryParameters['state']!,
            'code': 'owned-code',
          },
        ),
      );
      return true;
    }),
    storeFactory: () async => sessions,
  );
}

void main() {
  testWidgets(
    'login success waits for secure handoff and logout deletes credentials and database',
    (tester) async {
      final root = await tester.runAsync(
        () => Directory.systemTemp.createTemp('im-login-session-test-'),
      );
      final vault = disk.MemoryVault()
        ..writeGate = Completer<void>()
        ..entered = Completer<void>();
      final sessions = SessionStore(vault, directory: root!);
      addTearDown(() async {
        if (vault.writeGate?.isCompleted == false) vault.writeGate!.complete();
        await tester.pumpWidget(const SizedBox());
        var closed = false;
        final closing = sessions.close().whenComplete(() => closed = true);
        await pumpUntil(tester, () => closed);
        await closing;
        await tester.runAsync(() => root.delete(recursive: true));
      });
      await tester.pumpWidget(MaterialApp(home: page(sessions)));
      await pumpUntil(
        tester,
        () =>
            tester.widget<FilledButton>(find.byType(FilledButton)).onPressed !=
            null,
      );
      await tester.enterText(find.byType(TextField), 'https://im.example');
      await tester.tap(find.text('使用企业账号登录'));
      await pumpUntil(tester, () => vault.entered!.isCompleted);
      expect(find.text('登录成功'), findsNothing);
      vault.writeGate!.complete();
      await pumpUntil(tester, () => find.text('登录成功').evaluate().isNotEmpty);
      expect(vault.values.values.join(), contains('stored-refresh'));
      expect(vault.values.values.join(), isNot(contains('private-access')));
      await tester.runAsync(() async {
        await tester.tap(find.text('退出登录'));
      });
      await pumpUntil(tester, () => find.text('已退出登录。').evaluate().isNotEmpty);
      expect(find.text('登录成功'), findsNothing);
      expect(vault.values, isEmpty);
      expect(root.listSync(), isEmpty);
    },
  );
  testWidgets(
    'unavailable system credential write never presents successful login',
    (tester) async {
      final root = await tester.runAsync(
        () => Directory.systemTemp.createTemp('im-login-session-test-'),
      );
      final vault = disk.MemoryVault()..failWrites = true;
      final sessions = SessionStore(vault, directory: root!);
      addTearDown(() async {
        if (vault.writeGate?.isCompleted == false) vault.writeGate!.complete();
        await tester.pumpWidget(const SizedBox());
        var closed = false;
        final closing = sessions.close().whenComplete(() => closed = true);
        await pumpUntil(tester, () => closed);
        await closing;
        await tester.runAsync(() => root.delete(recursive: true));
      });
      await tester.pumpWidget(MaterialApp(home: page(sessions)));
      await pumpUntil(
        tester,
        () =>
            tester.widget<FilledButton>(find.byType(FilledButton)).onPressed !=
            null,
      );
      await tester.enterText(find.byType(TextField), 'https://im.example');
      await tester.runAsync(() async {
        await tester.tap(find.text('使用企业账号登录'));
      });
      await pumpUntil(
        tester,
        () => find.text('系统安全存储不可用，登录未完成。请检查系统配置后重试。').evaluate().isNotEmpty,
      );
      expect(find.text('登录成功'), findsNothing);
      expect(vault.values, isEmpty);
      expect(root.listSync(), isEmpty);
    },
  );
}
