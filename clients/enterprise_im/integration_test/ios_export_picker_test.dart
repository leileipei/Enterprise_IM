import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/platform/file_export.dart';

// Interactive gate: real system Cancel / folder Open actions are required.
// No destination URL, test channel, or method-channel mock is supplied here.
// The independent host verifier reads the other fixture app's chosen directory.
void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  const timeout = Timeout(Duration(minutes: 4));
  Future<void> screen(WidgetTester tester, String phase) async {
    expect(Platform.isIOS, isTrue);
    await tester.pumpWidget(
      MaterialApp(
        home: Scaffold(body: Center(child: Text('系统导出验证：$phase'))),
      ),
    );
    // Let the previous native dismissal finish before another presentation.
    await tester.runAsync(
      () => Future<void>.delayed(const Duration(seconds: 1)),
    );
    // ignore: avoid_print
    print('IOS_EXPORT_PICKER phase=$phase runtime_pid=$pid');
  }

  testWidgets('cancel real iOS folder picker without opening source', (
    tester,
  ) async {
    await screen(tester, 'cancel');
    var sourceOpened = false;
    final saved = await tester.runAsync(
      () => FileExport().save('取消验证.bin', () {
        sourceOpened = true;
        return Stream.value([1, 2, 3]);
      }, Cancellation()),
    );
    expect(saved, isFalse);
    expect(sourceOpened, isFalse);
  }, timeout: timeout);

  testWidgets(
    'export exact multi-chunk bytes through real selected iOS folder',
    (tester) async {
      await screen(tester, 'save');
      var sourceOpened = 0;
      final bytes = List<int>.generate(70000, (i) => i % 251);
      final saved = await tester.runAsync(
        () => FileExport().save('导出验证.bin', () {
          sourceOpened++;
          return Stream.fromIterable([
            bytes.sublist(0, 40000),
            bytes.sublist(40000),
          ]);
        }, Cancellation()),
      );
      expect(saved, isTrue);
      expect(sourceOpened, 1);
    },
    timeout: timeout,
  );

  testWidgets(
    'real selected folder refuses existing name before source opens',
    (tester) async {
      await screen(tester, 'duplicate');
      var sourceOpened = false;
      await tester.runAsync(() async {
        await expectLater(
          FileExport().save('导出验证.bin', () {
            sourceOpened = true;
            return Stream.value([9, 9, 9]);
          }, Cancellation()),
          throwsA(
            isA<PlatformException>().having(
              (e) => e.code,
              'code',
              'export_exists',
            ),
          ),
        );
      });
      expect(sourceOpened, isFalse);
    },
    timeout: timeout,
  );

  testWidgets('fresh authorization failure cleans selected iOS target', (
    tester,
  ) async {
    await screen(tester, 'authorization-failure');
    var authorizationChecks = 0;
    await tester.runAsync(() async {
      await expectLater(
        FileExport().save('拒绝导出.bin', () {
          authorizationChecks++;
          return Stream.error(const ApiFailure('invalid_identity'));
        }, Cancellation()),
        throwsA(isA<ApiFailure>()),
      );
    });
    expect(authorizationChecks, 1);
  }, timeout: timeout);
}
