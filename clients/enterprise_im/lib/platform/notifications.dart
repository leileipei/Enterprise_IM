import 'dart:async';
import 'dart:convert';
import 'dart:math';
import 'package:flutter_local_notifications/flutter_local_notifications.dart';
import '../core/protocol/seq.dart';
import '../core/sync/sync_engine.dart';
import 'app_activity.dart';

abstract interface class NotificationPort {
  Future<bool> enable(void Function(String?) onTap);
  Future<void> show({required String body, required String payload});
  Future<void> cancel();
}

// No permission prompt until the user explicitly enables notifications.
// Only the process-active response is handled; cold-start payloads confer no
// session authority and are deliberately not restored from the OS.
final class NativeNotificationPort implements NotificationPort {
  final String platform;
  final FlutterLocalNotificationsPlugin plugin;
  static int _nextId = 0;
  final int _id = ++_nextId;
  bool _initialized = false;
  NativeNotificationPort(
    this.platform, {
    FlutterLocalNotificationsPlugin? plugin,
  }) : plugin = plugin ?? FlutterLocalNotificationsPlugin();
  @override
  Future<bool> enable(void Function(String?) onTap) async {
    if (platform != 'macos' && platform != 'windows') return false;
    final initialized = await plugin.initialize(
      settings: const InitializationSettings(
        macOS: DarwinInitializationSettings(
          requestAlertPermission: false,
          requestBadgePermission: false,
          requestSoundPermission: false,
        ),
        windows: WindowsInitializationSettings(
          appName: '企业 IM',
          appUserModelId: 'EnterpriseIM.Client.Desktop',
          guid: 'd52dfe0e-367e-493a-9a44-5a078285699f',
        ),
      ),
      onDidReceiveNotificationResponse: (response) => onTap(response.payload),
    );
    if (initialized != true) return false;
    _initialized = true;
    if (platform == 'macos') {
      return await plugin
              .resolvePlatformSpecificImplementation<
                MacOSFlutterLocalNotificationsPlugin
              >()
              ?.requestPermissions(alert: true) ==
          true;
    }
    // Windows toast permission is controlled by OS settings. This records the
    // user's opt-in and initialized adapter, not a verified OS permission grant.
    return true;
  }

  @override
  Future<void> show({required String body, required String payload}) => plugin
      .show(
        id: _id,
        title: '企业 IM',
        body: body,
        payload: payload,
        notificationDetails: const NotificationDetails(
          macOS: DarwinNotificationDetails(
            presentAlert: true,
            presentBadge: false,
            presentSound: false,
          ),
          windows: WindowsNotificationDetails(),
        ),
      )
      .timeout(const Duration(seconds: 5));
  @override
  Future<void> cancel() async {
    if (_initialized)
      await plugin.cancel(id: _id).timeout(const Duration(seconds: 5));
  }
}

// The payload is a random process-local reference, never a URL, account,
// membership, message body, device token or conversation identifier.
final class MessageNotifications {
  final AppActivity activity;
  final NotificationPort port;
  ClientContext? _context;
  bool Function()? _isCurrent;
  Future<void> Function(ClientContext)? _onTap;
  Seq _highest = Seq.parse('0');
  String? _payload;
  bool _enabled = false, _closed = false, _showQueued = false;
  int _epoch = 0;
  Future<void> _tail = Future.value();
  Future<bool>? _enabling;
  MessageNotifications(this.activity, this.port);
  bool _current(int epoch) =>
      !_closed &&
      epoch == _epoch &&
      _context != null &&
      !_context!.session.isExpired &&
      _isCurrent?.call() == true;
  Future<void> _queue(Future<void> Function() action) {
    final operation = _tail.then((_) async {
      try {
        await action();
      } catch (_) {
        /* Notification failure cannot affect messaging. */
      }
    });
    _tail = operation;
    return operation;
  }

  Future<bool> enable() => _enabling ??= _enable();
  Future<bool> _enable() async {
    try {
      final allowed = await port.enable((payload) {
        unawaited(activate(payload));
      });
      _enabled = !_closed && allowed;
      return _enabled;
    } catch (_) {
      _enabled = false;
      return false;
    } finally {
      _enabling = null;
    }
  }

  void bind(
    ClientContext context, {
    required Seq after,
    required bool Function() isCurrent,
    required Future<void> Function(ClientContext) onTap,
  }) {
    if (_closed) return;
    _epoch++;
    _payload = null;
    _context = context;
    _highest = after;
    _isCurrent = isCurrent;
    _onTap = onTap;
    unawaited(_queue(port.cancel));
  }

  Future<void> acceptPage(SyncResult page) {
    final ctx = _context, epoch = _epoch;
    if (ctx == null || !_current(epoch) || !page.matchesContext(ctx))
      return Future.value();
    var incoming = false;
    for (final message in page.messages) {
      if (message.seq.compareTo(_highest) <= 0) continue;
      _highest = message.seq;
      if (!message.redacted &&
          message.senderUserId != ctx.session.userId &&
          page.canRead(message.seq))
        incoming = true;
    }
    if (!incoming ||
        !_enabled ||
        !activity.foreground ||
        activity.blocked ||
        activity.activeConversation != ctx.conversationId)
      return Future.value();
    if (_showQueued) return Future.value();
    _showQueued = true;
    return _queue(() async {
      try {
        if (!_current(epoch) ||
            !_enabled ||
            !activity.foreground ||
            activity.blocked ||
            activity.activeConversation != ctx.conversationId ||
            !page.matchesContext(ctx) ||
            !page.messages.any(
              (m) =>
                  m.senderUserId != ctx.session.userId && page.canRead(m.seq),
            ))
          return;
        final random = Random.secure();
        final payload = base64Url
            .encode(List<int>.generate(32, (_) => random.nextInt(256)))
            .replaceAll('=', '');
        _payload = payload;
        await port.show(body: '收到新消息', payload: payload);
        if (!_current(epoch)) await port.cancel();
      } finally {
        _showQueued = false;
      }
    });
  }

  Future<void> activate(String? payload) async {
    final epoch = _epoch, ctx = _context, callback = _onTap;
    if (payload == null ||
        payload != _payload ||
        ctx == null ||
        callback == null ||
        activity.blocked ||
        !_current(epoch))
      return;
    // The caller must fetch an authorized list and fresh sync before opening.
    // This reference is a navigation hint, not a content authorization.
    try {
      await callback(ctx);
    } catch (_) {
      /* Remain on the current safe page. */
    }
  }

  Future<void> clear() {
    _epoch++;
    _context = null;
    _payload = null;
    _isCurrent = null;
    _onTap = null;
    return _queue(port.cancel);
  }

  Future<void> close() {
    _closed = true;
    _enabled = false;
    return clear();
  }
}
