import 'dart:async';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/models.dart';
import 'package:enterprise_im/features/conversations/conversation_view.dart';
import 'package:enterprise_im/platform/app_activity.dart';
import 'auth_test.dart' as oidc;
import 'navigation_test.dart' as nav;
import 'message_send_test.dart' as sending;
import 'message_view_test.dart' as viewFixture;
import 'sync_test.dart' as sync;
import 'file_transfer_test.dart' as files;

void main() {
  test(
    'message UI is notified of upload busy state and background cancels original transfer',
    () async {
      final entered = Completer<void>(), release = Completer<ApiResponse>();
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/tickets'))
          throw const ApiFailure('unavailable');
        if (r.uri.path.endsWith('/sync'))
          return oidc.response(viewFixture.page());
        if (r.uri.path.endsWith('/delivered'))
          return oidc.response(sync.state());
        if (r.uri.path.endsWith('/files'))
          return oidc.response(files.status('allocated'));
        return viewFixture.metadata(r);
      });
      addTearDown(owner.logout);
      final (roles, ctx) = await sending.admission(owner);
      addTearDown(roles.close);
      final activity = AppActivity()
        ..update(foreground: true, focused: true, activeConversation: nav.dm);
      addTearDown(activity.dispose);
      final view = ConversationView(
        roles,
        activity,
        connector: viewFixture.OfflineSocket(),
        fileWire: files.UploadWire(
          respond: () {
            entered.complete();
            return release.future;
          },
        ),
      );
      addTearDown(view.close);
      var lastCanSend = false;
      view.addListener(() => lastCanSend = view.canSend);
      await view.select(ctx, ConversationSummary.fromJson(nav.direct()));
      await view.refreshMetadata();
      expect(lastCanSend, isTrue);
      final uploading = view.files.reserveAndUpload(ctx, files.selection());
      final rejected = expectLater(uploading, throwsA(isA<ApiFailure>()));
      await entered.future;
      try {
        expect(lastCanSend, isFalse);
      } finally {
        activity.update(
          foreground: false,
          focused: false,
          activeConversation: nav.dm,
        );
        release.complete(oidc.response(files.status('uploaded')));
      }
      await rejected;
      expect(view.files.current, isNull);
      expect(await owner.current!.store.pendingUpload(), isNotNull);
    },
  );
}
