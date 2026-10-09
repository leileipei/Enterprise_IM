import 'dart:convert';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/auth/client_session.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/platform/app_activity.dart';
import 'package:enterprise_im/ui/client_shell.dart';
import 'package:enterprise_im/ui/message_composer.dart';
import 'auth_test.dart' as oidc;
import 'session_bootstrap_test.dart' as bootstrap;
import 'membership_admission_test.dart' as membership;
import 'navigation_test.dart' as nav;
import 'message_view_test.dart' as view;
import 'message_send_test.dart' as sending;
import 'sync_test.dart' as sync;
import 'login_startup_test.dart' as startup;
import 'login_session_test.dart' show pumpUntil;

void main() {
  for (final width in [400.0, 1100.0]) {
    testWidgets(
      'message route at width $width preserves uncertain send and retries exact request',
      (tester) async {
        tester.view.physicalSize = Size(width, 900);
        tester.view.devicePixelRatio = 1;
        addTearDown(tester.view.resetPhysicalSize);
        addTearDown(tester.view.resetDevicePixelRatio);
        final (root, _, store) = (await tester.runAsync(startup.seed))!;
        startup.cleanup(tester, root, store);
        final posted = <Map<String, Object?>>[];
        final native = bootstrap.authentication(
          oidc.FakeTransport((r, _) async {
            if (r.uri.path.endsWith('/config'))
              return oidc.response(oidc.config());
            if (r.uri.path.endsWith('/refresh'))
              return oidc.response(oidc.tokens('fresh-refresh'));
            if (r.uri.path == '/api/v1/me')
              return oidc.response(membership.self());
            if (r.uri.path == '/api/v1/devices')
              return oidc.response(membership.grant());
            if (r.uri.path == '/api/v1/conversations')
              return oidc.response({
                'conversations': [
                  nav.direct(),
                  {...nav.direct(), 'id': nav.group, 'display_name': '李四'},
                ],
                'has_more': false,
              });
            if (r.uri.path.endsWith('/tickets'))
              throw const ApiFailure('unavailable');
            if (r.uri.path.endsWith('/sync'))
              return oidc.response(
                view.page(empty: r.uri.queryParameters['after_seq'] == '1'),
              );
            if (r.uri.path.endsWith('/delivered') ||
                r.uri.path.endsWith('/read'))
              return oidc.response(sync.state());
            if (r.uri.path.endsWith('/messages') && r.method == 'POST') {
              posted.add(
                (jsonDecode(utf8.decode(r.body!)) as Map)
                    .cast<String, Object?>(),
              );
              if (posted.length == 1) throw const ApiFailure('timeout');
              return oidc.response(sending.ack());
            }
            return view.metadata(r);
          }),
        );
        final owner = ClientSession(native, store, platform: 'macos');
        await tester.runAsync(() => owner.restore(bootstrap.account()));
        final activity = AppActivity()
          ..update(foreground: true, focused: true, activeConversation: null);
        addTearDown(activity.dispose);
        await tester.pumpWidget(
          MaterialApp(
            home: ClientShell(
              owner: owner,
              activity: activity,
              onLogout: () async {
                await owner.logout();
                return true;
              },
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
        await tester.tap(
          find.byKey(const ValueKey('membership-${membership.role}')),
        );
        await pumpUntil(tester, () => find.text('张三').evaluate().isNotEmpty);
        await tester.tap(find.text('张三'));
        await pumpUntil(
          tester,
          () => find.text('真实授权正文').evaluate().isNotEmpty,
        );
        await pumpUntil(
          tester,
          () => find.text('未读至少 1 条').evaluate().isNotEmpty,
        );
        expect(find.text('未读至少 1 条'), findsOneWidget);
        // File recovery reads the real SQLite worker. Wait for actual completion
        // before advancing animation time in the widget test's fake clock.
        await tester.pump();
        await pumpUntil(tester, () {
          final choice = find.widgetWithText(TextButton, '选择附件');
          return choice.evaluate().isNotEmpty &&
              tester.widget<TextButton>(choice).onPressed != null &&
              find.byType(LinearProgressIndicator).evaluate().isEmpty;
        });
        await tester.pumpAndSettle();
        expect(activity.activeConversation, nav.dm);
        if (width >= 800)
          expect(find.text('集团工作空间'), findsOneWidget);
        else
          expect(
            ModalRoute.of(
              tester.element(find.byKey(const ValueKey('message-editor'))),
            )!.isCurrent,
            isTrue,
          );
        await tester.enterText(
          find.descendant(
            of: find.byType(MessageComposer),
            matching: find.byType(TextField),
          ),
          '原文可靠发送',
        );
        await tester.pump();
        await tester.tap(find.text('发送'));
        await pumpUntil(tester, () => find.text('重试原消息').evaluate().isNotEmpty);
        expect(posted, hasLength(1));
        expect(find.text('已发送'), findsNothing);
        if (width >= 800) {
          await tester.tap(find.text('李四'));
          await tester.pump();
          await pumpUntil(
            tester,
            () => find.text('请先重试或明确放弃原消息，再切换会话。').evaluate().isNotEmpty,
          );
          expect(activity.activeConversation, nav.dm);
          expect(posted, hasLength(1));
        }
        await tester.tap(find.byTooltip('返回会话列表'));
        await tester.pump();
        expect(activity.blocked, isTrue);
        expect(find.text('保留并返回'), findsOneWidget);
        await tester.tap(find.text('取消'));
        await tester.pumpAndSettle();
        expect(activity.blocked, isFalse);
        expect(activity.activeConversation, nav.dm);
        await tester.tap(find.text('重试原消息'));
        await pumpUntil(tester, () => find.text('已发送').evaluate().isNotEmpty);
        expect(posted, hasLength(2));
        expect(posted[1], posted[0]);
        expect(find.text('重试原消息'), findsNothing);
        // Retried ACK must clear the old editor draft, avoiding accidental resend.
        final editor = tester.widget<TextField>(
          find.descendant(
            of: find.byType(MessageComposer),
            matching: find.byType(TextField),
          ),
        );
        expect(editor.controller!.text, isEmpty);
        await tester.enterText(
          find.byKey(const ValueKey('message-editor')),
          '第二条成功',
        );
        await tester.pump();
        await tester.tap(find.text('发送'));
        await pumpUntil(tester, () => posted.length == 3);
        await pumpUntil(
          tester,
          () => tester
              .widget<TextField>(find.byKey(const ValueKey('message-editor')))
              .controller!
              .text
              .isEmpty,
        );
        await tester.tap(find.byTooltip('返回会话列表'));
        await tester.pump();
        await tester.pumpAndSettle();
        await pumpUntil(
          tester,
          () => find.byType(MessageComposer).evaluate().isEmpty,
        );
        expect(activity.activeConversation, isNull);
        expect(find.text('集团工作空间'), findsOneWidget);
      },
    );
  }
}
