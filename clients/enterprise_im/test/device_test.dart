import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:sqlite3/sqlite3.dart';
import 'package:enterprise_im/core/storage/secure_vault.dart';
import 'package:enterprise_im/core/storage/encrypted_store.dart';
import 'package:enterprise_im/features/memberships/membership_controller.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/storage/records.dart';
import 'package:enterprise_im/features/devices/device_controller.dart';
import 'auth_test.dart' as oidc;
import 'package:enterprise_im/core/auth/client_session.dart';
import 'package:enterprise_im/core/auth/auth_session.dart';
import 'session_bootstrap_test.dart' as bootstrap;
import 'client_session_test.dart' as disk;
import 'message_send_test.dart' as sending;
import 'membership_admission_test.dart' as member;

const other = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const registration = '019a3e82-5c00-7000-8000-000000000001';
Map<String, Object?> device({
  String id = other,
  String reg = registration,
  String status = 'active',
}) => {
  'device_id': id,
  'registration_id': reg,
  'platform': 'macos',
  'name': '研发笔记本',
  'status': status,
  'created_at': '2026-10-09T00:00:00Z',
  'expires_at': '2026-11-08T00:00:00Z',
  if (status == 'revoked') 'revoked_at': '2026-10-09T00:05:00Z',
};
Map<String, Object?> policy() => {
  'version': '1',
  'device_lease_days': 30,
  'max_active_devices': 3,
  'offline_cache_seconds': 300,
  'group_receipt_visibility': 'owner_admin',
  'receipts_enabled': true,
  'server_time': '2026-10-09T00:00:00Z',
};
Future<ClientSession> deviceOwner(
  Future<ApiResponse> Function(ApiRequest, Cancellation) handler,
) async {
  final (_, _, store) = await disk.fixture();
  final auth = bootstrap.authentication(
    oidc.FakeTransport((r, c) async {
      if (r.uri.path.endsWith('/config')) return oidc.response(oidc.config());
      if (r.uri.path.endsWith('/refresh'))
        return oidc.response(oidc.tokens('fresh-refresh'));
      if (r.uri.path == '/api/v1/me') return oidc.response(member.self());
      if (r.uri.path == '/api/v1/devices' && r.method == 'POST')
        return oidc.response(member.grant());
      return handler(r, c);
    }),
  );
  final client = ClientSession(auth, store, platform: 'macos');
  await client.restore(bootstrap.account());
  return client;
}

void main() {
  test(
    'revoke survives account reopen with encrypted immutable original target and prevents admission',
    () async {
      var timeout = true;
      final deletes = <String>[];
      final owner = await deviceOwner((r, _) async {
        deletes.add(r.uri.path);
        if (timeout) throw const ApiFailure('timeout');
        return const ApiResponse(204, [], {});
      });
      addTearDown(owner.logout);
      final first = DeviceController(owner);
      await expectLater(first.revoke(other), throwsA(isA<ApiFailure>()));
      final original = (await first.pending())!.toJson();
      await expectLater(
        owner.current!.store.putPendingDeviceRevocation(
          PendingDeviceRevocation(deviceId: member.deviceID, wasCurrent: false),
        ),
        throwsA(isA<StoreFailure>()),
      );
      for (final file
          in owner.sessions.directory.listSync().whereType<File>()) {
        final raw = latin1.decode(await file.readAsBytes());
        expect(raw, isNot(contains(other)));
      }
      first.close();
      await owner.close();
      await owner.restore(bootstrap.account());
      final roles = MembershipController(owner);
      addTearDown(roles.close);
      await roles.load();
      await expectLater(roles.select(member.role), throwsA(isA<AuthFailure>()));
      expect(roles.current, isNull);
      final second = DeviceController(owner);
      addTearDown(second.close);
      expect((await second.pending())!.toJson(), original);
      timeout = false;
      await second.retry();
      expect(await second.pending(), isNull);
      expect(deletes, ['/api/v1/devices/$other', '/api/v1/devices/$other']);
    },
  );
  test(
    'orphan cleanup delete fault retains revoke record across partially completed local cleanup',
    () async {
      late ClientSession owner;
      var fail = true;
      Future<String> database() async =>
          '${owner.sessions.directory.path}/${await accountNamespace(bootstrap.account().origin, bootstrap.tenant, bootstrap.user)}.sqlite';
      final deletes = <String>[];
      owner = await deviceOwner((r, _) async {
        if (r.method == 'GET')
          return oidc.response({
            'devices': [device()],
            'has_more': false,
          });
        deletes.add(r.uri.path);
        if (fail) {
          final db = sqlite3.open(await database());
          try {
            db.execute(
              "CREATE TRIGGER reject_revoke_delete BEFORE DELETE ON records WHEN OLD.kind='device_revocation' BEGIN SELECT RAISE(ABORT,'owned-test'); END",
            );
          } finally {
            db.close();
          }
        }
        return const ApiResponse(204, [], {});
      });
      addTearDown(owner.logout);
      await owner.current!.store.putPendingRegistration(
        PendingRegistration(
          registrationId: registration,
          actingMembershipId: member.role,
          platform: 'macos',
          name: '本机',
        ),
      );
      final first = DeviceController(owner);
      final page = await first.list();
      await expectLater(
        first.revokeOrphan(page.devices.single),
        throwsA(isA<StoreFailure>()),
      );
      final pending = (await first.pending())!.toJson();
      expect(await first.pendingRegistration(), isNull);
      first.close();
      await owner.close();
      await owner.restore(bootstrap.account());
      final db = sqlite3.open(await database());
      try {
        db.execute('DROP TRIGGER reject_revoke_delete');
      } finally {
        db.close();
      }
      fail = false;
      final second = DeviceController(owner);
      addTearDown(second.close);
      expect((await second.pending())!.toJson(), pending);
      await second.retry();
      expect(await second.pending(), isNull);
      expect(deletes, ['/api/v1/devices/$other', '/api/v1/devices/$other']);
    },
  );
  test(
    'pagination rejects server cursor loops and duplicate identities across pages',
    () async {
      var phase = 0;
      final owner = await deviceOwner(
        (r, _) async => oidc.response({
          'devices': [device()],
          'has_more': phase++ == 0,
          if (phase == 1) 'next_cursor': 'next',
        }),
      );
      addTearDown(owner.logout);
      final controller = DeviceController(owner);
      addTearDown(controller.close);
      await controller.list();
      await expectLater(
        controller.list(cursor: 'next'),
        throwsA(isA<FormatException>()),
      );
      expect(
        () => DevicePageResult.fromJson({
          'devices': [],
          'has_more': true,
          'next_cursor': 'same',
        }, 'same'),
        throwsA(isA<FormatException>()),
      );
    },
  );

  test(
    'listed external revocation immediately withdraws own admission and cleans account',
    () async {
      final owner = await deviceOwner(
        (r, _) async => oidc.response({
          'devices': [device(id: member.deviceID, status: 'revoked')],
          'has_more': false,
        }),
      );
      addTearDown(owner.logout);
      final (roles, _) = await sending.admission(owner);
      addTearDown(roles.close);
      final controller = DeviceController(owner, memberships: roles);
      addTearDown(controller.close);
      await controller.list();
      expect(roles.current, isNull);
      expect(owner.current, isNull);
      expect(owner.sessions.directory.listSync(), isEmpty);
    },
  );
  test('orphan mapping must come from this controller server list', () async {
    var deletes = 0;
    final owner = await deviceOwner((r, _) async {
      deletes++;
      return const ApiResponse(204, [], {});
    });
    addTearDown(owner.logout);
    await owner.current!.store.putPendingRegistration(
      PendingRegistration(
        registrationId: registration,
        actingMembershipId: member.role,
        platform: 'macos',
        name: '本机',
      ),
    );
    final controller = DeviceController(owner);
    addTearDown(controller.close);
    await expectLater(
      controller.revokeOrphan(OwnDevice.fromJson(device())),
      throwsA(isA<ApiFailure>()),
    );
    expect(deletes, 0);
    expect(await controller.pending(), isNull);
  });
  for (final response in [
    const ApiResponse(204, [123, 125], {}),
    const ApiResponse(200, [123, 125], {}),
    ApiResponse(404, utf8.encode(jsonEncode({'error_code': 'not_found'})), {}),
  ]) {
    test(
      'non exact revoke confirmation ${response.status} retains original operation',
      () async {
        final owner = await deviceOwner((r, _) async => response);
        addTearDown(owner.logout);
        final controller = DeviceController(owner);
        addTearDown(controller.close);
        await expectLater(controller.revoke(other), throwsA(isA<ApiFailure>()));
        expect((await controller.pending())!.deviceId, other);
        await expectLater(
          owner.prepareMembershipSwitch(),
          throwsA(isA<AuthFailure>()),
        );
      },
    );
  }

  test(
    'account device list needs no admitted device and uses configured quota',
    () async {
      final owner = await deviceOwner((r, _) async {
        expect(r.headers.containsKey('X-IM-Device-ID'), isFalse);
        if (r.uri.path.endsWith('/client-capabilities')) {
          expect(r.headers['X-Acting-Membership-ID'], member.role);
          return oidc.response(policy());
        }
        expect(r.headers.containsKey('X-Acting-Membership-ID'), isFalse);
        return oidc.response({
          'devices': [device()],
          'has_more': true,
          'next_cursor': 'page2',
        });
      });
      addTearDown(owner.logout);
      final controller = DeviceController(owner);
      addTearDown(controller.close);
      expect((await controller.list()).devices.single.name, '研发笔记本');
      expect((await controller.capabilities(member.role)).maxActiveDevices, 3);
      expect(owner.sessions.device, isNull);
    },
  );
  test('device pages reject duplicate identities', () async {
    final owner = await deviceOwner(
      (r, _) async => oidc.response({
        'devices': [device(), device()],
        'has_more': false,
      }),
    );
    addTearDown(owner.logout);
    final controller = DeviceController(owner);
    addTearDown(controller.close);
    await expectLater(controller.list(), throwsA(isA<FormatException>()));
  });
  test(
    'revoke timeout retains immutable target and only explicit retry accepts exact 204',
    () async {
      var timeout = true;
      final deletes = <String>[];
      final owner = await deviceOwner((r, _) async {
        deletes.add(r.uri.path);
        expect(r.method, 'DELETE');
        expect(r.body, isNull);
        if (timeout) throw const ApiFailure('timeout');
        return const ApiResponse(204, [], {});
      });
      addTearDown(owner.logout);
      final controller = DeviceController(owner);
      addTearDown(controller.close);
      await expectLater(controller.revoke(other), throwsA(isA<ApiFailure>()));
      expect((await controller.pending())!.deviceId, other);
      await expectLater(
        controller.revoke(member.deviceID),
        throwsA(isA<ApiFailure>()),
      );
      timeout = false;
      expect((await controller.retry()).stoppedOwnClient, isFalse);
      expect(deletes, ['/api/v1/devices/$other', '/api/v1/devices/$other']);
      expect(await controller.pending(), isNull);
      expect(owner.current, isNotNull);
    },
  );
  test(
    'current device revocation freezes reads immediately and erases own session after confirmed response',
    () async {
      final late = Completer<ApiResponse>(), entered = Completer<void>();
      final owner = await deviceOwner((r, _) {
        entered.complete();
        return late.future;
      });
      addTearDown(owner.logout);
      final (roles, _) = await sending.admission(owner);
      addTearDown(roles.close);
      final controller = DeviceController(owner, memberships: roles);
      addTearDown(controller.close);
      final request = controller.revoke(member.deviceID);
      await entered.future;
      expect(roles.current, isNull);
      expect(owner.current, isNotNull);
      late.complete(const ApiResponse(204, [], {}));
      expect((await request).stoppedOwnClient, isTrue);
      expect(owner.current, isNull);
      expect(owner.sessions.directory.listSync(), isEmpty);
    },
  );
  test(
    'orphan registration clears original record only after exact matching device is revoked',
    () async {
      var reject = true;
      final owner = await deviceOwner((r, _) async {
        if (r.method == 'DELETE') {
          if (reject) throw const ApiFailure('timeout');
          return const ApiResponse(204, [], {});
        }
        return oidc.response({
          'devices': [device()],
          'has_more': false,
        });
      });
      addTearDown(owner.logout);
      await owner.current!.store.putPendingRegistration(
        PendingRegistration(
          registrationId: registration,
          actingMembershipId: member.role,
          platform: 'macos',
          name: '本机',
        ),
      );
      final controller = DeviceController(owner);
      addTearDown(controller.close);
      final page = await controller.list();
      await expectLater(
        controller.revokeOrphan(page.devices.single),
        throwsA(isA<ApiFailure>()),
      );
      expect(await owner.current!.store.pendingRegistration(), isNotNull);
      reject = false;
      await controller.retry();
      expect(await owner.current!.store.pendingRegistration(), isNull);
    },
  );
  test(
    'closing controller rejects late device metadata and late revoke ACK',
    () async {
      final late = Completer<ApiResponse>(), entered = Completer<void>();
      final owner = await deviceOwner((r, _) {
        entered.complete();
        return late.future;
      });
      addTearDown(owner.logout);
      final controller = DeviceController(owner);
      final request = controller.revoke(other);
      final rejected = expectLater(request, throwsA(isA<ApiFailure>()));
      await entered.future;
      controller.close();
      late.complete(const ApiResponse(204, [], {}));
      await rejected;
      expect(await owner.current!.store.pendingDeviceRevocation(), isNotNull);
    },
  );
  test(
    'renewal publishes freshly verified device only after vault save and old admission is withdrawn',
    () async {
      final owner = await deviceOwner(
        (r, _) async => oidc.response(member.grant(renew: true)),
      );
      addTearDown(owner.logout);
      final (roles, _) = await sending.admission(owner);
      addTearDown(roles.close);
      final old = roles.current;
      final controller = DeviceController(owner, memberships: roles);
      addTearDown(controller.close);
      await controller.renew();
      expect(roles.current, isNot(same(old)));
      expect(roles.current!.device.deviceId, member.deviceID);
    },
  );
}
