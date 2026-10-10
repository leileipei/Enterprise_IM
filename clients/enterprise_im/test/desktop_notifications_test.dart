import 'dart:async';
import 'package:flutter_test/flutter_test.dart';
import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';
import 'package:flutter_local_notifications/flutter_local_notifications.dart';
import 'package:enterprise_im/platform/app_activity.dart';
import 'package:enterprise_im/platform/notifications.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/seq.dart';
import 'package:enterprise_im/core/sync/sync_engine.dart';
import 'sync_test.dart' as fixture;

class NotificationFixture implements NotificationPort {
  bool permission = true, fail = false;
  void Function(String?)? tap;
  final shown = <(String, String)>[];
  int cancelled = 0;
  Completer<void>? showGate;
  @override
  Future<bool> enable(void Function(String?) onTap) async {
    tap = onTap;
    if (fail) throw StateError('plugin unavailable');
    return permission;
  }

  @override
  Future<void> show({required String body, required String payload}) async {
    shown.add((body, payload));
    await showGate?.future;
  }

  @override
  Future<void> cancel() async {
    cancelled++;
  }
}

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  test(
    'native adapter cancellation owns a distinct notification id per workspace',
    () async {
      debugDefaultTargetPlatformOverride = TargetPlatform.macOS;
      MacOSFlutterLocalNotificationsPlugin.registerWith();
      addTearDown(() => debugDefaultTargetPlatformOverride = null);
      const channel = MethodChannel(
        'dexterous.com/flutter/local_notifications',
      );
      final calls = <MethodCall>[];
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(channel, (call) async {
            calls.add(call);
            return true;
          });
      addTearDown(
        () => TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
            .setMockMethodCallHandler(channel, null),
      );
      final first = NativeNotificationPort('macos'),
          second = NativeNotificationPort('macos');
      expect(await first.enable((_) {}), isTrue);
      expect(await second.enable((_) {}), isTrue);
      await first.show(body: '收到新消息', payload: 'first-opaque');
      await second.show(body: '收到新消息', payload: 'second-opaque');
      await first.cancel();
      final shown = calls.where((c) => c.method == 'show').toList();
      final firstId = (shown[0].arguments as Map)['id'],
          secondId = (shown[1].arguments as Map)['id'];
      expect(
        secondId,
        isNot(firstId),
        reason:
            'late cleanup of an old workspace must not remove the new workspace alert',
      );
      expect(calls.last.arguments, firstId);
      final initialization =
          calls.firstWhere((c) => c.method == 'initialize').arguments as Map;
      expect(initialization['requestAlertPermission'], isFalse);
    },
  );
  Future<(MessageNotifications, NotificationFixture, SyncEngine, AppActivity)>
  setup({bool granted = true, bool newPages = false}) async {
    var last = 1;
    final (_, store) = await fixture.storage();
    final api = ApiClient(
      Uri.parse('https://im.example'),
      fixture.ScriptedTransport((r) {
        if (r.method == 'GET') {
          last = int.parse(r.uri.queryParameters['after_seq']!) + 1;
          return fixture.response(
            newPages
                ? fixture.syncPage(seq: last)
                : fixture.syncPage(empty: last > 1),
          );
        }
        return fixture.response(fixture.state(scanned: newPages ? last : 1));
      }),
    );
    addTearDown(api.close);
    final engine = SyncEngine(api, store);
    final activity = AppActivity()
      ..update(
        foreground: true,
        focused: true,
        activeConversation: fixture.conversation,
      );
    addTearDown(activity.dispose);
    final port = NotificationFixture()..permission = granted;
    final notices = MessageNotifications(activity, port);
    addTearDown(notices.close);
    return (notices, port, engine, activity);
  }

  test(
    'message burst coalesces while the native notification request is in flight',
    () async {
      final (notices, port, engine, _) = await setup(newPages: true);
      final ctx = fixture.context();
      await notices.enable();
      notices.bind(
        ctx,
        after: Seq.parse('0'),
        isCurrent: () => true,
        onTap: (_) async {},
      );
      port.showGate = Completer<void>();
      final first = notices.acceptPage(await engine.syncConversation(ctx));
      for (var i = 0; i < 100 && port.shown.isEmpty; i++) {
        await Future<void>.delayed(Duration.zero);
      }
      expect(port.shown, hasLength(1));
      final second = notices.acceptPage(await engine.syncConversation(ctx));
      port.showGate!.complete();
      await first;
      await second;
      expect(
        port.shown,
        hasLength(1),
        reason:
            'one in-flight generic alert covers the burst without retaining a page backlog',
      );
    },
  );
  test('testNotificationDeniedDoesNotBlockMessaging', () async {
    final (notices, port, engine, _) = await setup(granted: false);
    final ctx = fixture.context();
    notices.bind(
      ctx,
      after: Seq.parse('0'),
      isCurrent: () => true,
      onTap: (_) async {},
    );
    expect(await notices.enable(), isFalse);
    final page = await engine.syncConversation(ctx);
    await notices.acceptPage(page);
    expect(page.messages.single.text, 'recipient body');
    expect(page.deliveredSeqs.map((s) => s.toString()), ['1']);
    expect(port.shown, isEmpty);
    port.fail = true;
    expect(await notices.enable(), isFalse);
    expect((await engine.syncConversation(ctx)).matchesContext(ctx), isTrue);
  });
  test(
    'notification contains only generic body and opaque process payload; stale tap denied',
    () async {
      final (notices, port, engine, _) = await setup();
      final ctx = fixture.context();
      var current = true, opened = 0;
      notices.bind(
        ctx,
        after: Seq.parse('0'),
        isCurrent: () => current,
        onTap: (scope) async {
          expect(scope.sameScope(ctx), isTrue);
          opened++;
        },
      );
      expect(await notices.enable(), isTrue);
      final page = await engine.syncConversation(ctx);
      await notices.acceptPage(page);
      expect(port.shown.single.$1, '收到新消息');
      expect(port.shown.single.$2, matches(RegExp(r'^[A-Za-z0-9_-]{43}$')));
      expect(port.shown.single.$2, isNot(contains(ctx.conversationId)));
      await notices.activate(port.shown.single.$2);
      expect(opened, 1);
      current = false;
      await notices.activate(port.shown.single.$2);
      expect(opened, 1);
    },
  );
  test(
    'history and repeated pages do not notify; background pages cannot notify',
    () async {
      final (notices, port, engine, activity) = await setup();
      final ctx = fixture.context();
      await notices.enable();
      notices.bind(
        ctx,
        after: Seq.parse('1'),
        isCurrent: () => true,
        onTap: (_) async {},
      );
      final page = await engine.syncConversation(ctx);
      await notices.acceptPage(page);
      expect(port.shown, isEmpty);
      notices.bind(
        ctx,
        after: Seq.parse('0'),
        isCurrent: () => true,
        onTap: (_) async {},
      );
      activity.update(
        foreground: false,
        focused: false,
        activeConversation: fixture.conversation,
      );
      await notices.acceptPage(page);
      expect(port.shown, isEmpty);
      activity.update(
        foreground: true,
        focused: true,
        activeConversation: fixture.conversation,
      );
      await notices.acceptPage(page);
      await notices.acceptPage(page);
      expect(port.shown, isEmpty);
      notices.bind(
        ctx,
        after: Seq.parse('0'),
        isCurrent: () => true,
        onTap: (_) async {},
      );
      await notices.acceptPage(page);
      expect(port.shown, hasLength(1));
      await notices.clear();
      await notices.activate(port.shown.single.$2);
      await notices.acceptPage(page);
      expect(port.shown, hasLength(1));
    },
  );
  test(
    'late show after scope clear is cancelled and cannot activate',
    () async {
      final (notices, port, engine, _) = await setup();
      final ctx = fixture.context();
      var opened = 0;
      await notices.enable();
      notices.bind(
        ctx,
        after: Seq.parse('0'),
        isCurrent: () => true,
        onTap: (_) async {
          opened++;
        },
      );
      port.showGate = Completer<void>();
      final page = await engine.syncConversation(ctx);
      final show = notices.acceptPage(page);
      for (var i = 0; i < 20 && port.shown.isEmpty; i++) {
        await Future<void>.delayed(Duration.zero);
      }
      expect(port.shown, hasLength(1));
      final clear = notices.clear();
      port.showGate!.complete();
      await show;
      await clear;
      await notices.activate(port.shown.single.$2);
      expect(opened, 0);
      expect(port.cancelled, greaterThan(0));
    },
  );
}
