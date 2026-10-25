import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/models.dart';
import 'package:enterprise_im/core/storage/encrypted_store.dart';
import 'package:enterprise_im/core/sync/realtime_channel.dart';
import 'package:enterprise_im/core/sync/receipt_queue.dart';
import 'package:enterprise_im/core/sync/sync_engine.dart';
import 'package:enterprise_im/platform/app_activity.dart';
import 'package:enterprise_im/features/conversations/conversation_view.dart';
import 'package:enterprise_im/ui/message_list.dart';
import 'auth_test.dart' as oidc;
import 'sync_test.dart' as disk;
import 'message_send_test.dart' as sending;
import 'navigation_test.dart' as nav;
import 'login_session_test.dart' show pumpUntil;

class OfflineSocket implements RealtimeConnector {
  @override
  Future<RealtimeConnection> connect(
    Uri uri,
    List<String> protocols,
    Cancellation cancellation,
  ) async => throw const ApiFailure('unavailable');
}

Map<String, Object?> page({int cacheSeconds = 300, bool empty = false}) => {
  ...disk.syncPage(empty: empty),
  'conversation_id': nav.dm,
  if (!empty)
    'messages': [
      {
        ...(disk.syncPage()['messages'] as List).first as Map<String, Object?>,
        'text': '真实授权正文',
        'cache_until': disk.server
            .add(Duration(seconds: cacheSeconds))
            .toIso8601String(),
      },
    ],
};
ApiResponse metadata(ApiRequest request) => oidc.response(
  request.uri.path.endsWith('/receipts')
      ? {
          'receipts': [
            {
              'seq': '1',
              'status': 'sent',
              'summary_available': false,
              'visible_recipient_count': 0,
              'visible_delivered_count': 0,
              'visible_read_count': 0,
              'delivered_at': null,
              'read_at': null,
            },
          ],
        }
      : {
          'unread_count': 1,
          'exact': false,
          'head_seq': '1',
          'receipt_revision': '1',
          'policy_version': '1',
        },
);
void main() {
  for (final denied in [false, true]) {
    test(
      'manual refresh reauthorizes expired history denied=$denied',
      () async {
        final afters = <String>[];
        var revoke = false;
        final owner = await nav.owner((r, _) async {
          if (r.uri.path.endsWith('/tickets'))
            throw const ApiFailure('unavailable');
          if (r.uri.path.endsWith('/sync')) {
            final after = r.uri.queryParameters['after_seq']!;
            afters.add(after);
            if (after == '1') return oidc.response(page(empty: true));
            final fresh = page(cacheSeconds: afters.length == 1 ? 1 : 300);
            if (revoke)
              fresh['messages'] = [
                {'seq': '1', 'redacted': true},
              ];
            return oidc.response(fresh);
          }
          if (r.uri.path.endsWith('/delivered'))
            return oidc.response(disk.state());
          return metadata(r);
        });
        addTearDown(owner.logout);
        final (roles, context) = await sending.admission(owner);
        addTearDown(roles.close);
        final activity = AppActivity()
          ..update(foreground: true, focused: true, activeConversation: nav.dm);
        addTearDown(activity.dispose);
        final view = ConversationView(
          roles,
          activity,
          connector: OfflineSocket(),
        );
        addTearDown(view.close);
        final expired = Completer<void>();
        view.addListener(() {
          if (view.messages.isNotEmpty &&
              view.messages.single.redacted &&
              !expired.isCompleted) {
            expired.complete();
          }
        });
        await view.select(context, ConversationSummary.fromJson(nav.direct()));
        expect(view.messages.single.text, '真实授权正文');
        await expired.future.timeout(const Duration(seconds: 4));
        expect(view.messages.single.redacted, isTrue);
        revoke = denied;
        await view.refresh();
        expect(afters, ['0', '1', '0']);
        expect(view.messages.single.redacted, denied);
        expect(view.messages.single.text, denied ? isNull : '真实授权正文');
        expect(
          (await roles.current!.client.store.committedCursor(
            nav.dm,
          )).toString(),
          '1',
        );
      },
    );
  }

  for (final paginated in [false, true]) {
    test(
      'manual refresh finishes in-flight proof retry before history replay paginated=$paginated',
      () async {
        final afters = <String>[], proofs = <String>[];
        final entered = Completer<void>(), recovered = Completer<void>();
        final blocked = Completer<ApiResponse>();
        final proofA = page()['sync_proof'] as String;
        final proofB = '${'Ag' * 21}A',
            proofC = '${'Aw' * 21}A',
            proofD = '${'BA' * 21}A';
        Map<String, Object?> second() {
          final result = page();
          result['sync_proof'] = proofB;
          result['next_after_seq'] = '2';
          (result['messages'] as List).single['seq'] = '2';
          return result;
        }

        final owner = await nav.owner((r, _) async {
          if (r.uri.path.endsWith('/tickets'))
            throw const ApiFailure('unavailable');
          if (r.uri.path.endsWith('/sync')) {
            final after = r.uri.queryParameters['after_seq']!;
            afters.add(after);
            if (afters.length == 1) return oidc.response(page());
            if (afters.length == 2) {
              entered.complete();
              return blocked.future;
            }
            if (paginated) {
              final replay = after == '0' ? page() : second();
              replay['sync_proof'] = after == '0' ? proofC : proofD;
              replay['has_more'] = after == '0';
              return oidc.response(replay);
            }
            final full = second();
            full['sync_proof'] = proofC;
            full['messages'] = [
              ...page()['messages'] as List,
              ...full['messages'] as List,
            ];
            return oidc.response(full);
          }
          if (r.uri.path.endsWith('/delivered')) {
            final proof =
                (jsonDecode(utf8.decode(r.body!)) as Map)['sync_proof']
                    as String;
            proofs.add(proof);
            if (proofs.length == 2) {
              return ApiResponse(
                503,
                utf8.encode('{"error_code":"unavailable"}'),
                {'retry-after': '1'},
              );
            }
            return oidc.response(
              disk.state(scanned: proofs.length == 1 ? 1 : 2),
            );
          }
          return metadata(r);
        });
        addTearDown(owner.logout);
        final (roles, context) = await sending.admission(owner);
        addTearDown(roles.close);
        final activity = AppActivity()
          ..update(foreground: true, focused: true, activeConversation: nav.dm);
        addTearDown(activity.dispose);
        final view = ConversationView(
          roles,
          activity,
          connector: OfflineSocket(),
        );
        addTearDown(view.close);
        view.addListener(() {
          if (proofs.length >= 4 &&
              view.messages.length == 2 &&
              view.messages.every((m) => !m.redacted) &&
              !recovered.isCompleted)
            recovered.complete();
        });
        await view.select(context, ConversationSummary.fromJson(nav.direct()));
        final incremental = view.coordinator.resume();
        await entered.future;
        final refresh = view.refresh();
        final coalesced = view.refresh();
        blocked.complete(oidc.response(second()));
        await Future.wait([incremental, refresh, coalesced]);
        expect(
          (await roles.current!.client.store.pendingReceipts())
              .single
              .syncProof,
          proofB,
        );
        await view.refresh();
        expect(proofs, [
          proofA,
          proofB,
        ], reason: 'manual refresh cannot bypass Retry-After');
        expect(afters, ['0', '1']);
        await recovered.future.timeout(const Duration(seconds: 5));
        expect(
          proofs,
          paginated
              ? [proofA, proofB, proofB, proofC, proofD]
              : [proofA, proofB, proofB, proofC],
        );
        expect(afters, paginated ? ['0', '1', '0', '1'] : ['0', '1', '0']);
        expect(await roles.current!.client.store.pendingReceipts(), isEmpty);
        expect(
          (await roles.current!.client.store.committedCursor(
            nav.dm,
          )).toString(),
          '2',
        );
        expect(view.messages.every((m) => !m.redacted), isTrue);
      },
    );
  }

  test(
    'manual refresh does not replay a late page after backgrounding',
    () async {
      final afters = <String>[];
      final blocked = Completer<ApiResponse>(), entered = Completer<void>();
      var delivered = 0;
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/tickets'))
          throw const ApiFailure('unavailable');
        if (r.uri.path.endsWith('/sync')) {
          afters.add(r.uri.queryParameters['after_seq']!);
          if (afters.length == 1) return oidc.response(page());
          entered.complete();
          return blocked.future;
        }
        if (r.uri.path.endsWith('/delivered')) {
          delivered++;
          return oidc.response(disk.state());
        }
        return metadata(r);
      });
      addTearDown(owner.logout);
      final (roles, context) = await sending.admission(owner);
      addTearDown(roles.close);
      final activity = AppActivity()
        ..update(foreground: true, focused: true, activeConversation: nav.dm);
      addTearDown(activity.dispose);
      final view = ConversationView(
        roles,
        activity,
        connector: OfflineSocket(),
      );
      addTearDown(view.close);
      await view.select(context, ConversationSummary.fromJson(nav.direct()));
      final syncing = view.coordinator.resume();
      await entered.future;
      final refresh = view.refresh();
      activity.update(
        foreground: false,
        focused: false,
        activeConversation: nav.dm,
      );
      blocked.complete(oidc.response(page(empty: true)));
      await Future.wait([syncing, refresh]);
      await view.refresh();
      expect(afters, ['0', '1']);
      expect(
        delivered,
        1,
        reason: 'a cancelled late page has no delivery authority',
      );
      expect(view.messages.every((m) => m.redacted), isTrue);
      expect(view.canSend, isFalse);
    },
  );

  test(
    'decorative notification failure cannot stop authorized messaging',
    () async {
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/tickets'))
          throw const ApiFailure('unavailable');
        if (r.uri.path.endsWith('/sync')) return oidc.response(page());
        if (r.uri.path.endsWith('/delivered'))
          return oidc.response(disk.state());
        return metadata(r);
      });
      addTearDown(owner.logout);
      final (roles, context) = await sending.admission(owner);
      addTearDown(roles.close);
      final activity = AppActivity()
        ..update(foreground: true, focused: true, activeConversation: nav.dm);
      addTearDown(activity.dispose);
      var notifications = 0;
      final view = ConversationView(
        roles,
        activity,
        connector: OfflineSocket(),
        onAuthorizedPage: (result) {
          notifications++;
          throw StateError('notification unavailable');
        },
      );
      addTearDown(view.close);
      await view.select(context, ConversationSummary.fromJson(nav.direct()));
      expect(notifications, 1);
      expect(view.canSend, isTrue);
      expect(view.messages.single.text, '真实授权正文');
    },
  );

  for (final laterMetadataError in [false, true]) {
    test(
      'read retry success clears only its own warning metadata=$laterMetadataError',
      () async {
        final secondRead = Completer<void>(),
            releaseRead = Completer<ApiResponse>();
        final failed = Completer<void>(), recovered = Completer<void>();
        var attempts = 0, metadataError = false;
        final owner = await nav.owner((request, _) async {
          if (request.uri.path.endsWith('/tickets'))
            throw const ApiFailure('unavailable');
          if (request.uri.path.endsWith('/sync')) return oidc.response(page());
          if (request.uri.path.endsWith('/delivered'))
            return oidc.response(disk.state());
          if (request.uri.path.endsWith('/read')) {
            attempts++;
            if (attempts == 1)
              return ApiResponse(
                503,
                utf8.encode('{"error_code":"unavailable"}'),
                {'retry-after': '1'},
              );
            if (!secondRead.isCompleted) secondRead.complete();
            return releaseRead.future;
          }
          if (metadataError)
            return ApiResponse(
              429,
              utf8.encode('{"error_code":"rate_limited"}'),
              {'retry-after': '3600'},
            );
          return metadata(request);
        });
        addTearDown(owner.logout);
        final (roles, context) = await sending.admission(owner);
        addTearDown(roles.close);
        final activity = AppActivity()
          ..update(foreground: true, focused: true, activeConversation: nav.dm);
        addTearDown(activity.dispose);
        final view = ConversationView(
          roles,
          activity,
          connector: OfflineSocket(),
        );
        addTearDown(view.close);
        view.addListener(() {
          if (view.failure?.status == 503 && !failed.isCompleted)
            failed.complete();
          if (view.receipts.locallyReadSeqs.isNotEmpty &&
              !recovered.isCompleted)
            recovered.complete();
        });
        await view.select(context, ConversationSummary.fromJson(nav.direct()));
        view.receipts.observe(view.messages.single.seq, 1, context);
        await failed.future.timeout(Duration(seconds: 6));
        expect(view.failure?.status, 503);
        await secondRead.future.timeout(Duration(seconds: 6));
        if (laterMetadataError) {
          metadataError = true;
          await view.refreshMetadata();
          expect(view.failure?.status, 429);
        }
        releaseRead.complete(oidc.response(disk.state(read: 1)));
        await recovered.future.timeout(Duration(seconds: 6));
        expect(attempts, 2);
        expect(view.receipts.locallyReadSeqs.map((s) => s.toString()), ['1']);
        expect(view.failure?.status, laterMetadataError ? 429 : isNull);
        expect(view.canSend, isTrue);
      },
    );
  }

  for (final mixed in [false, true]) {
    test(
      'recipient metadata preserves conversation and only queries own messages mixed=$mixed',
      () async {
        late String userId;
        final owner = await nav.owner((r, _) async {
          if (r.uri.path.endsWith('/tickets'))
            throw const ApiFailure('unavailable');
          if (r.uri.path.endsWith('/sync')) {
            final incoming =
                (page()['messages'] as List).single as Map<String, Object?>;
            return oidc.response({
              ...page(),
              'next_after_seq': mixed ? '2' : '1',
              'messages': [
                incoming,
                if (mixed)
                  {
                    ...incoming,
                    'seq': '2',
                    'sender_user_id': userId,
                    'text': '本人发送',
                  },
              ],
            });
          }
          if (r.uri.path.endsWith('/delivered'))
            return oidc.response(disk.state(scanned: mixed ? 2 : 1));
          if (r.uri.path.endsWith('/receipts')) {
            // The production endpoint rejects any incoming message in this batch.
            if (r.uri.queryParameters['seqs'] != '2')
              return ApiResponse(
                404,
                utf8.encode('{"error_code":"not_found"}'),
                const {},
              );
            return oidc.response({
              'receipts': [
                {
                  'seq': '2',
                  'status': 'read',
                  'summary_available': true,
                  'visible_recipient_count': 1,
                  'visible_delivered_count': 1,
                  'visible_read_count': 1,
                  'delivered_at': '2026-10-09T00:00:00Z',
                  'read_at': '2026-10-09T00:00:00Z',
                },
              ],
            });
          }
          return metadata(r);
        });
        userId = owner.current!.session.userId;
        addTearDown(owner.logout);
        final (roles, context) = await sending.admission(owner);
        addTearDown(roles.close);
        final activity = AppActivity()
          ..update(foreground: true, focused: true, activeConversation: nav.dm);
        addTearDown(activity.dispose);
        final view = ConversationView(
          roles,
          activity,
          connector: OfflineSocket(),
        );
        addTearDown(view.close);
        await view.select(context, ConversationSummary.fromJson(nav.direct()));
        await view.refreshMetadata();
        expect(view.context, isNotNull);
        expect(view.canSend, isTrue);
        expect(
          view.messages.map((m) => m.text),
          mixed ? ['真实授权正文', '本人发送'] : ['真实授权正文'],
        );
        expect(view.unread, isNotNull);
        expect(
          view.summaries.keys.map((seq) => seq.toString()),
          mixed ? ['2'] : isEmpty,
        );
        if (mixed) expect(view.summaries.values.single.status, 'read');
      },
    );
  }
  test(
    'view withdraws ordinary cached body at its deadline without another poll',
    () async {
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/tickets'))
          throw const ApiFailure('unavailable');
        if (r.uri.path.endsWith('/sync'))
          return oidc.response(page(cacheSeconds: 1));
        if (r.uri.path.endsWith('/delivered'))
          return oidc.response(disk.state());
        return metadata(r);
      });
      addTearDown(owner.logout);
      final (roles, context) = await sending.admission(owner);
      addTearDown(roles.close);
      final activity = AppActivity()
        ..update(foreground: true, focused: true, activeConversation: nav.dm);
      addTearDown(activity.dispose);
      final view = ConversationView(
        roles,
        activity,
        connector: OfflineSocket(),
      );
      addTearDown(view.close);
      var withdrawn = false;
      view.addListener(() {
        if (view.messages.isNotEmpty && view.messages.every((m) => m.redacted))
          withdrawn = true;
      });
      await view.select(context, ConversationSummary.fromJson(nav.direct()));
      expect(view.messages.single.text, '真实授权正文');
      await Future<void>.delayed(const Duration(milliseconds: 1200));
      expect(view.messages.single.redacted, isTrue);
      expect(withdrawn, isTrue);
    },
  );
  test(
    'delayed delivered response does not expose body or permit read',
    () async {
      final delivery = Completer<ApiResponse>(), entered = Completer<void>();
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/tickets'))
          throw const ApiFailure('unavailable');
        if (r.uri.path.endsWith('/sync')) return oidc.response(page());
        if (r.uri.path.endsWith('/delivered')) {
          entered.complete();
          return delivery.future;
        }
        return metadata(r);
      });
      addTearDown(owner.logout);
      final (roles, context) = await sending.admission(owner);
      addTearDown(roles.close);
      final activity = AppActivity()
        ..update(foreground: true, focused: true, activeConversation: nav.dm);
      addTearDown(activity.dispose);
      final view = ConversationView(
        roles,
        activity,
        connector: OfflineSocket(),
      );
      addTearDown(view.close);
      final loading = view.select(
        context,
        ConversationSummary.fromJson(nav.direct()),
      );
      await entered.future;
      expect(view.messages, isEmpty);
      expect(view.canSend, isFalse);
      delivery.complete(oidc.response(disk.state()));
      await loading;
      expect(view.messages.single.text, '真实授权正文');
    },
  );
  test(
    'authorization denial for metadata immediately clears message and receipt state',
    () async {
      var deny = false;
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/tickets'))
          throw const ApiFailure('unavailable');
        if (r.uri.path.endsWith('/sync')) return oidc.response(page());
        if (r.uri.path.endsWith('/delivered'))
          return oidc.response(disk.state());
        if (deny)
          return ApiResponse(
            403,
            utf8.encode('{"error_code":"hard_deny"}'),
            {},
          );
        return metadata(r);
      });
      addTearDown(owner.logout);
      final (roles, context) = await sending.admission(owner);
      addTearDown(roles.close);
      final activity = AppActivity()
        ..update(foreground: true, focused: true, activeConversation: nav.dm);
      addTearDown(activity.dispose);
      final view = ConversationView(
        roles,
        activity,
        connector: OfflineSocket(),
      );
      addTearDown(view.close);
      await view.select(context, ConversationSummary.fromJson(nav.direct()));
      await view.refreshMetadata();
      expect(view.messages, isNotEmpty);
      deny = true;
      await view.refreshMetadata();
      expect(view.messages, isEmpty);
      expect(view.summaries, isEmpty);
      expect(view.unread, isNull);
      expect(view.canSend, isFalse);
    },
  );
  test(
    'mixed deadlines advance to next live body instead of a zero-delay loop',
    () async {
      final (_, store) = await disk.storage();
      final first =
          (page(cacheSeconds: 1)['messages'] as List).single
              as Map<String, Object?>;
      final second = {
        ...first,
        'seq': '2',
        'cache_until': disk.server
            .add(const Duration(seconds: 300))
            .toIso8601String(),
      };
      final engine = SyncEngine(
        ApiClient(
          Uri.parse('https://im.example'),
          disk.ScriptedTransport(
            (r) => disk.response(
              r.method == 'GET'
                  ? {
                      ...disk.syncPage(),
                      'messages': [first, second],
                      'next_after_seq': '2',
                    }
                  : disk.state(scanned: 2),
            ),
          ),
        ),
        store,
      );
      addTearDown(engine.stop);
      final result = await engine.syncConversation(disk.context());
      await Future<void>.delayed(const Duration(milliseconds: 1200));
      expect(result.messages.first.redacted, isTrue);
      expect(result.messages.last.redacted, isFalse);
      expect(
        result.contentAuthorizationRemaining,
        greaterThan(const Duration(seconds: 1)),
      );
    },
  );

  test(
    'background clears metadata and requires fresh sync before sending again',
    () async {
      var delaySync = false;
      final delayed = Completer<ApiResponse>(), entered = Completer<void>();
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/tickets'))
          throw const ApiFailure('unavailable');
        if (r.uri.path.endsWith('/sync')) {
          if (r.uri.queryParameters['after_seq'] == '1')
            return oidc.response(page(empty: true));
          if (delaySync) {
            if (!entered.isCompleted) entered.complete();
            return delayed.future;
          }
          return oidc.response(page());
        }
        if (r.uri.path.endsWith('/delivered'))
          return oidc.response(disk.state());
        return metadata(r);
      });
      addTearDown(owner.logout);
      final (roles, context) = await sending.admission(owner);
      addTearDown(roles.close);
      final activity = AppActivity()
        ..update(foreground: true, focused: true, activeConversation: nav.dm);
      addTearDown(activity.dispose);
      final view = ConversationView(
        roles,
        activity,
        connector: OfflineSocket(),
      );
      addTearDown(view.close);
      await view.select(context, ConversationSummary.fromJson(nav.direct()));
      await view.refreshMetadata();
      expect(view.canSend, isTrue);
      expect(view.unread, isNotNull);
      activity.update(
        foreground: false,
        focused: false,
        activeConversation: nav.dm,
      );
      expect(view.unread, isNull);
      expect(view.summaries, isEmpty);
      delaySync = true;
      activity.update(
        foreground: true,
        focused: true,
        activeConversation: nav.dm,
      );
      await entered.future;
      expect(view.canSend, isFalse);
      delayed.complete(oidc.response(page()));
      await view.refresh();
      expect(view.canSend, isTrue);
    },
  );

  test(
    'external close synchronously notifies the visible view to withdraw body',
    () async {
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/tickets'))
          throw const ApiFailure('unavailable');
        if (r.uri.path.endsWith('/sync')) return oidc.response(page());
        if (r.uri.path.endsWith('/delivered'))
          return oidc.response(disk.state());
        return metadata(r);
      });
      addTearDown(owner.logout);
      final (roles, context) = await sending.admission(owner);
      addTearDown(roles.close);
      final activity = AppActivity()
        ..update(foreground: true, focused: true, activeConversation: nav.dm);
      addTearDown(activity.dispose);
      final view = ConversationView(
        roles,
        activity,
        connector: OfflineSocket(),
      );
      addTearDown(view.close);
      await view.select(context, ConversationSummary.fromJson(nav.direct()));
      var withdrawn = false;
      view.addListener(() {
        if (view.messages.isEmpty) withdrawn = true;
      });
      await view.close();
      expect(withdrawn, isTrue);
    },
  );

  test(
    'terminal sync failure cannot leave an enabled sender after authority stopped',
    () async {
      var malformed = false;
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/tickets'))
          throw const ApiFailure('unavailable');
        if (r.uri.path.endsWith('/sync')) {
          if (malformed) return ApiResponse(200, utf8.encode('[]'), {});
          return oidc.response(page());
        }
        if (r.uri.path.endsWith('/delivered'))
          return oidc.response(disk.state());
        return metadata(r);
      });
      addTearDown(owner.logout);
      final (roles, context) = await sending.admission(owner);
      addTearDown(roles.close);
      final activity = AppActivity()
        ..update(foreground: true, focused: true, activeConversation: nav.dm);
      addTearDown(activity.dispose);
      final view = ConversationView(
        roles,
        activity,
        connector: OfflineSocket(),
      );
      addTearDown(view.close);
      await view.select(context, ConversationSummary.fromJson(nav.direct()));
      await view.refreshMetadata();
      expect(view.canSend, isTrue);
      malformed = true;
      await view.refresh();
      expect(view.canSend, isFalse);
      expect(view.messages.every((m) => m.redacted), isTrue);
      expect(view.unread, isNull);
      expect(view.summaries, isEmpty);
    },
  );

  testWidgets('testOnlyVisibleDeliveredMessagesProduceRead', (tester) async {
    final prepared = (await tester.runAsync(() async {
      final root = await Directory.systemTemp.createTemp('im-visible-test-');
      final store = await EncryptedStore.open(
        'a' * 64,
        List<int>.filled(32, 5),
        directory: root,
        generation: 1,
      );
      return (root, store);
    }))!;
    addTearDown(() async {
      var closed = false;
      final closing = prepared.$2.close().whenComplete(() => closed = true);
      await pumpUntil(tester, () => closed);
      await closing;
      await tester.runAsync(() => prepared.$1.delete(recursive: true));
    });
    final store = prepared.$2;
    var reads = 0;
    final transport = disk.ScriptedTransport((r) {
      if (r.uri.path.endsWith('/read')) {
        reads++;
        expect(disk.requestBody(r)['seqs'], ['1']);
      }
      return disk.response(
        r.method == 'GET'
            ? disk.syncPage()
            : disk.state(read: r.uri.path.endsWith('/read') ? 1 : 0),
      );
    });
    final api = ApiClient(Uri.parse('https://im.example'), transport),
        activity = AppActivity();
    final engine = SyncEngine(api, store),
        queue = ReceiptQueue(api, activity),
        context = disk.context();
    final result = await tester.runAsync(
      () => engine.syncConversation(context),
    );
    queue.acceptDelivered(result!, context);
    activity.update(
      foreground: true,
      focused: true,
      activeConversation: disk.conversation,
    );
    var height = 200.0;
    Widget ui() => MaterialApp(
      home: Scaffold(
        body: Align(
          alignment: Alignment.topCenter,
          child: SizedBox(
            width: 400,
            height: height,
            child: MessageList(
              messages: result.messages,
              userId: disk.user,
              kind: 'direct',
              summaries: const {},
              context: context,
              receipts: queue,
            ),
          ),
        ),
      ),
    );
    await tester.pumpWidget(ui());
    await tester.pump();
    final rowHeight = tester
        .getSize(find.byKey(const ValueKey('message-1')))
        .height;
    activity.update(
      foreground: false,
      focused: true,
      activeConversation: disk.conversation,
    );
    height = rowHeight * .49;
    await tester.pumpWidget(ui());
    await tester.pump();
    activity.update(
      foreground: true,
      focused: true,
      activeConversation: disk.conversation,
    );
    await tester.pump(const Duration(milliseconds: 900));
    expect(reads, 0);
    height = rowHeight * .5;
    await tester.pumpWidget(ui());
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 499));
    expect(reads, 0);
    await tester.pump(const Duration(milliseconds: 301));
    await tester.pump();
    expect(reads, 1);
    await tester.pumpWidget(const MaterialApp(home: SizedBox()));
    queue.dispose();
    engine.stop();
    activity.dispose();
  });
}
