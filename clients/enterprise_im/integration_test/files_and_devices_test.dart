// One shared scenario for real backend/host-core and installed native runs.
// Host runner substitutes only the OS vault/export destination; API/TLS, SQLite,
// FileTransfer/Download/Wire, device controllers and scanner/storage remain real.
import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:enterprise_im/core/auth/native_auth.dart';
import 'package:enterprise_im/core/auth/account_reference.dart';
import 'package:enterprise_im/core/auth/client_session.dart';
import 'package:enterprise_im/core/auth/session_store.dart';
import 'package:enterprise_im/core/storage/secure_vault.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/models.dart';
import 'package:enterprise_im/core/sync/sync_engine.dart';
import 'package:enterprise_im/features/memberships/membership_controller.dart';
import 'package:enterprise_im/features/conversations/conversation_controller.dart';
import 'package:enterprise_im/features/devices/device_controller.dart';
import 'package:enterprise_im/features/files/file_transfer.dart';
import 'package:enterprise_im/features/files/file_download.dart';
import 'package:enterprise_im/platform/browser_launcher.dart';
import 'package:enterprise_im/platform/file_picker.dart';
import 'package:enterprise_im/platform/file_export.dart';

final class HostFixtureVault implements SecureVault {
  final values = <String, String>{};
  @override
  Future<String?> read(String ns, String key) async => values['$ns/$key'];
  @override
  Future<void> write(String ns, String key, String value) async {
    values['$ns/$key'] = value;
  }

  @override
  Future<void> delete(String ns, String key) async {
    values.remove('$ns/$key');
  }
}

final class NoInteractiveBrowser implements BrowserLauncher {
  @override
  Future<bool> open(Uri _) =>
      throw StateError('refresh fixture cannot open a browser');
}

final class FixtureTrust extends HttpOverrides {
  final SecurityContext trust;
  FixtureTrust(this.trust);
  @override
  HttpClient createHttpClient(SecurityContext? _) =>
      super.createHttpClient(trust);
}

final class FixtureDestination implements FileExporter {
  final bytes = <int>[];
  @override
  Future<bool> save(
    String name,
    Stream<List<int>> Function() contents,
    Cancellation cancel,
  ) async {
    // Represents an explicit chosen destination; no system chooser is claimed.
    await for (final chunk in contents()) {
      cancel.check();
      bytes.addAll(chunk);
    }
    return true;
  }
}

void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  testWidgets('real backend files and own device lifecycle', (tester) async {
    await tester.runAsync(() => runFilesAndDevicesScenario(host: false));
  });
}

Future<void> runFilesAndDevicesScenario({required bool host}) async {
  const path = String.fromEnvironment('IM_FILES_FIXTURE');
  if (path.isEmpty)
    throw StateError('explicit owned real-backend config required; no skip');
  final config = objectMap(jsonDecode(await File(path).readAsString()));
  final trust = SecurityContext(withTrustedRoots: false)
    ..setTrustedCertificates(config['ca'] as String);
  await HttpOverrides.runWithHttpOverrides(() async {
    final origin = Uri.parse(config['origin'] as String);
    final api = ApiClient(origin, HttpApiTransport());
    final auth = NativeAuth(
      api,
      callbackFactory: (_, __) => throw StateError('refresh only'),
      browser: NoInteractiveBrowser(),
    );
    final root = Directory(config['storage'] as String);
    await root.create();
    final vault = host ? HostFixtureVault() : OsSecureVault();
    final sessions = SessionStore(vault, directory: root);
    final owner = ClientSession(auth, sessions, platform: 'macos');
    MembershipController? roles;
    ConversationController? sender;
    FileTransfer? transfer;
    FileDownload? download;
    DeviceController? devices;
    SyncEngine? engine;
    final account = AccountReference(
      origin: origin,
      tenantId: config['tenant'] as String,
      userId: config['user'] as String,
    );
    Future<void> control(
      String action, {
      String? fileId,
      int? runtimePid,
    }) async {
      final client = HttpClient();
      try {
        final r = await client.postUrl(
          Uri.parse('${config['control']}/$action'),
        );
        r.headers.set('X-Owned-Fixture', config['control_key'] as String);
        r.headers.contentType = ContentType.json;
        r.write(jsonEncode({'file_id': fileId, 'runtime_pid': runtimePid}));
        final result = await r.close();
        await result.drain<void>();
        expect(result.statusCode, 204);
      } finally {
        client.close(force: true);
      }
    }

    try {
      await control('runtime', runtimePid: pid);
      // Seed only a fresh, real broker+me-verified refresh result, then close and
      // restore through the production ClientSession boundary in a fresh generation.
      final seed = await auth.restore(
        account,
        'macos',
        config['refresh'] as String,
      );
      await sessions.open(seed);
      await sessions.saveCredentials(
        seed,
        refreshToken: auth.refreshCredential,
      );
      await sessions.close();
      await auth.cancel();
      final admitted = await owner.restore(account);
      expect(admitted.session.userId, config['user']);
      roles = MembershipController(owner);
      await roles.load();
      final ready = await roles.select(config['membership'] as String);
      final context = ClientContext(
        session: ready.client.session,
        actingMembershipId: ready.membership.id,
        device: ready.device,
        generation: ready.client.store.generation,
        conversationId: config['conversation'] as String,
      );
      final rows = await api.json(
        'GET',
        '/api/v1/conversations',
        bearer: context.session.accessToken,
        actingMembershipId: context.actingMembershipId,
      );
      final conversation = (rows['conversations'] as List)
          .map((r) => ConversationSummary.fromJson(objectMap(r)))
          .singleWhere((c) => c.id == context.conversationId);
      sender = ConversationController(roles);
      await sender.select(context, conversation);
      final files = FileTransfer(roles, sender);
      transfer = files;
      Future<FileStatus> upload(
        String name,
        String contents,
        String expected,
      ) async {
        final bytes = utf8.encode(contents);
        await files.reserveAndUpload(
          context,
          SelectedFile(
            name: name,
            mediaType: 'text/plain',
            size: bytes.length,
            open: () => Stream.value(bytes),
          ),
        );
        final deadline = Stopwatch()..start();
        while (files.current?.state != expected) {
          if (deadline.elapsed > const Duration(seconds: 90))
            throw StateError('actual scanner did not reach $expected');
          await Future<void>.delayed(const Duration(milliseconds: 100));
          await files.poll();
        }
        return files.current!;
      }

      const cleanBody = 'real native clean attachment\n';
      final clean = await upload('原生联调.txt', cleanBody, 'ready');
      final ack = await files.bindToMessage('真实附件说明');
      expect(ack.conversationId, context.conversationId);
      expect(await admitted.store.pendingUpload(), isNull);
      expect(await admitted.store.pendingSends(), isEmpty);
      engine = SyncEngine(api, admitted.store);
      final synced = await engine.syncConversation(context);
      final message = synced.messages.singleWhere((m) => m.seq == ack.seq);
      expect(message.messageType, 'file');
      expect(message.caption, '真实附件说明');
      expect(message.attachment!['file_id'], clean.id);
      final sink = FixtureDestination();
      download = FileDownload(roles, exporter: sink);
      Duration permission() =>
          synced.contentAuthorizationRemaining ?? Duration.zero;
      final artifact = await download.downloadAuthorized(
        context,
        message,
        authorizationRemaining: permission,
      );
      await control('settled');
      expect(await download.exportPrivateFile(context, artifact), isTrue);
      expect(sink.bytes, utf8.encode(cleanBody));
      await control('settled');
      await control('download-fault');
      await expectLater(
        download.downloadAuthorized(
          context,
          message,
          authorizationRemaining: permission,
        ),
        throwsA(
          isA<ApiFailure>()
              .having((e) => e.status, 'storage outage status', 503)
              .having(
                (e) => e.code,
                'storage outage code',
                'file_download_unavailable',
              ),
        ),
      );
      expect(
        root
            .listSync(recursive: true)
            .whereType<File>()
            .where((f) => f.path.endsWith('/content')),
        hasLength(1),
      );
      await control('download-healthy');
      await control('settled');
      await control('expire', fileId: clean.id);
      await expectLater(
        download.downloadAuthorized(
          context,
          message,
          authorizationRemaining: () => const Duration(minutes: 1),
        ),
        throwsA(
          isA<ApiFailure>()
              .having((e) => e.status, 'expired file status', 404)
              .having((e) => e.code, 'expired file code', 'not_found'),
        ),
      );
      await download.close();
      expect(
        root
            .listSync(recursive: true)
            .whereType<File>()
            .where((f) => f.path.endsWith('/content')),
        isEmpty,
      );
      download = null;
      final eicar = await upload(
        'eicar测试.txt',
        r'X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*',
        'rejected',
      );
      expect(eicar.state, 'rejected');
      await expectLater(
        files.bindToMessage('拒绝文件'),
        throwsA(isA<ApiFailure>()),
      );
      await files.cancel();
      await control('scan-fault');
      final failed = await upload(
        '扫描失败.txt',
        'scanner storage failure',
        'scan_failed',
      );
      expect(failed.state, 'scan_failed');
      await expectLater(
        files.bindToMessage('失败文件'),
        throwsA(isA<ApiFailure>()),
      );
      await files.cancel();
      await control('scan-healthy');
      devices = DeviceController(owner, memberships: roles);
      final caps = await devices.capabilities(ready.membership.id);
      expect(caps.maxActiveDevices, greaterThan(0));
      final page = await devices.list();
      expect(
        page.devices.any(
          (d) => d.id == context.device.deviceId && d.status == 'active',
        ),
        isTrue,
      );
      final revoked = await devices.revoke(context.device.deviceId);
      expect(revoked.stoppedOwnClient, isTrue);
      expect(owner.current, isNull);
      expect(roles.current, isNull);
      expect(root.listSync(recursive: true).whereType<File>(), isEmpty);
      if (vault is HostFixtureVault) expect(vault.values, isEmpty);
      await expectLater(
        api.json(
          'GET',
          '/api/v1/conversations/${context.conversationId}/sync',
          bearer: context.session.accessToken,
          actingMembershipId: context.actingMembershipId,
          device: context.device,
        ),
        throwsA(
          isA<ApiFailure>()
              .having((e) => e.status, 'revoked device status', 401)
              .having((e) => e.code, 'revoked device code', 'invalid_device'),
        ),
      );
      // ignore: avoid_print
      print(
        'NATIVE_FILES_REPORT ${jsonEncode({'status': 'passed', 'clean': true, 'eicar': true, 'scan_failed': true, 'download_failed': true, 'expired': true, 'revoked': true, 'os_vault_real': !host, 'installed_native_ui': false, 'native_export_real': false, 'runtime_pid': pid})}',
      );
    } finally {
      devices?.close();
      engine?.stop();
      await download?.close();
      await transfer?.close();
      sender?.close();
      roles?.close();
      await owner.logout();
      api.close();
    }
  }, FixtureTrust(trust));
}
