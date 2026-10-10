import 'dart:async';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/platform/app_activity.dart';
import 'package:enterprise_im/platform/notifications.dart';
import 'package:enterprise_im/ui/client_shell.dart';
import 'auth_test.dart' as oidc;
import 'navigation_test.dart' as nav;
import 'login_session_test.dart' show pumpUntil;

class PermissionPort implements NotificationPort {
  int prompts = 0;
  final Completer<bool> permission = Completer<bool>();
  @override
  Future<bool> enable(void Function(String?) tap) {
    prompts++;
    return permission.future;
  }

  @override
  Future<void> show({required String body, required String payload}) async {}
  @override
  Future<void> cancel() async {}
}

void main() {
  testWidgets(
    'desktop notification permission is requested only after an explicit click',
    (tester) async {
      final owner = (await tester.runAsync(
        () => nav.owner(
          (r, _) async => oidc.response({
            'conversations': [nav.direct()],
            'next_cursor': null,
          }),
        ),
      ))!;
      addTearDown(() => tester.runAsync(owner.logout));
      final activity = AppActivity()
        ..update(foreground: true, focused: true, activeConversation: null);
      addTearDown(activity.dispose);
      final port = PermissionPort();
      await tester.pumpWidget(
        MaterialApp(
          home: ClientShell(
            owner: owner,
            activity: activity,
            notificationPort: port,
            onLogout: () async {
              await owner.logout();
              return true;
            },
          ),
        ),
      );
      await pumpUntil(tester, () => find.text('选择当前任职').evaluate().isNotEmpty);
      expect(port.prompts, 0);
      await tester.tap(find.byTooltip('启用消息通知'));
      await tester.pump();
      expect(port.prompts, 1);
      port.permission.complete(false);
      await pumpUntil(
        tester,
        () => find.text('消息通知未启用，不影响正常收发。').evaluate().isNotEmpty,
      );
      expect(find.text('选择当前任职'), findsOneWidget);
      await tester.pumpWidget(const SizedBox.shrink());
    },
  );
  testWidgets(
    'late notification permission cannot revive a disposed workspace',
    (tester) async {
      final owner = (await tester.runAsync(
        () => nav.owner(
          (r, _) async =>
              oidc.response({'conversations': [], 'next_cursor': null}),
        ),
      ))!;
      addTearDown(() => tester.runAsync(owner.logout));
      final activity = AppActivity()
        ..update(foreground: true, focused: true, activeConversation: null);
      addTearDown(activity.dispose);
      final port = PermissionPort();
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
      await pumpUntil(tester, () => find.text('选择当前任职').evaluate().isNotEmpty);
      await tester.tap(find.byTooltip('启用消息通知'));
      await tester.pumpWidget(const SizedBox.shrink());
      port.permission.complete(true);
      await tester.pump();
      expect(tester.takeException(), isNull);
      expect(find.text('集团工作空间'), findsNothing);
    },
  );
}
