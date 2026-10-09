import 'dart:convert';
import 'dart:async';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/ui/login_page.dart';
import 'auth_test.dart' as oidc;
import 'directory_group_test.dart' as fixture;
import 'navigation_test.dart' as nav;
import 'membership_admission_test.dart' as membership;
import 'login_startup_test.dart' as startup;
import 'login_session_test.dart' show pumpUntil;

void main() {
  testWidgets(
    'returning during role reauthentication still hands latest owner to login parent',
    (tester) async {
      final (root, _, store) = (await tester.runAsync(startup.seed))!;
      startup.cleanup(tester, root, store);
      var switching = false, entered = false;
      final late = Completer<ApiResponse>();
      final transport = oidc.FakeTransport((r, _) async {
        if (r.uri.path.endsWith('/config')) return oidc.response(oidc.config());
        if (r.uri.path.endsWith('/refresh')) {
          if (switching) {
            entered = true;
            return late.future;
          }
          return oidc.response(oidc.tokens('fresh-refresh'));
        }
        if (r.uri.path == '/api/v1/me') return oidc.response(membership.self());
        if (r.uri.path == '/api/v1/devices')
          return oidc.response(membership.grant());
        return oidc.response({'conversations': [], 'has_more': false});
      });
      await tester.pumpWidget(
        MaterialApp(home: startup.login(store, transport)),
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
      await tester.tap(
        find.byKey(const ValueKey('membership-${membership.role}')),
      );
      await pumpUntil(
        tester,
        () =>
            find.text('切换任职').evaluate().isNotEmpty &&
            find.byType(LinearProgressIndicator).evaluate().isEmpty,
      );
      switching = true;
      await tester.tap(find.text('切换任职'));
      await pumpUntil(tester, () => entered);
      await tester.pageBack();
      await tester.pumpAndSettle();
      expect(find.byType(LoginPage), findsOneWidget);
      expect(find.text('进入工作空间'), findsNothing);
      late.complete(oidc.response(oidc.tokens('late-refresh')));
      await pumpUntil(tester, () => find.text('进入工作空间').evaluate().isNotEmpty);
    },
  );

  testWidgets(
    'directory selects cross-organization membership; denied group is not shown as created; confirmed leave withdraws group',
    (tester) async {
      final (root, _, store) = (await tester.runAsync(startup.seed))!;
      startup.cleanup(tester, root, store);
      var created = false, left = false, denied = true;
      final groupBodies = <Object?>[];
      final transport = oidc.FakeTransport((r, _) async {
        if (r.uri.path.endsWith('/config')) return oidc.response(oidc.config());
        if (r.uri.path.endsWith('/refresh'))
          return oidc.response(oidc.tokens('fresh-refresh'));
        if (r.uri.path == '/api/v1/me') return oidc.response(membership.self());
        if (r.uri.path == '/api/v1/devices' || r.uri.path.endsWith('/renew'))
          return oidc.response(
            membership.grant(renew: r.uri.path.endsWith('/renew')),
          );
        if (r.uri.path == '/api/v1/directory/users')
          return oidc.response({
            'people': [fixture.person()],
            'has_more': false,
          });
        if (r.uri.path == '/api/v1/conversations')
          return oidc.response({'conversations': [], 'has_more': false});
        if (r.uri.path == '/api/v1/groups' && r.method == 'GET')
          return oidc.response({
            'groups': created && !left ? [nav.groupItem()] : [],
            'has_more': false,
          });
        if (r.uri.path == '/api/v1/groups' && r.method == 'POST') {
          groupBodies.add(jsonDecode(utf8.decode(r.body!)));
          if (denied)
            return ApiResponse(
              403,
              utf8.encode('{"error_code":"invalid_identity"}'),
              {},
            );
          created = true;
          return oidc.response({
            'id': nav.group,
            'type': 'group',
            'last_seq': 0,
            'policy_version': 1,
            'member_count': 2,
          });
        }
        if (r.uri.path.endsWith('/membership'))
          return oidc.response(fixture.ownGroup());
        if (r.uri.path.endsWith('/leave')) {
          expect(jsonDecode(utf8.decode(r.body!)), {
            'interval_id': fixture.interval,
          });
          left = true;
          return oidc.response({
            'interval_id': fixture.interval,
            'status': 'left',
            'leave_seq': 2,
          });
        }
        throw const ApiFailure('unexpected');
      });
      await tester.pumpWidget(
        MaterialApp(home: startup.login(store, transport)),
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
      await tester.tap(
        find.byKey(const ValueKey('membership-${membership.role}')),
      );
      await pumpUntil(tester, () => find.text('创建群聊').evaluate().isNotEmpty);
      await tester.tap(find.text('创建群聊'));
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
      await tester.tap(find.text('搜索'));
      await pumpUntil(
        tester,
        () => find
            .byKey(const ValueKey('directory-role-${membership.otherRole}'))
            .evaluate()
            .isNotEmpty,
      );
      await tester.ensureVisible(
        find.byKey(const ValueKey('directory-role-${membership.otherRole}')),
      );
      await tester.tap(
        find.byKey(const ValueKey('directory-role-${membership.otherRole}')),
      );
      await tester.enterText(
        find.byKey(const ValueKey('group-name')),
        '跨组织开发群',
      );
      await tester.ensureVisible(find.text('确认创建群聊'));
      await tester.tap(find.text('确认创建群聊'));
      await pumpUntil(tester, () => find.text('重试原操作').evaluate().isNotEmpty);
      expect(created, isFalse);
      expect(groupBodies, hasLength(1));
      denied = false;
      await tester.ensureVisible(find.text('重试原操作'));
      await tester.tap(find.text('重试原操作'));
      await pumpUntil(tester, () => find.text('操作已确认').evaluate().isNotEmpty);
      expect(groupBodies, hasLength(2));
      expect(groupBodies[0], groupBodies[1]);
      await tester.tap(find.byTooltip('返回工作空间'));
      await tester.pumpAndSettle();
      await pumpUntil(tester, () => find.text('跨组织项目群').evaluate().isNotEmpty);
      await tester.tap(find.byTooltip('管理跨组织项目群'));
      await tester.pump();
      await pumpUntil(tester, () => find.text('群管理').evaluate().isNotEmpty);
      await pumpUntil(
        tester,
        () => find.byType(LinearProgressIndicator).evaluate().isEmpty,
      );
      await tester.pumpAndSettle();
      await pumpUntil(tester, () => find.text('退出群聊').evaluate().isNotEmpty);
      await tester.tap(find.text('退出群聊'));
      await tester.pumpAndSettle();
      await tester.tap(find.text('确认退出'));
      await tester.pump();
      await pumpUntil(tester, () => find.text('已退出群聊').evaluate().isNotEmpty);
      expect(left, isTrue);
      await tester.tap(find.byTooltip('返回工作空间'));
      await tester.pumpAndSettle();
      await pumpUntil(tester, () => find.text('跨组织项目群').evaluate().isEmpty);
      expect(find.text('暂无会话'), findsOneWidget);
    },
  );
  testWidgets(
    'workspace role switch re-admits and login parent follows latest session',
    (tester) async {
      final (root, _, store) = (await tester.runAsync(startup.seed))!;
      startup.cleanup(tester, root, store);
      var renews = 0;
      final transport = oidc.FakeTransport((r, _) async {
        if (r.uri.path.endsWith('/config')) return oidc.response(oidc.config());
        if (r.uri.path.endsWith('/refresh'))
          return oidc.response(oidc.tokens('fresh-refresh'));
        if (r.uri.path == '/api/v1/me') return oidc.response(membership.self());
        if (r.uri.path.endsWith('/renew')) {
          renews++;
          return oidc.response(membership.grant(renew: true));
        }
        if (r.uri.path == '/api/v1/devices')
          return oidc.response(membership.grant());
        return oidc.response({'conversations': [], 'has_more': false});
      });
      await tester.pumpWidget(
        MaterialApp(home: startup.login(store, transport)),
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
      await tester.tap(
        find.byKey(const ValueKey('membership-${membership.role}')),
      );
      await pumpUntil(tester, () => find.text('切换任职').evaluate().isNotEmpty);
      await tester.tap(find.text('切换任职'));
      await pumpUntil(
        tester,
        () => find
            .byKey(const ValueKey('membership-${membership.otherRole}'))
            .evaluate()
            .isNotEmpty,
      );
      await tester.tap(
        find.byKey(const ValueKey('membership-${membership.otherRole}')),
      );
      await pumpUntil(tester, () => renews == 1);
      await tester.pageBack();
      await tester.pumpAndSettle();
      expect(find.byType(LoginPage), findsOneWidget);
      expect(find.text('进入工作空间'), findsOneWidget);
    },
  );
}
