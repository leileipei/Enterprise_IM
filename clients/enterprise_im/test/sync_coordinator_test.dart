import 'dart:io' show sleep;
import 'dart:async';
import 'dart:ui';
import 'package:enterprise_im/platform/native_activity.dart';
import 'package:enterprise_im/core/protocol/seq.dart';
import 'dart:convert';
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
  TestWidgetsFlutterBinding.ensureInitialized();
  for (final stalled in [false, true]) {
    test(
      stalled
          ? 'testDesktopMinimizeStopsReadAndRefocusResyncs with stalled event loop'
          : 'testDesktopMinimizeStopsReadAndRefocusResyncs',
      () async {
        final (_, store) = await fixture.storage();
        final pending = Completer<ApiResponse>();
        final readAcknowledged = Completer<void>();
        var gets = 0, reads = 0;
        final api = ApiClient(
          Uri.parse('https://im.example'),
          fixture.ScriptedTransport((r) {
            if (r.uri.path.endsWith('/tickets'))
              return fixture.response({
                'ticket': realtime.ticket,
                'expires_in_seconds': 30,
              });
            if (r.uri.path.endsWith('/read')) {
              reads++;
              expect(fixture.requestBody(r)['seqs'], ['1']);
              return fixture.response(fixture.state(read: 1));
            }
            if (r.method == 'PUT') return fixture.response(fixture.state());
            gets++;
            return gets == 2
                ? pending.future
                : fixture.response(fixture.syncPage());
          }),
        );
        addTearDown(api.close);
        final adapter = NativeActivityAdapter(viewId: 0),
            ctx = fixture.context();
        final activity = adapter.activity;
        adapter.didChangeAppLifecycleState(AppLifecycleState.resumed);
        adapter.didChangeViewFocus(
          const ViewFocusEvent(
            viewId: 0,
            state: ViewFocusState.focused,
            direction: ViewFocusDirection.undefined,
          ),
        );
        activity.update(
          foreground: true,
          focused: true,
          activeConversation: fixture.conversation,
        );
        final engine = SyncEngine(api, store),
            queue = ReceiptQueue(
              api,
              activity,
              onState: (_) {
                if (!readAcknowledged.isCompleted) readAcknowledged.complete();
              },
            ),
            channel = RealtimeChannel(api, realtime.DialFixture());
        final results = <SyncResult>[];
        final coordinator = SyncCoordinator(
          engine,
          channel,
          queue,
          activity,
          onResult: results.add,
        );
        addTearDown(() async {
          await coordinator.dispose();
          queue.dispose();
          await channel.dispose();
          adapter.dispose();
        });
        await coordinator.start(ctx);
        queue.observe(Seq.parse('1'), 1, ctx);
        await Future<void>.delayed(const Duration(milliseconds: 200));
        adapter.didChangeAppLifecycleState(AppLifecycleState.hidden);
        await Future<void>.delayed(const Duration(milliseconds: 850));
        expect(reads, 0);
        adapter.didChangeAppLifecycleState(AppLifecycleState.resumed);
        for (var i = 0; i < 100 && gets < 2; i++) {
          await Future<void>.delayed(const Duration(milliseconds: 10));
        }
        expect(gets, 2);
        expect(activity.focused, isFalse);
        queue.observe(Seq.parse('1'), 1, ctx);
        await Future<void>.delayed(const Duration(milliseconds: 550));
        expect(reads, 0);
        expect(results, hasLength(1));
        pending.complete(fixture.response(fixture.syncPage()));
        for (var i = 0; i < 100 && results.length < 2; i++) {
          await Future<void>.delayed(const Duration(milliseconds: 10));
        }
        expect(results, hasLength(2));
        adapter.didChangeViewFocus(
          const ViewFocusEvent(
            viewId: 0,
            state: ViewFocusState.focused,
            direction: ViewFocusDirection.undefined,
          ),
        );
        queue.observe(Seq.parse('1'), 1, ctx);
        if (stalled) {
          final stall = Timer(const Duration(milliseconds: 100), () {
            sleep(const Duration(milliseconds: 1100));
          });
          addTearDown(stall.cancel);
        }
        // Exposure and batching are consecutive timers. A late exposure
        // callback starts a new batch delay; elapsed wall time is not an ACK.
        await readAcknowledged.future.timeout(const Duration(seconds: 5));
        expect(reads, 1);
        expect(queue.locallyReadSeqs, [Seq.parse('1')]);
      },
    );
  }

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
  test(
    'online-only lease expiry redacts before fresh full authorization',
    () async {
      final (_, store) = await fixture.storage();
      var elapsed = Duration.zero;
      var gets = 0;
      final renewal = Completer<ApiResponse>();
      final afters = <String>[];
      Map<String, Object?> online() {
        final page = fixture.syncPage();
        final message =
            (page['messages'] as List).single as Map<String, Object?>;
        message['cache_until'] = fixture.server.toIso8601String();
        message['online_until'] = fixture.server
            .add(const Duration(seconds: 5))
            .toIso8601String();
        return page;
      }

      final transport = fixture.ScriptedTransport((request) {
        if (request.uri.path.endsWith('/tickets'))
          return fixture.response({
            'ticket': realtime.ticket,
            'expires_in_seconds': 30,
          });
        if (request.method == 'PUT') return fixture.response(fixture.state());
        afters.add(request.uri.queryParameters['after_seq']!);
        if (++gets == 3) return renewal.future;
        return fixture.response(
          gets == 1 ? online() : fixture.syncPage(empty: true),
        );
      });
      final api = ApiClient(Uri.parse('https://im.example'), transport);
      final engine = SyncEngine(api, store, now: () => elapsed);
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
      final refreshed = Completer<SyncResult>();
      final coordinator = SyncCoordinator(
        engine,
        channel,
        receipts,
        activity,
        onResult: (result) {
          results.add(result);
          if (renewal.isCompleted &&
              result.messages.any((message) => !message.redacted) &&
              !refreshed.isCompleted) {
            refreshed.complete(result);
          }
        },
        schedule: scheduler.create,
        retry: RetryPolicy(random: () => .5),
      );
      addTearDown(() async {
        if (!renewal.isCompleted)
          renewal.complete(fixture.response(fixture.syncPage(empty: true)));
        await coordinator.dispose();
        receipts.dispose();
        activity.dispose();
        await channel.dispose();
      });
      await coordinator.start(fixture.context());
      final original = results.single;
      expect(original.messages.single.redacted, isFalse);
      dial.socket.controller.add('{"type":"ready","resync_required":true}');
      await Future<void>.delayed(const Duration(milliseconds: 30));
      expect(gets, 2);
      expect(scheduler.has(const Duration(seconds: 35)), isTrue);
      expect(
        scheduler.has(const Duration(seconds: 5)),
        isTrue,
        reason: 'server online lease needs its own expiry timer',
      );
      elapsed = const Duration(seconds: 5);
      scheduler.fire(const Duration(seconds: 5));
      await Future<void>.delayed(const Duration(milliseconds: 30));
      expect(
        results.last.messages.single.redacted,
        isTrue,
        reason: 'publish redaction while new authorization remains blocked',
      );
      expect(afters, ['0', '1', '0']);
      expect(await store.committedCursor(fixture.conversation), isNotNull);
      renewal.complete(fixture.response(online()));
      await refreshed.future.timeout(const Duration(seconds: 5));
      expect(original.messages.single.redacted, isTrue);
      expect(results.last.messages.single.redacted, isFalse);
    },
  );
  test(
    'online-only body loses authorization immediately on focus loss',
    () async {
      final (_, store) = await fixture.storage();
      final transport = fixture.ScriptedTransport((request) {
        if (request.uri.path.endsWith('/tickets'))
          return fixture.response({
            'ticket': realtime.ticket,
            'expires_in_seconds': 30,
          });
        if (request.method == 'PUT') return fixture.response(fixture.state());
        final page = fixture.syncPage();
        final message =
            (page['messages'] as List).single as Map<String, Object?>;
        message['cache_until'] = fixture.server.toIso8601String();
        message['online_until'] = fixture.server
            .add(const Duration(seconds: 5))
            .toIso8601String();
        return fixture.response(page);
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
      await coordinator.start(fixture.context());
      final original = results.single;
      expect(original.messages.single.redacted, isFalse);
      activity.update(
        foreground: true,
        focused: false,
        activeConversation: fixture.conversation,
      );
      expect(original.messages.single.redacted, isTrue);
      expect(results.last.messages.single.redacted, isTrue);
      expect(scheduler.scheduled.where((entry) => entry.$2.isActive), isEmpty);
      activity.update(
        foreground: true,
        focused: true,
        activeConversation: fixture.conversation,
      );
      await Future<void>.delayed(const Duration(milliseconds: 30));
      expect(results.last.messages.single.redacted, isFalse);
      expect(original.messages.single.redacted, isTrue);
    },
  );
  test(
    'failed online authorization redacts and respects RetryAfter before recovery',
    () async {
      final (_, store) = await fixture.storage();
      var gets = 0;
      final afters = <String>[];
      Map<String, Object?> online() {
        final page = fixture.syncPage();
        final message =
            (page['messages'] as List).single as Map<String, Object?>;
        message['cache_until'] = fixture.server.toIso8601String();
        message['online_until'] = fixture.server
            .add(const Duration(seconds: 5))
            .toIso8601String();
        return page;
      }

      final transport = fixture.ScriptedTransport((request) {
        if (request.uri.path.endsWith('/tickets'))
          return fixture.response({
            'ticket': realtime.ticket,
            'expires_in_seconds': 30,
          });
        if (request.method == 'PUT') return fixture.response(fixture.state());
        afters.add(request.uri.queryParameters['after_seq']!);
        gets++;
        if (gets == 2)
          return ApiResponse(
            429,
            fixture.response({'error_code': 'rate_limited'}).body,
            {'retry-after': '7'},
          );
        return fixture.response(
          gets == 3 ? fixture.syncPage(empty: true) : online(),
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
      final receipts = ReceiptQueue(api, activity);
      final channel = RealtimeChannel(api, realtime.DialFixture());
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
      await coordinator.start(fixture.context());
      final original = results.single;
      await coordinator.resume();
      expect(
        original.messages.single.redacted,
        isTrue,
        reason: 'authorization failure revokes online-only body immediately',
      );
      expect(gets, 2);
      expect(scheduler.has(const Duration(seconds: 7)), isTrue);
      await coordinator.resume();
      expect(gets, 2, reason: 'resume cannot bypass RetryAfter');
      scheduler.fire(const Duration(seconds: 7));
      await Future<void>.delayed(const Duration(milliseconds: 40));
      expect(afters, ['0', '1', '1', '0']);
      expect(results.last.messages.single.redacted, isFalse);
      expect(original.messages.single.redacted, isTrue);
    },
  );
  test(
    'online failure preserves the pending delivery proof before history replay',
    () async {
      final (_, store) = await fixture.storage();
      var gets = 0, puts = 0;
      final proofs = <String>[];
      final afters = <String>[];
      final proofA = fixture.syncPage()['sync_proof'] as String;
      final proofB = '${'Ag' * 21}A';
      final proofC = '${'Aw' * 21}A';
      Map<String, Object?> online(int seq, String proof) {
        final page = fixture.syncPage(seq: seq, proof: proof);
        final message =
            (page['messages'] as List).single as Map<String, Object?>;
        message['cache_until'] = fixture.server.toIso8601String();
        message['online_until'] = fixture.server
            .add(const Duration(seconds: 5))
            .toIso8601String();
        return page;
      }

      final transport = fixture.ScriptedTransport((request) {
        if (request.uri.path.endsWith('/tickets'))
          return fixture.response({
            'ticket': realtime.ticket,
            'expires_in_seconds': 30,
          });
        if (request.method == 'PUT') {
          // Evidence is the real product request payload, not a mock call count.
          proofs.add(
            (jsonDecode(utf8.decode(request.body!)) as Map)['sync_proof']
                as String,
          );
          if (++puts == 2)
            return fixture.response({'error_code': 'unavailable'}, status: 503);
          return fixture.response(fixture.state(scanned: puts == 1 ? 1 : 2));
        }
        afters.add(request.uri.queryParameters['after_seq']!);
        gets++;
        if (gets == 1) return fixture.response(online(1, proofA));
        if (gets == 2) return fixture.response(online(2, proofB));
        final page = online(2, proofC);
        page['messages'] = [
          ...online(1, proofC)['messages'] as List,
          ...page['messages'] as List,
        ];
        return fixture.response(page);
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
      final original = results.single;
      await coordinator.resume();
      expect(original.messages.single.redacted, isTrue);
      expect((await store.pendingReceipts()).single.syncProof, proofB);
      // Both purge and retry timers are due at this instant. Fire the captured
      // due timers rather than accidentally selecting only the purge timer.
      final due = scheduler.scheduled
          .where(
            (item) => item.$1 == const Duration(seconds: 1) && item.$2.isActive,
          )
          .map((item) => item.$2)
          .toList();
      for (final timer in due) {
        timer.fire();
      }
      final waited = Stopwatch()..start();
      while (proofs.length < 4 && waited.elapsed < const Duration(seconds: 3)) {
        await Future<void>.delayed(const Duration(milliseconds: 10));
      }
      expect(proofs, [proofA, proofB, proofB, proofC]);
      expect(afters, ['0', '1', '0']);
      expect(await store.pendingReceipts(), isEmpty);
      expect(results.last.messages.where((m) => !m.redacted).length, 2);
    },
  );
}
