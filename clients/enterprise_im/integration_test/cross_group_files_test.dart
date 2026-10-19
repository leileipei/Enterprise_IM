// One real account/device per installed OS process. Runtime credentials are
// injected into this disposable application's private Documents, never assets.
import 'dart:convert';
import 'package:cryptography/cryptography.dart';
import 'package:enterprise_im/core/protocol/client_id.dart';
import 'package:enterprise_im/features/groups/group_controller.dart';
import 'package:enterprise_im/features/devices/device_controller.dart';
import 'package:enterprise_im/features/files/file_download.dart';
import 'package:enterprise_im/platform/file_picker.dart';
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
  testWidgets('independent native group files device actor', (tester) async {
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
      String? group;
      String? fileId;
      String? lastProof;
      FileDownload? downloads;
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
              group ?? config['conversation'] as String,
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
          expect(group, isNotNull);
          final page = await list.load('group');
          list.close();
          summary = page.items.singleWhere((c) => c.id == group);
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
                // Live frames execute outside the surrounding test zone.
                // Keep the actual UI-owned transport bound to this fixture CA.
                createDownloads: () => HttpOverrides.runWithHttpOverrides(
                  () => downloads = FileDownload(
                    membership!,
                    privateRoot: Directory('${storage!.path}/downloads'),
                  ),
                  FixtureTrust(trust),
                ),
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
        if (downloads != null) await tester.runAsync(downloads!.close);
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
            case 'create_group':
              final groups = GroupController(membership!);
              final result = await tester.runAsync(
                () => groups.create('跨组织独立进程群', [
                  cmd['bob_membership'] as String,
                ]),
              );
              groups.close();
              expect(result!.conversationId, isNotNull);
              group = result.conversationId;
              details['group_id'] = group;
            case 'select_group':
              group = cmd['group_id'] as String;
              await openView();
              final groups = GroupController(membership!);
              final access = await tester.runAsync(
                () => groups.membership(group!),
              );
              expect(access!.role, role == 'alice' ? 'owner' : 'member');
              details['group_id'] = group;
              details['group_role'] = access.role;
              if (role == 'alice') {
                final roster = await tester.runAsync(
                  () => groups.roster(group!),
                );
                expect(roster!.members.length, 2);
                final organizations = roster.members
                    .map((m) => m.organization)
                    .toSet();
                expect(organizations.length, 2);
                details['organizations'] = organizations.toList();
              } else {
                final denial = await tester.runAsync(() async {
                  try {
                    await groups.roster(group!);
                    return null;
                  } on ApiFailure catch (e) {
                    return e;
                  }
                });
                expect(denial?.status, 403);
                expect(denial?.code, 'group_permission_denied');
                details['roster_denial'] = {
                  'status': denial!.status,
                  'code': denial.code,
                };
              }
              groups.close();
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
              expect(mounted, isTrue);
              final receipt = view!.summaries[seq]!;
              expect(receipt.summaryAvailable, isTrue);
              expect(receipt.visibleRecipientCount, 1);
              expect(receipt.visibleDeliveredCount, 1);
              expect(receipt.visibleReadCount, 1);
              const label = '可见接收者：已送达 1/1 · 已读 1/1';
              await until(
                () =>
                    find
                        .descendant(
                          of: find.byKey(ValueKey('message-$seq')),
                          matching: find.text(label),
                        )
                        .evaluate()
                        .length ==
                    1,
                'exact group message visible read badge',
              );
              details['visible_read_seq'] = seq.toString();
              details['visible_read_label'] = label;
            case 'file_send':
              await mount();
              final bytes = utf8.encode(cmd['contents'] as String);
              await tester.runAsync(
                () => view!.files.reserveAndUpload(
                  context!,
                  SelectedFile(
                    name: '跨端附件.txt',
                    mediaType: 'text/plain',
                    size: bytes.length,
                    open: () => Stream.value(bytes),
                  ),
                ),
              );
              final scanned = Stopwatch()..start();
              while (view!.files.current?.state != 'ready') {
                if (scanned.elapsed > const Duration(seconds: 90))
                  throw StateError('real scanner deadline');
                await tester.runAsync(view!.files.poll);
                await pause();
              }
              fileId = view!.files.current!.id;
              await tester.runAsync(() => view!.bindFile('跨端附件校验'));
              await until(
                () =>
                    !view!.sending &&
                    view!.sender.lastACK?.seq == Seq.parse('3') &&
                    view!.pending.isEmpty,
                'file server ACK',
              );
              details['file_id'] = fileId;
              details['ack'] = view!.sender.lastACK!.seq.toString();
            case 'download':
              await mount();
              await until(
                () => view!.messages.any(
                  (m) => m.seq == Seq.parse('3') && m.messageType == 'file',
                ),
                'file card',
              );
              final message = view!.messages.singleWhere(
                (m) => m.seq == Seq.parse('3'),
              );
              fileId = message.attachment!['file_id'] as String;
              expect(
                view!.fileAuthorizationRemaining(message),
                greaterThan(const Duration(seconds: 120)),
              );
              final data = await tester.runAsync(() async {
                try {
                  await downloads!.downloadAuthorized(
                    context!,
                    message,
                    authorizationRemaining: () =>
                        view!.fileAuthorizationRemaining(message),
                  );
                  final files = await Directory('${storage!.path}/downloads')
                      .list(recursive: true)
                      .where((p) => p is File && p.path.endsWith('/content'))
                      .cast<File>()
                      .toList();
                  expect(files.length, 1);
                  return await files.single.readAsBytes();
                } catch (error, stack) {
                  details['download_failure_type'] = error.runtimeType
                      .toString();
                  details['download_failure_stack'] = stack.toString();
                  if (error is ApiFailure) {
                    details['download_failure_code'] = error.code;
                    details['download_failure_status'] = error.status;
                  }
                  rethrow;
                }
              });
              expect(data, utf8.encode(cmd['contents'] as String));
              final hash = await tester.runAsync(() => Sha256().hash(data!));
              details['sha256'] = hash!.bytes
                  .map((b) => b.toRadixString(16).padLeft(2, '0'))
                  .join();
              details['size'] = data!.length;
              details['device_id'] = device;
              details['file_id'] = fileId;
              final proofPage = await tester.runAsync(
                () => api!.json(
                  'GET',
                  '/api/v1/conversations/$group/sync?after_seq=0&limit=100',
                  bearer: context!.session.accessToken,
                  actingMembershipId: context!.actingMembershipId,
                  device: context!.device,
                ),
              );
              lastProof = proofPage!['sync_proof'] as String;
              details['cache_lease_remaining_ms'] = view!
                  .fileAuthorizationRemaining(message)
                  .inMilliseconds;
            case 'cache_present':
              final present = await tester.runAsync(
                () => Directory('${storage!.path}/downloads')
                    .list(recursive: true)
                    .any((p) => p is File && p.path.endsWith('/content')),
              );
              expect(present, isTrue);
              details['private_artifact_present'] = true;
            case 'revoke_other':
              final devices = DeviceController(owner!, memberships: membership);
              final result = await tester.runAsync(
                () => devices.revoke(cmd['device_id'] as String),
              );
              devices.close();
              expect(result!.stoppedOwnClient, isFalse);
              expect(view!.canSend, isTrue);
              details['revoked_device_id'] = cmd['device_id'];
            case 'terminal':
              // Let the actual production coordinator observe the remote revoke.
              await until(
                () =>
                    view!.failure?.status == 401 &&
                    view!.failure?.code == 'invalid_device' &&
                    view!.context == null &&
                    !view!.channel.connected,
                'remote invalid_device401',
              );
              expect(view!.canSend, isFalse);
              expect(view!.messages, isEmpty);
              expect(view!.receipts.locallyReadSeqs, isEmpty);
              final rejected = <String>[];
              await tester.runAsync(() async {
                final checks = <String, (String, String, Map<String, Object?>?)>{
                  'sync': (
                    'GET',
                    '/api/v1/conversations/$group/sync?after_seq=0&limit=100',
                    null,
                  ),
                  'delivered': (
                    'PUT',
                    '/api/v1/conversations/$group/delivered',
                    {'sync_proof': lastProof!},
                  ),
                  'read': (
                    'PUT',
                    '/api/v1/conversations/$group/read',
                    {
                      'seqs': ['1'],
                    },
                  ),
                  'messages': (
                    'POST',
                    '/api/v1/groups/$group/messages',
                    {
                      'client_msg_id': ClientMessageId.create(
                        membership!.current!.clock,
                      ),
                      'message_type': 'text',
                      'text': '撤销后禁止提交',
                    },
                  ),
                  'download': ('GET', '/api/v1/files/$fileId/content', null),
                  'renew': ('POST', '/api/v1/devices/$device/renew', null),
                };
                for (final entry in checks.entries) {
                  ApiFailure? denied;
                  try {
                    await api!.json(
                      entry.value.$1,
                      entry.value.$2,
                      body: entry.value.$3,
                      bearer: context!.session.accessToken,
                      actingMembershipId: context!.actingMembershipId,
                      device: context!.device,
                    );
                  } on ApiFailure catch (e) {
                    denied = e;
                  }
                  expect(denied?.status, 401);
                  expect(denied?.code, 'invalid_device');
                  rejected.add(entry.key);
                }
                expect(
                  await owner!.current!.store.committedCursor(group!),
                  isNull,
                );
                expect(await owner!.current!.store.pendingReceipts(), isEmpty);
                expect(await owner!.current!.store.pendingSends(), isEmpty);
              });
              await until(
                () => find.textContaining('独立群消息一').evaluate().isEmpty,
                'UI body removed',
              );
              final erased = await tester.runAsync(() async {
                final dir = Directory('${storage!.path}/downloads');
                if (!await dir.exists()) return true;
                return !(await dir.list(recursive: true).any((p) => p is File));
              });
              expect(erased, isTrue);
              details.addAll({
                'rejected': rejected,
                'terminal': true,
                'private_artifacts_absent': erased,
                'received_cleared': true,
                'can_send': view!.canSend,
              });
            case 'terminal_stable':
              expect(view!.context, isNull);
              expect(view!.messages, isEmpty);
              expect(view!.canSend, isFalse);
              expect(
                await tester.runAsync(
                  () => owner!.current!.store.committedCursor(group!),
                ),
                isNull,
              );
              details['terminal_still_stopped'] = true;
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
      } catch (error, stack) {
        details['failure_stack'] = stack.toString();
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
