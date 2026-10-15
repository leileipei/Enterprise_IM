import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';

// Real Android engine and MainActivity bridge. No mocked channel or picker URL.
void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  const channel = MethodChannel('enterprise_im/file_export');
  testWidgets('native Android export rejects invalid leaf names', (
    tester,
  ) async {
    expect(Platform.isAndroid, isTrue);
    await tester.pumpWidget(
      const MaterialApp(home: Scaffold(body: Text('Android 导出边界验证'))),
    );
    // ignore: avoid_print
    print('ANDROID_EXPORT_PROBE runtime_pid=$pid');
    for (final name in ['', '../escape', 'a/b', 'a\u0000b']) {
      await expectLater(
        channel.invokeMethod<String>('beginExport', {'name': name}),
        throwsA(
          isA<PlatformException>().having(
            (e) => e.code,
            'code',
            'invalid_filename',
          ),
        ),
      );
    }
  });
  testWidgets('native Android refuses writes for unknown tickets', (
    tester,
  ) async {
    await expectLater(
      channel.invokeMethod<void>('writeExport', {
        'ticket': 'unknown',
        'bytes': Uint8List.fromList([1]),
      }),
      throwsA(
        isA<PlatformException>().having(
          (e) => e.code,
          'code',
          'export_unavailable',
        ),
      ),
    );
  });
  testWidgets('native Android abort is idempotent without active export', (
    tester,
  ) async {
    await channel.invokeMethod<void>('abortExport', {
      'ticket': 'stale-owned-ticket',
    });
    await channel.invokeMethod<void>('abortExport', {
      'ticket': 'stale-owned-ticket',
    });
  });
}
