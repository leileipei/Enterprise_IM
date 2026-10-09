import 'dart:convert';
import 'dart:async';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/auth/client_session.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/features/memberships/membership_controller.dart';
import 'package:enterprise_im/features/conversations/conversation_list.dart';
import 'package:enterprise_im/ui/client_shell.dart';
import 'auth_test.dart' as oidc;
import 'session_bootstrap_test.dart' as bootstrap;
import 'client_session_test.dart' as disk;
import 'login_session_test.dart' show pumpUntil;
import 'membership_admission_test.dart' as fixture;
import 'login_startup_test.dart' as startup;

const dm = '88888888-8888-4888-8888-888888888888';
const group = '99999999-9999-4999-8999-999999999999';
Map<String, Object?> direct({bool visible = true}) => {
  'id': dm,
  'type': 'direct',
  'last_seq': 9007199254740993,
  'updated_at': '2026-10-09T00:00:00Z',
  'peer_visible': visible,
  if (visible) 'display_name': '张三',
  if (visible) 'organization_name': '制造公司',
};
Map<String, Object?> groupItem() => {
  'id': group,
  'type': 'group',
  'name': '跨组织项目群',
  'status': 'active',
  'role': 'member',
  'source_membership_id': fixture.role,
  'last_seq': 2,
  'updated_at': '2026-10-09T00:00:00Z',
};
Future<ClientSession> owner(
  Future<ApiResponse> Function(ApiRequest, Cancellation) messages,
) async {
  final (_, _, store) = await disk.fixture();
  final native = bootstrap.authentication(
    oidc.FakeTransport((r, c) async {
      if (r.uri.path.endsWith('/config')) return oidc.response(oidc.config());
      if (r.uri.path.endsWith('/refresh'))
        return oidc.response(oidc.tokens('fresh-refresh'));
      if (r.uri.path == '/api/v1/me') return oidc.response(fixture.self());
      if (r.uri.path == '/api/v1/devices')
        return oidc.response(fixture.grant());
      return messages(r, c);
    }),
  );
  final result = ClientSession(native, store, platform: 'macos');
  await result.restore(bootstrap.account());
  return result;
}

void main() {
  testWidgets(
    'cold-start login opens role/device workspace and logout clears the account',
    (tester) async {
      final (root, vault, store) = (await tester.runAsync(startup.seed))!;
      startup.cleanup(tester, root, store);
      await tester.pumpWidget(
        MaterialApp(
          home: startup.login(
            store,
            oidc.FakeTransport((r, _) async {
              if (r.uri.path.endsWith('/config'))
                return oidc.response(oidc.config());
              if (r.uri.path.endsWith('/refresh'))
                return oidc.response(oidc.tokens('fresh-refresh'));
              if (r.uri.path == '/api/v1/me')
                return oidc.response(fixture.self());
              if (r.uri.path == '/api/v1/devices')
                return oidc.response(fixture.grant());
              return oidc.response({
                'conversations': [direct()],
                'has_more': false,
              });
            }),
          ),
        ),
      );
      await pumpUntil(tester, () => find.text('进入工作空间').evaluate().isNotEmpty);
      await tester.tap(find.text('进入工作空间'));
      await pumpUntil(
        tester,
        () => find
            .byKey(const ValueKey('membership-${fixture.role}'))
            .evaluate()
            .isNotEmpty,
      );
      await tester.pump(const Duration(milliseconds: 500));
      await tester.tap(
        find.byKey(const ValueKey('membership-${fixture.role}')),
      );
      await pumpUntil(tester, () => find.text('张三').evaluate().isNotEmpty);
      await tester.tap(
        find.descendant(
          of: find.byType(ClientShell),
          matching: find.text('退出登录'),
        ),
      );
      await pumpUntil(tester, () => vault.values.isEmpty);
      await tester.pump(const Duration(milliseconds: 500));
      await pumpUntil(
        tester,
        () => vault.values.isEmpty && find.text('集团工作空间').evaluate().isEmpty,
      );
      expect(find.text('登录成功'), findsNothing);
      expect(root.listSync(), isEmpty);
    },
  );

  test(
    'conversation pages preserve int64 and distinguish hidden peer and group',
    () async {
      final session = await owner((r, _) async {
        expect(r.headers['X-Acting-Membership-ID'], fixture.role);
        if (r.uri.path == '/api/v1/conversations')
          return oidc.response({
            'conversations': [direct(visible: false)],
            'has_more': false,
          });
        expect(r.uri.path, '/api/v1/groups');
        return oidc.response({
          'groups': [groupItem()],
          'has_more': false,
        });
      });
      addTearDown(session.logout);
      final memberships = MembershipController(session);
      addTearDown(memberships.close);
      await memberships.load();
      await memberships.select(fixture.role);
      final lists = ConversationList(memberships);
      addTearDown(lists.close);
      final directs = await lists.load('direct');
      expect(directs.items.single.name, '不可见联系人');
      expect(directs.items.single.lastSeq.toString(), '9007199254740993');
      final groups = await lists.load('group');
      expect(groups.items.single.name, '跨组织项目群');
    },
  );
  test(
    'pagination rejects changed kind, repeated cursor and stale logout response',
    () async {
      var malformed = false;
      final late = Completer<ApiResponse>(), entered = Completer<void>();
      final session = await owner((r, _) async {
        if (malformed)
          return oidc.response({
            'conversations': [groupItem()],
            'has_more': false,
          });
        if (r.uri.queryParameters['cursor'] == 'first')
          return oidc.response({
            'conversations': [],
            'has_more': true,
            'next_cursor': 'first',
          });
        if (r.uri.queryParameters['cursor'] == 'late') {
          entered.complete();
          return late.future;
        }
        return oidc.response({
          'conversations': [direct()],
          'has_more': true,
          'next_cursor': 'first',
        });
      });
      addTearDown(session.logout);
      final memberships = MembershipController(session);
      addTearDown(memberships.close);
      await memberships.load();
      await memberships.select(fixture.role);
      final lists = ConversationList(memberships);
      addTearDown(lists.close);
      expect((await lists.load('direct')).hasMore, isTrue);
      await expectLater(
        lists.load('direct', cursor: 'first'),
        throwsA(isA<FormatException>()),
      );
      malformed = true;
      await expectLater(lists.load('direct'), throwsA(isA<FormatException>()));
      malformed = false;
      final loading = lists.load('direct', cursor: 'late');
      final checked = expectLater(loading, throwsA(isA<ApiFailure>()));
      await entered.future;
      await session.logout();
      late.complete(
        oidc.response({
          'conversations': [direct()],
          'has_more': false,
        }),
      );
      await checked;
    },
  );
  testWidgets(
    'workspace requires explicit membership and verified device before listing conversations',
    (tester) async {
      var quota = true, lists = 0;
      final prepared = (await tester.runAsync(disk.fixture))!;
      final store = prepared.$3;
      final native = bootstrap.authentication(
        oidc.FakeTransport((r, _) async {
          if (r.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (r.uri.path.endsWith('/refresh'))
            return oidc.response(oidc.tokens('fresh-refresh'));
          if (r.uri.path == '/api/v1/me') return oidc.response(fixture.self());
          if (r.uri.path == '/api/v1/devices') {
            if (quota)
              return ApiResponse(
                409,
                utf8.encode('{"error_code":"device_quota"}'),
                {},
              );
            return oidc.response(fixture.grant());
          }
          lists++;
          return oidc.response({
            'conversations': [direct()],
            'has_more': false,
          });
        }),
      );
      final session = ClientSession(native, store, platform: 'macos');
      await tester.runAsync(() => session.restore(bootstrap.account()));
      await tester.pumpWidget(
        MaterialApp(
          home: ClientShell(
            owner: session,
            onLogout: () async {
              await session.logout();
              return true;
            },
          ),
        ),
      );
      await pumpUntil(
        tester,
        () => find
            .byKey(const ValueKey('membership-${fixture.role}'))
            .evaluate()
            .isNotEmpty,
      );
      expect(lists, 0);
      expect(find.text('张三'), findsNothing);
      await tester.tap(
        find.byKey(const ValueKey('membership-${fixture.role}')),
      );
      await pumpUntil(
        tester,
        () => find.textContaining('设备额度').evaluate().isNotEmpty,
      );
      expect(lists, 0);
      expect(find.text('张三'), findsNothing);
      quota = false;
      await tester.tap(
        find.byKey(const ValueKey('membership-${fixture.role}')),
      );
      await pumpUntil(tester, () => find.text('张三').evaluate().isNotEmpty);
      expect(lists, 1);
      await tester.pumpWidget(const MaterialApp(home: SizedBox()));
      await tester.pump();
      var closed = false;
      final closing = session.logout().whenComplete(() => closed = true);
      await pumpUntil(tester, () => closed);
      await closing;
    },
  );
  testWidgets('closing workspace cancels late membership display', (
    tester,
  ) async {
    final prepared = (await tester.runAsync(disk.fixture))!;
    final store = prepared.$3;
    var lateMode = false;
    final late = Completer<ApiResponse>();
    final native = bootstrap.authentication(
      oidc.FakeTransport((r, _) async {
        if (r.uri.path.endsWith('/config')) return oidc.response(oidc.config());
        if (r.uri.path.endsWith('/refresh'))
          return oidc.response(oidc.tokens('fresh-refresh'));
        if (lateMode) return late.future;
        return oidc.response(fixture.self());
      }),
    );
    final session = ClientSession(native, store, platform: 'macos');
    await tester.runAsync(() => session.restore(bootstrap.account()));
    lateMode = true;
    await tester.pumpWidget(
      MaterialApp(
        home: ClientShell(owner: session, onLogout: () async => true),
      ),
    );
    await tester.pumpWidget(const MaterialApp(home: SizedBox()));
    late.complete(oidc.response(fixture.self()));
    await tester.pump();
    expect(tester.takeException(), isNull);
    var closed = false;
    final closing = session.logout().whenComplete(() => closed = true);
    await pumpUntil(tester, () => closed);
    await closing;
  });
}
