// Installed mobile runtime + production controllers/widgets and native vault.
// API/PG/Redis/workers are real. Only identity issuance and test CA are owned fixtures.
import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:enterprise_im/core/auth/account_reference.dart';
import 'package:enterprise_im/core/auth/client_session.dart';
import 'package:enterprise_im/core/auth/native_auth.dart';
import 'package:enterprise_im/core/auth/session_store.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/client_id.dart';
import 'package:enterprise_im/core/protocol/models.dart';
import 'package:enterprise_im/core/protocol/seq.dart';
import 'package:enterprise_im/core/storage/secure_vault.dart';
import 'package:enterprise_im/core/sync/client_context.dart';
import 'package:enterprise_im/features/memberships/membership_controller.dart';
import 'package:enterprise_im/features/conversations/conversation_list.dart';
import 'package:enterprise_im/features/conversations/conversation_view.dart';
import 'package:enterprise_im/platform/mobile_activity.dart';
import 'package:enterprise_im/ui/message_page.dart';
import 'owned_backend_fixture.dart';
import 'files_and_devices_test.dart' show FixtureTrust, NoInteractiveBrowser;

void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  testWidgets('mobile production backend send delivery read and revoked device', (
    tester,
  ) async {
    expect(Platform.isAndroid || Platform.isIOS, isTrue);
    const bundled = bool.fromEnvironment('IM_NATIVE_BUNDLED_FIXTURE');
    expect(bundled, isTrue, reason: 'requires this run owned fixture');
    final config = (await tester.runAsync(
      () => readOwnedBackendFixture(host: false, path: ''),
    ))!;
    final trust = ownedBackendTrust(config, host: false);
    await HttpOverrides.runWithHttpOverrides(() async {
      final origin = Uri.parse(config['origin'] as String);
      final vault = OsSecureVault();
      final adapter = mobileActivityAdapter();
      final owners = <ClientSession>[],
          roles = <MembershipController>[],
          apis = <ApiClient>[],
          views = <ConversationView>[];
      Directory? root;
      String? bobId;
      Future<void> wait(bool Function() condition, String stage) async {
        final sw = Stopwatch()..start();
        while (!condition()) {
          if (sw.elapsed > const Duration(seconds: 40))
            throw StateError('mobile stage timeout: $stage');
          await tester.runAsync(
            () => Future<void>.delayed(const Duration(milliseconds: 40)),
          );
          await tester.pump(const Duration(milliseconds: 80));
          expect(tester.takeException(), isNull);
        }
      }

      Future<void> control(String phase) async {
        final client = HttpClient();
        try {
          final req = await client.postUrl(
            Uri.parse('${config['control']}/$phase'),
          );
          req.headers.set('X-Owned-Fixture', config['control_key'] as String);
          req.headers.contentType = ContentType.json;
          req.write(jsonEncode({'runtime_pid': pid, 'bob_device_id': bobId}));
          final res = await req.close();
          await res.drain<void>();
          if (res.statusCode != 204)
            throw StateError(
              'independent database checkpoint $phase rejected: ${res.statusCode}',
            );
        } finally {
          client.close(force: true);
        }
      }

      Future<(ClientSession, MembershipController)> admit(String name) async {
        final api = ApiClient(origin, HttpApiTransport());
        apis.add(api);
        final auth = NativeAuth(
          api,
          callbackFactory: (_, __) => throw StateError('owned refresh only'),
          browser: NoInteractiveBrowser(),
        );
        final store = SessionStore(
          vault,
          directory: Directory('${root!.path}/$name'),
        );
        final owner = ClientSession(
          auth,
          store,
          platform: Platform.operatingSystem,
        );
        owners.add(owner);
        final account = AccountReference(
          origin: origin,
          tenantId: config['tenant'] as String,
          userId: config['${name}_user'] as String,
        );
        final ns = await accountNamespace(
          origin,
          account.tenantId,
          account.userId,
        );
        // Opaque recovery handle only; no tokens or identifiers in logs.
        // ignore: avoid_print
        print('MOBILE_BACKEND_VAULT_NAMESPACE $ns');
        final seed = await auth.restore(
          account,
          Platform.operatingSystem,
          config['${name}_refresh'] as String,
        );
        await store.open(seed);
        await store.saveCredentials(seed, refreshToken: auth.refreshCredential);
        await store.close();
        await auth.cancel();
        await owner.restore(account);
        final member = MembershipController(owner);
        roles.add(member);
        await member.load();
        await member.select(config['${name}_membership'] as String);
        return (owner, member);
      }

      Future<(ConversationView, ConversationSummary, ClientContext)> open(
        MembershipController role,
      ) async {
        final list = ConversationList(role);
        final summaries = await list.load('direct');
        list.close();
        final summary = summaries.items.singleWhere(
          (c) => c.id == config['conversation'],
        );
        final ready = role.current!;
        final ctx = ClientContext(
          session: ready.client.session,
          actingMembershipId: ready.membership.id,
          device: ready.device,
          generation: ready.client.store.generation,
          conversationId: summary.id,
        );
        final view = ConversationView(role, adapter.activity);
        views.add(view);
        await view.select(ctx, summary);
        return (view, summary, ctx);
      }

      Future<void> mount(
        ConversationView view,
        ConversationSummary summary,
      ) async {
        await tester.pumpWidget(
          MaterialApp(
            home: Scaffold(
              body: MessagePage(
                view: view,
                conversation: summary,
                onBack: () {},
              ),
            ),
          ),
        );
        await wait(
          () =>
              view.canSend &&
              find
                  .byKey(const ValueKey('message-editor'))
                  .evaluate()
                  .isNotEmpty,
          'send-ready',
        );
      }

      try {
        expect(
          await tester.runAsync(
            () => vault.read(bootstrapNamespace, 'account'),
          ),
          isNull,
          reason: 'fresh owned simulator account only',
        );
        root = await tester.runAsync<Directory?>(
          () => createOwnedNativeStorage(host: false),
        );
        final (alice, aRoles) = (await tester.runAsync(() => admit('alice')))!;
        final (bob, bRoles) = (await tester.runAsync(() => admit('bob')))!;
        bobId = bRoles.current!.device.deviceId;
        expect(aRoles.current!.device.deviceId, isNot(bobId));
        await wait(
          () => adapter.activity.foreground && adapter.activity.focused,
          'actual active mobile scene',
        );
        final (aView, summary, aCtx) = (await tester.runAsync(
          () => open(aRoles),
        ))!;
        await mount(aView, summary);
        for (var index = 1; index <= 3; index++) {
          final body = '真实移动消息$index';
          await tester.enterText(
            find.byKey(const ValueKey('message-editor')),
            body,
          );
          await tester.pump();
          await tester.tap(find.widgetWithText(FilledButton, '发送'));
          await wait(
            () =>
                !aView.sending &&
                aView.sender.lastACK?.seq == Seq.parse('$index') &&
                aView.messages.any((m) => m.text == body),
            'actual send $index',
          );
          expect(aView.sender.lastACK!.duplicate, isFalse);
          expect(aView.pending, isEmpty);
        }
        await tester.runAsync(() => control('sent'));
        await tester.runAsync(aView.close);
        views.remove(aView);
        await tester.pumpWidget(const SizedBox.shrink());
        final (bView, bSummary, bCtx) = (await tester.runAsync(
          () => open(bRoles),
        ))!;
        await wait(
          () => bView.messages.length == 3 && bView.canSend,
          'device delivery',
        );
        expect(
          await tester.runAsync(
            () => bob.current!.store.committedCursor(bCtx.conversationId),
          ),
          Seq.parse('3'),
        );
        expect(bView.receipts.locallyReadSeqs, isEmpty);
        await tester.runAsync(() => control('delivered'));
        // Real production MessagePage visible rows qualify read; delivery alone above did not.
        await mount(bView, bSummary);
        for (var index = 1; index <= 3; index++) {
          expect(find.text('真实移动消息$index'), findsOneWidget);
        }
        await wait(
          () => bView.receipts.locallyReadSeqs.length == 3,
          'visible read ACK',
        );
        Future<Map<String, Object?>> aliceGet(String suffix) =>
            alice.auth.api.json(
              'GET',
              '/api/v1/conversations/${aCtx.conversationId}/$suffix',
              bearer: aCtx.session.accessToken,
              actingMembershipId: aCtx.actingMembershipId,
              device: aCtx.device,
            );
        final receipts = (await tester.runAsync(
          () => aliceGet('receipts?seqs=1,2,3'),
        ))!;
        expect(
          (receipts['receipts'] as List).map((r) => objectMap(r)['status']),
          everyElement('read'),
        );
        await tester.runAsync(() => control('read'));
        final oldPage = (await tester.runAsync(
          () => bob.auth.api.json(
            'GET',
            '/api/v1/conversations/${bCtx.conversationId}/sync?after_seq=0&limit=100',
            bearer: bCtx.session.accessToken,
            actingMembershipId: bCtx.actingMembershipId,
            device: bCtx.device,
          ),
        ))!;
        final proof = SyncPage.fromJson(
          oldPage,
          after: Seq.parse('0'),
        ).syncProof!;
        // Production own-device DELETE acts as another management session; no DB mutation.
        await tester.runAsync(
          () => bob.auth.api.json(
            'DELETE',
            '/api/v1/devices/$bobId',
            bearer: bCtx.session.accessToken,
            expectNoContent: true,
          ),
        );
        await tester.runAsync(() => control('revoked'));
        final rejected = <String>[];
        Future<void> deny(
          String method,
          String action, {
          Map<String, Object?>? body,
        }) async {
          try {
            await bob.auth.api.json(
              method,
              '/api/v1/conversations/${bCtx.conversationId}/$action',
              bearer: bCtx.session.accessToken,
              actingMembershipId: bCtx.actingMembershipId,
              device: bCtx.device,
              body: body,
            );
            throw StateError('revoked device unexpectedly accepted');
          } on ApiFailure catch (error) {
            expect(error.status, 401);
            expect(error.code, 'invalid_device');
            rejected.add(action.split('?').first);
          }
        }

        await tester.runAsync(() => deny('GET', 'sync?after_seq=0&limit=100'));
        await tester.runAsync(
          () => deny('PUT', 'delivered', body: {'sync_proof': proof}),
        );
        await tester.runAsync(
          () => deny(
            'PUT',
            'read',
            body: {
              'seqs': ['1'],
            },
          ),
        );
        await tester.runAsync(
          () => deny(
            'POST',
            'messages',
            body: {
              'client_msg_id': ClientMessageId.create(bRoles.current!.clock),
              'message_type': 'text',
              'text': '撤销后禁止提交',
            },
          ),
        );
        // Actual server401 reaches the production coordinator and clears received state.
        await tester.runAsync(() async {
          try {
            // A concurrent metadata response may already have delivered the
            // real invalid_device401 and stopped this exact coordinator.
            if (bView.failure?.status != 401) await bView.coordinator.resume();
          } on ApiFailure catch (error) {
            if (error.code != 'invalid_context' ||
                bView.failure?.status != 401 ||
                bView.failure?.code != 'invalid_device' ||
                bView.context != null)
              rethrow;
          }
        });
        await wait(
          () =>
              bView.failure?.status == 401 &&
              !bView.canSend &&
              !bView.channel.connected,
          'terminal device denial',
        );
        expect(bView.failure!.code, 'invalid_device');
        expect(bView.messages, isEmpty);
        expect(bView.receipts.locallyReadSeqs, isEmpty);
        expect(
          await tester.runAsync(
            () => bob.current!.store.committedCursor(bCtx.conversationId),
          ),
          isNull,
        );
        expect(
          await tester.runAsync(() => bob.current!.store.pendingReceipts()),
          isEmpty,
        );
        await tester.runAsync(
          () => expectLater(
            bView.sendText('不应生成待发送'),
            throwsA(isA<ApiFailure>()),
          ),
        );
        expect(
          await tester.runAsync(() => bob.current!.store.pendingSends()),
          isEmpty,
        );
        await tester.runAsync(() => control('terminal'));
        // Longer than the ordinary disconnected poll. DB fixture also checks no new device.
        await tester.runAsync(
          () => Future<void>.delayed(const Duration(seconds: 6)),
        );
        await tester.pump();
        expect(bView.canSend, isFalse);
        expect(bView.channel.connected, isFalse);
        expect(
          await tester.runAsync(
            () => bob.current!.store.committedCursor(bCtx.conversationId),
          ),
          isNull,
        );
        expect(rejected, ['sync', 'delivered', 'read', 'messages']);
      } finally {
        Object? failure;
        Future<void> cleanup(Future<void> Function() fn) async {
          try {
            await fn();
          } catch (e) {
            failure ??= e;
          }
        }

        for (final view in views) {
          await cleanup(() => tester.runAsync(view.close));
        }
        await cleanup(() => tester.pumpWidget(const SizedBox.shrink()));
        for (final role in roles) {
          role.close();
        }
        for (final owner in owners) {
          await cleanup(() => tester.runAsync(owner.logout));
        }
        for (final name in ['alice', 'bob']) {
          await cleanup(() async {
            final ns = await accountNamespace(
              origin,
              config['tenant'] as String,
              config['${name}_user'] as String,
            );
            for (final key in ['session', 'key', 'refresh', 'device']) {
              expect(await tester.runAsync(() => vault.read(ns, key)), isNull);
            }
          });
        }
        await cleanup(() async {
          expect(
            await tester.runAsync(
              () => vault.read(bootstrapNamespace, 'account'),
            ),
            isNull,
          );
        });
        for (final api in apis) {
          api.close();
        }
        if (failure == null && root != null) {
          await cleanup(() async {
            await tester.runAsync(() => root!.delete(recursive: true));
            expect(await tester.runAsync(() => root!.exists()), isFalse);
          });
        }
        adapter.dispose();
        if (failure != null)
          throw StateError(
            'owned mobile cleanup failed: ${failure.runtimeType}',
          );
      }
      await tester.runAsync(() => control('done'));
      // ignore: avoid_print
      print(
        'MOBILE_BACKEND_REPORT ${jsonEncode({
          'status': 'passed',
          'platform': Platform.operatingSystem,
          'runtime_pid': pid,
          'messages': 3,
          'delivered': 3,
          'read': 3,
          'revoked_rejections': ['sync', 'delivered', 'read', 'messages'],
          'terminal_stopped': true,
          'received_cleared': true,
          'os_vault_real': true,
          'own_records_absent': true,
          'storage_removed': true,
        })}',
      );
    }, FixtureTrust(trust));
  }, timeout: const Timeout(Duration(minutes: 5)));
}
