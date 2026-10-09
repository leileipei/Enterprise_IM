import 'dart:async';
import 'dart:convert';
import 'package:flutter_test/flutter_test.dart';
import 'package:sqlite3/sqlite3.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/models.dart';
import 'package:enterprise_im/core/auth/client_session.dart';
import 'package:enterprise_im/core/auth/auth_session.dart';
import 'package:enterprise_im/features/memberships/membership_controller.dart';
import 'package:enterprise_im/core/storage/encrypted_store.dart';
import 'package:enterprise_im/core/storage/secure_vault.dart';
import 'package:enterprise_im/features/conversations/conversation_controller.dart';
import 'package:enterprise_im/features/groups/group_controller.dart';
import 'auth_test.dart' as oidc;
import 'navigation_test.dart' as nav;
import 'message_send_test.dart' as sending;
import 'membership_admission_test.dart' as membership;
import 'session_bootstrap_test.dart' as bootstrap;
import 'directory_group_test.dart' as fixture;

Map<String, Object?> created() => {
  'id': nav.group,
  'type': 'group',
  'last_seq': 0,
  'policy_version': 1,
  'member_count': 2,
};
void main() {
  test(
    'cold restoration cannot choose a different role while original group action is pending',
    () async {
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/renew'))
          return oidc.response(membership.grant(renew: true));
        throw const ApiFailure('timeout');
      });
      addTearDown(owner.logout);
      final (old, _) = await sending.admission(owner);
      addTearDown(old.close);
      final groups = GroupController(old);
      addTearDown(groups.close);
      await expectLater(
        groups.create('原任职群', [membership.otherRole]),
        throwsA(isA<ApiFailure>()),
      );
      old.close();
      await owner.close();
      await owner.restore(bootstrap.account());
      final next = MembershipController(owner);
      addTearDown(next.close);
      await next.load();
      await expectLater(
        next.select(membership.otherRole),
        throwsA(
          isA<AuthFailure>().having(
            (e) => e.code,
            'code',
            'operation_context_mismatch',
          ),
        ),
      );
      expect(next.current, isNull);
      expect(
        (await next.select(membership.role)).membership.id,
        membership.role,
      );
    },
  );

  test(
    'pending group request survives real account database reopen and fresh generation',
    () async {
      var timeout = true;
      final bodies = <Object?>[];
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/renew'))
          return oidc.response(membership.grant(renew: true));
        bodies.add(jsonDecode(utf8.decode(r.body!)));
        if (timeout) throw const ApiFailure('timeout');
        return oidc.response(created());
      });
      addTearDown(owner.logout);
      final (old, _) = await sending.admission(owner);
      addTearDown(old.close);
      final first = GroupController(old);
      await expectLater(
        first.create('重开恢复群', [membership.otherRole]),
        throwsA(isA<ApiFailure>()),
      );
      final record = (await first.pending())!;
      final oldStore = owner.current!.store;
      first.close();
      old.close();
      await owner.close();
      await owner.restore(bootstrap.account());
      expect(owner.current!.store, isNot(same(oldStore)));
      expect(owner.current!.session.generation, greaterThan(record.generation));
      final (roles, _) = await sending.admission(owner);
      addTearDown(roles.close);
      final second = GroupController(roles);
      addTearDown(second.close);
      expect((await second.pending())!.toJson(), record.toJson());
      timeout = false;
      expect((await second.retry()).conversationId, nav.group);
      expect(bodies, hasLength(2));
      expect(bodies.first, bodies.last);
    },
  );
  test(
    'group ACK delete fault retains exact pending request until explicit retry',
    () async {
      late ClientSession owner;
      var fail = true;
      final bodies = <Object?>[];
      Future<String> database() async =>
          '${owner.sessions.directory.path}/${await accountNamespace(bootstrap.account().origin, bootstrap.tenant, bootstrap.user)}.sqlite';
      owner = await nav.owner((r, _) async {
        bodies.add(jsonDecode(utf8.decode(r.body!)));
        if (fail) {
          final db = sqlite3.open(await database());
          try {
            db.execute(
              "CREATE TRIGGER reject_action_delete BEFORE DELETE ON records WHEN OLD.kind='action' BEGIN SELECT RAISE(ABORT,'owned-test'); END",
            );
          } finally {
            db.close();
          }
        }
        return oidc.response(created());
      });
      addTearDown(owner.logout);
      final (roles, _) = await sending.admission(owner);
      addTearDown(roles.close);
      final groups = GroupController(roles);
      addTearDown(groups.close);
      await expectLater(
        groups.create('故障恢复群', [membership.otherRole]),
        throwsA(isA<StoreFailure>()),
      );
      final saved = (await groups.pending())!;
      final db = sqlite3.open(await database());
      try {
        db.execute('DROP TRIGGER reject_action_delete');
      } finally {
        db.close();
      }
      fail = false;
      await groups.retry();
      expect(bodies.last, saved.body);
      expect(await groups.pending(), isNull);
    },
  );
  test(
    'closing group controller rejects late ACK and preserves its queued request',
    () async {
      final response = Completer<ApiResponse>(), entered = Completer<void>();
      final owner = await nav.owner((r, _) {
        entered.complete();
        return response.future;
      });
      addTearDown(owner.logout);
      final (roles, _) = await sending.admission(owner);
      addTearDown(roles.close);
      final groups = GroupController(roles);
      final request = groups.create('迟到结果群', [membership.otherRole]);
      final rejected = expectLater(request, throwsA(isA<ApiFailure>()));
      await entered.future;
      groups.close();
      response.complete(oidc.response(created()));
      await rejected;
      expect(await owner.current!.store.pendingAction(), isNotNull);
    },
  );
  test(
    'pending group operation blocks existing sender and a newly selected controller',
    () async {
      var posts = 0;
      final owner = await nav.owner((r, _) async {
        posts++;
        throw const ApiFailure('timeout');
      });
      addTearDown(owner.logout);
      final (roles, context) = await sending.admission(owner);
      addTearDown(roles.close);
      final messages = await sending.controller(roles, context);
      final groups = GroupController(roles);
      addTearDown(groups.close);
      await expectLater(
        groups.create('原操作群', [membership.otherRole]),
        throwsA(isA<ApiFailure>()),
      );
      await expectLater(
        messages.sendText('禁止跨操作发送'),
        throwsA(
          isA<ApiFailure>().having(
            (e) => e.code,
            'code',
            'operation_unconfirmed',
          ),
        ),
      );
      final reopened = ConversationController(roles);
      addTearDown(reopened.close);
      await expectLater(
        reopened.select(context, ConversationSummary.fromJson(nav.direct())),
        throwsA(isA<ApiFailure>()),
      );
      expect(posts, 1);
      expect(await owner.current!.store.pendingSends(), isEmpty);
    },
  );
  test(
    'invite remove transfer and policy recheck use exact intervals and immutable request IDs',
    () async {
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/policy-rechecks')) {
          expect(r.body, isNull);
          return oidc.response({
            'status': 'policy_blocked',
            'policy_version': 9007199254740993,
          });
        }
        final body = jsonDecode(utf8.decode(r.body!)) as Map;
        if (r.uri.path.endsWith('/invitations')) {
          expect(body['target_membership_id'], membership.otherRole);
          expect(body['client_request_id'], isNotEmpty);
          return oidc.response({
            'interval_id': fixture.interval,
            'join_seq': 3,
            'policy_version': 1,
          });
        }
        if (r.uri.path.endsWith('/removals'))
          return oidc.response({
            'interval_id': body['interval_id'],
            'status': 'removed',
            'leave_seq': 5,
          });
        return oidc.response({
          'source_interval_id': body['source_interval_id'],
          'target_interval_id': body['target_interval_id'],
        });
      });
      addTearDown(owner.logout);
      final (roles, _) = await sending.admission(owner);
      addTearDown(roles.close);
      final groups = GroupController(roles);
      addTearDown(groups.close);
      expect(
        (await groups.invite(
          nav.group,
          membership.otherRole,
        )).sequence.toString(),
        '3',
      );
      expect(
        (await groups.remove(nav.group, fixture.interval)).status,
        'removed',
      );
      await groups.transfer(nav.group, fixture.interval, nav.dm);
      expect((await groups.recheck(nav.group)).status, 'policy_blocked');
      expect(await groups.pending(), isNull);
    },
  );
}
