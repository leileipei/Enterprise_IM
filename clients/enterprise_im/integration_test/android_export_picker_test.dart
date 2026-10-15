import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/platform/file_export.dart';

// Real Android DocumentsUI actions are required. No result Intent, content URI,
// provider, destination path, or method-channel mock is injected into the app.
// The host independently reads files from this fresh emulator's selected folder.
void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  const probe = String.fromEnvironment('IM_ANDROID_EXPORT_PROBE_ID');
  const timeout = Timeout(Duration(minutes: 4));
  final completeName = 'im-$probe-complete.bin';

  Future<void> screen(WidgetTester tester, String phase) async {
    expect(Platform.isAndroid, isTrue);
    expect(probe, matches(RegExp(r'^[a-f0-9]{32}$')));
    await tester.pumpWidget(
      MaterialApp(
        home: Scaffold(body: Center(child: Text('系统导出验证：$phase'))),
      ),
    );
    await tester.runAsync(
      () => Future<void>.delayed(const Duration(seconds: 1)),
    );
    // ignore: avoid_print
    print('ANDROID_EXPORT_PICKER phase=$phase runtime_pid=$pid probe=$probe');
  }

  testWidgets('cancel real Android document picker before source opens', (
    tester,
  ) async {
    await screen(tester, 'cancel');
    var sourceOpened = false;
    final saved = await tester.runAsync(
      () => FileExport().save('im-$probe-cancel.bin', () {
        sourceOpened = true;
        return Stream.value([1, 2, 3]);
      }, Cancellation()),
    );
    expect(saved, isFalse);
    expect(sourceOpened, isFalse);
  }, timeout: timeout);

  testWidgets('export exact multi-chunk bytes through real Android picker', (
    tester,
  ) async {
    await screen(tester, 'save');
    var sourceOpened = 0;
    final bytes = List<int>.generate(70000, (i) => i % 251);
    final saved = await tester.runAsync(
      () => FileExport().save(completeName, () {
        sourceOpened++;
        return Stream.fromIterable([
          bytes.sublist(0, 40000),
          bytes.sublist(40000),
        ]);
      }, Cancellation()),
    );
    expect(saved, isTrue);
    expect(sourceOpened, 1);
  }, timeout: timeout);

  testWidgets('system duplicate naming preserves the original Android export', (
    tester,
  ) async {
    await screen(tester, 'duplicate');
    var sourceOpened = 0;
    final saved = await tester.runAsync(
      () => FileExport().save(completeName, () {
        sourceOpened++;
        return Stream.value([9, 8, 7]);
      }, Cancellation()),
    );
    expect(saved, isTrue);
    expect(sourceOpened, 1);
  }, timeout: timeout);

  testWidgets('source authorization failure removes the selected document', (
    tester,
  ) async {
    await screen(tester, 'authorization-failure');
    var authorizationChecks = 0;
    await tester.runAsync(() async {
      await expectLater(
        FileExport().save('im-$probe-denied.bin', () {
          authorizationChecks++;
          return Stream.error(const ApiFailure('invalid_identity'));
        }, Cancellation()),
        throwsA(isA<ApiFailure>()),
      );
    });
    expect(authorizationChecks, 1);
  }, timeout: timeout);
}
