import 'dart:async';
import 'package:flutter_test/flutter_test.dart';
import 'package:sqlite3/sqlite3.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/sync/sync_engine.dart';
import 'package:enterprise_im/core/sync/receipt_queue.dart';
import 'package:enterprise_im/core/sync/realtime_channel.dart';
import 'package:enterprise_im/core/sync/retry_policy.dart';
import 'package:enterprise_im/core/sync/sync_coordinator.dart';
import 'package:enterprise_im/platform/app_activity.dart';
import 'sync_test.dart' as fixture;
import 'realtime_channel_test.dart' as realtime;

final class ManualTimer implements Timer {
  final void Function() callback;
  bool active = true;
  ManualTimer(this.callback);
  void fire() {
    if (active) {
      active = false;
      callback();
    }
  }

  @override
  bool get isActive => active;
  @override
  int get tick => active ? 0 : 1;
  @override
  void cancel() {
    active = false;
  }
}

final class ManualScheduler {
  final scheduled = <(Duration, ManualTimer)>[];
  Timer create(Duration delay, void Function() callback) {
    final timer = ManualTimer(callback);
    scheduled.add((delay, timer));
    return timer;
  }

  bool has(Duration delay) =>
      scheduled.any((item) => item.$1 == delay && item.$2.isActive);
  void fire(Duration delay) => scheduled
      .firstWhere((item) => item.$1 == delay && item.$2.isActive)
      .$2
      .fire();
}

void main() {
  test(
    'notification burst coalesces refresh and connected polling; stop cancels all',
    () async {
      final (_, store) = await fixture.storage();
      final blocked = Completer<ApiResponse>();
      var gets = 0;
      final transport = fixture.ScriptedTransport((request) {
        if (request.uri.path.endsWith('/tickets'))
          return fixture.response({
            'ticket': realtime.ticket,
            'expires_in_seconds': 30,
          });
        if (request.method == 'PUT') return fixture.response(fixture.state());
        gets++;
        if (gets == 2) return blocked.future;
        return fixture.response(fixture.syncPage(empty: gets > 1));
      });
      final api = ApiClient(Uri.parse('https://im.example'), transport);
      final engine = SyncEngine(api, store);
      final activity = AppActivity()
        ..update(
          foreground: true,
          focused: true,
          activeConversation: fixture.conversation,
        );
      final receipts = ReceiptQueue(api, activity);
      final dial = realtime.DialFixture();
      final channel = RealtimeChannel(api, dial);
      final scheduler = ManualScheduler();
      final results = <SyncResult>[];
      final coordinator = SyncCoordinator(
        engine,
        channel,
        receipts,
        activity,
        onResult: results.add,
        schedule: scheduler.create,
        retry: RetryPolicy(random: () => .5),
      );
      addTearDown(() async {
        await coordinator.dispose();
        receipts.dispose();
        activity.dispose();
        await channel.dispose();
      });
      await coordinator.start(fixture.context());
      expect(gets, 1);
      expect(scheduler.has(const Duration(seconds: 5)), isTrue);
      dial.socket.controller.add('{"type":"ready","resync_required":true}');
      for (var i = 0; i < 30; i++) {
        dial.socket.controller.add('{"type":"sync_required"}');
      }
      await Future<void>.delayed(const Duration(milliseconds: 20));
      expect(gets, 2);
      blocked.complete(fixture.response(fixture.syncPage(empty: true)));
      await Future<void>.delayed(const Duration(milliseconds: 30));
      expect(gets, 3);
      expect(results.length, 3);
      expect(scheduler.has(const Duration(seconds: 35)), isTrue);
      expect(scheduler.has(const Duration(seconds: 5)), isFalse);
      await coordinator.stop();
      expect(scheduler.scheduled.where((item) => item.$2.isActive), isEmpty);
      expect(dial.socket.closed, isTrue);
    },
  );
  test('429 uses RetryAfter without immediate sync retry', () async {
    final (_, store) = await fixture.storage();
    var gets = 0;
    final transport = fixture.ScriptedTransport((request) {
      if (request.uri.path.endsWith('/tickets'))
        return fixture.response({
          'ticket': realtime.ticket,
          'expires_in_seconds': 30,
        });
      if (request.method == 'PUT') return fixture.response(fixture.state());
      if (++gets == 1)
        return ApiResponse(
          429,
          fixture.response({'error_code': 'rate_limited'}).body,
          {'retry-after': '7'},
        );
      return fixture.response(fixture.syncPage());
    });
    final api = ApiClient(Uri.parse('https://im.example'), transport);
    final engine = SyncEngine(api, store);
    final activity = AppActivity()
      ..update(
        foreground: true,
        focused: true,
        activeConversation: fixture.conversation,
      );
    final receipts = ReceiptQueue(api, activity);
    final channel = RealtimeChannel(api, realtime.DialFixture());
    final scheduler = ManualScheduler();
    final gotResult = Completer<void>();
    final coordinator = SyncCoordinator(
      engine,
      channel,
      receipts,
      activity,
      onResult: (_) {
        if (!gotResult.isCompleted) gotResult.complete();
      },
      schedule: scheduler.create,
    );
    addTearDown(() async {
      await coordinator.dispose();
      receipts.dispose();
      activity.dispose();
      await channel.dispose();
    });
    await coordinator.start(fixture.context());
    expect(gets, 1);
    expect(scheduler.has(const Duration(seconds: 7)), isTrue);
    scheduler.fire(const Duration(seconds: 5));
    await Future<void>.delayed(const Duration(milliseconds: 10));
    expect(gets, 1, reason: 'fallback poll must respect RetryAfter');
    scheduler.fire(const Duration(seconds: 7));
    await gotResult.future;
    expect(gets, 2);
  });
  test('background interrupts late sync without delivery', () async {
    final (_, store) = await fixture.storage();
    var gets = 0;
    final pending = Completer<ApiResponse>();
    final transport = fixture.ScriptedTransport((request) {
      if (request.uri.path.endsWith('/tickets'))
        return fixture.response({
          'ticket': realtime.ticket,
          'expires_in_seconds': 30,
        });
      if (request.method == 'PUT') return fixture.response(fixture.state());
      gets++;
      return gets == 1 ? pending.future : fixture.response(fixture.syncPage());
    });
    final api = ApiClient(Uri.parse('https://im.example'), transport);
    final engine = SyncEngine(api, store);
    final activity = AppActivity()
      ..update(
        foreground: true,
        focused: true,
        activeConversation: fixture.conversation,
      );
    final receipts = ReceiptQueue(api, activity);
    final dial = realtime.DialFixture();
    final channel = RealtimeChannel(api, dial);
    final scheduler = ManualScheduler();
    final results = <SyncResult>[];
    final coordinator = SyncCoordinator(
      engine,
      channel,
      receipts,
      activity,
      onResult: results.add,
      schedule: scheduler.create,
    );
    addTearDown(() async {
      await coordinator.dispose();
      receipts.dispose();
      activity.dispose();
      await channel.dispose();
    });
    final started = coordinator.start(fixture.context());
    await Future<void>.delayed(const Duration(milliseconds: 20));
    activity.update(
      foreground: false,
      focused: false,
      activeConversation: fixture.conversation,
    );
    pending.complete(fixture.response(fixture.syncPage()));
    await started;
    expect(results, isEmpty);
    expect(transport.requests.where((req) => req.method == 'PUT'), isEmpty);
    expect(scheduler.scheduled.where((item) => item.$2.isActive), isEmpty);
  });
  test(
    'delayed old socket close never clears a newly authorized page',
    () async {
      final (root, store) = await fixture.storage();
      var gets = 0;
      final transport = fixture.ScriptedTransport((request) {
        if (request.uri.path.endsWith('/tickets'))
          return fixture.response({
            'ticket': realtime.ticket,
            'expires_in_seconds': 30,
          });
        if (request.method == 'PUT') return fixture.response(fixture.state());
        if (++gets == 2)
          return fixture.response({'error_code': 'hard_deny'}, status: 403);
        return fixture.response(fixture.syncPage());
      });
      final api = ApiClient(Uri.parse('https://im.example'), transport);
      final engine = SyncEngine(api, store);
      final activity = AppActivity()
        ..update(
          foreground: true,
          focused: true,
          activeConversation: fixture.conversation,
        );
      final receipts = ReceiptQueue(api, activity);
      final dial = realtime.DialFixture();
      final channel = RealtimeChannel(api, dial);
      final scheduler = ManualScheduler();
      final failure = Completer<void>();
      final coordinator = SyncCoordinator(
        engine,
        channel,
        receipts,
        activity,
        onResult: (_) {},
        onFailure: (_) {
          if (!failure.isCompleted) failure.complete();
        },
        schedule: scheduler.create,
      );
      addTearDown(() async {
        await coordinator.dispose();
        receipts.dispose();
        activity.dispose();
        await channel.dispose();
      });
      await coordinator.start(fixture.context());
      final oldSocket = dial.socket;
      oldSocket.closeGate = Completer<void>();
      scheduler.fire(const Duration(seconds: 5));
      await failure.future;
      await coordinator.start(fixture.context());
      oldSocket.closeGate!.complete();
      await oldSocket.closedDone.future;
      await Future<void>.delayed(const Duration(milliseconds: 30));
      final db = sqlite3.open('${root.path}/${'a' * 64}.sqlite');
      try {
        expect(
          db
              .select("SELECT COUNT(*) AS n FROM records WHERE kind='message'")
              .single['n'],
          1,
        );
      } finally {
        db.close();
      }
    },
  );

  test('503 socket ticket retries with bounded timer and fresh ticket', () async {
    final (_, store) = await fixture.storage();
    var tickets = 0;
    final retried = Completer<void>();
    final transport = fixture.ScriptedTransport((request) {
      if (request.uri.path.endsWith('/tickets')) {
        if (++tickets == 1)
          return fixture.response({'error_code': 'unavailable'}, status: 503);
        retried.complete();
        return fixture.response({
          'ticket': realtime.ticket,
          'expires_in_seconds': 30,
        });
      }
      return fixture.response(
        request.method == 'GET'
            ? fixture.syncPage(
                empty: request.uri.queryParameters['after_seq'] != '0',
              )
            : fixture.state(),
      );
    });
    final api = ApiClient(Uri.parse('https://im.example'), transport);
    final activity = AppActivity()
      ..update(
        foreground: true,
        focused: true,
        activeConversation: fixture.conversation,
      );
    final engine = SyncEngine(api, store);
    final queue = ReceiptQueue(api, activity);
    final dial = realtime.DialFixture();
    final channel = RealtimeChannel(api, dial);
    final scheduler = ManualScheduler();
    final coordinator = SyncCoordinator(
      engine,
      channel,
      queue,
      activity,
      onResult: (_) {},
      schedule: scheduler.create,
      retry: RetryPolicy(random: () => .5),
    );
    addTearDown(() async {
      await coordinator.dispose();
      queue.dispose();
      activity.dispose();
      await channel.dispose();
    });
    await coordinator.start(fixture.context());
    expect(tickets, 1);
    expect(scheduler.has(const Duration(seconds: 1)), isTrue);
    // Fire the already scheduled purge and reconnect, excluding newly armed timers.
    final due = scheduler.scheduled
        .where(
          (item) => item.$1 == const Duration(seconds: 1) && item.$2.isActive,
        )
        .toList();
    for (final item in due) {
      item.$2.fire();
    }
    await retried.future;
    await Future<void>.delayed(const Duration(milliseconds: 20));
    expect(tickets, 2);
    expect(dial.protocols, ['enterprise-im.v1', 'ticket.${realtime.ticket}']);
    expect(channel.connected, isFalse);
    dial.socket.controller.add('{"type":"ready","resync_required":true}');
    await Future<void>.delayed(const Duration(milliseconds: 20));
    expect(channel.connected, isTrue);
    expect(scheduler.has(const Duration(seconds: 35)), isTrue);
  });
  test(
    'foreground restores cached body only through a fresh full page',
    () async {
      final (_, store) = await fixture.storage();
      final transport = fixture.ScriptedTransport((request) {
        if (request.uri.path.endsWith('/tickets'))
          return fixture.response({
            'ticket': realtime.ticket,
            'expires_in_seconds': 30,
          });
        return fixture.response(
          request.method == 'GET'
              ? fixture.syncPage(
                  empty: request.uri.queryParameters['after_seq'] != '0',
                )
              : fixture.state(),
        );
      });
      final api = ApiClient(Uri.parse('https://im.example'), transport);
      final engine = SyncEngine(api, store);
      final activity = AppActivity()
        ..update(
          foreground: true,
          focused: true,
          activeConversation: fixture.conversation,
        );
      final queue = ReceiptQueue(api, activity);
      final channel = RealtimeChannel(api, realtime.DialFixture());
      final results = <SyncResult>[];
      final resumed = Completer<SyncResult>();
      final coordinator = SyncCoordinator(
        engine,
        channel,
        queue,
        activity,
        onResult: (r) {
          results.add(r);
          if (results.length == 2) resumed.complete(r);
        },
        schedule: ManualScheduler().create,
      );
      addTearDown(() async {
        await coordinator.dispose();
        queue.dispose();
        activity.dispose();
        await channel.dispose();
      });
      await coordinator.start(fixture.context());
      activity.update(
        foreground: false,
        focused: false,
        activeConversation: fixture.conversation,
      );
      expect(results.first.messages.single.redacted, isTrue);
      activity.update(
        foreground: true,
        focused: true,
        activeConversation: fixture.conversation,
      );
      final refreshed = await resumed.future;
      expect(refreshed.messages.length, 1);
      expect(refreshed.messages.single.redacted, isFalse);
      expect(
        transport.requests
            .where((r) => r.method == 'GET')
            .last
            .uri
            .queryParameters['after_seq'],
        '0',
      );
    },
  );
}
