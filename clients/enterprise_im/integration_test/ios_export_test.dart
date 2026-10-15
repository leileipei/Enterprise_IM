import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';

// Compiled iOS bridge boundary gate. Real picker operations are tested separately.
void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  const channel = MethodChannel('enterprise_im/file_export');
  testWidgets('iOS export rejects invalid names and unknown write tickets', (
    tester,
  ) async {
    expect(Platform.isIOS, isTrue);
    await tester.pumpWidget(
      const MaterialApp(home: Scaffold(body: Text('系统导出验证'))),
    );
    for (final name in ['', '../escape', 'a/b', 'a\u0000b']) {
      await expectLater(
        channel.invokeMethod<String>('beginExport', {'name': name}),
        throwsA(
          isA<PlatformException>().having(
            (e) => e.code,
            'code',
            'export_unavailable',
          ),
        ),
      );
    }
    await expectLater(
      channel.invokeMethod<void>('writeExport', {
        'ticket': 'unknown',
        'bytes': Uint8List.fromList([1]),
      }),
      throwsA(isA<PlatformException>()),
    );
  });
}
