import 'dart:async';
import 'dart:convert';
import 'package:flutter_test/flutter_test.dart';
import 'package:sqlite3/sqlite3.dart';
import 'package:enterprise_im/core/storage/encrypted_store.dart';
import 'package:enterprise_im/core/storage/secure_vault.dart';
import 'package:enterprise_im/core/auth/client_session.dart';
import 'package:enterprise_im/core/auth/session_store.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/models.dart';
import 'package:enterprise_im/core/sync/client_context.dart';
import 'package:enterprise_im/features/memberships/membership_controller.dart';
import 'package:enterprise_im/features/conversations/conversation_controller.dart';
import 'auth_test.dart' as oidc;
import 'session_bootstrap_test.dart' as bootstrap;
import 'membership_admission_test.dart' as fixture;
import 'navigation_test.dart' as nav;

Map<String, Object?> ack() => {
  'message_id': 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
  'conversation_id': nav.dm,
  'seq': 9007199254740993,
  'server_time': '2026-10-09T00:00:00Z',
  'duplicate': false,
};
Future<(MembershipController, ClientContext)> admission(
  ClientSession owner,
) async {
  final roles = MembershipController(owner);
  await roles.load();
  final ready = await roles.select(fixture.role);
  return (
    roles,
    ClientContext(
      session: ready.client.session,
      actingMembershipId: fixture.role,
      device: ready.device,
      generation: ready.client.session.generation,
      conversationId: nav.dm,
    ),
  );
}

Future<ConversationController> controller(
  MembershipController roles,
  ClientContext context,
) async {
  final value = ConversationController(roles);
  addTearDown(value.close);
  await value.select(context, ConversationSummary.fromJson(nav.direct()));
  return value;
}

void main() {
  test(
    'failed new attempt cannot expose the previous message ACK as success',
    () async {
      var deny = false;
      final owner = await nav.owner((r, _) async {
        if (deny)
          return ApiResponse(
            403,
            utf8.encode('{"error_code":"policy_denied"}'),
            {},
          );
        return oidc.response(ack());
      });
      addTearDown(owner.logout);
      final (roles, context) = await admission(owner);
      addTearDown(roles.close);
      final value = await controller(roles, context);
      await value.sendText('第一条成功');
      expect(value.lastACK, isNotNull);
      deny = true;
      await expectLater(value.sendText('第二条拒绝'), throwsA(isA<ApiFailure>()));
      expect(value.lastACK, isNull);
      expect((await owner.current!.store.pendingSends()).single.text, '第二条拒绝');
    },
  );

  test(
    'ACK transaction failure preserves pending and rolls back confirmation',
    () async {
      late ClientSession owner;
      var fail = true;
      Future<String> database() async =>
          '${owner.sessions.directory.path}/${await accountNamespace(bootstrap.account().origin, bootstrap.tenant, bootstrap.user)}.sqlite';
      owner = await nav.owner((r, _) async {
        if (fail) {
          final db = sqlite3.open(await database());
          try {
            db.execute(
              "CREATE TRIGGER reject_pending_delete BEFORE DELETE ON records WHEN OLD.kind='pending_send' BEGIN SELECT RAISE(ABORT,'owned-test'); END",
            );
          } finally {
            db.close();
          }
        }
        return oidc.response(ack());
      });
      addTearDown(owner.logout);
      final (roles, context) = await admission(owner);
      addTearDown(roles.close);
      final value = await controller(roles, context);
      await expectLater(value.sendText('原子确认正文'), throwsA(isA<StoreFailure>()));
      final pending = (await owner.current!.store.pendingSends()).single;
      expect(value.lastACK, isNull);
      expect(
        await owner.current!.store.sendConfirmation(pending.clientMsgId),
        isNull,
      );
      final db = sqlite3.open(await database());
      try {
        db.execute('DROP TRIGGER reject_pending_delete');
      } finally {
        db.close();
      }
      fail = false;
      final result = await value.retryPending(pending.clientMsgId);
      expect(result.conversationId, nav.dm);
      expect(await owner.current!.store.pendingSends(), isEmpty);
    },
  );

  test('testSendTimeoutAndRestartReuseOriginalClientMsgId', () async {
    final bodies = <String>[];
    var timeout = true;
    Future<ApiResponse> send(ApiRequest r, Cancellation c) async {
      if (r.uri.path.endsWith('/renew'))
        return oidc.response(fixture.grant(renew: true));
      expect(r.uri.path, '/api/v1/conversations/${nav.dm}/messages');
      final body = utf8.decode(r.body!);
      bodies.add(body);
      expect(r.headers['X-Acting-Membership-ID'], fixture.role);
      if (timeout) throw const ApiFailure('timeout');
      return oidc.response(ack());
    }

    final owner = await nav.owner(send);
    await owner.close();
    await owner.restore(bootstrap.account());
    final (roles, context) = await admission(owner);
    final first = await controller(roles, context);
    await expectLater(first.sendText('原始正文'), throwsA(isA<ApiFailure>()));
    final pending = (await owner.current!.store.pendingSends()).single;
    expect(pending.text, '原始正文');
    expect(first.lastACK, isNull);
    first.close();
    roles.close();
    await owner.close();
    final transport = oidc.FakeTransport((r, c) async {
      if (r.uri.path.endsWith('/config')) return oidc.response(oidc.config());
      if (r.uri.path.endsWith('/refresh'))
        return oidc.response(oidc.tokens('fresh-refresh'));
      if (r.uri.path == '/api/v1/me') return oidc.response(fixture.self());
      return send(r, c);
    });
    final restarted = ClientSession(
      bootstrap.authentication(transport),
      SessionStore(owner.sessions.vault, directory: owner.sessions.directory),
      platform: 'macos',
    );
    addTearDown(restarted.logout);
    await restarted.restore(bootstrap.account());
    final (nextRoles, nextContext) = await admission(restarted);
    addTearDown(nextRoles.close);
    expect(pending.generation, greaterThan(nextContext.generation));
    final next = await controller(nextRoles, nextContext);
    timeout = false;
    final result = await next.retryPending(pending.clientMsgId);
    expect(result.seq.toString(), '9007199254740993');
    expect(next.lastACK, same(result));
    expect(bodies.length, 2);
    expect(bodies.last, bodies.first);
    expect(await restarted.current!.store.pendingSends(), isEmpty);
    final confirmed = await next.retryPending(pending.clientMsgId);
    expect(confirmed.messageId, result.messageId);
    expect(bodies.length, 2);
  });
  test('testAccountOrMembershipSwitchBlocksPendingSend', () async {
    var requests = 0;
    final owner = await nav.owner((r, _) async {
      requests++;
      throw const ApiFailure('timeout');
    });
    addTearDown(owner.logout);
    final (roles, context) = await admission(owner);
    addTearDown(roles.close);
    final value = await controller(roles, context);
    await expectLater(value.sendText('待确认'), throwsA(isA<ApiFailure>()));
    final pending = (await owner.current!.store.pendingSends()).single;
    final forged = ClientContext(
      session: context.session,
      actingMembershipId: fixture.otherRole,
      device: context.device,
      generation: context.generation,
      conversationId: nav.dm,
    );
    await expectLater(
      value.select(forged, ConversationSummary.fromJson(nav.direct())),
      throwsA(isA<ApiFailure>()),
    );
    await owner.refresh();
    await expectLater(
      value.retryPending(pending.clientMsgId),
      throwsA(isA<ApiFailure>()),
    );
    expect(requests, 1);
    expect(
      (await owner.current!.store.pendingSends()).single.clientMsgId,
      pending.clientMsgId,
    );
  });
  test(
    'unresolved send blocks replacement body until explicit abandonment',
    () async {
      var requests = 0;
      final owner = await nav.owner((r, _) async {
        requests++;
        throw const ApiFailure('timeout');
      });
      addTearDown(owner.logout);
      final (roles, context) = await admission(owner);
      addTearDown(roles.close);
      final value = await controller(roles, context);
      await expectLater(value.sendText('第一条'), throwsA(isA<ApiFailure>()));
      final id = (await owner.current!.store.pendingSends()).single.clientMsgId;
      await expectLater(
        value.sendText('改了正文'),
        throwsA(
          isA<ApiFailure>().having((e) => e.code, 'code', 'send_unconfirmed'),
        ),
      );
      expect(requests, 1);
      await value.abandonPending(id);
      expect(await owner.current!.store.pendingSends(), isEmpty);
      await expectLater(value.sendText('新消息'), throwsA(isA<ApiFailure>()));
      expect(requests, 2);
      expect(
        (await owner.current!.store.pendingSends()).single.clientMsgId,
        isNot(id),
      );
    },
  );
  test(
    'wrong conversation ACK and expired id never imply successful send',
    () async {
      var expire = false;
      final owner = await nav.owner((r, _) async {
        if (expire)
          return ApiResponse(
            410,
            utf8.encode('{"error_code":"client_msg_id_expired"}'),
            {},
          );
        return oidc.response({...ack(), 'conversation_id': nav.group});
      });
      addTearDown(owner.logout);
      final (roles, context) = await admission(owner);
      addTearDown(roles.close);
      final value = await controller(roles, context);
      await expectLater(value.sendText('不能假成功'), throwsA(isA<ApiFailure>()));
      final pending = (await owner.current!.store.pendingSends()).single;
      expect(value.lastACK, isNull);
      expire = true;
      await expectLater(
        value.retryPending(pending.clientMsgId),
        throwsA(isA<ApiFailure>().having((e) => e.status, 'status', 410)),
      );
      expect(value.lastACK, isNull);
      expect(
        (await owner.current!.store.pendingSends()).single.clientMsgId,
        pending.clientMsgId,
      );
    },
  );
  test(
    'stopping while request is pending rejects late ACK and retains original send',
    () async {
      final entered = Completer<void>(), late = Completer<ApiResponse>();
      final owner = await nav.owner((r, _) async {
        entered.complete();
        return late.future;
      });
      addTearDown(owner.logout);
      final (roles, context) = await admission(owner);
      addTearDown(roles.close);
      final value = await controller(roles, context);
      final sending = value.sendText('不能迟到确认');
      final checked = expectLater(sending, throwsA(isA<ApiFailure>()));
      await entered.future;
      value.close();
      late.complete(oidc.response(ack()));
      await checked;
      expect(value.lastACK, isNull);
      expect((await owner.current!.store.pendingSends()).single.text, '不能迟到确认');
    },
  );
}
