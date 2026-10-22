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
import 'package:enterprise_im/core/protocol/client_id.dart';
import 'package:enterprise_im/core/protocol/seq.dart';
import 'package:enterprise_im/core/protocol/models.dart';
import 'package:enterprise_im/core/storage/secure_vault.dart';
import 'package:enterprise_im/core/sync/client_context.dart';
import 'package:enterprise_im/features/memberships/membership_controller.dart';
import 'package:enterprise_im/features/devices/device_controller.dart';
import 'package:enterprise_im/features/conversations/conversation_list.dart';
import 'package:enterprise_im/features/conversations/conversation_view.dart';
import 'package:enterprise_im/platform/mobile_activity.dart';
import 'package:enterprise_im/platform/native_storage.dart';
import 'package:enterprise_im/ui/message_page.dart';
import 'package:enterprise_im/ui/login_page.dart';
import 'package:enterprise_im/ui/client_shell.dart';
import 'owned_backend_fixture.dart';
import 'owned_login_provider.dart';
import 'package:enterprise_im/platform/login_callback.dart';
import 'files_and_devices_test.dart' show FixtureTrust, NoInteractiveBrowser;

// Passive test observer: delegates every request/cancellation to the original
// production HTTP transport and returns its exact response. No retry or ACK.
final class ObservedReceiptTransport implements ApiTransport {
  final ApiTransport inner;
  final void Function(ApiRequest, ApiResponse) observed;
  ObservedReceiptTransport(this.inner, this.observed);
  @override
  Future<ApiResponse> send(ApiRequest request, Cancellation cancel) async {
    final response = await inner.send(request, cancel);
    observed(request, response);
    return response;
  }

  @override
  void close() => inner.close();
}

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
    final previousProcessHttp = HttpOverrides.current;
    var navigationProcessHttpInstalled = false;
    try {
      await HttpOverrides.runWithHttpOverrides(() async {
        final origin = Uri.parse(config['origin'] as String);
        final role = config['role'] as String,
            nonce = config['nonce'] as String;
        final vault = OsSecureVault(), adapter = mobileActivityAdapter();
        final recovery = File('${documents.path}/cross-recovery.json');
        final restarting = config['restart'] == true;
        final loggedOutRestart = config['logged_out_restart'] == true;
        final freshLogin = config['fresh_login'] == true;
        AppLinksSource? loginLinks;
        OwnedLoginProvider? loginProvider;
        var loginAttemptCallbackBaseline = 0;
        var loginCallbackCompletions = 0,
            loginTokenRequests = 0,
            loginRefreshRequests = 0;
        ClientSession? owner;
        MembershipController? membership;
        ApiClient? api;
        ConversationView? view;
        ConversationSummary? summary;
        ClientContext? context;
        Directory? storage;
        String storageName() =>
            storage!.uri.pathSegments.where((s) => s.isNotEmpty).last;
        String? device, permissionProof;
        var last = 0;
        final initialPid = pid;
        SessionStore? navigationStore;
        var loginApiCreations = 0;
        var loggedOut = false;
        var mounted = false;
        var cleaned = false;
        var seenReceipt503 = false;
        final observedFailures = <String>[];
        final observedReceiptHTTP = <Map<String, Object?>>[];
        final details = <String, Object?>{};
        Future<void> until(bool Function() fn, String name) async {
          final watch = Stopwatch()..start();
          while (!fn()) {
            if (watch.elapsed > const Duration(seconds: 80)) {
              details['failed_condition'] = name;
              details['login_api_creations'] = loginApiCreations;
              details['navigation_store_created'] = navigationStore != null;
              details['navigation_visible_labels'] = [
                for (final label in [
                  '登录集团工作空间',
                  '登录成功',
                  '恢复上次登录…',
                  '使用企业账号登录',
                  '恢复登录',
                  '系统安全存储不可用，登录未完成。请检查系统配置后重试。',
                  '无法读取上次账号，请检查系统安全存储后重试。',
                  '登录暂时不可用，请稍后重试。',
                  '当前账号没有可用任职，请联系管理员。',
                ])
                  if (find.text(label).evaluate().isNotEmpty) label,
              ];
              throw StateError('actor condition: $name');
            }
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
            details['observed_failures'] = List<String>.of(observedFailures);
            details['observed_receipt_http'] = List<Map<String, Object?>>.of(
              observedReceiptHTTP,
            );
            details['latest_network_code'] = view!.failure?.code;
            details['latest_network_status'] = view!.failure?.status;
            details['loading'] = view!.loading;
            details['observed_503'] = seenReceipt503;
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

        Future<void> openView({bool waitForAdmission = true}) async {
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
            view!.addListener(() {
              final error = view!.failure;
              if (error == null) return;
              if (error.status == 503) seenReceipt503 = true;
              final label = '${error.code}:${error.status}';
              if ((observedFailures.isEmpty ||
                      observedFailures.last != label) &&
                  observedFailures.length < 32)
                observedFailures.add(label);
            });
            await view!.select(context!, summary!);
          });
          if (waitForAdmission)
            await until(
              () =>
                  view!.context != null &&
                  !view!.loading &&
                  (view!.canSend || view!.pending.isNotEmpty),
              'conversation admitted',
            );
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

        Future<void> captureLogout() async {
          expect(loggedOut, isTrue);
          expect(owner!.current, isNull);
          expect(view!.context, isNull);
          expect(view!.canSend, isFalse);
          expect(view!.messages, isEmpty);
          expect(view!.receipts.locallyReadSeqs, isEmpty);
          final checks = await tester.runAsync(() async {
            final ns = await accountNamespace(
              origin,
              config['tenant'] as String,
              config['user'] as String,
            );
            for (final key in ['session', 'key', 'refresh', 'device']) {
              expect(await vault.read(ns, key), isNull);
            }
            expect(await vault.read(bootstrapNamespace, 'account'), isNull);
            return !await storage!.list(recursive: true).any((f) => f is File);
          });
          expect(checks, isTrue);
          final bodies = [
            '退出竞态消息一',
            '退出前在途消息二',
            '存活设备回复三',
            'Android 退出前在途消息四',
            '双方退出后消息五',
          ];
          await until(
            () => bodies.every(
              (body) => find.textContaining(body).evaluate().isEmpty,
            ),
            'logged out UI has no old body',
          );
          details.addAll({
            'account_absent': true,
            'vault_absent': true,
            'database_files_absent': checks,
            'context_cleared': true,
            'can_send': false,
            'seqs': <String>[],
            'read_seqs': <String>[],
            'ui_body_absent': true,
            'cursor': null,
            'same_process': pid == initialPid,
          });
        }

        Future<void> mountNavigationLogin({
          required bool restored,
          bool interactive = false,
        }) async {
          // Live widget frames run outside the test's zone. This disposable app
          // needs its fixture CA for UI-owned HTTP and WebSocket clients too.
          // TLS verification stays enabled; no OS trust store is modified.
          if (!navigationProcessHttpInstalled) {
            expect(previousProcessHttp, isNull);
            HttpOverrides.global = FixtureTrust(trust);
            navigationProcessHttpInstalled = true;
          }

          if (view != null) await tester.runAsync(view!.close);
          membership?.close();
          if (owner != null) await tester.runAsync(owner!.close);
          api?.close();
          details['navigation_saved_account_after_close'] = await tester
              .runAsync(
                () => vault
                    .read(bootstrapNamespace, 'account')
                    .then((value) => value != null),
              );
          view = null;
          membership = null;
          mounted = false;
          await tester.pumpWidget(
            MaterialApp(
              key: UniqueKey(),
              home: LoginPage(
                platform: Platform.operatingSystem,
                activity: adapter.activity,
                callbackFactory: (_, state) {
                  if (!interactive)
                    throw StateError('saved refresh only; no callback');
                  return ObservedMobileCallback(
                    MobileLoginCallback(state, loginLinks!),
                    () {
                      loginCallbackCompletions++;
                    },
                  );
                },
                browser: interactive ? loginProvider! : NoInteractiveBrowser(),
                storeFactory: () async =>
                    navigationStore = SessionStore(vault, directory: storage!),
                apiFactory: (address) {
                  loginApiCreations++;
                  details['navigation_transport_trust'] =
                      HttpOverrides.current is FixtureTrust;
                  expect(details['navigation_transport_trust'], isTrue);
                  return api = ApiClient(
                    address,
                    ObservedReceiptTransport(HttpApiTransport(), (
                      request,
                      response,
                    ) {
                      if (request.method == 'POST' &&
                          request.uri.path == '/api/v1/native/oauth/token')
                        loginTokenRequests++;
                      if (request.method == 'POST' &&
                          request.uri.path == '/api/v1/native/oauth/refresh')
                        loginRefreshRequests++;
                      if (response.status < 200 || response.status >= 300)
                        details['navigation_http_failure'] = {
                          'method': request.method,
                          'path': request.uri.path,
                          'status': response.status,
                        };
                    }),
                  );
                },
              ),
            ),
          );
          if (restored) {
            await until(
              () => find.text('登录成功').evaluate().isNotEmpty,
              'real LoginPage restored',
            );
          } else {
            await until(
              () =>
                  find.text('登录集团工作空间').evaluate().isNotEmpty &&
                  find
                      .widgetWithText(FilledButton, '使用企业账号登录')
                      .evaluate()
                      .isNotEmpty &&
                  tester
                          .widget<FilledButton>(
                            find.widgetWithText(FilledButton, '使用企业账号登录'),
                          )
                          .onPressed !=
                      null,
              'real LoginPage logged out ready',
            );
          }
        }

        Future<void> enterNavigationWorkspace({bool fresh = false}) async {
          await tester.tap(find.text('进入工作空间'));
          await until(
            () => find
                .byKey(ValueKey('membership-${config['membership']}'))
                .evaluate()
                .isNotEmpty,
            'actual workspace membership choice',
          );
          await tester.tap(
            find.byKey(ValueKey('membership-${config['membership']}')),
          );
          final tile = find.widgetWithText(
            ListTile,
            role == 'alice' ? '用户 A' : '管理员',
          );
          await until(
            () => tile.evaluate().isNotEmpty,
            'actual conversation tile',
          );
          await tester.tap(tile);
          await until(
            () => find.byType(MessagePage).evaluate().isNotEmpty,
            'actual MessagePage route',
          );
          final page = tester.widget<MessagePage>(find.byType(MessagePage));
          view = page.view;
          summary = page.conversation;
          membership = view!.memberships;
          owner = membership!.owner;
          api = owner!.auth.api;
          await until(
            () => view!.context != null && view!.canSend,
            'actual UI view admitted',
          );
          context = view!.context;
          if (!fresh) expect(context!.device.deviceId, device);
          device = context!.device.deviceId;
          mounted = true;
        }

        Future<void> captureNavigationLogout() async {
          expect(owner?.current, isNull);
          expect(find.text('登录集团工作空间'), findsOneWidget);
          expect(find.byType(ClientShell), findsNothing);
          expect(find.byType(MessagePage), findsNothing);
          expect(find.text('登录成功'), findsNothing);
          expect(find.text('进入工作空间'), findsNothing);
          expect(find.text('恢复登录'), findsNothing);
          final button = find.widgetWithText(FilledButton, '使用企业账号登录');
          expect(button, findsOneWidget);
          expect(tester.widget<FilledButton>(button).onPressed, isNotNull);
          final empty = await tester.runAsync(() async {
            final ns = await accountNamespace(
              origin,
              config['tenant'] as String,
              config['user'] as String,
            );
            for (final key in ['session', 'key', 'refresh', 'device']) {
              expect(await vault.read(ns, key), isNull);
            }
            expect(await vault.read(bootstrapNamespace, 'account'), isNull);
            return !await storage!.list(recursive: true).any((f) => f is File);
          });
          expect(empty, isTrue);
          details.addAll({
            'login_heading': true,
            'client_shell_count': 0,
            'message_page_count': 0,
            'workspace_entry': false,
            'restore_button': false,
            'account_absent': true,
            'vault_absent': true,
            'database_files_absent': empty,
            'login_button_enabled': true,
            'storage': storageName(),
            'login_api_creations': loginApiCreations,
            'no_seed_refresh': !config.containsKey('refresh'),
          });
        }

        Future<void> cleanup() async {
          if (view != null) await tester.runAsync(view!.close);
          await tester.pumpWidget(const SizedBox.shrink());
          if (navigationStore != null)
            await tester.runAsync(navigationStore!.close);
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
          if (loginLinks != null) await tester.runAsync(loginLinks.close);
          adapter.dispose();
          if (storage != null) {
            await tester.runAsync(() => storage!.delete(recursive: true));
            expect(await tester.runAsync(storage!.exists), isFalse);
          }
          await tester.runAsync(() async {
            if (await input.exists()) await input.delete();
            if (await command.exists()) await command.delete();
            if (await recovery.exists()) await recovery.delete();
          });
          cleaned = true;
        }

        try {
          expect(
            await tester.runAsync(
              () => vault.read(bootstrapNamespace, 'account'),
            ),
            restarting && !loggedOutRestart ? isNotNull : isNull,
          );
          await tester.pumpWidget(
            MaterialApp(home: Scaffold(body: Text('独立进程 $role'))),
          );
          await until(
            () => adapter.activity.foreground && adapter.activity.focused,
            'actual native active scene',
          );
          await tester.runAsync(() async {
            if (restarting) {
              expect(config.containsKey('refresh'), isFalse);
              final saved =
                  jsonDecode(await recovery.readAsString())
                      as Map<String, dynamic>;
              expect(saved['nonce'], nonce);
              final name = saved['storage'] as String;
              expect(
                RegExp(r'^im-native-backend-[a-zA-Z0-9]+$').hasMatch(name),
                isTrue,
              );
              storage = Directory('${(await nativeStorageRoot()).path}/$name');
              expect(
                await FileSystemEntity.type(storage!.path, followLinks: false),
                FileSystemEntityType.directory,
              );
              device = saved['device'] as String;
            } else {
              storage = await createOwnedNativeStorage(host: false);
            }
            api = ApiClient(
              origin,
              ObservedReceiptTransport(HttpApiTransport(), (request, response) {
                if (request.method == 'PUT' &&
                    (request.uri.path.endsWith('/delivered') ||
                        request.uri.path.endsWith('/read'))) {
                  if (response.status == 503) seenReceipt503 = true;
                  if (observedReceiptHTTP.length < 32)
                    observedReceiptHTTP.add({
                      'action': request.uri.pathSegments.last,
                      'status': response.status,
                      'device_id': request.headers['X-IM-Device-ID'],
                    });
                }
              }),
            );
            final auth = NativeAuth(
              api!,
              callbackFactory: (_, __) =>
                  throw StateError('owned refresh only'),
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
            if (!restarting && !freshLogin) {
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
              await recovery.writeAsString(
                jsonEncode({
                  'nonce': nonce,
                  'storage': storageName(),
                  'device': device,
                }),
                flush: true,
              );
            } else if (freshLogin) {
              expect(config.containsKey('refresh'), isFalse);
              expect(await owner!.accounts.load(), isNull);
            } else if (loggedOutRestart) {
              expect(config.containsKey('refresh'), isFalse);
              expect(await owner!.accounts.load(), isNull);
              final ns = await accountNamespace(
                origin,
                config['tenant'] as String,
                config['user'] as String,
              );
              expect(await vault.read(ns, 'session'), isNull);
              expect(
                await storage!.list(recursive: true).any((f) => f is File),
                isFalse,
              );
              details.addAll({
                'logged_out_restart': true,
                'no_seed_refresh': true,
                'storage': storageName(),
              });
            } else {
              expect(await owner!.accounts.load(), isNotNull);
              expect(owner!.current, isNull);
              final ns = await accountNamespace(
                origin,
                config['tenant'] as String,
                config['user'] as String,
              );
              expect(await vault.read(ns, 'session'), isNotNull);
              details.addAll({
                'preauth_hidden': true,
                'vault_reused': true,
                'storage': storageName(),
              });
            }
          });
          if (freshLogin) {
            expect(restarting, isFalse);
            loginLinks = AppLinksSource();
            await tester.runAsync(loginLinks.initialize);
            loginProvider = OwnedLoginProvider(
              origin,
              role,
              loginLinks,
              () => loginCallbackCompletions != loginAttemptCallbackBaseline,
            );
            await mountNavigationLogin(restored: false, interactive: true);
            expect(loginApiCreations, 0);
            await tester.enterText(find.byType(TextField).first, origin.origin);
            await tester.tap(find.widgetWithText(FilledButton, '使用企业账号登录'));
            await until(
              () => find.text('登录成功').evaluate().isNotEmpty,
              'fresh LoginPage sign-in',
            );
            expect(loginCallbackCompletions, 1);
            expect(loginProvider.wrongStateIgnored, isTrue);
            expect(loginProvider.callbackViaOS, isTrue);
            expect(loginTokenRequests, 1);
            expect(loginRefreshRequests, 0);
            await enterNavigationWorkspace(fresh: true);
            final saved = await tester.runAsync(() async {
              final ns = await accountNamespace(
                origin,
                config['tenant'] as String,
                config['user'] as String,
              );
              final account = AccountReference(
                origin: origin,
                tenantId: config['tenant'] as String,
                userId: config['user'] as String,
              );
              final complete =
                  await vault.read(ns, 'session') != null &&
                  await owner!.sessions.loadRefresh(account) != null &&
                  await vault.read(bootstrapNamespace, 'account') != null;
              await recovery.writeAsString(
                jsonEncode({
                  'nonce': nonce,
                  'storage': storageName(),
                  'device': device,
                }),
                flush: true,
              );
              return complete;
            });
            expect(saved, isTrue);
            details.addAll({
              'fresh_login_button': true,
              'no_seed_refresh': !config.containsKey('refresh'),
              'real_mobile_callback': true,
              'wrong_state_ignored': loginProvider.wrongStateIgnored,
              'callback_via_os': loginProvider.callbackViaOS,
              'callback_completions': loginCallbackCompletions,
              'authorize_requests': loginProvider.authorizeRequests,
              'token_requests': loginTokenRequests,
              'refresh_requests': loginRefreshRequests,
              'workspace_entered': true,
              'vault_saved': saved,
              'same_process': pid == initialPid,
              'storage': storageName(),
              'membership_selected': context!.actingMembershipId,
            });
          }
          expect(owner, isNotNull);
          expect(
            restarting ? owner!.current == null : membership?.current != null,
            isTrue,
          );
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
              case 'restore_pending':
                expect(restarting, isTrue);
                expect(owner!.current, isNull);
                final savedDevice = device;
                await tester.runAsync(() async {
                  final account = await owner!.accounts.load();
                  expect(account, isNotNull);
                  await owner!.restore(account!);
                  final pending = await owner!.current!.store.pendingSends();
                  expect(pending, hasLength(1));
                  details['pending_id'] = pending.single.clientMsgId;
                  membership = MembershipController(owner!);
                  await membership!.load();
                  await membership!.select(config['membership'] as String);
                  device = membership!.current!.device.deviceId;
                  expect(device, savedDevice);
                  details.addAll({
                    'vault_reused': true,
                    'storage': storageName(),
                  });
                });
                expect(membership!.current, isNotNull);
                expect(details['pending_id'], isNotNull);
              case 'pending_restored':
                await mount();
                expect(view!.pending, hasLength(1));
                expect(view!.sender.lastACK, isNull);
                expect(find.text('重试原消息'), findsOneWidget);
                details.addAll({
                  'pending_id': view!.pending.single.clientMsgId,
                  'storage': storageName(),
                  'pending_count': view!.pending.length,
                });
              case 'retry_pending':
                expect(view!.pending, hasLength(1));
                final pendingId = view!.pending.single.clientMsgId;
                await tester.tap(find.text('重试原消息'));
                await until(
                  () =>
                      !view!.sending &&
                      view!.pending.isEmpty &&
                      view!.sender.lastACK?.seq ==
                          Seq.parse(cmd['seq'] as String),
                  'duplicate retry ACK',
                );
                final ack = view!.sender.lastACK!;
                expect(ack.duplicate, isTrue);
                final confirmed = await tester.runAsync(
                  () => owner!.current!.store.sendConfirmation(pendingId),
                );
                expect(confirmed, isNotNull);
                expect(confirmed!.ack.messageId, ack.messageId);
                expect(confirmed.ack.seq, ack.seq);
                expect(confirmed.ack.duplicate, isTrue);
                expect(find.text('重试原消息'), findsNothing);
                details.addAll({
                  'pending_id': pendingId,
                  'message_id': ack.messageId,
                  'seq': ack.seq.toString(),
                  'duplicate': ack.duplicate,
                  'pending_after': view!.pending.length,
                  'storage': storageName(),
                  'confirmation_saved': true,
                });
              case 'send_lost_ack':
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
                      view!.pending.length == 1 &&
                      view!.failure != null,
                  'lost ACK pending',
                );
                expect(view!.sender.lastACK, isNull);
                expect(view!.failure!.code, 'unavailable');
                expect(view!.failure!.status, isNull);
                expect(find.text('重试原消息'), findsOneWidget);
                final saved = await tester.runAsync(
                  () => owner!.current!.store.pendingSends(),
                );
                expect(saved, hasLength(1));
                expect(
                  saved!.single.clientMsgId,
                  view!.pending.single.clientMsgId,
                );
                details.addAll({
                  'pending_id': saved.single.clientMsgId,
                  'pending_count': saved.length,
                  'storage': storageName(),
                  'native_ack': null,
                  'network_error': view!.failure!.code,
                  'network_status': view!.failure!.status,
                });
              case 'open_unconfirmed':
                await openView(waitForAdmission: false);
                await until(
                  () => seenReceipt503 && !view!.loading,
                  'delivery failure observed',
                );
                expect(view!.canSend, isFalse);
                expect(view!.messages, isEmpty);
                expect(view!.receipts.locallyReadSeqs, isEmpty);
                final pending = await tester.runAsync(
                  () => owner!.current!.store.pendingReceipts(),
                );
                expect(pending, hasLength(1));
                details.addAll({
                  'pending_receipts': pending!.length,
                  'pending_next': pending
                      .map((p) => p.nextAfterSeq.toString())
                      .toList(),
                  'can_send': view!.canSend,
                  'observed_503': seenReceipt503,
                });
              case 'delivered_restored':
                // Wait only: production coordinator owns the retry, not this command.
                await until(
                  () =>
                      view!.canSend &&
                      view!.messages.any((m) => m.seq == Seq.parse('1')),
                  'automatic delivery retry',
                );
                final pending = await tester.runAsync(
                  () => owner!.current!.store.pendingReceipts(),
                );
                expect(pending, isEmpty);
                expect(view!.receipts.locallyReadSeqs, isEmpty);
                details.addAll({
                  'pending_receipts': pending!.length,
                  'can_send': view!.canSend,
                });
              case 'show_for_read':
                seenReceipt503 = false;
                await mount();
              case 'receipt_snapshot':
                final pending = await tester.runAsync(
                  () => owner!.current!.store.pendingReceipts(),
                );
                details.addAll({
                  'pending_receipts': pending!.length,
                  'pending_next': pending
                      .map((p) => p.nextAfterSeq.toString())
                      .toList(),
                  'can_send': view!.canSend,
                  'observed_503': seenReceipt503,
                });
                if (cmd['expect_unread'] == true)
                  expect(view!.receipts.locallyReadSeqs, isEmpty);
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
                    (s) =>
                        view!.receipts.locallyReadSeqs.contains(Seq.parse(s)),
                  ),
                  'actual visible read ACK',
                );
                if (cmd['clear_read_failure'] == true) {
                  expect(view!.failure, isNull);
                  expect(find.text('连接或状态查询未完成，请刷新重试。'), findsNothing);
                  details['recovered_warning_cleared'] = true;
                }
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
              case 'permission_prepare':
                await mount();
                expect(view!.canSend, isTrue);
                final page = await tester.runAsync(
                  () => api!.json(
                    'GET',
                    '/api/v1/conversations/${config['conversation']}/sync?after_seq=0&limit=100',
                    bearer: context!.session.accessToken,
                    actingMembershipId: context!.actingMembershipId,
                    device: context!.device,
                  ),
                );
                permissionProof = page!['sync_proof'] as String;
                expect(permissionProof, isNotEmpty);
                details['proof_was_real'] = true;
              case 'end_membership':
                final target = uuidField(cmd, 'membership');
                await tester.runAsync(
                  () => api!.json(
                    'POST',
                    '/api/v1/admin/memberships/$target:end',
                    bearer: context!.session.accessToken,
                    actingMembershipId: context!.actingMembershipId,
                    expectNoContent: true,
                  ),
                );
                details['ended_membership'] = target;
              case 'permission_terminal':
                // No refresh/reselect: the real coordinator must observe denial.
                await until(
                  () =>
                      view!.failure?.status == 403 &&
                      view!.failure?.code == 'invalid_identity' &&
                      view!.context == null &&
                      !view!.channel.connected,
                  'ended membership invalid_identity403',
                );
                expect(view!.canSend, isFalse);
                expect(view!.messages, isEmpty);
                expect(view!.receipts.locallyReadSeqs, isEmpty);
                expect(permissionProof, isNotEmpty);
                final rejections = <String, Object?>{};
                await tester.runAsync(() async {
                  final conversation = config['conversation'] as String;
                  final checks = <String, (String, String, Map<String, Object?>?)>{
                    'sync': (
                      'GET',
                      '/api/v1/conversations/$conversation/sync?after_seq=0&limit=100',
                      null,
                    ),
                    'history': (
                      'GET',
                      '/api/v1/conversations/$conversation/messages?after_seq=0&limit=100',
                      null,
                    ),
                    'delivered': (
                      'PUT',
                      '/api/v1/conversations/$conversation/delivered',
                      {'sync_proof': permissionProof!},
                    ),
                    'read': (
                      'PUT',
                      '/api/v1/conversations/$conversation/read',
                      {
                        'seqs': ['1'],
                      },
                    ),
                    'send': (
                      'POST',
                      '/api/v1/conversations/$conversation/messages',
                      {
                        'client_msg_id': ClientMessageId.create(
                          membership!.current!.clock,
                        ),
                        'message_type': 'text',
                        'text': '任职结束后禁止提交',
                      },
                    ),
                  };
                  for (final entry in checks.entries) {
                    ApiFailure? failure;
                    try {
                      await api!.json(
                        entry.value.$1,
                        entry.value.$2,
                        body: entry.value.$3,
                        bearer: context!.session.accessToken,
                        actingMembershipId: context!.actingMembershipId,
                        device: context!.device,
                      );
                    } on ApiFailure catch (error) {
                      failure = error;
                    }
                    rejections[entry.key] = {
                      'status': failure?.status,
                      'code': failure?.code,
                    };
                    expect(failure?.status, 403);
                    expect(failure?.code, 'invalid_identity');
                  }
                  final store = owner!.current!.store;
                  expect(await store.committedCursor(conversation), isNull);
                  expect(await store.pendingReceipts(), isEmpty);
                  expect(await store.pendingSends(), isEmpty);
                  final self = SelfContext.fromJson(
                    await api!.json(
                      'GET',
                      '/api/v1/me',
                      bearer: context!.session.accessToken,
                    ),
                  );
                  expect(self.userId, config['user']);
                  expect(self.memberships.map((m) => m.id).toList(), [
                    cmd['other_membership'],
                  ]);
                  final devices = DevicePageResult.fromJson(
                    await api!.json(
                      'GET',
                      '/api/v1/devices',
                      bearer: context!.session.accessToken,
                    ),
                    null,
                  );
                  expect(
                    devices.devices.singleWhere((d) => d.id == device).status,
                    'active',
                  );
                  expect(devices.devices.length, 2);
                  details.addAll({
                    'pending_receipts': 0,
                    'pending_sends': 0,
                    'account_active': true,
                    'remaining_memberships': self.memberships
                        .map((m) => m.id)
                        .toList(),
                    'same_device':
                        membership!.current!.device.deviceId == device,
                  });
                });
                expect(rejections, hasLength(5));
                await until(
                  () =>
                      find.textContaining('任职变更前消息一').evaluate().isEmpty &&
                      find.textContaining('任职变更前回复二').evaluate().isEmpty,
                  'ended scope UI erased',
                );
                details.addAll({
                  'rejections': rejections,
                  'context_cleared': true,
                  'can_send': view!.canSend,
                  'ui_body_absent': true,
                  'proof_was_real': true,
                });
              case 'permission_stable':
                expect(view!.context, isNull);
                expect(view!.canSend, isFalse);
                expect(view!.messages, isEmpty);
                expect(view!.receipts.locallyReadSeqs, isEmpty);
                expect(find.textContaining('任职变更前消息一'), findsNothing);
                expect(find.textContaining('任职变更前回复二'), findsNothing);
                expect(
                  await tester.runAsync(
                    () => owner!.current!.store.committedCursor(
                      config['conversation'] as String,
                    ),
                  ),
                  isNull,
                );
                details.addAll({
                  'context_cleared': true,
                  'can_send': false,
                  'ui_body_absent': true,
                });
              case 'identity_still_active':
                final self = await tester.runAsync(
                  () => api!.json(
                    'GET',
                    '/api/v1/me',
                    bearer: owner!.current!.session.accessToken,
                  ),
                );
                expect(SelfContext.fromJson(self!).userId, config['user']);
                expect(owner!.current, isNotNull);
                details['account_active'] = true;
              case 'logout_prepare':
                await mount();
                final waiting = Stopwatch()..start();
                while (true) {
                  final cursor = await tester.runAsync(
                    () => owner!.current!.store.committedCursor(
                      config['conversation'] as String,
                    ),
                  );
                  if (cursor?.toString() == cmd['cursor']) break;
                  if (waiting.elapsed > const Duration(seconds: 60))
                    throw StateError('logout durable cursor not ready');
                  await pause();
                }
                expect(view!.canSend, isTrue);
                expect(
                  await tester.runAsync(
                    () => owner!.current!.store.pendingReceipts(),
                  ),
                  isEmpty,
                );
                details['prepared_cursor'] = cmd['cursor'];
              case 'logout_while_held':
                final before = await tester.runAsync(
                  () => owner!.current!.store.committedCursor(
                    config['conversation'] as String,
                  ),
                );
                expect(before?.toString(), cmd['cursor']);
                expect(
                  view!.messages.any(
                    (m) => m.seq == Seq.parse(cmd['blocked_seq'] as String),
                  ),
                  isFalse,
                );
                details['before_cursor'] = before.toString();
                // Same shutdown order as ClientShell: stop detail, membership,
                // then ClientSession.logout. This test does not navigate Shell.
                await tester.runAsync(() async {
                  await view!.close();
                  membership!.close();
                  await owner!.logout();
                });
                loggedOut = true;
                await captureLogout();
              case 'logout_stable':
                await captureLogout();
              case 'navigation_open':
                await mountNavigationLogin(restored: true);
                await enterNavigationWorkspace();
                details.addAll({
                  'login_restore_via_ui': true,
                  'workspace_entered': true,
                  'membership_selected': context!.actingMembershipId,
                  'same_device': context!.device.deviceId == device,
                  'storage': storageName(),
                  'login_api_creations': loginApiCreations,
                });
              case 'navigation_logout':
                expect(find.byType(MessagePage), findsOneWidget);
                await tester.tap(find.byIcon(Icons.arrow_back));
                await until(
                  () =>
                      find.byType(MessagePage).evaluate().isEmpty &&
                      find.byType(ClientShell).evaluate().isNotEmpty,
                  'back to actual ClientShell',
                );
                final exit = find.widgetWithText(TextButton, '退出登录');
                await until(
                  () =>
                      exit.evaluate().isNotEmpty &&
                      tester.widget<TextButton>(exit).onPressed != null,
                  'actual logout enabled',
                );
                await tester.tap(exit);
                await until(
                  () =>
                      find.byType(ClientShell).evaluate().isEmpty &&
                      find.text('登录集团工作空间').evaluate().isNotEmpty,
                  'logout pops workspace to LoginPage',
                );
                loggedOut = true;
                mounted = false;
                await captureNavigationLogout();
                details['logout_button_tapped'] = true;
              case 'navigation_relogin':
                expect(freshLogin, isTrue);
                expect(loggedOut, isTrue);
                await captureNavigationLogout();
                final preRelogin = Map<String, Object?>.from(details);
                details.clear();
                final oldOwner = owner!, oldView = view!;
                final oldDevice = device;
                final samePage = tester.state(find.byType(LoginPage));
                expect(loginTokenRequests, 1);
                expect(loginRefreshRequests, 0);
                expect(loginCallbackCompletions, 1);
                loginAttemptCallbackBaseline = loginCallbackCompletions;
                await tester.enterText(
                  find.byType(TextField).first,
                  origin.origin,
                );
                await tester.tap(find.widgetWithText(FilledButton, '使用企业账号登录'));
                await until(
                  () => find.text('登录成功').evaluate().isNotEmpty,
                  'actual LoginPage re-login',
                );
                final pageRetained = identical(
                  tester.state(find.byType(LoginPage)),
                  samePage,
                );
                expect(pageRetained, isTrue);
                expect(loginTokenRequests, 2);
                expect(loginRefreshRequests, 0);
                expect(loginCallbackCompletions, 2);
                expect(loginProvider!.authorizeRequests, 2);
                expect(loginProvider!.freshPKCE, isTrue);
                expect(loginProvider!.wrongStateIgnored, isTrue);
                expect(loginProvider!.callbackViaOS, isTrue);
                await enterNavigationWorkspace(fresh: true);
                expect(device, isNot(oldDevice));
                expect(oldOwner.current, isNull);
                expect(oldView.context, isNull);
                expect(oldView.canSend, isFalse);
                final saved = await tester.runAsync(() async {
                  final account = AccountReference(
                    origin: origin,
                    tenantId: config['tenant'] as String,
                    userId: config['user'] as String,
                  );
                  final ns = await accountNamespace(
                    origin,
                    account.tenantId,
                    account.userId,
                  );
                  final complete =
                      await vault.read(ns, 'session') != null &&
                      await owner!.sessions.loadRefresh(account) != null &&
                      await vault.read(bootstrapNamespace, 'account') != null;
                  await recovery.writeAsString(
                    jsonEncode({
                      'nonce': nonce,
                      'storage': storageName(),
                      'device': device,
                    }),
                    flush: true,
                  );
                  return complete;
                });
                expect(saved, isTrue);
                loggedOut = false;
                details.addAll({
                  'pre_relogin_logout': preRelogin,
                  'old_device_id': oldDevice,
                  'storage': storageName(),
                  'relogin_from_same_page': pageRetained,
                  'fresh_pkce': loginProvider!.freshPKCE,
                  'wrong_state_ignored': loginProvider!.wrongStateIgnored,
                  'callback_via_os': loginProvider!.callbackViaOS,
                  'old_owner_stopped': oldOwner.current == null,
                  'old_view_stopped':
                      oldView.context == null && !oldView.canSend,
                  'vault_saved': saved,
                  'workspace_entered': true,
                  'no_seed_refresh': !config.containsKey('refresh'),
                  'token_requests': loginTokenRequests,
                  'refresh_requests': loginRefreshRequests,
                  'callback_completions': loginCallbackCompletions,
                  'authorize_requests': loginProvider!.authorizeRequests,
                });
              case 'navigation_reopen':
                final before = loginApiCreations;
                await mountNavigationLogin(restored: false);
                await captureNavigationLogout();
                expect(loginApiCreations, before);
                details['new_api_creations'] = loginApiCreations - before;
              case 'navigation_cold_start':
                expect(loggedOutRestart, isTrue);
                await mountNavigationLogin(restored: false);
                await captureNavigationLogout();
                expect(loginApiCreations, 0);
                expect(details['no_seed_refresh'], isTrue);
              case 'navigation_logged_out':
                await captureNavigationLogout();
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
    } finally {
      if (navigationProcessHttpInstalled)
        HttpOverrides.global = previousProcessHttp;
    }
  }, timeout: const Timeout(Duration(minutes: 18)));
}
