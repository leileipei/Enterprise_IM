import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/features/files/file_transfer.dart';
import 'package:enterprise_im/features/files/file_upload_panel.dart';
import 'auth_test.dart' as oidc;
import 'navigation_test.dart' as nav;
import 'message_send_test.dart' as sending;
import 'file_transfer_test.dart' as files;
import 'login_session_test.dart' show pumpUntil;

void main() {
  testWidgets(
    'attachment panel waits for ready and sends caption through original file workflow',
    (tester) async {
      var state = 'allocated';
      final owner = (await tester.runAsync(
        () => nav.owner(
          (r, _) async => oidc.response(
            r.uri.path.endsWith('/messages')
                ? sending.ack()
                : files.status(state),
          ),
        ),
      ))!;
      final (roles, ctx) = (await tester.runAsync(
        () => sending.admission(owner),
      ))!;
      final sender = (await tester.runAsync(
        () => sending.controller(roles, ctx),
      ))!;
      final transfer = FileTransfer(roles, sender, wire: files.UploadWire());
      addTearDown(() async {
        await tester.pumpWidget(const SizedBox());
        await tester.runAsync(transfer.close);
        roles.close();
        await tester.runAsync(owner.logout);
      });
      await tester.pumpWidget(
        MaterialApp(
          home: Scaffold(
            body: FileUploadPanel(
              transfer: transfer,
              context: ctx,
              enabled: true,
              pick: () async => files.selection(),
              bind: transfer.bindToMessage,
              onChanged: () async {},
            ),
          ),
        ),
      );
      await pumpUntil(
        tester,
        () => find.byType(LinearProgressIndicator).evaluate().isEmpty,
      );
      await tester.pumpAndSettle();
      await tester.tap(find.text('选择附件'));
      await pumpUntil(
        tester,
        () =>
            find.text('等待扫描').evaluate().isNotEmpty &&
            find.byType(LinearProgressIndicator).evaluate().isEmpty,
      );
      await tester.pumpAndSettle();
      expect(
        tester
            .widget<FilledButton>(find.widgetWithText(FilledButton, '发送附件'))
            .onPressed,
        isNull,
      );
      state = 'ready';
      await tester.tap(find.text('查询扫描状态'));
      await pumpUntil(
        tester,
        () => find.text('扫描通过，可发送').evaluate().isNotEmpty,
      );
      await tester.pumpAndSettle();
      await tester.enterText(find.byType(TextField), '附件说明');
      await tester.tap(find.text('发送附件'));
      await pumpUntil(tester, () => find.text('附件已发送').evaluate().isNotEmpty);
      expect(
        await tester.runAsync(() => owner.current!.store.pendingUpload()),
        isNull,
      );
    },
  );
}
