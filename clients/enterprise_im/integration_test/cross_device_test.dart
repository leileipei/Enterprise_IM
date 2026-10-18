// One real account/device per installed OS process. Runtime credentials are
// injected into this disposable application's private Documents, never assets.
import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:path_provider/path_provider.dart';
import 'package:enterprise_im/core/auth/account_reference.dart';
import 'package:enterprise_im/core/auth/client_session.dart';
import 'package:enterprise_im/core/auth/native_auth.dart';
import 'package:enterprise_im/core/auth/session_store.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/seq.dart';
import 'package:enterprise_im/core/protocol/models.dart';
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
  testWidgets('independent native cross-platform messaging actor', (
    tester,
  ) async {
    expect(Platform.isAndroid || Platform.isIOS, isTrue);
    final documents = (await tester.runAsync(
      getApplicationDocumentsDirectory,
    ))!;
    final input = File('${documents.path}/cross-input.json');
    final command = File('${documents.path}/cross-command.json');
    final state = File('${documents.path}/cross-state.json');
    Future<void> pause() async {
      await tester.runAsync(
        () => Future<void>.delayed(const Duration(milliseconds: 60)),
      );
      await tester.pump(const Duration(milliseconds: 80));
      expect(tester.takeException(), isNull);
    }

    final configDeadline = Stopwatch()..start();
    while (!(await tester.runAsync(input.exists))!) {
      if (configDeadline.elapsed > const Duration(seconds: 120))
        throw StateError('owned bootstrap missing');
      await pause();
    }
    final config =
        jsonDecode((await tester.runAsync(input.readAsString))!)
            as Map<String, dynamic>;
    expect(config['platform'], Platform.operatingSystem);
    final trust = ownedBackendTrust(config, host: false);
    await HttpOverrides.runWithHttpOverrides(() async {
      final origin = Uri.parse(config['origin'] as String);
      final role = config['role'] as String, nonce = config['nonce'] as String;
      final vault = OsSecureVault(), adapter = mobileActivityAdapter();
      ClientSession? owner;
      MembershipController? membership;
      ApiClient? api;
      ConversationView? view;
      ConversationSummary? summary;
      ClientContext? context;
      Directory? storage;
      String? device;
      var last = 0;
      var mounted = false;
      var cleaned = false;
      final details = <String, Object?>{};
      Future<void> until(bool Function() fn, String name) async {
        final watch = Stopwatch()..start();
        while (!fn()) {
          if (watch.elapsed > const Duration(seconds: 80))
            throw StateError('actor condition: $name');
          await pause();
        }
      }

      Future<void> snapshot(String status) async {
        if (owner?.current != null)
          details['cursor'] = (await tester.runAsync(
            () => owner!.current!.store.committedCursor(
              config['conversation'] as String,
            ),
          ))?.toString();
        if (view != null) {
          details['seqs'] = view!.messages
              .map((m) => m.seq.toString())
              .toList();
          details['read_seqs'] = view!.receipts.locallyReadSeqs
              .map((s) => s.toString())
              .toList();
          details['summaries'] = {
            for (final entry in view!.summaries.entries)
              entry.key.toString(): entry.value.status,
          };
        }
        final output = {
          'role': role,
          'platform': Platform.operatingSystem,
          'nonce': nonce,
          'pid': pid,
          'user': config['user'],
          'device_id': device,
          'id': last,
          'status': status,
          'details': details,
          'cleanup': cleaned,
        };
        await tester.runAsync(() async {
          final temp = File('${state.path}.new');
          await temp.writeAsString(jsonEncode(output), flush: true);
          await temp.rename(state.path);
        });
      }

      Future<void> openView() async {
        if (view != null) return;
        await tester.runAsync(() async {
          final list = ConversationList(membership!);
          final page = await list.load('direct');
          list.close();
          summary = page.items.singleWhere(
            (c) => c.id == config['conversation'],
          );
          final ready = membership!.current!;
          context = ClientContext(
            session: ready.client.session,
            actingMembershipId: ready.membership.id,
            device: ready.device,
            generation: ready.client.store.generation,
            conversationId: summary!.id,
          );
          view = ConversationView(membership!, adapter.activity);
          await view!.select(context!, summary!);
        });
        await until(() => view!.canSend, 'conversation admitted');
      }

      Future<void> mount() async {
        await openView();
        if (mounted) return;
        await tester.pumpWidget(
          MaterialApp(
            home: Scaffold(
              body: MessagePage(
                view: view!,
                conversation: summary!,
                onBack: () {},
              ),
            ),
          ),
        );
        mounted = true;
        await until(
          () => find
              .byKey(const ValueKey('message-editor'))
              .evaluate()
              .isNotEmpty,
          'editor visible',
        );
      }

      Future<void> cleanup() async {
        if (view != null) await tester.runAsync(view!.close);
        await tester.pumpWidget(const SizedBox.shrink());
        membership?.close();
        if (owner != null) await tester.runAsync(owner!.logout);
        final ns = await accountNamespace(
          origin,
          config['tenant'] as String,
          config['user'] as String,
        );
        for (final key in ['session', 'key', 'refresh', 'device']) {
          expect(await tester.runAsync(() => vault.read(ns, key)), isNull);
        }
        expect(
          await tester.runAsync(
            () => vault.read(bootstrapNamespace, 'account'),
          ),
          isNull,
        );
        api?.close();
        adapter.dispose();
        if (storage != null) {
          await tester.runAsync(() => storage!.delete(recursive: true));
          expect(await tester.runAsync(storage!.exists), isFalse);
        }
        await tester.runAsync(() async {
          if (await input.exists()) await input.delete();
          if (await command.exists()) await command.delete();
        });
        cleaned = true;
      }

      try {
        expect(
          await tester.runAsync(
            () => vault.read(bootstrapNamespace, 'account'),
          ),
          isNull,
        );
        await tester.pumpWidget(
          MaterialApp(home: Scaffold(body: Text('独立进程 $role'))),
        );
        await until(
          () => adapter.activity.foreground && adapter.activity.focused,
          'actual native active scene',
        );
        await tester.runAsync(() async {
          storage = await createOwnedNativeStorage(host: false);
          api = ApiClient(origin, HttpApiTransport());
          final auth = NativeAuth(
            api!,
            callbackFactory: (_, __) => throw StateError('owned refresh only'),
            browser: NoInteractiveBrowser(),
          );
          final store = SessionStore(vault, directory: storage!);
          owner = ClientSession(
            auth,
            store,
            platform: Platform.operatingSystem,
          );
          final account = AccountReference(
            origin: origin,
            tenantId: config['tenant'] as String,
            userId: config['user'] as String,
          );
          final seed = await auth.restore(
            account,
            Platform.operatingSystem,
            config['refresh'] as String,
          );
          await store.open(seed);
          await store.saveCredentials(
            seed,
            refreshToken: auth.refreshCredential,
          );
          await store.close();
          await auth.cancel();
          await owner!.restore(account);
          membership = MembershipController(owner!);
          await membership!.load();
          await membership!.select(config['membership'] as String);
          device = membership!.current!.device.deviceId;
        });
        await snapshot('ok');
        final lifetime = Stopwatch()..start();
        while (lifetime.elapsed < const Duration(minutes: 15)) {
          if (!(await tester.runAsync(command.exists))!) {
            await pause();
            continue;
          }
          final cmd =
              jsonDecode((await tester.runAsync(command.readAsString))!)
                  as Map<String, dynamic>;
          if (cmd['id'] == last) {
            await pause();
            continue;
          }
          expect(cmd['id'], last + 1);
          last = cmd['id'] as int;
          details.clear();
          switch (cmd['action']) {
            case 'open':
              await openView();
              expect(view!.receipts.locallyReadSeqs, isEmpty);
            case 'send':
              await mount();
              await until(() => view!.canSend, 'send admitted');
              await tester.enterText(
                find.byKey(const ValueKey('message-editor')),
                cmd['text'] as String,
              );
              await tester.pump();
              await tester.tap(find.widgetWithText(FilledButton, '发送'));
              await until(
                () =>
                    !view!.sending &&
                    view!.sender.lastACK?.seq ==
                        Seq.parse(cmd['seq'] as String) &&
                    view!.pending.isEmpty,
                'server ACK',
              );
              expect(view!.sender.lastACK!.duplicate, isFalse);
              details['ack'] = view!.sender.lastACK!.seq.toString();
            case 'read':
              await mount();
              final wanted = (cmd['seqs'] as List).cast<String>();
              await until(
                () => wanted.every(
                  (s) => view!.receipts.locallyReadSeqs.contains(Seq.parse(s)),
                ),
                'actual visible read ACK',
              );
            case 'summary':
              final seq = Seq.parse(cmd['seq'] as String);
              await until(
                () => view!.summaries[seq]?.status == cmd['status'],
                'sender receipt converged',
              );
              if (mounted)
                expect(
                  find.text(cmd['status'] == 'read' ? '已读' : '已送达'),
                  findsWidgets,
                );
            case 'probe_offline':
              // runAsync reports uncaught exceptions to Flutter and returns null.
              // Capture this expected network failure within its real async zone.
              final failure = await tester.runAsync(() async {
                try {
                  await api!.json(
                    'GET',
                    '/api/v1/conversations/${context!.conversationId}/sync?after_seq=1&limit=100',
                    timeout: const Duration(seconds: 5),
                    bearer: context!.session.accessToken,
                    actingMembershipId: context!.actingMembershipId,
                    device: context!.device,
                  );
                  return null;
                } on ApiFailure catch (error) {
                  return error;
                }
              });
              details['network_error'] = failure?.code;
              details['network_status'] = failure?.status;
              expect(failure, isNotNull);
              expect(['unavailable', 'timeout'], contains(failure!.code));
              expect(failure.status, isNull);
              expect(
                view!.messages.any((m) => m.seq == Seq.parse('2')),
                isFalse,
              );
              expect(view!.receipts.locallyReadSeqs, isEmpty);
            case 'offline_stable':
              expect(
                view!.messages.any((m) => m.seq == Seq.parse('2')),
                isFalse,
              );
              expect(view!.receipts.locallyReadSeqs, isEmpty);
            case 'auto_catchup':
              // No manual refresh/re-admission: production retry/socket/polling must recover.
              await until(
                () => view!.messages.any((m) => m.seq == Seq.parse('2')),
                'automatic catchup',
              );
              expect(
                view!.messages.map((m) => m.seq).toSet().length,
                view!.messages.length,
              );
              expect(view!.receipts.locallyReadSeqs, isEmpty);
              details['automatic_catchup'] = true;
            case 'received_read':
              await until(
                () =>
                    view!.messages.any(
                      (m) => m.seq == Seq.parse(cmd['seq'] as String),
                    ) &&
                    view!.receipts.locallyReadSeqs.contains(
                      Seq.parse(cmd['seq'] as String),
                    ),
                'other native process reply read',
              );
            case 'done':
              await cleanup();
              await snapshot('done');
              return;
            default:
              throw StateError('unknown owned command');
          }
          await snapshot('ok');
        }
        throw StateError('cross actor lifetime exceeded');
      } catch (error) {
        details['failure_type'] = error.runtimeType.toString();
        try {
          await snapshot('failed');
        } catch (_) {}
        rethrow;
      } finally {
        if (!cleaned) await cleanup();
      }
    }, FixtureTrust(trust));
  }, timeout: const Timeout(Duration(minutes: 18)));
}
