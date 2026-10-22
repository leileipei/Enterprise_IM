import 'dart:async';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/platform/app_activity.dart';
import 'package:enterprise_im/ui/client_shell.dart';
import 'package:enterprise_im/ui/message_page.dart';
import 'auth_test.dart' as oidc;
import 'navigation_test.dart' as nav;
import 'membership_admission_test.dart' as membership;
import 'message_view_test.dart' as view;
import 'sync_test.dart' as sync;
import 'desktop_notifications_test.dart' show NotificationFixture;
import 'login_session_test.dart' show pumpUntil;

void main() {
  for (final stillAuthorized in [true, false]) {
    testWidgets(
      'narrow notification tap rechecks authorization ($stillAuthorized) without stacking routes',
      (tester) async {
        tester.view.physicalSize = const Size(400, 900);
        tester.view.devicePixelRatio = 1;
        addTearDown(tester.view.resetPhysicalSize);
        addTearDown(tester.view.resetDevicePixelRatio);
        var lists = 0, syncs = 0;
        final owner = (await tester.runAsync(
          () => nav.owner((r, _) async {
            if (r.uri.path == '/api/v1/conversations') {
              lists++;
              return oidc.response({
                'conversations': [
                  if (lists == 1 || stillAuthorized)
                    {...nav.direct(), 'last_seq': 0},
                ],
                'has_more': false,
              });
            }
            if (r.uri.path.endsWith('/tickets'))
              throw const ApiFailure('unavailable');
            if (r.uri.path.endsWith('/sync')) {
              syncs++;
              return oidc.response(
                view.page(empty: r.uri.queryParameters['after_seq'] == '1'),
              );
            }
            if (r.uri.path.endsWith('/delivered') ||
                r.uri.path.endsWith('/read'))
              return oidc.response(sync.state());
            return view.metadata(r);
          }),
        ))!;
        addTearDown(() async {
          var done = false;
          unawaited(owner.logout().then((_) => done = true));
          await pumpUntil(tester, () => done);
        });
        addTearDown(() => tester.pumpWidget(const SizedBox.shrink()));
        final activity = AppActivity()
          ..update(foreground: true, focused: true, activeConversation: null);
        addTearDown(activity.dispose);
        final port = NotificationFixture();
        await tester.pumpWidget(
          MaterialApp(
            home: ClientShell(
              owner: owner,
              activity: activity,
              notificationPort: port,
              onLogout: () async => true,
            ),
          ),
        );
        await pumpUntil(
          tester,
          () => find
              .byKey(const ValueKey('membership-${membership.role}'))
              .evaluate()
              .isNotEmpty,
        );
        await tester.tap(find.byTooltip('启用消息通知'));
        await tester.pump();
        await tester.tap(
          find.byKey(const ValueKey('membership-${membership.role}')),
        );
        await pumpUntil(tester, () => find.text('张三').evaluate().isNotEmpty);
        await tester.tap(find.text('张三'));
        await pumpUntil(
          tester,
          () =>
              find.text('真实授权正文').evaluate().isNotEmpty &&
              port.shown.isNotEmpty,
        );
        final original = tester
            .widget<MessagePage>(find.byType(MessagePage))
            .view;
        final beforeSync = syncs;
        port.tap!(port.shown.single.$2);
        for (
          var i = 0;
          i < 100 && (lists < 2 || stillAuthorized && syncs == beforeSync);
          i++
        ) {
          await tester.runAsync(
            () => Future<void>.delayed(const Duration(milliseconds: 10)),
          );
          await tester.pump();
        }
        final authorizedLists = lists, freshSyncs = syncs;
        final sameView = identical(
          tester.widget<MessagePage>(find.byType(MessagePage)).view,
          original,
        );
        await pumpUntil(tester, () => !original.loading);
        await tester.pump(const Duration(milliseconds: 400));
        var stopped = false;
        unawaited(original.close().then((_) => stopped = true));
        await pumpUntil(tester, () => stopped);
        await tester.tap(find.byTooltip('返回会话列表'));
        await tester.pumpAndSettle();
        await pumpUntil(
          tester,
          () => find.byType(MessagePage).evaluate().isEmpty,
        );
        expect(
          find.byType(ClientShell),
          findsOneWidget,
          reason: 'one back must return to Shell, never a second message route',
        );
        var loggedOut = false;
        unawaited(owner.logout().then((_) => loggedOut = true));
        await pumpUntil(tester, () => loggedOut);
        await tester.pumpWidget(const SizedBox.shrink());
        expect(
          authorizedLists,
          2,
          reason: 'tap must fetch a fresh authorized conversation list',
        );
        expect(sameView, true);
        if (stillAuthorized) {
          expect(freshSyncs, greaterThan(beforeSync));
        } else {
          expect(
            freshSyncs,
            beforeSync,
            reason:
                'a notification cannot bypass a freshly denied conversation list',
          );
        }
      },
    );
  }
}
