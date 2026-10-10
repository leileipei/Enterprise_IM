// Run in a compiled desktop app. This file verifies the real native loopback
// listener only. OS minimization/focus + backend read restoration are separate
// required gates, not simulated here or inferred from this test's success.
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:enterprise_im/core/auth/auth_session.dart';
import 'package:enterprise_im/platform/login_callback.dart';

Future<void> _assertClosed(Uri uri) async {
  Socket? unexpected;
  try {
    unexpected = await Socket.connect(
      '127.0.0.1',
      uri.port,
      timeout: const Duration(seconds: 1),
    );
    fail('cancelled callback still accepts a connection');
  } on SocketException {
    // Connection refusal on the owned loopback port is expected.
  } finally {
    unexpected?.destroy();
  }
}

void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  testWidgets('testDesktopLoopbackCancelClosesListener', (tester) async {
    expect(Platform.isMacOS || Platform.isWindows, isTrue);
    await tester.pumpWidget(
      const MaterialApp(home: Scaffold(body: Text('桌面回调验收'))),
    );
    await tester.runAsync(() async {
      final callback = LoopbackLoginCallback('owned-desktop-state');
      try {
        final attempt = await callback.start();
        expect(attempt.redirectUri.host, '127.0.0.1');
        expect(attempt.redirectUri.path, '/oauth/callback');
        final cancelled = expectLater(
          attempt.callback,
          throwsA(isA<AuthCancelled>()),
        );
        await callback.close();
        await cancelled;
        await _assertClosed(attempt.redirectUri);
        await callback.close();
      } finally {
        await callback.close();
      }
    });
  });
  testWidgets('desktop cancellation cannot reopen an old callback', (
    tester,
  ) async {
    expect(Platform.isMacOS || Platform.isWindows, isTrue);
    await tester.runAsync(() async {
      final callback = LoopbackLoginCallback('owned-closed-state');
      await callback.close();
      await expectLater(
        callback.start(),
        throwsA(
          isA<AuthFailure>().having(
            (e) => e.code,
            'code',
            'callback_unavailable',
          ),
        ),
      );
    });
  });
}
