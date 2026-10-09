import 'dart:convert';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'auth_test.dart' as oidc;
import 'device_test.dart' as devices;
import 'membership_admission_test.dart' as member;
import 'login_startup_test.dart' as startup;
import 'login_session_test.dart' show pumpUntil;

Future<void> enterDevicePage(WidgetTester tester) async {
  await pumpUntil(tester, () => find.text('进入工作空间').evaluate().isNotEmpty);
  await tester.tap(find.text('进入工作空间'));
  await tester.pumpAndSettle();
  await pumpUntil(
    tester,
    () => find
        .byKey(const ValueKey('membership-${member.role}'))
        .evaluate()
        .isNotEmpty,
  );
  await tester.tap(find.byKey(const ValueKey('membership-${member.role}')));
  await pumpUntil(
    tester,
    () =>
        find.text('通讯录').evaluate().isNotEmpty &&
        find.byType(LinearProgressIndicator).evaluate().isEmpty,
  );
  await tester.tap(find.text('设备管理'));
  await tester.pump();
}

void main() {
  testWidgets(
    'unconfirmed own revoke remains frozen after navigation until explicit original retry',
    (tester) async {
      final (root, _, store) = (await tester.runAsync(startup.seed))!;
      startup.cleanup(tester, root, store);
      var reject = true;
      final deleted = <String>[];
      final transport = oidc.FakeTransport((r, _) async {
        if (r.uri.path.endsWith('/config')) return oidc.response(oidc.config());
        if (r.uri.path.endsWith('/refresh'))
          return oidc.response(oidc.tokens('fresh-refresh'));
        if (r.uri.path == '/api/v1/me') return oidc.response(member.self());
        if (r.uri.path.endsWith('/client-capabilities'))
          return oidc.response(devices.policy());
        if (r.uri.path == '/api/v1/devices')
          return oidc.response(
            r.method == 'POST'
                ? member.grant()
                : {
                    'devices': [devices.device(id: member.deviceID)],
                    'has_more': false,
                  },
          );
        if (r.method == 'DELETE') {
          deleted.add(r.uri.path);
          if (reject) throw const ApiFailure('timeout');
          return const ApiResponse(204, [], {});
        }
        return oidc.response({'conversations': [], 'has_more': false});
      });
      await tester.pumpWidget(
        MaterialApp(home: startup.login(store, transport)),
      );
      await enterDevicePage(tester);
      await pumpUntil(
        tester,
        () =>
            find.text('本机').evaluate().isNotEmpty &&
            find.byType(LinearProgressIndicator).evaluate().isEmpty,
      );
      await tester.pumpAndSettle();
      await tester.tap(
        find.byKey(const ValueKey('revoke-device-${member.deviceID}')),
      );
      await tester.pumpAndSettle();
      await tester.tap(find.text('确认撤销'));
      await pumpUntil(
        tester,
        () =>
            find.text('重试原撤销').evaluate().isNotEmpty &&
            find.byType(LinearProgressIndicator).evaluate().isEmpty,
      );
      await tester.tap(find.byTooltip('返回工作空间'));
      await tester.pumpAndSettle();
      expect(find.text('通讯录'), findsNothing);
      await tester.tap(find.byKey(const ValueKey('membership-${member.role}')));
      await pumpUntil(
        tester,
        () => find.textContaining('设备撤销尚未确认').evaluate().isNotEmpty,
      );
      expect(deleted, hasLength(1));
      await tester.tap(find.text('设备管理'));
      await tester.pump();
      await pumpUntil(
        tester,
        () =>
            find.text('重试原撤销').evaluate().isNotEmpty &&
            find.byType(LinearProgressIndicator).evaluate().isEmpty,
      );
      await tester.pumpAndSettle();
      expect(deleted, hasLength(1));
      reject = false;
      await tester.tap(find.text('重试原撤销'));
      await pumpUntil(
        tester,
        () => find.text('登录集团工作空间').evaluate().isNotEmpty,
      );
      await tester.pumpAndSettle();
      expect(deleted, [
        '/api/v1/devices/${member.deviceID}',
        '/api/v1/devices/${member.deviceID}',
      ]);
      expect(root.listSync(), isEmpty);
    },
  );
  for (final cleanupFailure in [false, true]) {
    testWidgets(
      cleanupFailure
          ? 'confirmed own revoke with vault failure offers explicit logout cleanup retry'
          : 'server list confirms external own revoke and returns login',
      (tester) async {
        final (root, vault, store) = (await tester.runAsync(startup.seed))!;
        startup.cleanup(tester, root, store);
        var deletes = 0;
        final transport = oidc.FakeTransport((r, _) async {
          if (r.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (r.uri.path.endsWith('/refresh'))
            return oidc.response(oidc.tokens('fresh-refresh'));
          if (r.uri.path == '/api/v1/me') return oidc.response(member.self());
          if (r.uri.path.endsWith('/client-capabilities'))
            return oidc.response(devices.policy());
          if (r.uri.path == '/api/v1/devices')
            return oidc.response(
              r.method == 'POST'
                  ? member.grant()
                  : {
                      'devices': [
                        devices.device(
                          id: member.deviceID,
                          status: cleanupFailure ? 'active' : 'revoked',
                        ),
                      ],
                      'has_more': false,
                    },
            );
          if (r.method == 'DELETE') {
            deletes++;
            vault.unavailable = true;
            return const ApiResponse(204, [], {});
          }
          return oidc.response({'conversations': [], 'has_more': false});
        });
        await tester.pumpWidget(
          MaterialApp(home: startup.login(store, transport)),
        );
        await enterDevicePage(tester);
        if (cleanupFailure) {
          await pumpUntil(
            tester,
            () =>
                find.text('本机').evaluate().isNotEmpty &&
                find.byType(LinearProgressIndicator).evaluate().isEmpty,
          );
          await tester.pumpAndSettle();
          await tester.tap(
            find.byKey(const ValueKey('revoke-device-${member.deviceID}')),
          );
          await tester.pumpAndSettle();
          await tester.tap(find.text('确认撤销'));
          await pumpUntil(
            tester,
            () =>
                find.text('重试退出清理').evaluate().isNotEmpty &&
                find.byType(LinearProgressIndicator).evaluate().isEmpty,
          );
          await tester.pumpAndSettle();
          expect(find.text('本机'), findsNothing);
          expect(find.text('重试原撤销'), findsNothing);
          vault.unavailable = false;
          await tester.tap(find.text('重试退出清理'));
        }
        await pumpUntil(
          tester,
          () => find.text('登录集团工作空间').evaluate().isNotEmpty,
        );
        await tester.pumpAndSettle();
        expect(vault.values, isEmpty);
        expect(root.listSync(), isEmpty);
        expect(deletes, cleanupFailure ? 1 : 0);
      },
    );
  }

  for (final orphan in [false, true]) {
    testWidgets(
      orphan
          ? 'orphan recovery requires exact revoke then creates a new registration ID'
          : 'quota management revokes selected old device and retries original registration',
      (tester) async {
        final (root, _, store) = (await tester.runAsync(startup.seed))!;
        startup.cleanup(tester, root, store);
        var blocked = true, revoked = false;
        final registrations = <Map<String, dynamic>>[];
        final transport = oidc.FakeTransport((r, _) async {
          if (r.uri.path.endsWith('/config'))
            return oidc.response(oidc.config());
          if (r.uri.path.endsWith('/refresh'))
            return oidc.response(oidc.tokens('fresh-refresh'));
          if (r.uri.path == '/api/v1/me') return oidc.response(member.self());
          if (r.uri.path.endsWith('/client-capabilities'))
            return oidc.response(devices.policy());
          if (r.uri.path == '/api/v1/devices' && r.method == 'POST') {
            registrations.add(
              jsonDecode(utf8.decode(r.body!)) as Map<String, dynamic>,
            );
            if (blocked)
              return ApiResponse(
                409,
                utf8.encode(
                  jsonEncode({
                    'error_code': orphan
                        ? 'registration_exists'
                        : 'device_quota',
                    'device_id': devices.other,
                  }),
                ),
                {},
              );
            return oidc.response(member.grant());
          }
          if (r.uri.path == '/api/v1/devices' && r.method == 'GET')
            return oidc.response({
              'devices': [
                devices.device(
                  reg: orphan
                      ? registrations.first['registration_id'] as String
                      : devices.registration,
                  status: revoked ? 'revoked' : 'active',
                ),
              ],
              'has_more': false,
            });
          if (r.method == 'DELETE') {
            expect(r.uri.path, '/api/v1/devices/${devices.other}');
            revoked = true;
            blocked = false;
            return const ApiResponse(204, [], {});
          }
          return oidc.response({'conversations': [], 'has_more': false});
        });
        await tester.pumpWidget(
          MaterialApp(home: startup.login(store, transport)),
        );
        await pumpUntil(
          tester,
          () => find.text('进入工作空间').evaluate().isNotEmpty,
        );
        await tester.tap(find.text('进入工作空间'));
        await tester.pumpAndSettle();
        await pumpUntil(
          tester,
          () => find
              .byKey(const ValueKey('membership-${member.role}'))
              .evaluate()
              .isNotEmpty,
        );
        await tester.tap(
          find.byKey(const ValueKey('membership-${member.role}')),
        );
        await pumpUntil(
          tester,
          () =>
              registrations.length == 1 &&
              find.byType(LinearProgressIndicator).evaluate().isEmpty,
        );
        expect(find.text('通讯录'), findsNothing);
        await tester.tap(find.text('设备管理'));
        await tester.pump();
        await pumpUntil(
          tester,
          () =>
              find
                  .byKey(const ValueKey('revoke-device-${devices.other}'))
                  .evaluate()
                  .isNotEmpty &&
              find.byType(LinearProgressIndicator).evaluate().isEmpty,
        );
        await tester.pumpAndSettle();
        expect(find.text('设备上限：3'), findsOneWidget);
        if (orphan) expect(find.text('孤立登记（本机凭据未保存）'), findsOneWidget);
        await tester.tap(
          find.byKey(const ValueKey('revoke-device-${devices.other}')),
        );
        await tester.pumpAndSettle();
        await tester.tap(find.text('确认撤销'));
        await pumpUntil(tester, () => find.text('撤销已确认').evaluate().isNotEmpty);
        await tester.tap(find.byTooltip('返回工作空间'));
        await tester.pumpAndSettle();
        await tester.tap(
          find.byKey(const ValueKey('membership-${member.role}')),
        );
        await pumpUntil(
          tester,
          () =>
              find.text('通讯录').evaluate().isNotEmpty &&
              find.byType(LinearProgressIndicator).evaluate().isEmpty,
        );
        expect(registrations, hasLength(2));
        if (orphan)
          expect(
            registrations.last['registration_id'],
            isNot(registrations.first['registration_id']),
          );
        else
          expect(registrations.last, registrations.first);
      },
    );
  }
  testWidgets(
    'own device confirmed revoke returns to login and clears vault and account database',
    (tester) async {
      final (root, vault, store) = (await tester.runAsync(startup.seed))!;
      startup.cleanup(tester, root, store);
      final transport = oidc.FakeTransport((r, _) async {
        if (r.uri.path.endsWith('/config')) return oidc.response(oidc.config());
        if (r.uri.path.endsWith('/refresh'))
          return oidc.response(oidc.tokens('fresh-refresh'));
        if (r.uri.path == '/api/v1/me') return oidc.response(member.self());
        if (r.uri.path.endsWith('/client-capabilities'))
          return oidc.response(devices.policy());
        if (r.uri.path == '/api/v1/devices')
          return oidc.response(
            r.method == 'POST'
                ? member.grant()
                : {
                    'devices': [devices.device(id: member.deviceID)],
                    'has_more': false,
                  },
          );
        if (r.method == 'DELETE') return const ApiResponse(204, [], {});
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
            .byKey(const ValueKey('membership-${member.role}'))
            .evaluate()
            .isNotEmpty,
      );
      await tester.tap(find.byKey(const ValueKey('membership-${member.role}')));
      await pumpUntil(
        tester,
        () =>
            find.text('通讯录').evaluate().isNotEmpty &&
            find.byType(LinearProgressIndicator).evaluate().isEmpty,
      );
      await tester.tap(find.text('设备管理'));
      await tester.pump();
      await pumpUntil(
        tester,
        () =>
            find.text('本机').evaluate().isNotEmpty &&
            find.byType(LinearProgressIndicator).evaluate().isEmpty,
      );
      await tester.pumpAndSettle();
      await tester.tap(
        find.byKey(const ValueKey('revoke-device-${member.deviceID}')),
      );
      await tester.pumpAndSettle();
      expect(find.textContaining('待确认消息'), findsOneWidget);
      await tester.tap(find.text('确认撤销'));
      await pumpUntil(
        tester,
        () => find.text('登录集团工作空间').evaluate().isNotEmpty,
      );
      await tester.pumpAndSettle();
      expect(find.text('进入工作空间'), findsNothing);
      expect(vault.values, isEmpty);
      expect(root.listSync(), isEmpty);
    },
  );
}
