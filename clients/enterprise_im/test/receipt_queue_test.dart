import 'dart:async';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/seq.dart';
import 'package:enterprise_im/core/sync/sync_engine.dart';
import 'package:enterprise_im/core/sync/receipt_queue.dart';
import 'package:enterprise_im/platform/app_activity.dart';
import 'sync_test.dart' as fixture;

void main() {
  test('testBackgroundAndStaleGenerationNeverRead', () async {
    final (_, store) = await fixture.storage();
    var reads = 0;
    final transport = fixture.ScriptedTransport((req) {
      if (req.uri.path.endsWith('/read')) reads++;
      return fixture.response(
        req.method == 'GET' ? fixture.syncPage() : fixture.state(),
      );
    });
    final api = ApiClient(Uri.parse('https://im.example'), transport),
        activity = AppActivity();
    final engine = SyncEngine(api, store);
    addTearDown(engine.stop);
    final queue = ReceiptQueue(api, activity);
    addTearDown(queue.dispose);
    addTearDown(activity.dispose);
    final ctx = fixture.context();
    final result = await engine.syncConversation(ctx);
    queue.acceptDelivered(result, ctx);
    activity.update(
      foreground: false,
      focused: true,
      activeConversation: fixture.conversation,
    );
    queue.observe(Seq.parse('1'), 1, ctx);
    queue.confirmVisible([Seq.parse('1')], ctx);
    await Future<void>.delayed(const Duration(milliseconds: 850));
    expect(reads, 0);
    activity.update(
      foreground: true,
      focused: true,
      activeConversation: fixture.conversation,
    );
    queue.observe(Seq.parse('1'), 1, ctx);
    queue.contextChanged();
    queue.confirmVisible([Seq.parse('1')], fixture.context(generation: 2));
    await Future<void>.delayed(const Duration(milliseconds: 850));
    expect(reads, 0);
  });
  test(
    'visible half message requires uninterrupted500ms and300ms batch delay',
    () async {
      final (_, store) = await fixture.storage();
      final read = Completer<void>();
      var calls = 0;
      final elapsed = Stopwatch();
      final transport = fixture.ScriptedTransport((req) {
        if (req.uri.path.endsWith('/read')) {
          calls++;
          expect(elapsed.elapsedMilliseconds, greaterThanOrEqualTo(800));
          expect(fixture.requestBody(req)['seqs'], ['1']);
        }
        return fixture.response(
          req.method == 'GET'
              ? fixture.syncPage()
              : fixture.state(read: req.uri.path.endsWith('/read') ? 1 : 0),
        );
      });
      final api = ApiClient(Uri.parse('https://im.example'), transport),
          activity = AppActivity();
      final engine = SyncEngine(api, store);
      addTearDown(engine.stop);
      final queue = ReceiptQueue(
        api,
        activity,
        onState: (_) {
          if (!read.isCompleted) read.complete();
        },
      );
      addTearDown(queue.dispose);
      addTearDown(activity.dispose);
      final ctx = fixture.context();
      queue.acceptDelivered(await engine.syncConversation(ctx), ctx);
      activity.update(
        foreground: true,
        focused: true,
        activeConversation: fixture.conversation,
      );
      elapsed.start();
      queue.observe(Seq.parse('1'), .49, ctx);
      queue.confirmVisible([Seq.parse('1')], ctx);
      expect(calls, 0);
      queue.observe(Seq.parse('1'), .5, ctx);
      queue.confirmVisible([Seq.parse('1')], ctx);
      await Future<void>.delayed(const Duration(milliseconds: 450));
      expect(calls, 0);
      await read.future.timeout(const Duration(seconds: 2));
      expect(calls, 1);
      expect(queue.locallyReadSeqs, [Seq.parse('1')]);
    },
  );
  test('testTwoDevicesMergeUserReadWithoutOwnDeliveryForgery', () async {
    final (_, store) = await fixture.storage();
    final read = Completer<void>();
    var reads = 0;
    final transport = fixture.ScriptedTransport((req) {
      if (req.uri.path.endsWith('/read')) {
        reads++;
        expect(fixture.requestBody(req)['seqs'], ['1']);
      }
      return fixture.response(
        req.method == 'GET' ? fixture.syncPage() : fixture.state(read: 9),
      );
    });
    final api = ApiClient(Uri.parse('https://im.example'), transport),
        activity = AppActivity();
    final engine = SyncEngine(api, store);
    addTearDown(engine.stop);
    final queue = ReceiptQueue(
      api,
      activity,
      onState: (_) {
        if (!read.isCompleted) read.complete();
      },
    );
    addTearDown(queue.dispose);
    addTearDown(activity.dispose);
    final ctx = fixture.context();
    final result = await engine.syncConversation(ctx);
    expect(result.locallyReadSeqs, isEmpty);
    queue.acceptDelivered(result, ctx);
    activity.update(
      foreground: true,
      focused: true,
      activeConversation: fixture.conversation,
    );
    queue.observe(Seq.parse('9'), 1, ctx);
    queue.confirmVisible([Seq.parse('9')], ctx);
    await Future<void>.delayed(const Duration(milliseconds: 850));
    expect(reads, 0);
    queue.observe(Seq.parse('1'), 1, ctx);
    await read.future.timeout(const Duration(seconds: 2));
    expect(reads, 1);
    expect(queue.locallyReadSeqs, [Seq.parse('1')]);
  });
  test('focus and modal changes interrupt continuous exposure', () async {
    final (_, store) = await fixture.storage();
    final read = Completer<void>();
    var reads = 0;
    final transport = fixture.ScriptedTransport((req) {
      if (req.uri.path.endsWith('/read')) {
        reads++;
      }
      return fixture.response(
        req.method == 'GET' ? fixture.syncPage() : fixture.state(),
      );
    });
    final api = ApiClient(Uri.parse('https://im.example'), transport),
        activity = AppActivity();
    final engine = SyncEngine(api, store);
    addTearDown(engine.stop);
    final queue = ReceiptQueue(
      api,
      activity,
      onState: (_) {
        if (!read.isCompleted) read.complete();
      },
    );
    addTearDown(queue.dispose);
    addTearDown(activity.dispose);
    final ctx = fixture.context();
    queue.acceptDelivered(await engine.syncConversation(ctx), ctx);
    activity.update(
      foreground: true,
      focused: true,
      activeConversation: fixture.conversation,
    );
    queue.observe(Seq.parse('1'), 1, ctx);
    await Future<void>.delayed(const Duration(milliseconds: 350));
    activity.update(
      foreground: true,
      focused: false,
      activeConversation: fixture.conversation,
      blocked: true,
    );
    await Future<void>.delayed(const Duration(milliseconds: 100));
    activity.update(
      foreground: true,
      focused: true,
      activeConversation: fixture.conversation,
    );
    await Future<void>.delayed(const Duration(milliseconds: 450));
    expect(reads, 0);
    await read.future.timeout(const Duration(seconds: 2));
    expect(reads, 1);
  });
  test('501 visible messages use exact sorted batches capped at500', () async {
    final (_, store) = await fixture.storage();
    var head = 0;
    final batches = <List<Object?>>[];
    final finished = Completer<void>();
    final transport = fixture.ScriptedTransport((request) {
      if (request.uri.path.endsWith('/read')) {
        batches.add(
          (fixture.requestBody(request)['seqs'] as List<Object?>).toList(),
        );
        return fixture.response(fixture.state(scanned: 501, read: 501));
      }
      if (request.method == 'PUT')
        return fixture.response(fixture.state(scanned: head));
      final after = int.parse(request.uri.queryParameters['after_seq']!);
      head = (after + 100).clamp(0, 501);
      final page = fixture.syncPage(seq: head);
      final one =
          (page['messages'] as List<Object?>).single as Map<String, Object?>;
      page['messages'] = [
        for (var seq = after + 1; seq <= head; seq++) {...one, 'seq': '$seq'},
      ];
      page['has_more'] = head < 501;
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
    final queue = ReceiptQueue(
      api,
      activity,
      onState: (_) {
        if (batches.length == 2 && !finished.isCompleted) finished.complete();
      },
    );
    addTearDown(engine.stop);
    addTearDown(queue.dispose);
    addTearDown(activity.dispose);
    final ctx = fixture.context();
    for (var i = 0; i < 6; i++) {
      queue.acceptDelivered(await engine.syncConversation(ctx), ctx);
    }
    for (var i = 501; i > 0; i--) {
      queue.observe(Seq.parse('$i'), 1, ctx);
    }
    await finished.future.timeout(const Duration(seconds: 4));
    expect(batches.map((batch) => batch.length).toList(), [500, 1]);
    expect(batches.expand((batch) => batch).toList(), [
      for (var seq = 1; seq <= 501; seq++) '$seq',
    ]);
    expect(queue.locallyReadSeqs.length, 501);
  });
}
