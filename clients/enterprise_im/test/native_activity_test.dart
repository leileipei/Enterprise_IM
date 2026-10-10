import 'dart:ui';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/platform/native_activity.dart';

void main() {
  testWidgets(
    'desktop resume cannot reuse focus observed before minimization',
    (tester) async {
      final adapter = NativeActivityAdapter(viewId: tester.view.viewId);
      addTearDown(adapter.dispose);
      adapter.didChangeAppLifecycleState(AppLifecycleState.resumed);
      adapter.didChangeViewFocus(
        ViewFocusEvent(
          viewId: tester.view.viewId,
          state: ViewFocusState.focused,
          direction: ViewFocusDirection.undefined,
        ),
      );
      expect(adapter.activity.focused, isTrue);
      adapter.didChangeAppLifecycleState(AppLifecycleState.hidden);
      expect(adapter.activity.foreground, isFalse);
      adapter.didChangeAppLifecycleState(AppLifecycleState.resumed);
      expect(
        adapter.activity.focused,
        isFalse,
        reason:
            'a fresh matching focus event is required after a hidden window',
      );
    },
  );

  testWidgets(
    'native lifecycle and matching view focus fence read eligibility',
    (tester) async {
      final adapter = NativeActivityAdapter(viewId: tester.view.viewId);
      final activity = adapter.activity;
      addTearDown(adapter.dispose);
      expect(activity.focused, isFalse);
      adapter.didChangeAppLifecycleState(AppLifecycleState.resumed);
      activity.update(
        foreground: activity.foreground,
        focused: false,
        activeConversation: 'selected',
        blocked: true,
      );
      adapter.didChangeViewFocus(
        ViewFocusEvent(
          viewId: tester.view.viewId + 1,
          state: ViewFocusState.focused,
          direction: ViewFocusDirection.undefined,
        ),
      );
      expect(activity.focused, isFalse);
      adapter.didChangeViewFocus(
        ViewFocusEvent(
          viewId: tester.view.viewId,
          state: ViewFocusState.focused,
          direction: ViewFocusDirection.undefined,
        ),
      );
      expect(activity.focused, isTrue);
      expect(activity.activeConversation, 'selected');
      expect(activity.blocked, isTrue);
      adapter.didChangeAppLifecycleState(AppLifecycleState.inactive);
      expect(activity.foreground, isFalse);
      expect(activity.focused, isFalse);
      adapter.didChangeAppLifecycleState(AppLifecycleState.resumed);
      expect(activity.foreground, isTrue);
      adapter.didChangeViewFocus(
        ViewFocusEvent(
          viewId: tester.view.viewId,
          state: ViewFocusState.unfocused,
          direction: ViewFocusDirection.undefined,
        ),
      );
      expect(activity.focused, isFalse);
    },
  );
}
