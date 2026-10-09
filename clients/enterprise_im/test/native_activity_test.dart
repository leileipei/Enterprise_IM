import 'dart:ui';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/platform/native_activity.dart';

void main() {
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
