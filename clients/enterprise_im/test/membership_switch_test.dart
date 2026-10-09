import 'dart:async';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/auth/client_session.dart';
import 'package:enterprise_im/core/auth/native_auth.dart';
import 'client_session_test.dart' as disk;
import 'session_bootstrap_test.dart' as bootstrap;
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/features/groups/group_controller.dart';
import 'package:enterprise_im/features/memberships/membership_controller.dart';
import 'auth_test.dart' as oidc;
import 'navigation_test.dart' as nav;
import 'message_send_test.dart' as sending;
import 'membership_admission_test.dart' as fixture;

void main() {
  test(
    'browser reauthentication for role switch rejects a different principal',
    () async {
      final (_, _, store) = await disk.fixture();
      var different = false;
      late oidc.FakeCallback callback;
      final auth = NativeAuth(
        ApiClient(
          bootstrap.account().origin,
          oidc.FakeTransport((r, _) async {
            if (r.uri.path.endsWith('/config'))
              return oidc.response({
                ...oidc.config(),
                'refresh_enabled': false,
              });
            if (r.uri.path.endsWith('/token'))
              return oidc.response({...oidc.tokens(''), 'refresh_token': null});
            if (r.uri.path == '/api/v1/me')
              return oidc.response({
                ...fixture.self(),
                if (different) 'user_id': bootstrap.tenant,
              });
            return oidc.response(fixture.grant());
          }),
        ),
        callbackFactory: (_, __) => callback = oidc.FakeCallback(),
        browser: oidc.FakeBrowser((uri) async {
          callback.result.complete(
            Uri.parse('http://127.0.0.1:49152/oauth/callback').replace(
              queryParameters: {
                'state': uri.queryParameters['state']!,
                'code': 'owned-code',
              },
            ),
          );
          return true;
        }),
      );
      final owner = ClientSession(auth, store, platform: 'macos');
      addTearDown(owner.logout);
      await owner.signIn();
      final (roles, _) = await sending.admission(owner);
      addTearDown(roles.close);
      expect(auth.canRefresh, isFalse);
      different = true;
      await expectLater(
        owner.prepareMembershipSwitch(),
        throwsA(
          isA<AuthFailure>().having((e) => e.code, 'code', 'identity_changed'),
        ),
      );
      expect(owner.current, isNull);
      expect(roles.current, isNull);
    },
  );

  test(
    'membership switch reauthenticates with new generation/store and freshly renews device',
    () async {
      var renews = 0;
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/renew')) {
          renews++;
          return oidc.response(fixture.grant(renew: true));
        }
        throw const ApiFailure('unexpected');
      });
      addTearDown(owner.logout);
      final (old, context) = await sending.admission(owner);
      addTearDown(old.close);
      final previous = owner.current!, oldStore = previous.store;
      final fresh = await owner.prepareMembershipSwitch();
      expect(fresh.session.generation, greaterThan(context.generation));
      expect(fresh.store, isNot(same(oldStore)));
      expect(old.current, isNull);
      final next = MembershipController(owner);
      addTearDown(next.close);
      await next.load();
      final ready = await next.select(fixture.otherRole);
      expect(ready.membership.id, fixture.otherRole);
      expect(ready.device.deviceId, context.device.deviceId);
      expect(renews, 1);
    },
  );
  test(
    'pending message or uncertain group operation blocks reauthentication and preserves old owner',
    () async {
      final owner = await nav.owner(
        (r, _) async => throw const ApiFailure('timeout'),
      );
      addTearDown(owner.logout);
      final (roles, context) = await sending.admission(owner);
      addTearDown(roles.close);
      final messages = await sending.controller(roles, context);
      await expectLater(messages.sendText('待确认'), throwsA(isA<ApiFailure>()));
      final old = owner.current;
      await expectLater(
        owner.prepareMembershipSwitch(),
        throwsA(
          isA<AuthFailure>().having((e) => e.code, 'code', 'send_unconfirmed'),
        ),
      );
      expect(owner.current, same(old));
      await messages.abandonPending(
        (await owner.current!.store.pendingSends()).single.clientMsgId,
      );
      final groups = GroupController(roles);
      addTearDown(groups.close);
      await expectLater(
        groups.create('原群', [fixture.otherRole]),
        throwsA(isA<ApiFailure>()),
      );
      await expectLater(
        owner.prepareMembershipSwitch(),
        throwsA(
          isA<AuthFailure>().having(
            (e) => e.code,
            'code',
            'operation_unconfirmed',
          ),
        ),
      );
      expect(owner.current, same(old));
    },
  );
  test(
    'logout during switch rejects late renewed authentication and never restores old owner',
    () async {
      final late = Completer<ApiResponse>(), entered = Completer<void>();
      var switching = false;
      final (_, _, store) = await disk.fixture();
      final transport = oidc.FakeTransport((r, c) async {
        if (r.uri.path.endsWith('/config')) return oidc.response(oidc.config());
        if (r.uri.path.endsWith('/refresh')) {
          if (switching) {
            entered.complete();
            return late.future;
          }
          return oidc.response(oidc.tokens('fresh-refresh'));
        }
        if (r.uri.path == '/api/v1/me') return oidc.response(fixture.self());
        return oidc.response(fixture.grant());
      });
      final owner = ClientSession(
        bootstrap.authentication(transport),
        store,
        platform: 'macos',
      );
      addTearDown(owner.logout);
      await owner.restore(bootstrap.account());
      final (roles, _) = await sending.admission(owner);
      addTearDown(roles.close);
      switching = true;
      final request = owner.prepareMembershipSwitch();
      final rejected = expectLater(request, throwsA(isA<AuthFailure>()));
      await entered.future;
      await owner.logout();
      late.complete(oidc.response(oidc.tokens('late-refresh')));
      await rejected;
      expect(owner.current, isNull);
    },
  );
}
