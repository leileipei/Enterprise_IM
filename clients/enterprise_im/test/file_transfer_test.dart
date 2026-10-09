import 'dart:async';
import 'dart:convert';
import 'package:sqlite3/sqlite3.dart';
import 'package:enterprise_im/core/auth/client_session.dart';
import 'package:enterprise_im/core/storage/encrypted_store.dart';
import 'package:enterprise_im/core/storage/secure_vault.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'membership_admission_test.dart' as member;
import 'package:enterprise_im/features/files/file_transfer.dart';
import 'package:enterprise_im/features/files/file_wire.dart';
import 'package:enterprise_im/platform/file_picker.dart';
import 'auth_test.dart' as oidc;
import 'navigation_test.dart' as nav;
import 'message_send_test.dart' as sending;
import 'session_bootstrap_test.dart' as bootstrap;

const fileID = 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee';
Map<String, Object?> status(String state) => {
  'file_id': fileID,
  'conversation_id': nav.dm,
  'original_filename': '说明.txt',
  'declared_media_type': 'text/plain',
  'declared_size_bytes': '3',
  'state': state,
  'state_version': state == 'allocated'
      ? '1'
      : state == 'uploaded'
      ? '2'
      : '4',
  'created_at': '2026-10-09T00:00:00Z',
  'upload_expires_at': '2027-10-09T00:10:00Z',
};
SelectedFile selection([String text = 'abc']) => SelectedFile(
  name: '说明.txt',
  mediaType: 'text/plain',
  size: 3,
  open: () => Stream.value(utf8.encode(text)),
);

class UploadWire implements FileWireTransport {
  int uploads = 0;
  final Future<ApiResponse> Function()? respond;
  UploadWire({this.respond});
  @override
  Future<ApiResponse> upload(
    ApiRequest r,
    Stream<List<int>> body,
    int size,
    Cancellation c,
  ) async {
    uploads++;
    expect(r.uri.path, '/api/v1/files/$fileID/content');
    expect(r.headers['Content-Type'], 'application/octet-stream');
    final bytes = await body.expand((b) => b).toList();
    expect(utf8.decode(bytes), 'abc');
    expect(size, 3);
    return respond == null
        ? oidc.response(status('uploaded'))
        : await respond!();
  }

  @override
  Future<FileWireResponse> download(ApiRequest r, Cancellation c) =>
      throw StateError('unexpected download');
  @override
  void close() {}
}

void main() {
  test(
    'cancelled local file read releases its original stream subscription',
    () async {
      final subscribed = Completer<void>();
      var cancelled = false;
      final source = StreamController<List<int>>(
        onListen: subscribed.complete,
        onCancel: () {
          cancelled = true;
        },
      );
      addTearDown(source.close);
      final owner = await nav.owner(
        (r, _) async => oidc.response(status('allocated')),
      );
      addTearDown(owner.logout);
      final (roles, ctx) = await sending.admission(owner);
      addTearDown(roles.close);
      final sender = await sending.controller(roles, ctx);
      final transfer = FileTransfer(roles, sender, wire: UploadWire());
      addTearDown(transfer.close);
      final rejected = expectLater(
        transfer.reserveAndUpload(
          ctx,
          SelectedFile(
            name: '说明.txt',
            mediaType: 'text/plain',
            size: 3,
            open: () => source.stream,
          ),
        ),
        throwsA(isA<ApiFailure>()),
      );
      await subscribed.future;
      transfer.stopRequest();
      await rejected;
      expect(cancelled, isTrue);
      expect(await owner.current!.store.pendingUpload(), isNull);
    },
  );
  test(
    'file ACK transaction rolls back when original upload deletion fails',
    () async {
      var state = 'allocated', fault = true;
      late ClientSession owner;
      Future<String> path() async =>
          '${owner.sessions.directory.path}/${await accountNamespace(bootstrap.account().origin, bootstrap.tenant, bootstrap.user)}.sqlite';
      owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/messages')) {
          if (fault) {
            final db = sqlite3.open(await path());
            try {
              db.execute(
                "CREATE TRIGGER reject_upload_delete BEFORE DELETE ON records WHEN OLD.kind='upload' BEGIN SELECT RAISE(ABORT,'owned-test'); END",
              );
            } finally {
              db.close();
            }
          }
          return oidc.response(sending.ack());
        }
        return oidc.response(status(state));
      });
      addTearDown(owner.logout);
      final (roles, ctx) = await sending.admission(owner);
      addTearDown(roles.close);
      final sender = await sending.controller(roles, ctx);
      final transfer = FileTransfer(roles, sender, wire: UploadWire());
      addTearDown(transfer.close);
      await transfer.reserveAndUpload(ctx, selection());
      state = 'ready';
      await transfer.poll();
      await expectLater(
        transfer.bindToMessage('原说明'),
        throwsA(isA<StoreFailure>()),
      );
      final pending = (await owner.current!.store.pendingSends()).single;
      expect(
        await owner.current!.store.sendConfirmation(pending.clientMsgId),
        isNull,
      );
      expect(await owner.current!.store.pendingUpload(), isNotNull);
      final db = sqlite3.open(await path());
      try {
        db.execute('DROP TRIGGER reject_upload_delete');
      } finally {
        db.close();
      }
      fault = false;
      await sender.retryPending(pending.clientMsgId);
      expect(await owner.current!.store.pendingUpload(), isNull);
      expect(await owner.current!.store.pendingSends(), isEmpty);
    },
  );
  test(
    'failed scan and a withdrawn admission never permit attachment binding',
    () async {
      var state = 'allocated';
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/messages'))
          fail('no file message should be sent');
        return oidc.response(status(state));
      });
      addTearDown(owner.logout);
      final (roles, ctx) = await sending.admission(owner);
      addTearDown(roles.close);
      final sender = await sending.controller(roles, ctx);
      final transfer = FileTransfer(roles, sender, wire: UploadWire());
      addTearDown(transfer.close);
      await transfer.reserveAndUpload(ctx, selection());
      for (final rejected in ['rejected', 'scan_failed', 'delete_pending']) {
        state = rejected;
        await transfer.poll();
        await expectLater(
          transfer.bindToMessage('说明'),
          throwsA(isA<ApiFailure>()),
        );
      }
      roles.withdrawDevice();
      await expectLater(transfer.poll(), throwsA(isA<ApiFailure>()));
      expect(transfer.current, isNull);
    },
  );
  test('unsupported or oversized file creates no server reservation', () async {
    var requests = 0;
    final owner = await nav.owner((r, _) async {
      requests++;
      return oidc.response(status('allocated'));
    });
    addTearDown(owner.logout);
    final (roles, ctx) = await sending.admission(owner);
    addTearDown(roles.close);
    final sender = await sending.controller(roles, ctx);
    final transfer = FileTransfer(roles, sender, wire: UploadWire());
    addTearDown(transfer.close);
    await expectLater(
      transfer.reserveAndUpload(
        ctx,
        SelectedFile(
          name: 'bad.exe',
          mediaType: 'application/x-executable',
          size: 3,
          open: () => Stream.value([1, 2, 3]),
        ),
      ),
      throwsA(isA<FormatException>()),
    );
    await expectLater(
      transfer.reserveAndUpload(
        ctx,
        SelectedFile(
          name: 'large.txt',
          mediaType: 'text/plain',
          size: 26214401,
          open: () => throw StateError('oversized must not read'),
        ),
      ),
      throwsA(isA<ApiFailure>()),
    );
    expect(requests, 0);
    expect(await owner.current!.store.pendingUpload(), isNull);
  });

  test(
    'file transfer binds only ready status with its exact original body',
    () async {
      var state = 'allocated';
      final bodies = <Map<String, dynamic>>[];
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/messages')) {
          bodies.add(jsonDecode(utf8.decode(r.body!)) as Map<String, dynamic>);
          return oidc.response(sending.ack());
        }
        return oidc.response(status(state));
      });
      addTearDown(owner.logout);
      final (roles, ctx) = await sending.admission(owner);
      addTearDown(roles.close);
      final sender = await sending.controller(roles, ctx);
      final wire = UploadWire();
      final transfer = FileTransfer(roles, sender, wire: wire);
      addTearDown(transfer.close);
      await transfer.reserveAndUpload(ctx, selection());
      expect(transfer.current!.state, 'uploaded');
      await expectLater(
        transfer.bindToMessage('附件说明'),
        throwsA(isA<ApiFailure>()),
      );
      expect(bodies, isEmpty);
      state = 'ready';
      await transfer.poll();
      await transfer.bindToMessage('附件说明');
      expect(bodies.single['message_type'], 'file');
      expect(bodies.single['file_id'], fileID);
      expect(bodies.single['caption'], '附件说明');
      expect(bodies.single.containsKey('text'), isFalse);
      expect(await owner.current!.store.pendingUpload(), isNull);
    },
  );
  test(
    'reservation timeout survives reopen and selected bytes must match before original retry',
    () async {
      var reject = true;
      final reservations = <Object?>[];
      final owner = await nav.owner((r, _) async {
        if (r.uri.path.endsWith('/renew'))
          return oidc.response(member.grant(renew: true));
        reservations.add(jsonDecode(utf8.decode(r.body!)));
        if (reject) throw const ApiFailure('timeout');
        return oidc.response(status('allocated'));
      });
      addTearDown(owner.logout);
      final (roles, ctx) = await sending.admission(owner);
      addTearDown(roles.close);
      final sender = await sending.controller(roles, ctx);
      final first = FileTransfer(roles, sender, wire: UploadWire());
      await expectLater(
        first.reserveAndUpload(ctx, selection()),
        throwsA(isA<ApiFailure>()),
      );
      await first.close();
      roles.close();
      await owner.close();
      await owner.restore(bootstrap.account());
      final (next, nextCtx) = await sending.admission(owner);
      addTearDown(next.close);
      final nextSender = await sending.controller(next, nextCtx);
      final wire = UploadWire();
      final second = FileTransfer(next, nextSender, wire: wire);
      addTearDown(second.close);
      reject = false;
      await expectLater(
        second.reserveAndUpload(nextCtx, selection('xyz')),
        throwsA(isA<ApiFailure>()),
      );
      expect(reservations, hasLength(1));
      await second.reserveAndUpload(nextCtx, selection());
      expect(reservations, hasLength(2));
      expect(reservations.first, reservations.last);
      expect(wire.uploads, 1);
    },
  );
  test('file bind lost ACK retries exact original file body', () async {
    var state = 'allocated', reject = true;
    final bodies = <Object?>[];
    final owner = await nav.owner((r, _) async {
      if (r.uri.path.endsWith('/messages')) {
        bodies.add(jsonDecode(utf8.decode(r.body!)));
        if (reject) throw const ApiFailure('timeout');
        return oidc.response(sending.ack());
      }
      return oidc.response(status(state));
    });
    addTearDown(owner.logout);
    final (roles, ctx) = await sending.admission(owner);
    addTearDown(roles.close);
    final sender = await sending.controller(roles, ctx);
    final transfer = FileTransfer(roles, sender, wire: UploadWire());
    addTearDown(transfer.close);
    await transfer.reserveAndUpload(ctx, selection());
    state = 'ready';
    await transfer.poll();
    await expectLater(
      transfer.bindToMessage('原说明'),
      throwsA(isA<ApiFailure>()),
    );
    final pending = (await owner.current!.store.pendingSends()).single;
    expect(pending.fileId, fileID);
    await expectLater(sender.sendText('替换内容'), throwsA(isA<ApiFailure>()));
    reject = false;
    await sender.retryPending(pending.clientMsgId);
    expect(bodies.first, bodies.last);
    expect(await owner.current!.store.pendingUpload(), isNull);
  });
  test(
    'closing transfer rejects late upload success and preserves original record',
    () async {
      final entered = Completer<void>(), late = Completer<ApiResponse>();
      final owner = await nav.owner(
        (r, _) async => oidc.response(status('allocated')),
      );
      addTearDown(owner.logout);
      final (roles, ctx) = await sending.admission(owner);
      addTearDown(roles.close);
      final sender = await sending.controller(roles, ctx);
      final transfer = FileTransfer(
        roles,
        sender,
        wire: UploadWire(
          respond: () {
            entered.complete();
            return late.future;
          },
        ),
      );
      final sendingFile = transfer.reserveAndUpload(ctx, selection());
      final rejected = expectLater(sendingFile, throwsA(isA<ApiFailure>()));
      await entered.future;
      await transfer.close();
      late.complete(oidc.response(status('uploaded')));
      await rejected;
      expect(transfer.current, isNull);
      expect(await owner.current!.store.pendingUpload(), isNotNull);
    },
  );
}
