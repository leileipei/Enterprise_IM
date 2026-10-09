import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/client_id.dart';
import 'package:enterprise_im/core/storage/records.dart';
import 'package:enterprise_im/core/storage/encrypted_store.dart';
import 'package:enterprise_im/features/directory/directory_controller.dart';
import 'package:enterprise_im/features/groups/group_controller.dart';
import 'auth_test.dart' as oidc;
import 'navigation_test.dart' as nav;
import 'message_send_test.dart' as sending;
import 'membership_admission_test.dart' as membership;
import 'session_bootstrap_test.dart' as bootstrap;

const interval = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
Map<String, Object?> person() => {
  'id': bootstrap.tenant,
  'display_name': '李四',
  'employee_no': 'E100',
  'memberships': [
    {
      'membership_id': membership.otherRole,
      'organization_id': bootstrap.user,
      'organization_name': '子公司',
      'title': '工程师',
      'is_primary': true,
      'departments': [
        {'id': nav.dm, 'name': '研发部'},
      ],
    },
  ],
};
Map<String, Object?> ownGroup({String role = 'member'}) => {
  'interval_id': interval,
  'role': role,
  'join_seq': 9007199254740993,
  'group_status': 'active',
};
void main() {
  test(
    'serialized storage cannot admit a message and a group action together',
    () async {
      final owner = await nav.owner(
        (r, _) async => throw const ApiFailure('unexpected'),
      );
      addTearDown(owner.logout);
      final (roles, context) = await sending.admission(owner);
      addTearDown(roles.close);
      final store = owner.current!.store;
      final action = PendingAction(
        id: interval,
        actingMembershipId: membership.role,
        target: '',
        kind: ActionKind.direct,
        generation: store.generation,
        body: {'target_membership_id': membership.otherRole},
      );
      final message = PendingSend(
        clientMsgId: ClientMessageId.create(roles.current!.clock),
        conversationId: nav.dm,
        actingMembershipId: membership.role,
        text: '原请求',
        generation: store.generation,
        createdAt: context.device.serverTime,
      );
      await store.putPendingAction(action);
      await expectLater(
        store.putPendingSend(message),
        throwsA(isA<StoreFailure>()),
      );
      await store.finishAction(action);
      await store.putPendingSend(message);
      await expectLater(
        store.putPendingAction(action),
        throwsA(isA<StoreFailure>()),
      );
    },
  );

  test(
    'visible search encodes query under exact membership; direct creation binds target role',
    () async {
      final calls = <ApiRequest>[];
      final owner = await nav.owner((r, _) async {
        calls.add(r);
        if (r.method == 'GET')
          return oidc.response({
            'people': [person()],
            'has_more': true,
          });
        expect(jsonDecode(utf8.decode(r.body!)), {
          'target_membership_id': membership.otherRole,
        });
        return oidc.response({
          'id': nav.dm,
          'type': 'direct',
          'last_seq': 9007199254740993,
          'policy_version': 1,
          'cross_legal': true,
          'decision_reason': 'allow',
        });
      });
      addTearDown(owner.logout);
      final (roles, _) = await sending.admission(owner);
      addTearDown(roles.close);
      final directory = DirectoryController(roles),
          groups = GroupController(roles);
      addTearDown(directory.close);
      addTearDown(groups.close);
      final people = await directory.search('李四 &A');
      expect(people.people.single.memberships.single.id, membership.otherRole);
      expect(people.hasMore, isTrue);
      expect(calls.first.uri.queryParameters['q'], '李四 &A');
      expect(calls.first.headers['X-Acting-Membership-ID'], membership.role);
      final result = await groups.startDirect(membership.otherRole);
      expect(result.conversationId, nav.dm);
      expect(result.sequence.toString(), '9007199254740993');
      expect(await owner.current!.store.pendingAction(), isNull);
    },
  );
  test(
    'search cancellation and fresh owner reject late private people',
    () async {
      final late = Completer<ApiResponse>();
      final owner = await nav.owner((r, _) => late.future);
      addTearDown(owner.logout);
      final (roles, _) = await sending.admission(owner);
      addTearDown(roles.close);
      final directory = DirectoryController(roles);
      final request = directory.search('李四');
      final denied = expectLater(request, throwsA(isA<ApiFailure>()));
      directory.close();
      late.complete(
        oidc.response({
          'people': [person()],
          'has_more': false,
        }),
      );
      await denied;
    },
  );
  test('duplicate membership or invalid short query is not exposed', () async {
    final duplicate = person();
    duplicate['memberships'] = [
      person()['memberships'] is List
          ? (person()['memberships'] as List).first
          : null,
      (person()['memberships'] as List).first,
    ];
    final owner = await nav.owner(
      (r, _) async => oidc.response({
        'people': [duplicate],
        'has_more': false,
      }),
    );
    addTearDown(owner.logout);
    final (roles, _) = await sending.admission(owner);
    addTearDown(roles.close);
    final directory = DirectoryController(roles);
    addTearDown(directory.close);
    await expectLater(directory.search('李'), throwsA(isA<FormatException>()));
    await expectLater(directory.search('李四'), throwsA(isA<FormatException>()));
  });
  test(
    'group create timeout survives controller restart with exact encrypted request',
    () async {
      final bodies = <Object?>[];
      var timeout = true;
      late GroupController groups;
      final owner = await nav.owner((r, _) async {
        final body = jsonDecode(utf8.decode(r.body!));
        bodies.add(body);
        final saved = await groups.pending();
        expect(saved!.body, body);
        if (timeout) throw const ApiFailure('timeout');
        return oidc.response({
          'id': nav.group,
          'type': 'group',
          'last_seq': 0,
          'policy_version': 1,
          'member_count': 2,
        });
      });
      addTearDown(owner.logout);
      final (roles, _) = await sending.admission(owner);
      addTearDown(roles.close);
      groups = GroupController(roles);
      await expectLater(
        groups.create('跨组织开发群-秘密', [membership.otherRole]),
        throwsA(isA<ApiFailure>()),
      );
      final saved = (await groups.pending())!;
      await expectLater(
        owner.current!.store.putPendingAction(
          PendingAction.fromJson({
            ...saved.toJson(),
            'body': {...saved.body!, 'name': '篡改名称'},
          }),
        ),
        throwsA(isA<StoreFailure>()),
      );
      final files = owner.sessions.directory.listSync().whereType<File>().where(
        (f) => f.path.endsWith('.sqlite') || f.path.endsWith('-wal'),
      );
      for (final file in files)
        expect(
          utf8.decode(await file.readAsBytes(), allowMalformed: true),
          isNot(contains('跨组织开发群-秘密')),
        );
      groups.close();
      groups = GroupController(roles);
      addTearDown(groups.close);
      await expectLater(
        groups.create('另一个群', [membership.otherRole]),
        throwsA(isA<ApiFailure>()),
      );
      timeout = false;
      expect((await groups.retry()).conversationId, nav.group);
      expect(bodies, hasLength(2));
      expect(bodies[0], bodies[1]);
      expect(await groups.pending(), isNull);
    },
  );
  test(
    '403 group denial and wrong interval ACK retain pending; explicit abandon permits new action',
    () async {
      var deny = true;
      final owner = await nav.owner((r, _) async {
        if (r.method == 'GET') return oidc.response(ownGroup());
        if (deny)
          return ApiResponse(
            403,
            utf8.encode('{"error_code":"group_permission_denied"}'),
            {},
          );
        return oidc.response({
          'interval_id': nav.dm,
          'status': 'left',
          'leave_seq': 3,
        });
      });
      addTearDown(owner.logout);
      final (roles, _) = await sending.admission(owner);
      addTearDown(roles.close);
      final groups = GroupController(roles);
      addTearDown(groups.close);
      final own = await groups.membership(nav.group);
      expect(own.joinSeq.toString(), '9007199254740993');
      await expectLater(
        groups.leave(nav.group, own.intervalId),
        throwsA(isA<ApiFailure>()),
      );
      expect(await groups.pending(), isNotNull);
      deny = false;
      await expectLater(groups.retry(), throwsA(isA<FormatException>()));
      expect(await groups.pending(), isNotNull);
      await groups.abandon();
      expect(await groups.pending(), isNull);
    },
  );
  test(
    'roster paginates strict minimal intervals and own leave matches server status',
    () async {
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/members'))
          return oidc.response({
            'members': [
              {
                'interval_id': interval,
                'display_name': '李四',
                'role': 'member',
                'organization_name': '子公司',
              },
            ],
            'has_more': true,
            'next_cursor': 'page2',
          });
        return oidc.response({
          'interval_id': interval,
          'status': 'left',
          'leave_seq': 9007199254740993,
        });
      });
      addTearDown(owner.logout);
      final (roles, _) = await sending.admission(owner);
      addTearDown(roles.close);
      final groups = GroupController(roles);
      addTearDown(groups.close);
      final roster = await groups.roster(nav.group);
      expect(roster.members.single.intervalId, interval);
      expect(roster.cursor, 'page2');
      expect((await groups.leave(nav.group, interval)).status, 'left');
      expect(await groups.pending(), isNull);
    },
  );
}
