import 'dart:async';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:flutter/material.dart';
import 'package:enterprise_im/features/files/file_message_card.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/models.dart';
import 'package:enterprise_im/features/files/file_download.dart';
import 'package:enterprise_im/features/files/file_wire.dart';
import 'package:enterprise_im/platform/file_export.dart';
import 'auth_test.dart' as oidc;
import 'navigation_test.dart' as nav;
import 'message_send_test.dart' as sending;
import 'membership_admission_test.dart' as fixture;
import 'file_transfer_test.dart' show fileID;

MessageDTO message() => MessageDTO.fromJson({
  'seq': '1',
  'message_id': 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
  'sender_user_id': 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb',
  'message_type': 'file',
  'caption': '说明',
  'server_time': '2026-10-09T00:00:00Z',
  'content_expires_at': '2027-10-09T00:00:00Z',
  'cache_until': '2027-10-09T00:00:00Z',
  'attachment': {
    'file_id': fileID,
    'available': true,
    'download_available': false,
    'original_filename': '说明.txt',
    'detected_media_type': 'text/plain',
    'actual_size_bytes': '3',
  },
});
FileWireResponse response(Stream<List<int>> body) => FileWireResponse(200, {
  'content-type': 'application/octet-stream',
  'content-length': '3',
  'cache-control': 'no-store',
}, body);

class DownloadWire implements FileWireTransport {
  final Future<FileWireResponse> Function(Cancellation) handler;
  int downloads = 0;
  DownloadWire(this.handler);
  @override
  Future<FileWireResponse> download(ApiRequest r, Cancellation c) {
    downloads++;
    expect(r.uri.path, '/api/v1/files/$fileID/content');
    return handler(c);
  }

  @override
  Future<ApiResponse> upload(
    ApiRequest r,
    Stream<List<int>> b,
    int size,
    Cancellation c,
  ) => throw StateError('unexpected upload');
  @override
  void close() {}
}

class ExportSink implements FileExporter {
  Future<void> Function()? beforeCopy;
  final bytes = <int>[];
  @override
  Future<bool> save(
    String name,
    Stream<List<int>> Function() contents,
    Cancellation c,
  ) async {
    await beforeCopy?.call();
    await for (final chunk in contents()) {
      c.check();
      bytes.addAll(chunk);
    }
    return true;
  }
}

Map<String, Object?> capabilities({bool enabled = true}) => {
  'upload_enabled': true,
  'message_send_enabled': true,
  'download_enabled': enabled,
  'filename_search_enabled': true,
};

void main() {
  for (final caps in [capabilities(enabled: false), <String, Object?>{}]) {
    test(
      'disabled or malformed assembly capability never requests contents $caps',
      () async {
        final root = await Directory.systemTemp.createTemp(
          'im-file-download-test-',
        );
        addTearDown(() => root.delete(recursive: true));
        var checked = 0;
        final owner = await nav.owner((r, _) async {
          expect(r.uri.path, '/api/v1/file-capabilities');
          expect(r.headers['X-Acting-Membership-ID'], fixture.role);
          expect(r.headers['X-IM-Device-ID'], isNotEmpty);
          checked++;
          return oidc.response(caps);
        });
        addTearDown(owner.logout);
        final (roles, ctx) = await sending.admission(owner);
        addTearDown(roles.close);
        final wire = DownloadWire(
          (c) async => response(Stream.value([97, 98, 99])),
        );
        final download = FileDownload(
          roles,
          wire: wire,
          exporter: ExportSink(),
          privateRoot: root,
        );
        addTearDown(download.close);
        await expectLater(
          download.downloadAuthorized(
            ctx,
            message(),
            authorizationRemaining: () => const Duration(minutes: 5),
          ),
          throwsA(
            isA<ApiFailure>().having(
              (e) => e.code,
              'capability denial',
              caps.isEmpty ? 'invalid_response' : 'file_download_disabled',
            ),
          ),
        );
        expect(checked, 1);
        expect(wire.downloads, 0);
        expect(root.listSync(recursive: true), isEmpty);
      },
    );
  }
  test(
    'export rechecks assembly capability after destination selection',
    () async {
      var enabled = true, checked = 0;
      final owner = await nav.owner((r, _) async {
        checked++;
        return oidc.response(capabilities(enabled: enabled));
      });
      addTearDown(owner.logout);
      final (roles, ctx) = await sending.admission(owner);
      addTearDown(roles.close);
      final wire = DownloadWire(
        (c) async => response(Stream.value([97, 98, 99])),
      );
      final sink = ExportSink()
        ..beforeCopy = () async {
          enabled = false;
        };
      final download = FileDownload(roles, wire: wire, exporter: sink);
      addTearDown(download.close);
      final artifact = await download.downloadAuthorized(
        ctx,
        message(),
        authorizationRemaining: () => const Duration(minutes: 5),
      );
      await expectLater(
        download.exportPrivateFile(ctx, artifact),
        throwsA(
          isA<ApiFailure>().having(
            (e) => e.code,
            'fresh capability',
            'file_download_disabled',
          ),
        ),
      );
      expect(checked, 2);
      expect(wire.downloads, 1);
      expect(sink.bytes, isEmpty);
    },
  );
  testWidgets(
    'visible leased file with advisory false flag offers a download request',
    (tester) async {
      final owner = await tester.runAsync(
        () => nav.owner(
          (r, _) async => oidc.response(capabilities(enabled: false)),
        ),
      );
      addTearDown(() => tester.runAsync(owner!.logout));
      final (roles, ctx) = (await tester.runAsync(
        () => sending.admission(owner!),
      ))!;
      addTearDown(roles.close);
      final wire = DownloadWire(
        (c) async => response(Stream.value([97, 98, 99])),
      );
      final download = FileDownload(roles, wire: wire, exporter: ExportSink());
      addTearDown(() => tester.runAsync(download.close));
      await tester.pumpWidget(
        MaterialApp(
          home: Scaffold(
            body: FileMessageCard(
              message: message(),
              context: ctx,
              download: download,
              authorizationRemaining: () => const Duration(minutes: 5),
            ),
          ),
        ),
      );
      expect(find.text('请求下载附件'), findsOneWidget);
      await tester.tap(find.text('请求下载附件'));
      await tester.pumpAndSettle();
      expect(find.text('文件操作未完成或授权已失效，请刷新消息后重试。'), findsOneWidget);
      expect(wire.downloads, 0);
      await tester.pumpWidget(const SizedBox.shrink());
    },
  );
  test(
    'complete authorized download exports only a fresh stream and close removes private artifact',
    () async {
      final root = await Directory.systemTemp.createTemp(
        'im-file-download-test-',
      );
      addTearDown(() => root.delete(recursive: true));
      final owner = await nav.owner(
        (r, _) async => oidc.response(capabilities()),
      );
      addTearDown(owner.logout);
      final (roles, ctx) = await sending.admission(owner);
      addTearDown(roles.close);
      final sink = ExportSink(),
          wire = DownloadWire(
            (c) async => response(
              Stream.fromIterable([
                [97],
                [98, 99],
              ]),
            ),
          );
      final download = FileDownload(
        roles,
        wire: wire,
        exporter: sink,
        privateRoot: root,
      );
      addTearDown(download.close);
      final artifact = await download.downloadAuthorized(
        ctx,
        message(),
        authorizationRemaining: () => const Duration(minutes: 5),
      );
      expect(artifact.name, '说明.txt');
      expect(root.listSync(recursive: true).whereType<File>(), hasLength(1));
      expect(await download.exportPrivateFile(ctx, artifact), isTrue);
      expect(sink.bytes, [97, 98, 99]);
      expect(wire.downloads, 2);
      await download.close();
      expect(root.listSync(recursive: true).whereType<File>(), isEmpty);
    },
  );
  for (final mismatch in [
    FileWireResponse(200, {
      'content-type': 'application/octet-stream',
      'content-length': '3',
      'cache-control': 'no-store',
    }, Stream.value([97])),
    FileWireResponse(302, {}, const Stream.empty()),
    FileWireResponse(200, {
      'content-type': 'application/octet-stream',
      'content-length': '4',
      'cache-control': 'no-store',
    }, Stream.value([97, 98, 99, 100])),
  ]) {
    test(
      'mismatched or redirected download ${mismatch.status} leaves no private file',
      () async {
        final root = await Directory.systemTemp.createTemp(
          'im-file-download-test-',
        );
        addTearDown(() => root.delete(recursive: true));
        final owner = await nav.owner(
          (r, _) async => oidc.response(capabilities()),
        );
        addTearDown(owner.logout);
        final (roles, ctx) = await sending.admission(owner);
        addTearDown(roles.close);
        final download = FileDownload(
          roles,
          wire: DownloadWire((c) async => mismatch),
          exporter: ExportSink(),
          privateRoot: root,
        );
        addTearDown(download.close);
        await expectLater(
          download.downloadAuthorized(
            ctx,
            message(),
            authorizationRemaining: () => const Duration(minutes: 5),
          ),
          throwsA(isA<ApiFailure>()),
        );
        expect(root.listSync(recursive: true).whereType<File>(), isEmpty);
      },
    );
  }

  test('logout cancels body subscription after the first chunk', () async {
    var subscribed = false, cancelled = false;
    final stream = StreamController<List<int>>(
      onListen: () => subscribed = true,
      onCancel: () => cancelled = true,
    );
    addTearDown(stream.close);
    final owner = await nav.owner(
      (r, _) async => oidc.response(capabilities()),
    );
    addTearDown(owner.logout);
    final (roles, ctx) = await sending.admission(owner);
    addTearDown(roles.close);
    final download = FileDownload(
      roles,
      wire: DownloadWire((c) async => response(stream.stream)),
      exporter: ExportSink(),
    );
    addTearDown(download.close);
    final pending = download.downloadAuthorized(
      ctx,
      message(),
      authorizationRemaining: () => const Duration(minutes: 5),
    );
    final rejected = expectLater(pending, throwsA(isA<ApiFailure>()));
    final wait = Stopwatch()..start();
    while (!subscribed && wait.elapsed < const Duration(seconds: 2)) {
      await Future<void>.delayed(const Duration(milliseconds: 5));
    }
    expect(subscribed, isTrue);
    stream.add([97]);
    await Future<void>.delayed(const Duration(milliseconds: 20));
    await owner.logout();
    await rejected;
    expect(cancelled, isTrue);
  });
  test(
    'logout ends an export still awaiting the destination chooser',
    () async {
      final entered = Completer<void>(), release = Completer<void>();
      final owner = await nav.owner(
        (r, _) async => oidc.response(capabilities()),
      );
      addTearDown(owner.logout);
      final (roles, ctx) = await sending.admission(owner);
      addTearDown(roles.close);
      final sink = ExportSink()
        ..beforeCopy = () async {
          entered.complete();
          await release.future;
        };
      final download = FileDownload(
        roles,
        wire: DownloadWire((c) async => response(Stream.value([97, 98, 99]))),
        exporter: sink,
      );
      addTearDown(download.close);
      final artifact = await download.downloadAuthorized(
        ctx,
        message(),
        authorizationRemaining: () => const Duration(minutes: 5),
      );
      final pending = download.exportPrivateFile(ctx, artifact);
      final rejected = expectLater(
        pending.timeout(const Duration(milliseconds: 500)),
        throwsA(isA<ApiFailure>()),
      );
      await entered.future;
      await owner.logout();
      try {
        await rejected;
      } finally {
        release.complete();
      }
      expect(sink.bytes, isEmpty);
    },
  );

  test('testInterruptedDownloadErasesTemporaryFile', () async {
    final root = await Directory.systemTemp.createTemp(
      'im-file-download-test-',
    );
    addTearDown(() => root.delete(recursive: true));
    final owner = await nav.owner(
      (r, _) async => oidc.response(capabilities()),
    );
    addTearDown(owner.logout);
    final (roles, ctx) = await sending.admission(owner);
    addTearDown(roles.close);
    final wire = DownloadWire(
      (c) async => response(() async* {
        yield [97];
        throw const ApiFailure('unavailable');
      }()),
    );
    final download = FileDownload(
      roles,
      wire: wire,
      exporter: ExportSink(),
      privateRoot: root,
    );
    addTearDown(download.close);
    await expectLater(
      download.downloadAuthorized(
        ctx,
        message(),
        authorizationRemaining: () => const Duration(minutes: 5),
      ),
      throwsA(isA<ApiFailure>()),
    );
    expect(root.listSync(recursive: true).whereType<File>(), isEmpty);
  });
  test('testExpiredFileOrLateResponseCannotExport', () async {
    final root = await Directory.systemTemp.createTemp(
      'im-file-download-test-',
    );
    addTearDown(() => root.delete(recursive: true));
    final owner = await nav.owner(
      (r, _) async => oidc.response(capabilities()),
    );
    addTearDown(owner.logout);
    final (roles, ctx) = await sending.admission(owner);
    addTearDown(roles.close);
    var remaining = const Duration(minutes: 5);
    final sink = ExportSink(),
        wire = DownloadWire((c) async => response(Stream.value([97, 98, 99])));
    final download = FileDownload(
      roles,
      wire: wire,
      exporter: sink,
      privateRoot: root,
    );
    addTearDown(download.close);
    final artifact = await download.downloadAuthorized(
      ctx,
      message(),
      authorizationRemaining: () => remaining,
    );
    sink.beforeCopy = () async {
      remaining = Duration.zero;
    };
    await expectLater(
      download.exportPrivateFile(ctx, artifact),
      throwsA(isA<ApiFailure>()),
    );
    expect(sink.bytes, isEmpty);
    expect(wire.downloads, 1);
  });
  test(
    'explicit export reauthorizes after destination choice and does not reuse old download',
    () async {
      final root = await Directory.systemTemp.createTemp(
        'im-file-download-test-',
      );
      addTearDown(() => root.delete(recursive: true));
      final owner = await nav.owner(
        (r, _) async => oidc.response(capabilities()),
      );
      addTearDown(owner.logout);
      final (roles, ctx) = await sending.admission(owner);
      addTearDown(roles.close);
      var deny = false;
      final sink = ExportSink(),
          wire = DownloadWire((c) async {
            if (deny) throw const ApiFailure('invalid_identity', status: 403);
            return response(Stream.value([97, 98, 99]));
          });
      final download = FileDownload(
        roles,
        wire: wire,
        exporter: sink,
        privateRoot: root,
      );
      addTearDown(download.close);
      final artifact = await download.downloadAuthorized(
        ctx,
        message(),
        authorizationRemaining: () => const Duration(minutes: 5),
      );
      sink.beforeCopy = () async {
        deny = true;
      };
      await expectLater(
        download.exportPrivateFile(ctx, artifact),
        throwsA(isA<ApiFailure>()),
      );
      expect(sink.bytes, isEmpty);
      expect(wire.downloads, 2);
    },
  );
  test(
    'logout cancels stalled download and erases private account files before return',
    () async {
      final entered = Completer<void>();
      final wire = DownloadWire((c) async {
        entered.complete();
        await c.whenCancelled;
        throw const ApiFailure('cancelled');
      });
      final owner = await nav.owner(
        (r, _) async => oidc.response(capabilities()),
      );
      addTearDown(owner.logout);
      final (roles, ctx) = await sending.admission(owner);
      addTearDown(roles.close);
      final download = FileDownload(roles, wire: wire, exporter: ExportSink());
      addTearDown(download.close);
      final pending = download.downloadAuthorized(
        ctx,
        message(),
        authorizationRemaining: () => const Duration(minutes: 5),
      );
      final rejected = expectLater(pending, throwsA(isA<ApiFailure>()));
      await entered.future;
      await owner.logout();
      await rejected;
      expect(owner.sessions.directory.listSync(), isEmpty);
    },
  );
}
