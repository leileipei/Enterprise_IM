import 'dart:async';
import 'dart:convert';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/auth/auth_session.dart';
import 'package:enterprise_im/core/auth/client_session.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/models.dart';
import 'package:enterprise_im/core/protocol/client_id.dart';
import 'package:enterprise_im/core/auth/session_store.dart';
import 'package:enterprise_im/core/storage/secure_vault.dart';
import 'package:enterprise_im/features/memberships/membership_controller.dart';
import 'auth_test.dart' as oidc;
import 'client_session_test.dart' as disk;
import 'session_bootstrap_test.dart' as bootstrap;

const role = '33333333-3333-4333-8333-333333333333';
const otherRole = '44444444-4444-4444-8444-444444444444';
const deviceID = '55555555-5555-4555-8555-555555555555';
const deviceToken = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA';
Map<String, Object?> membership(String id) => {
  'id': id,
  'organization_id': '66666666-6666-4666-8666-666666666666',
  'organization_name': '总部',
  'legal_entity_id': '77777777-7777-4777-8777-777777777777',
  'legal_entity_name': '集团公司',
  'title': '工程师',
  'is_primary': id == role,
};
Map<String, Object?> self() => {
  ...oidc.self,
  'memberships': [membership(role), membership(otherRole)],
};
Map<String, Object?> grant({bool renew = false}) => {
  'device_id': deviceID,
  if (!renew) 'device_token': deviceToken,
  'server_time': '2026-10-09T00:00:00Z',
  'expires_at': '2026-11-08T00:00:00Z',
};

void main() {
  test(
    'fresh account admission cannot consume previous owner membership data',
    () async {
      final (_, _, store) = await disk.fixture();
      var registrations = 0;
      final native = bootstrap.authentication(
        oidc.FakeTransport((r, _) async {
          if (r.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (r.uri.path.endsWith('/refresh'))
            return oidc.response(oidc.tokens('fresh-refresh'));
          if (r.uri.path == '/api/v1/me') return oidc.response(self());
          registrations++;
          return oidc.response(grant());
        }),
      );
      final owner = ClientSession(native, store, platform: 'macos');
      addTearDown(owner.logout);
      await owner.restore(bootstrap.account());
      final controller = MembershipController(owner);
      addTearDown(controller.close);
      await controller.load();
      await owner.refresh();
      expect(controller.memberships, isEmpty);
      await expectLater(controller.select(role), throwsA(isA<AuthFailure>()));
      expect(registrations, 0);
      expect(controller.current, isNull);
    },
  );

  test(
    'reopening controller cannot bypass the authenticated role boundary',
    () async {
      final (_, _, store) = await disk.fixture();
      final native = bootstrap.authentication(
        oidc.FakeTransport((r, _) async {
          if (r.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (r.uri.path.endsWith('/refresh'))
            return oidc.response(oidc.tokens('fresh-refresh'));
          if (r.uri.path == '/api/v1/me') return oidc.response(self());
          return oidc.response(
            r.uri.path.endsWith('/renew') ? grant(renew: true) : grant(),
          );
        }),
      );
      final owner = ClientSession(native, store, platform: 'macos');
      addTearDown(owner.logout);
      await owner.restore(bootstrap.account());
      final first = MembershipController(owner);
      await first.load();
      await first.select(role);
      first.close();
      final reopened = MembershipController(owner);
      addTearDown(reopened.close);
      await reopened.load();
      await expectLater(
        reopened.select(otherRole),
        throwsA(
          isA<AuthFailure>().having(
            (e) => e.code,
            'code',
            'membership_switch_requires_reauthentication',
          ),
        ),
      );
      expect(reopened.current, isNull);
      await reopened.select(role);
      expect(reopened.current!.membership.id, role);
    },
  );
  test(
    'renewal for another device or token never overwrites saved credential',
    () async {
      final (_, _, store) = await disk.fixture();
      var rotate = false;
      final native = bootstrap.authentication(
        oidc.FakeTransport((r, _) async {
          if (r.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (r.uri.path.endsWith('/refresh'))
            return oidc.response(oidc.tokens('fresh-refresh'));
          if (r.uri.path == '/api/v1/me') return oidc.response(self());
          return oidc.response(
            rotate ? grant() : {...grant(renew: true), 'device_id': otherRole},
          );
        }),
      );
      final owner = ClientSession(native, store, platform: 'macos');
      addTearDown(owner.logout);
      final client = await owner.restore(bootstrap.account());
      await store.saveCredentials(
        client.session,
        refreshToken: store.refreshToken,
        device: DeviceGrant.fromJson(grant()),
      );
      final controller = MembershipController(owner);
      addTearDown(controller.close);
      await controller.load();
      await expectLater(
        controller.select(role),
        throwsA(isA<FormatException>()),
      );
      expect(controller.current, isNull);
      expect(store.device!.deviceId, deviceID);
      rotate = true;
      await expectLater(
        controller.select(role),
        throwsA(isA<FormatException>()),
      );
      expect(controller.current, isNull);
      expect(store.device!.deviceToken, deviceToken);
    },
  );
  test(
    'fresh me supplies selectable roles without acting header and rejects forged principal',
    () async {
      final (_, _, store) = await disk.fixture();
      var wrong = false;
      final api = bootstrap.authentication(
        oidc.FakeTransport((r, _) async {
          if (r.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (r.uri.path.endsWith('/refresh'))
            return oidc.response(oidc.tokens('fresh-refresh'));
          expect(r.uri.path, '/api/v1/me');
          expect(r.headers.containsKey('X-Acting-Membership-ID'), isFalse);
          return oidc.response(
            wrong ? {...self(), 'tenant_id': bootstrap.user} : self(),
          );
        }),
      );
      final owner = ClientSession(api, store, platform: 'macos');
      addTearDown(owner.logout);
      await owner.restore(bootstrap.account());
      final controller = MembershipController(owner);
      addTearDown(controller.close);
      expect(controller.current, isNull);
      expect((await controller.load()).map((m) => m.id), [role, otherRole]);
      wrong = true;
      await expectLater(
        controller.load(),
        throwsA(
          isA<AuthFailure>().having((e) => e.code, 'code', 'identity_changed'),
        ),
      );
      expect(controller.memberships, isEmpty);
      expect(controller.current, isNull);
    },
  );
  test(
    'duplicate roles and invalid role selection never authorize a device',
    () async {
      final (_, _, store) = await disk.fixture();
      var duplicate = false, registrations = 0;
      final native = bootstrap.authentication(
        oidc.FakeTransport((r, _) async {
          if (r.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (r.uri.path.endsWith('/refresh'))
            return oidc.response(oidc.tokens('fresh-refresh'));
          if (r.uri.path == '/api/v1/me')
            return oidc.response(
              duplicate
                  ? {
                      ...self(),
                      'memberships': [membership(role), membership(role)],
                    }
                  : self(),
            );
          registrations++;
          return oidc.response(grant());
        }),
      );
      final owner = ClientSession(native, store, platform: 'macos');
      addTearDown(owner.logout);
      await owner.restore(bootstrap.account());
      final controller = MembershipController(owner);
      addTearDown(controller.close);
      await controller.load();
      await expectLater(
        controller.select(deviceID),
        throwsA(isA<AuthFailure>()),
      );
      expect(registrations, 0);
      duplicate = true;
      await expectLater(controller.load(), throwsA(isA<FormatException>()));
      expect(controller.memberships, isEmpty);
    },
  );
  test(
    'new registration is durable before network and admission follows vault save',
    () async {
      final (_, vault, store) = await disk.fixture();
      late ClientSession owner;
      var requests = 0;
      final native = bootstrap.authentication(
        oidc.FakeTransport((r, _) async {
          if (r.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (r.uri.path.endsWith('/refresh'))
            return oidc.response(oidc.tokens('fresh-refresh'));
          if (r.uri.path == '/api/v1/me') return oidc.response(self());
          expect(r.method, 'POST');
          expect(r.uri.path, '/api/v1/devices');
          expect(r.headers['X-Acting-Membership-ID'], role);
          final body = jsonDecode(utf8.decode(r.body!)) as Map;
          final pending = await owner.current!.store.pendingRegistration();
          expect(pending!.registrationId, body['registration_id']);
          expect(body['platform'], 'macos');
          expect(
            body['registration_id'],
            matches(RegExp(r'^[0-9a-f-]{14}7[0-9a-f-]{4}[89ab][0-9a-f-]{16}$')),
          );
          requests++;
          return oidc.response(grant());
        }),
      );
      owner = ClientSession(native, store, platform: 'macos');
      addTearDown(owner.logout);
      await owner.restore(bootstrap.account());
      final controller = MembershipController(owner);
      addTearDown(controller.close);
      await controller.load();
      final admitted = await controller.select(role);
      expect(admitted.membership.id, role);
      expect(admitted.device.deviceId, deviceID);
      expect(store.device!.deviceId, deviceID);
      expect(await owner.current!.store.pendingRegistration(), isNull);
      expect(vault.values.values.join(), isNot(contains('private-access')));
      await controller.select(role);
      expect(requests, 1);
      await expectLater(
        controller.select(otherRole),
        throwsA(isA<AuthFailure>()),
      );
      expect(controller.current, same(admitted));
    },
  );
  test('registration timeout and restart reuse original id and body', () async {
    final (_, vault, store) = await disk.fixture();
    final bodies = <String>[];
    var loseResponse = true;
    final transport = oidc.FakeTransport((r, _) async {
      if (r.uri.path.endsWith('/config')) return oidc.response(oidc.config());
      if (r.uri.path.endsWith('/refresh'))
        return oidc.response(oidc.tokens('fresh-refresh'));
      if (r.uri.path == '/api/v1/me') return oidc.response(self());
      bodies.add(utf8.decode(r.body!));
      if (loseResponse) throw const ApiFailure('timeout');
      return oidc.response(grant());
    });
    final owner = ClientSession(
      bootstrap.authentication(transport),
      store,
      platform: 'macos',
    );
    await owner.restore(bootstrap.account());
    final controller = MembershipController(owner);
    await controller.load();
    await expectLater(controller.select(role), throwsA(isA<ApiFailure>()));
    expect(controller.current, isNull);
    final pending = await owner.current!.store.pendingRegistration();
    expect(pending, isNotNull);
    await expectLater(
      controller.select(otherRole),
      throwsA(isA<AuthFailure>()),
    );
    expect(bodies.length, 1);
    controller.close();
    await owner.close();
    final restarted = ClientSession(
      bootstrap.authentication(transport),
      SessionStore(vault, directory: store.directory),
      platform: 'macos',
    );
    addTearDown(restarted.logout);
    await restarted.restore(bootstrap.account());
    final next = MembershipController(restarted);
    addTearDown(next.close);
    await next.load();
    loseResponse = false;
    await next.select(role);
    expect(bodies.length, 2);
    expect(bodies.last, bodies.first);
  });
  test(
    'restored device is renewed without token rotation before admission',
    () async {
      final (_, _, store) = await disk.fixture();
      var renews = 0;
      final native = bootstrap.authentication(
        oidc.FakeTransport((r, _) async {
          if (r.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (r.uri.path.endsWith('/refresh'))
            return oidc.response(oidc.tokens('fresh-refresh'));
          if (r.uri.path == '/api/v1/me') return oidc.response(self());
          expect(r.uri.path, '/api/v1/devices/$deviceID/renew');
          expect(r.headers['X-IM-Device-ID'], deviceID);
          expect(r.headers['X-IM-Device-Token'], deviceToken);
          expect(r.headers['X-Acting-Membership-ID'], role);
          renews++;
          return oidc.response(grant(renew: true));
        }),
      );
      final owner = ClientSession(native, store, platform: 'macos');
      addTearDown(owner.logout);
      final client = await owner.restore(bootstrap.account());
      await store.saveCredentials(
        client.session,
        refreshToken: store.refreshToken,
        device: DeviceGrant.fromJson(grant()),
      );
      final controller = MembershipController(owner);
      addTearDown(controller.close);
      await controller.load();
      expect(controller.current, isNull);
      final admitted = await controller.select(role);
      expect(admitted.device.deviceToken, deviceToken);
      expect(renews, 1);
    },
  );
  test('quota or revoked device is not replaced or falsely admitted', () async {
    final (_, _, store) = await disk.fixture();
    var code = 'device_quota', registrations = 0;
    final native = bootstrap.authentication(
      oidc.FakeTransport((r, _) async {
        if (r.uri.path.endsWith('/config')) return oidc.response(oidc.config());
        if (r.uri.path.endsWith('/refresh'))
          return oidc.response(oidc.tokens('fresh-refresh'));
        if (r.uri.path == '/api/v1/me') return oidc.response(self());
        if (r.uri.path == '/api/v1/devices') registrations++;
        return ApiResponse(
          code == 'device_quota' ? 409 : 401,
          utf8.encode(jsonEncode({'error_code': code})),
          {},
        );
      }),
    );
    final owner = ClientSession(native, store, platform: 'macos');
    addTearDown(owner.logout);
    final client = await owner.restore(bootstrap.account());
    final controller = MembershipController(owner);
    addTearDown(controller.close);
    await controller.load();
    await expectLater(
      controller.select(role),
      throwsA(isA<ApiFailure>().having((e) => e.code, 'code', 'device_quota')),
    );
    expect(controller.current, isNull);
    expect(store.device, isNull);
    await store.saveCredentials(
      client.session,
      refreshToken: store.refreshToken,
      device: DeviceGrant.fromJson(grant()),
    );
    code = 'invalid_device';
    await expectLater(
      controller.select(role),
      throwsA(
        isA<ApiFailure>().having((e) => e.code, 'code', 'invalid_device'),
      ),
    );
    expect(controller.current, isNull);
    expect(registrations, 1);
  });
  test(
    'vault failure keeps registration unresolved and cannot admit a device',
    () async {
      final (_, vault, store) = await disk.fixture();
      final native = bootstrap.authentication(
        oidc.FakeTransport((r, _) async {
          if (r.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (r.uri.path.endsWith('/refresh'))
            return oidc.response(oidc.tokens('fresh-refresh'));
          if (r.uri.path == '/api/v1/me') return oidc.response(self());
          vault.failWrites = true;
          return oidc.response(grant());
        }),
      );
      final owner = ClientSession(native, store, platform: 'macos');
      addTearDown(() async {
        vault.failWrites = false;
        await owner.logout();
      });
      await owner.restore(bootstrap.account());
      final controller = MembershipController(owner);
      addTearDown(controller.close);
      await controller.load();
      await expectLater(controller.select(role), throwsA(isA<VaultFailure>()));
      expect(controller.current, isNull);
      expect(store.device, isNull);
      expect(await owner.current!.store.pendingRegistration(), isNotNull);
    },
  );
  test(
    'logout while registration request is pending rejects late grant and erases store',
    () async {
      final (root, vault, store) = await disk.fixture();
      final entered = Completer<void>(), late = Completer<ApiResponse>();
      final native = bootstrap.authentication(
        oidc.FakeTransport((r, _) async {
          if (r.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (r.uri.path.endsWith('/refresh'))
            return oidc.response(oidc.tokens('fresh-refresh'));
          if (r.uri.path == '/api/v1/me') return oidc.response(self());
          entered.complete();
          return late.future;
        }),
      );
      final owner = ClientSession(native, store, platform: 'macos');
      addTearDown(owner.logout);
      await owner.restore(bootstrap.account());
      final controller = MembershipController(owner);
      addTearDown(controller.close);
      await controller.load();
      final selecting = controller.select(role);
      final checked = expectLater(selecting, throwsA(isA<AuthCancelled>()));
      await entered.future;
      await owner.logout();
      late.complete(oidc.response(grant()));
      await checked;
      expect(controller.current, isNull);
      expect(vault.values.isEmpty, isTrue);
      expect(root.listSync(), isEmpty);
    },
  );
  test(
    'server clock generates UUIDv7 without wall time and rejects monotonic rollback',
    () {
      var elapsed = const Duration(seconds: 2);
      final clock = ServerClock.start(
        DateTime.utc(2026, 10, 9),
        Duration.zero,
        now: () => elapsed,
      );
      final id = ClientMessageId.create(clock, (_) => 0);
      expect(id, '01a11df5-abd0-7000-8000-000000000000');
      elapsed = const Duration(seconds: 3);
      expect(clock.now, DateTime.utc(2026, 10, 9, 0, 0, 3));
      elapsed = const Duration(seconds: 1);
      expect(() => clock.now, throwsA(isA<FormatException>()));
    },
  );
}
