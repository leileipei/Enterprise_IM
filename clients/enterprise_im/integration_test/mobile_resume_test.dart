import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:enterprise_im/platform/native_storage.dart';
import 'package:enterprise_im/core/storage/encrypted_store.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/seq.dart';
import 'package:enterprise_im/core/sync/sync_engine.dart';
import 'package:enterprise_im/core/sync/sync_coordinator.dart';
import 'package:enterprise_im/core/sync/receipt_queue.dart';
import 'package:enterprise_im/core/sync/realtime_channel.dart';
import 'package:enterprise_im/platform/mobile_activity.dart';
import '../test/sync_test.dart' as fixture;
import '../test/realtime_channel_test.dart' as realtime;

final class LifecycleProbe with WidgetsBindingObserver {
  final void Function(AppLifecycleState) changed;
  LifecycleProbe(this.changed);
  @override
  void didChangeAppLifecycleState(AppLifecycleState state) => changed(state);
}

// Actual OS background/foreground; production activity/sync/receipts/SQLite.
// API and socket are controlled contracts, not a deployed backend or IdP.
void main() {
  final binding = IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  final adapter = mobileActivityAdapter();
  const probe = String.fromEnvironment('IM_MOBILE_RESUME_PROBE_ID');
  testWidgets(
    'OS suspension invalidates read and resume reauthorizes device pull',
    (tester) async {
      expect(Platform.isAndroid || Platform.isIOS, isTrue);
      expect(probe, matches(RegExp(r'^[a-f0-9]{32}$')));
      addTearDown(adapter.dispose);
      await tester.pumpWidget(
        const MaterialApp(home: Scaffold(body: Text('挂起恢复与设备授权验证'))),
      );
      await tester.runAsync(() async {
        final file = File('${Directory.systemTemp.path}/im-resume-$probe.json');
        final support = await nativeStorageRoot();
        final storage = await support.createTemp('im-mobile-resume-');
        addTearDown(() async {
          if (await storage.exists()) await storage.delete(recursive: true);
        });
        final store = await EncryptedStore.open(
          'a' * 64,
          List<int>.filled(32, 5),
          directory: storage,
          generation: 1,
        );
        addTearDown(store.close);
        final ctx = fixture.context();
        final results = <SyncResult>[], failures = <ApiFailure>[];
        final lifecycle = <String>[], backgrounds = <int>[];
        final freshAfter = <String>[], reads = <List<String>>[];
        final transcript = <String>[];
        final pendingRead = Completer<ApiResponse>(),
            pendingPage = Completer<ApiResponse>();
        final readAck = Completer<void>();
        var revoked = false, delivered = 0, tickets = 0, requestCount = 0;
        var oldAckIgnored = false, oldRedacted = false, pendingReads = -1;
        var cleared = false, terminal = false, socketClosed = false;
        String? catchup, nextResume;
        var acknowledgedReads = <String>[];
        final api = ApiClient(
          Uri.parse('https://im.example'),
          fixture.ScriptedTransport((r) {
            requestCount++;
            expect(r.headers['Authorization'], 'Bearer memory-access');
            expect(r.headers['X-Acting-Membership-ID'], fixture.membership);
            if (r.uri.path.endsWith('/tickets')) {
              expect(r.headers.containsKey('X-IM-Device-ID'), isFalse);
              expect(r.headers.containsKey('X-IM-Device-Token'), isFalse);
              tickets++;
              return fixture.response({
                'ticket': realtime.ticket,
                'expires_in_seconds': 30,
              });
            }
            expect(r.headers['X-IM-Device-ID'], ctx.device.deviceId);
            expect(r.headers['X-IM-Device-Token'], ctx.device.deviceToken);
            if (r.uri.path.endsWith('/read')) {
              final seqs = (jsonDecode(utf8.decode(r.body!))['seqs'] as List)
                  .cast<String>();
              reads.add(seqs);
              transcript.add('read:${seqs.join(',')}');
              if (reads.length == 1) return pendingRead.future;
              expect(seqs, ['2']);
              return fixture.response(fixture.state(scanned: 2, read: 2));
            }
            if (r.uri.path.endsWith('/delivered')) {
              delivered++;
              transcript.add('delivered:$delivered');
              return fixture.response(
                fixture.state(scanned: delivered == 1 ? 1 : 2),
              );
            }
            expect(r.method, 'GET');
            expect(
              r.uri.path,
              '/api/v1/conversations/${fixture.conversation}/sync',
            );
            final cursor = r.uri.queryParameters['after_seq']!;
            transcript.add('sync:$cursor');
            if (cursor != '0') {
              // A ready socket also requests a normal incremental refresh.
              expect(cursor, freshAfter.length == 1 ? '1' : '2');
              expect(revoked, isFalse);
              return fixture.response(
                fixture.syncPage(seq: int.parse(cursor), empty: true),
              );
            }
            freshAfter.add(cursor);
            if (revoked)
              return fixture.response({
                'error_code': 'invalid_device',
              }, status: 401);
            if (freshAfter.length == 2) return pendingPage.future;
            expect(freshAfter.length, 1);
            return fixture.response(fixture.syncPage());
          }),
        );
        addTearDown(api.close);
        final dial = realtime.DialFixture();
        final channel = RealtimeChannel(api, dial);
        final queue = ReceiptQueue(
          api,
          adapter.activity,
          onState: (_) {
            if (!readAck.isCompleted) readAck.complete();
          },
        );
        final coordinator = SyncCoordinator(
          SyncEngine(api, store),
          channel,
          queue,
          adapter.activity,
          onResult: (result) {
            if (result.messages.isNotEmpty) results.add(result);
          },
          onFailure: failures.add,
        );
        addTearDown(() async {
          await coordinator.dispose();
          queue.dispose();
          await channel.dispose();
        });
        Future<void> until(bool Function() condition) async {
          final end = Stopwatch()..start();
          while (!condition() && end.elapsed < const Duration(seconds: 35)) {
            await Future<void>.delayed(const Duration(milliseconds: 20));
          }
          expect(
            condition(),
            isTrue,
            reason: 'native resume condition deadline',
          );
        }

        void stageNow(String phase, {String status = 'running'}) {
          final temp = File('${file.path}.new');
          temp.writeAsStringSync(
            jsonEncode({
              'phase': phase,
              'status': status,
              'pid': pid,
              'probe': probe,
              'lifecycle': lifecycle,
              'background_ms': backgrounds,
              'old_read_ack_ignored': oldAckIgnored,
              'old_page_redacted': oldRedacted,
              'pending_resume_reads': pendingReads,
              'fresh_after': freshAfter,
              'fresh_device_headers': true,
              'catchup_cursor': catchup,
              'read_seqs': acknowledgedReads,
              'socket_closed_on_pause': socketClosed,
              'device_denial': failures.isEmpty ? null : failures.last.code,
              'received_state_cleared': cleared,
              'terminal_no_restart': terminal,
              'transcript': transcript,
              'read_requests': reads,
              'delivered_requests': delivered,
            }),
            flush: true,
          );
          temp.renameSync(file.path);
        }

        Stopwatch? backgroundClock;
        Object? lifecycleError;
        final observer = LifecycleProbe((state) {
          try {
            if (state == AppLifecycleState.paused && nextResume != null) {
              if (adapter.activity.foreground ||
                  adapter.activity.focused ||
                  !results.last.messages.every((m) => m.redacted)) {
                throw StateError('native_pause_did_not_withdraw_authority');
              }
              lifecycle.add('paused');
              backgroundClock = Stopwatch()..start();
              final phase = nextResume!;
              nextResume = null;
              if (phase == 'resume-fresh') {
                oldRedacted = results.single.messages.every((m) => m.redacted);
                pendingRead.complete(fixture.response(fixture.state(read: 1)));
              }
              // Do not depend on a Dart timer executing while iOS is suspended.
              // Host reads this atomic marker, waits >=1.15s, then foregrounds us.
              stageNow(phase);
            } else if (state == AppLifecycleState.resumed &&
                backgroundClock != null) {
              lifecycle.add('resumed');
              backgrounds.add(backgroundClock!.elapsedMilliseconds);
              backgroundClock = null;
            }
          } catch (error) {
            lifecycleError = error;
            stageNow('failed', status: 'failed');
          }
        });
        WidgetsBinding.instance.addObserver(observer);
        addTearDown(() => WidgetsBinding.instance.removeObserver(observer));
        Future<void> cycle(String pausePhase, String resumePhase) async {
          final count = backgrounds.length;
          nextResume = resumePhase;
          stageNow(pausePhase);
          await until(
            () => backgrounds.length == count + 1 || lifecycleError != null,
          );
          if (lifecycleError != null) throw lifecycleError!;
          expect(backgrounds.last, greaterThanOrEqualTo(1000));
          expect(binding.lifecycleState, AppLifecycleState.resumed);
          expect(adapter.activity.foreground, isTrue);
          expect(adapter.activity.focused, isTrue);
        }

        Future<void> ready() async {
          dial.socket.controller.add('{"type":"ready","resync_required":true}');
          await until(() => channel.connected);
        }

        try {
          await until(
            () => adapter.activity.foreground && adapter.activity.focused,
          );
          adapter.activity.update(
            foreground: adapter.activity.foreground,
            focused: adapter.activity.focused,
            activeConversation: fixture.conversation,
          );
          await coordinator.start(ctx);
          await ready();
          expect(failures, isEmpty);
          expect(results, hasLength(1));
          expect(results.single.canRead(Seq.parse('1')), isTrue);
          queue.observe(Seq.parse('1'), 1, ctx);
          await until(() => reads.length == 1);
          final oldSocket = dial.socket;
          final beforeBackground = requestCount;
          await cycle('pause-read', 'resume-fresh');
          await oldSocket.closedDone.future.timeout(const Duration(seconds: 5));
          expect(oldSocket.closed, isTrue);
          socketClosed = true;
          expect(results.single.canRead(Seq.parse('1')), isFalse);
          expect(queue.locallyReadSeqs, isEmpty);
          oldAckIgnored = true;
          expect(reads, hasLength(1));
          await until(
            () => freshAfter.length == 2 && !identical(dial.socket, oldSocket),
          );
          expect(
            requestCount,
            beforeBackground + 2,
          ); // new fresh pull + ticket only
          await ready();
          queue.observe(Seq.parse('1'), 1, ctx);
          await Future<void>.delayed(const Duration(milliseconds: 1100));
          expect(failures, isEmpty);
          expect(results, hasLength(1));
          expect(reads, hasLength(1));
          expect(queue.locallyReadSeqs, isEmpty);
          pendingReads = reads.length - 1;
          final page = fixture.syncPage(seq: 2);
          page['messages'] = [
            (fixture.syncPage()['messages'] as List).single,
            (page['messages'] as List).single,
          ];
          pendingPage.complete(fixture.response(page));
          await until(() => results.length == 2);
          catchup = (await store.committedCursor(
            fixture.conversation,
          )).toString();
          expect(catchup, '2');
          expect(results.last.deliveredSeqs.map((s) => s.toString()), [
            '1',
            '2',
          ]);
          queue.observe(Seq.parse('2'), 1, ctx);
          await readAck.future.timeout(const Duration(seconds: 8));
          expect(queue.locallyReadSeqs, [Seq.parse('2')]);
          acknowledgedReads = queue.locallyReadSeqs
              .map((s) => s.toString())
              .toList();
          expect(reads, [
            ['1'],
            ['2'],
          ]);
          expect(failures, isEmpty);
          revoked = true;
          final before = reads.length, deliveredBefore = delivered;
          await cycle('pause-revoke', 'resume-revoke');
          await until(
            () => failures.any(
              (e) => e.code == 'invalid_device' && e.status == 401,
            ),
          );
          expect(failures, hasLength(1));
          expect(await store.committedCursor(fixture.conversation), isNull);
          expect(await store.pendingReceipts(), isEmpty);
          cleared = true;
          queue.observe(Seq.parse('1'), 1, ctx);
          queue.observe(Seq.parse('2'), 1, ctx);
          await Future<void>.delayed(const Duration(milliseconds: 1100));
          expect(reads.length, before);
          expect(delivered, deliveredBefore);
          expect(results, hasLength(2));
          expect(freshAfter, ['0', '0', '0']);
          final requests = requestCount, ticketCount = tickets;
          await cycle('pause-terminal', 'resume-terminal');
          await Future<void>.delayed(const Duration(milliseconds: 1100));
          expect(requestCount, requests);
          expect(tickets, ticketCount);
          expect(reads.length, before);
          expect(delivered, deliveredBefore);
          terminal = true;
          expect(queue.locallyReadSeqs, isEmpty);
          stageNow('done', status: 'passed');
        } catch (_) {
          stageNow('failed', status: 'failed');
          rethrow;
        }
      });
    },
    timeout: const Timeout(Duration(minutes: 6)),
  );
}
