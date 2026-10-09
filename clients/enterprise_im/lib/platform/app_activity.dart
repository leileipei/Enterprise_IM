import 'dart:async';

// Native lifecycle/window focus and the active conversation are injected by
// platform/UI adapters. Modal/preview overlays interrupt continuous exposure.
final class AppActivity {
  bool foreground = false, focused = false, blocked = false;
  String? activeConversation;
  final _changes = StreamController<void>.broadcast(sync: true);
  Stream<void> get changes => _changes.stream;
  void update({
    required bool foreground,
    required bool focused,
    required String? activeConversation,
    bool blocked = false,
  }) {
    if (this.foreground == foreground &&
        this.focused == focused &&
        this.activeConversation == activeConversation &&
        this.blocked == blocked)
      return;
    this.foreground = foreground;
    this.focused = focused;
    this.blocked = blocked;
    this.activeConversation = activeConversation;
    if (!_changes.isClosed) _changes.add(null);
  }

  void dispose() => _changes.close();
}
