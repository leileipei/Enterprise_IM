import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:enterprise_im/platform/mobile_activity.dart';

// Real mobile engine scene activation only. No synthetic lifecycle events;
// background/read recovery, callbacks, physical-device and backend gates remain.
void main() {
  final binding = IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  testWidgets('actual resumed mobile scene can report active visibility', (
    tester,
  ) async {
    expect(Platform.isIOS || Platform.isAndroid, isTrue);
    final adapter = mobileActivityAdapter();
    addTearDown(adapter.dispose);
    await tester.pumpWidget(
      const MaterialApp(home: Scaffold(body: Text('移动端活动状态验证'))),
    );
    final deadline = DateTime.now().add(const Duration(seconds: 30));
    while (binding.lifecycleState != AppLifecycleState.resumed &&
        DateTime.now().isBefore(deadline)) {
      await tester.pump(const Duration(milliseconds: 100));
    }
    expect(binding.lifecycleState, AppLifecycleState.resumed);
    expect(adapter.activity.foreground, isTrue);
    expect(
      adapter.activity.focused,
      isTrue,
      reason: 'mobile active scene must not require desktop focus events',
    );
  });
}
