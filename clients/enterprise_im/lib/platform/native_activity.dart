import 'dart:ui';
import 'package:flutter/widgets.dart';
import 'app_activity.dart';

// Register before runApp so window focus events are not lost during login.
// Desktop requires an observed view focus. Mobile resumed lifecycle already
// means the scene is active; inactive/paused still withdraw all visibility.
final class NativeActivityAdapter with WidgetsBindingObserver {
  final AppActivity activity = AppActivity();
  final int? viewId;
  final bool focusFromLifecycle;
  bool _focused = false, _disposed = false;
  NativeActivityAdapter({int? viewId, this.focusFromLifecycle = false})
    : viewId = viewId ?? PlatformDispatcher.instance.implicitView?.viewId {
    WidgetsBinding.instance.addObserver(this);
    _update(
      WidgetsBinding.instance.lifecycleState == AppLifecycleState.resumed,
    );
  }
  void _update(bool foreground) {
    if (_disposed) return;
    activity.update(
      foreground: foreground,
      focused: foreground && (focusFromLifecycle || _focused),
      activeConversation: activity.activeConversation,
      blocked: activity.blocked,
    );
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    if (state == AppLifecycleState.hidden ||
        state == AppLifecycleState.paused ||
        state == AppLifecycleState.detached)
      _focused = false;
    _update(state == AppLifecycleState.resumed);
  }

  @override
  void didChangeViewFocus(ViewFocusEvent event) {
    if (_disposed || event.viewId != viewId) return;
    _focused = event.state == ViewFocusState.focused;
    _update(activity.foreground);
  }

  void dispose() {
    if (_disposed) return;
    _update(false);
    _disposed = true;
    WidgetsBinding.instance.removeObserver(this);
    activity.dispose();
  }
}
