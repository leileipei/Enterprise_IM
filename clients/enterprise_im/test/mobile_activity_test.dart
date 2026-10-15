import 'package:flutter/widgets.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/platform/mobile_activity.dart';

void main() {
  testWidgets('resumed mobile scene does not wait for a desktop focus event', (
    tester,
  ) async {
    final adapter = mobileActivityAdapter();
    addTearDown(adapter.dispose);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
    expect(adapter.activity.foreground, isTrue);
    expect(adapter.activity.focused, isTrue);
  });
  testWidgets(
    'mobile suspension withdraws visibility and preserves admission block',
    (tester) async {
      final adapter = mobileActivityAdapter();
      addTearDown(adapter.dispose);
      tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
      adapter.activity.update(
        foreground: true,
        focused: true,
        activeConversation: 'owned',
        blocked: true,
      );
      tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.paused);
      expect(adapter.activity.foreground, isFalse);
      expect(adapter.activity.focused, isFalse);
      tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
      expect(adapter.activity.focused, isTrue);
      expect(adapter.activity.activeConversation, 'owned');
      expect(adapter.activity.blocked, isTrue);
    },
  );
}
