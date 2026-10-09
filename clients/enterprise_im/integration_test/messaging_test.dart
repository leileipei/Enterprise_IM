// Real backend + production widgets. Host vault/focus and screen size are
// explicit fixtures; installed apps, OS focus and plugin acceptance are separate.
import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:enterprise_im/core/auth/native_auth.dart';
import 'package:enterprise_im/core/auth/account_reference.dart';
import 'package:enterprise_im/core/auth/client_session.dart';
import 'package:enterprise_im/core/auth/session_store.dart';
import 'package:enterprise_im/core/storage/secure_vault.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/models.dart';
import 'package:enterprise_im/core/protocol/seq.dart';
import 'package:enterprise_im/core/sync/client_context.dart';
import 'package:enterprise_im/platform/app_activity.dart';
import 'package:enterprise_im/features/memberships/membership_controller.dart';
import 'package:enterprise_im/features/conversations/conversation_list.dart';
import 'package:enterprise_im/features/conversations/conversation_view.dart';
import 'package:enterprise_im/ui/message_page.dart';
import 'package:enterprise_im/ui/directory_page.dart';
import 'package:enterprise_im/ui/group_page.dart';
import 'files_and_devices_test.dart'
    show FixtureTrust, HostFixtureVault, NoInteractiveBrowser;

void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  testWidgets(
    'real messaging and cross-organization group widgets',
    (tester) => runMessagingScenario(tester, host: false),
  );
}

Future<void> runMessagingScenario(
  WidgetTester tester, {
  required bool host,
}) async {
  const path = String.fromEnvironment('IM_MESSAGING_FIXTURE');
  if (path.isEmpty)
    throw StateError('explicit owned real backend config required; no skip');
  final config = objectMap(
    jsonDecode((await tester.runAsync(() => File(path).readAsString()))!),
  );
  final trust = SecurityContext(withTrustedRoots: false)
    ..setTrustedCertificates(config['ca'] as String);
  await HttpOverrides.runWithHttpOverrides(() async {
    final origin = Uri.parse(config['origin'] as String);
    final owners = <ClientSession>[],
        roles = <MembershipController>[],
        apis = <ApiClient>[],
        activities = <AppActivity>[];
    final views = <ConversationView>[];
    Future<void> control(String action) async {
      final c = HttpClient();
      try {
        final r = await c.postUrl(Uri.parse('${config['control']}/$action'));
        r.headers.set('X-Owned-Fixture', config['control_key'] as String);
        r.headers.contentType = ContentType.json;
        r.write(jsonEncode({'runtime_pid': pid}));
        final res = await r.close();
        await res.drain<void>();
        expect(res.statusCode, 204);
      } finally {
        c.close(force: true);
      }
    }

    Future<void> wait(bool Function() condition) async {
      final sw = Stopwatch()..start();
      while (!condition()) {
        if (sw.elapsed > const Duration(seconds: 25))
          throw StateError('real widget condition timed out');
        await tester.runAsync(
          () => Future<void>.delayed(const Duration(milliseconds: 25)),
        );
        await tester.pump(const Duration(milliseconds: 50));
        expect(tester.takeException(), isNull);
      }
    }

    Future<void> mount(Widget page, {double width = 1100}) async {
      tester.view.physicalSize = Size(width, 900);
      tester.view.devicePixelRatio = 1;
      await tester.pumpWidget(MaterialApp(home: page));
      await tester.pump();
    }

    Future<(ClientSession, MembershipController)> admit(String name) async {
      final api = ApiClient(origin, HttpApiTransport());
      apis.add(api);
      final auth = NativeAuth(
        api,
        callbackFactory: (_, __) => throw StateError('refresh only'),
        browser: NoInteractiveBrowser(),
      );
      final vault = host ? HostFixtureVault() : OsSecureVault();
      final storage = Directory('${config['storage']}/$name');
      await storage.create(recursive: true);
      final sessions = SessionStore(vault, directory: storage);
      final account = AccountReference(
        origin: origin,
        tenantId: config['tenant'] as String,
        userId: config['${name}_user'] as String,
      );
      // ignore: avoid_print
      print('MESSAGING_ADMIT $name refresh');
      final seed = await auth.restore(
        account,
        'macos',
        config['${name}_refresh'] as String,
      );
      // ignore: avoid_print
      print('MESSAGING_ADMIT $name seed-open');
      await sessions.open(seed);
      await sessions.saveCredentials(
        seed,
        refreshToken: auth.refreshCredential,
      );
      // ignore: avoid_print
      print('MESSAGING_ADMIT $name seed-close');
      await sessions.close();
      await auth.cancel();
      final owner = ClientSession(auth, sessions, platform: 'macos');
      owners.add(owner);
      // ignore: avoid_print
      print('MESSAGING_ADMIT $name cold-restore');
      await owner.restore(account);
      final memberships = MembershipController(owner);
      roles.add(memberships);
      // ignore: avoid_print
      print('MESSAGING_ADMIT $name memberships');
      await memberships.load();
      await memberships.select(config['${name}_membership'] as String);
      return (owner, memberships);
    }

    Future<(ConversationView, ConversationSummary)> conversation(
      MembershipController memberships,
      String kind, {
      String? id,
    }) async {
      final lists = ConversationList(memberships);
      final page = await lists.load(kind);
      lists.close();
      final summary = id == null
          ? page.items.single
          : page.items.singleWhere((c) => c.id == id);
      final ready = memberships.current!;
      final ctx = ClientContext(
        session: ready.client.session,
        actingMembershipId: ready.membership.id,
        device: ready.device,
        generation: ready.client.store.generation,
        conversationId: summary.id,
      );
      final activity = AppActivity()
        ..update(
          foreground: true,
          focused: true,
          activeConversation: summary.id,
        );
      activities.add(activity);
      final view = ConversationView(memberships, activity);
      views.add(view);
      await view.select(ctx, summary);
      return (view, summary);
    }

    Future<void> showMessage(
      ConversationView view,
      ConversationSummary summary, {
      double width = 1100,
    }) async {
      await mount(
        Scaffold(
          body: MessagePage(view: view, conversation: summary, onBack: () {}),
        ),
        width: width,
      );
      // ignore: avoid_print
      print(
        'MESSAGING_VIEW canSend=${view.canSend} active=${view.context != null} loading=${view.loading} failure=${view.failure?.code} status=${view.failure?.status} editor=${find.byKey(const ValueKey('message-editor')).evaluate().length}',
      );
      await wait(
        () =>
            view.canSend &&
            find.byKey(const ValueKey('message-editor')).evaluate().isNotEmpty,
      );
    }

    Future<void> send(String text, ConversationView view) async {
      await tester.enterText(
        find.byKey(const ValueKey('message-editor')),
        text,
      );
      await tester.pump();
      expect(
        tester
            .widget<FilledButton>(find.widgetWithText(FilledButton, '发送'))
            .onPressed,
        isNotNull,
      );
      await tester.tap(find.widgetWithText(FilledButton, '发送'));
      await wait(
        () =>
            !view.sending &&
            (view.sender.lastACK != null || view.pending.isNotEmpty),
      );
    }

    try {
      await tester.runAsync(() => control('runtime'));
      // ignore: avoid_print
      print('MESSAGING_STAGE admit');
      final (alice, aRoles) = (await tester.runAsync(() => admit('alice')))!;
      final (bob, bRoles) = (await tester.runAsync(() => admit('bob')))!;
      final (aView, direct) = (await tester.runAsync(
        () => conversation(
          aRoles,
          'direct',
          id: config['conversation'] as String,
        ),
      ))!;
      await showMessage(aView, direct);
      const first = '真实桌面消息', second = '丢失确认后重试原消息', groupText = '真实跨组织群消息';
      // ignore: avoid_print
      print('MESSAGING_STAGE send');
      await send(first, aView);
      await wait(() => aView.messages.any((m) => m.text == first));
      await tester.runAsync(() => control('drop-ack'));
      await send(second, aView);
      await wait(() => aView.pending.length == 1);
      final original = aView.pending.single.clientMsgId;
      expect(find.text('重试原消息'), findsOneWidget);
      await tester.tap(find.text('重试原消息'));
      await wait(
        () => aView.pending.isEmpty && aView.sender.lastACK?.duplicate == true,
      );
      expect(
        (await tester.runAsync(
          () => alice.current!.store.sendConfirmation(original),
        ))!.ack.duplicate,
        isTrue,
      );
      expect(
        (await tester.runAsync(() => alice.current!.store.pendingSends()))!,
        isEmpty,
      );
      await tester.runAsync(aView.close);
      views.remove(aView);
      await tester.pumpWidget(const SizedBox.shrink());
      // ignore: avoid_print
      print('MESSAGING_STAGE read');
      final (bView, bDirect) = (await tester.runAsync(
        () => conversation(bRoles, 'direct', id: direct.id),
      ))!;
      await showMessage(bView, bDirect, width: 390);
      expect(find.text(first), findsOneWidget);
      expect(find.text(second), findsOneWidget);
      await tester.runAsync(
        () => Future<void>.delayed(const Duration(milliseconds: 550)),
      );
      await wait(() => bView.receipts.locallyReadSeqs.length == 2);
      final reads = await tester.runAsync(
        () => alice.auth.api.json(
          'GET',
          '/api/v1/conversations/${direct.id}/receipts?seqs=1,2',
          bearer: alice.current!.session.accessToken,
          actingMembershipId: aRoles.current!.membership.id,
          device: aRoles.current!.device,
        ),
      );
      expect(
        (reads!['receipts'] as List).map((r) => objectMap(r)['status']),
        everyElement('read'),
      );
      await tester.runAsync(bView.close);
      views.remove(bView);
      await tester.pumpWidget(const SizedBox.shrink());
      // ignore: avoid_print
      print('MESSAGING_STAGE group');
      await mount(
        DirectoryPage(memberships: aRoles, mode: DirectoryMode.create),
      );
      await wait(() => find.byType(LinearProgressIndicator).evaluate().isEmpty);
      await tester.enterText(
        find.byKey(const ValueKey('directory-search')),
        '用户 A',
      );
      await tester.tap(find.text('搜索'));
      final crossRole = find.byKey(
        ValueKey('directory-role-${config['bob_group_membership']}'),
      );
      await wait(() => crossRole.evaluate().isNotEmpty);
      await tester.tap(crossRole);
      await tester.enterText(
        find.byKey(const ValueKey('group-name')),
        '真实跨组织协作群',
      );
      await tester.pump();
      await tester.runAsync(() => control('deny-group'));
      await tester.tap(find.text('确认创建群聊'));
      await wait(() => find.text('重试原操作').evaluate().isNotEmpty);
      final pending = await tester.runAsync(
        () => alice.current!.store.pendingAction(),
      );
      expect(pending, isNotNull);
      final noGroups = ConversationList(aRoles);
      expect(
        (await tester.runAsync(() => noGroups.load('group')))!.items,
        isEmpty,
      );
      noGroups.close();
      await tester.tap(find.text('放弃本地操作'));
      await tester.pump();
      await tester.tap(find.text('确认放弃'));
      await wait(() => find.text('重试原操作').evaluate().isEmpty);
      await tester.runAsync(() => control('allow-group'));
      await tester.tap(find.text('确认创建群聊'));
      await wait(() => find.text('操作已确认').evaluate().isNotEmpty);
      expect(
        await tester.runAsync(() => alice.current!.store.pendingAction()),
        isNull,
      );
      await tester.pumpWidget(const SizedBox.shrink());
      final (aGroup, group) = (await tester.runAsync(
        () => conversation(aRoles, 'group'),
      ))!;
      await showMessage(aGroup, group);
      await send(groupText, aGroup);
      await wait(() => aGroup.messages.any((m) => m.text == groupText));
      await tester.runAsync(aGroup.close);
      views.remove(aGroup);
      await tester.pumpWidget(const SizedBox.shrink());
      final oldGeneration = bob.current!.session.generation;
      await tester.runAsync(bob.prepareMembershipSwitch);
      bRoles.close();
      final bGroupRoles = MembershipController(bob);
      roles.add(bGroupRoles);
      await tester.runAsync(() async {
        await bGroupRoles.load();
        await bGroupRoles.select(config['bob_group_membership'] as String);
      });
      expect(bob.current!.session.generation, greaterThan(oldGeneration));
      final (bGroup, bGroupSummary) = (await tester.runAsync(
        () => conversation(bGroupRoles, 'group', id: group.id),
      ))!;
      await showMessage(bGroup, bGroupSummary, width: 390);
      expect(find.text(groupText), findsOneWidget);
      await tester.runAsync(
        () => Future<void>.delayed(const Duration(milliseconds: 550)),
      );
      await wait(
        () => bGroup.receipts.locallyReadSeqs.contains(Seq.parse('1')),
      );
      await tester.runAsync(bGroup.close);
      views.remove(bGroup);
      await tester.pumpWidget(const SizedBox.shrink());
      await mount(
        GroupPage(memberships: bGroupRoles, group: bGroupSummary),
        width: 390,
      );
      await wait(() => find.text('退出群聊').evaluate().isNotEmpty);
      await tester.tap(find.text('退出群聊'));
      await tester.pump();
      await tester.tap(find.text('确认退出'));
      await wait(() => find.text('已退出群聊').evaluate().isNotEmpty);
      expect(find.text('退出群聊'), findsNothing);
      final finalList = ConversationList(bGroupRoles);
      expect(
        (await tester.runAsync(() => finalList.load('group')))!.items,
        isEmpty,
      );
      finalList.close();
      // ignore: avoid_print
      print(
        'NATIVE_MESSAGING_REPORT ${jsonEncode({'status': 'passed', 'messages': 3, 'denied_group': true, 'cross_organization_group': true, 'lost_ack_retry': true, 'visible_read': true, 'left': true, 'installed_native_ui': false, 'os_focus_real': false, 'os_vault_real': !host})}',
      );
    } catch (error, stack) {
      // No request bodies, identifiers or tokens are included by these errors.
      // ignore: avoid_print
      print('MESSAGING_FAILURE $error\n$stack');
      rethrow;
    } finally {
      // ignore: avoid_print
      print('MESSAGING_STAGE closing');
      for (final view in views) {
        await tester.runAsync(view.close);
      }
      await tester.pumpWidget(const SizedBox.shrink());
      for (final role in roles) {
        role.close();
      }
      for (final activity in activities) {
        activity.dispose();
      }
      for (final owner in owners) {
        await tester.runAsync(owner.logout);
      }
      for (final api in apis) {
        api.close();
      }
      tester.view.resetPhysicalSize();
      tester.view.resetDevicePixelRatio();
    }
  }, FixtureTrust(trust));
}
