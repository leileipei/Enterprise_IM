import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'auth_test.dart' as oidc;
import 'directory_group_test.dart' as fixture;
import 'navigation_test.dart' as nav;
import 'membership_admission_test.dart' as membership;
import 'login_startup_test.dart' as startup;
import 'login_session_test.dart' show pumpUntil;

Future<void> open(
  WidgetTester tester, {
  String role = 'owner',
  bool large = false,
  bool expiry = false,
}) async {
  final (root, _, store) = (await tester.runAsync(startup.seed))!;
  startup.cleanup(tester, root, store);
  final transport = oidc.FakeTransport((r, _) async {
    if (r.uri.path.endsWith('/config')) return oidc.response(oidc.config());
    if (r.uri.path.endsWith('/refresh'))
      return oidc.response(oidc.tokens('fresh-refresh'));
    if (r.uri.path == '/api/v1/me') return oidc.response(membership.self());
    if (r.uri.path == '/api/v1/devices')
      return oidc.response({
        ...membership.grant(),
        if (expiry) 'expires_at': '2026-10-09T00:00:02Z',
      });
    if (r.uri.path == '/api/v1/conversations')
      return oidc.response({'conversations': [], 'has_more': false});
    if (r.uri.path == '/api/v1/groups')
      return oidc.response({
        'groups': [nav.groupItem()],
        'has_more': false,
      });
    if (r.uri.path.endsWith('/membership'))
      return oidc.response(fixture.ownGroup(role: role));
    if (r.uri.path.endsWith('/members'))
      return oidc.response({
        'members': [
          {
            'interval_id': fixture.interval,
            'display_name': '本账号',
            'role': role,
            'organization_name': '集团总部',
          },
          {
            'interval_id': nav.dm,
            'display_name': '李四',
            'role': 'member',
            'organization_name': '海外研发子公司',
          },
        ],
        'has_more': false,
      });
    if (r.uri.path == '/api/v1/directory/users')
      return oidc.response({
        'people': [fixture.person()],
        'has_more': false,
      });
    throw const ApiFailure('unexpected');
  });
  await tester.pumpWidget(
    MaterialApp(
      builder: (context, child) => MediaQuery(
        data: MediaQuery.of(
          context,
        ).copyWith(textScaler: TextScaler.linear(large ? 2 : 1)),
        child: child!,
      ),
      home: startup.login(store, transport),
    ),
  );
  await pumpUntil(tester, () => find.text('进入工作空间').evaluate().isNotEmpty);
  await tester.tap(find.text('进入工作空间'));
  await tester.pumpAndSettle();
  await pumpUntil(
    tester,
    () => find
        .byKey(const ValueKey('membership-${membership.role}'))
        .evaluate()
        .isNotEmpty,
  );
  await tester.tap(find.byKey(const ValueKey('membership-${membership.role}')));
  await pumpUntil(
    tester,
    () =>
        find.text('通讯录').evaluate().isNotEmpty &&
        find.byType(LinearProgressIndicator).evaluate().isEmpty,
  );
}

void main() {
  testWidgets(
    'mobile large text group management preserves readable actions and button semantics',
    (tester) async {
      tester.view.physicalSize = const Size(420, 1000);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.resetPhysicalSize);
      addTearDown(tester.view.resetDevicePixelRatio);
      final semantics = tester.ensureSemantics();

      try {
        await open(tester, large: true);
        await tester.tap(find.text('群聊'));
        await tester.pump();
        await pumpUntil(
          tester,
          () => find.byTooltip('管理跨组织项目群').evaluate().isNotEmpty,
        );
        await tester.tap(find.byTooltip('管理跨组织项目群'));
        await tester.pump();
        await pumpUntil(
          tester,
          () =>
              find.text('转让群主').evaluate().isNotEmpty &&
              find.byType(LinearProgressIndicator).evaluate().isEmpty,
        );
        await tester.pumpAndSettle();
        expect(tester.takeException(), isNull);
        await tester.ensureVisible(find.text('邀请人员'));
        expect(
          tester
              .getSemantics(find.widgetWithText(FilledButton, '邀请人员'))
              .getSemanticsData()
              .flagsCollection
              .isButton,
          isTrue,
        );
        expect(find.text('退出群聊'), findsNothing);
        expect(find.text('退出前请先转让群主。'), findsOneWidget);
      } finally {
        semantics.dispose();
      }
    },
  );
  testWidgets(
    'directory keyboard search is withdrawn when device admission timer ends',
    (tester) async {
      await open(tester, expiry: true);
      await tester.tap(find.text('通讯录'));
      await tester.pump();
      await pumpUntil(
        tester,
        () => find
            .byKey(const ValueKey('directory-search'))
            .evaluate()
            .isNotEmpty,
      );
      await pumpUntil(
        tester,
        () => find.byType(LinearProgressIndicator).evaluate().isEmpty,
      );
      await tester.pumpAndSettle();
      await tester.enterText(
        find.byKey(const ValueKey('directory-search')),
        '李四',
      );
      await tester.testTextInput.receiveAction(TextInputAction.search);
      await pumpUntil(tester, () => find.text('发起单聊').evaluate().isNotEmpty);
      await tester.pump(const Duration(seconds: 3));
      expect(find.text('发起单聊'), findsNothing);
      expect(find.text('子公司'), findsNothing);
      expect(find.byKey(const ValueKey('directory-search')), findsNothing);
      expect(find.text('身份或设备验证已到期，请返回工作空间重新登录。'), findsOneWidget);
    },
  );
}
