import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/platform/file_export.dart';

// Interactive simulator/device gate: cancel the real system folder picker.
// This entry never supplies a destination through a test channel or mock.
void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  testWidgets('cancel real iOS folder picker without opening source', (
    tester,
  ) async {
    await tester.pumpWidget(
      const MaterialApp(home: Scaffold(body: Text('请取消系统文件夹选择器'))),
    );
    var sourceOpened = false;
    stdout.writeln('IOS_EXPORT_PICKER_READY runtime_pid=$pid');
    final saved = await tester.runAsync(
      () => FileExport().save('导出验证.txt', () {
        sourceOpened = true;
        return Stream.value([97, 98, 99]);
      }, Cancellation()),
    );
    expect(saved, isFalse);
    expect(sourceOpened, isFalse);
  }, timeout: const Timeout(Duration(minutes: 3)));
}
