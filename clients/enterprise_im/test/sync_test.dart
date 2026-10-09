import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:sqlite3/sqlite3.dart';
import 'package:enterprise_im/core/auth/auth_session.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/models.dart';
import 'package:enterprise_im/core/protocol/seq.dart';
import 'package:enterprise_im/core/storage/encrypted_store.dart';
import 'package:enterprise_im/core/sync/sync_engine.dart';

const conversation = '00000000-0000-4000-8000-000000000001';
const membership = '00000000-0000-4000-8000-000000000002';
const user = '00000000-0000-4000-8000-000000000003';
final server = DateTime.utc(2026, 10, 9);
Map<String, Object?> syncPage({
  int seq = 1,
  bool gap = false,
  bool empty = false,
  String? proof,
}) => {
  'conversation_id': conversation,
  'messages': empty
      ? <Object?>[]
      : [
          {
            'seq': gap ? '2' : '$seq',
            'message_id': conversation,
            'sender_user_id': membership,
            'message_type': 'text',
            'text': 'recipient body',
            'server_time': server.toIso8601String(),
            'content_expires_at': server
                .add(const Duration(hours: 1))
                .toIso8601String(),
            'cache_until': server
                .add(const Duration(seconds: 300))
                .toIso8601String(),
          },
        ],
  'next_after_seq': gap ? '2' : '$seq',
  'receipt_revision': '1',
  'has_more': false,
  'server_time': server.toIso8601String(),
  'sync_proof': empty
      ? null
      : proof ?? base64Url.encode(List<int>.filled(32, 1)).replaceAll('=', ''),
  'proof_expires_at': empty
      ? null
      : server.add(const Duration(seconds: 300)).toIso8601String(),
};
Map<String, Object?> state({int scanned = 1, int read = 0}) => {
  'scanned_seq': '$scanned',
  'delivered_seq': '$scanned',
  'read_seq': '$read',
  'receipt_revision': '2',
  'duplicate': false,
};
ClientContext context({int generation = 1}) => ClientContext(
  session: AuthSession(
    origin: Uri.parse('https://im.example'),
    tenantId: conversation,
    userId: user,
    accessToken: 'memory-access',
    expiresAt: server.add(const Duration(hours: 1)),
    generation: generation,
    budget: const Duration(hours: 1),
  ),
  actingMembershipId: membership,
  device: DeviceGrant.fromJson({
    'device_id': user,
    'device_token': base64Url
        .encode(List<int>.filled(32, 2))
        .replaceAll('=', ''),
    'expires_at': server.add(const Duration(days: 1)).toIso8601String(),
    'server_time': server.toIso8601String(),
  }),
  generation: generation,
  conversationId: conversation,
);

final class ScriptedTransport implements ApiTransport {
  final FutureOr<ApiResponse> Function(ApiRequest) handle;
  final requests = <ApiRequest>[];
  ScriptedTransport(this.handle);
  @override
  Future<ApiResponse> send(
    ApiRequest request,
    Cancellation cancellation,
  ) async {
    requests.add(request);
    return await handle(request);
  }

  @override
  void close() {}
}

ApiResponse response(Map<String, Object?> body, {int status = 200}) =>
    ApiResponse(status, utf8.encode(jsonEncode(body)), {});
Future<(Directory, EncryptedStore)> storage() async {
  final root = await Directory.systemTemp.createTemp('im-sync-test-');
  final store = await EncryptedStore.open(
    'a' * 64,
    List<int>.filled(32, 5),
    directory: root,
    generation: 1,
  );
  addTearDown(() async {
    await store.close();
    await root.delete(recursive: true);
  });
  return (root, store);
}

void main() {
  test('testGapOrTransactionFailureDoesNotConfirmDelivery', () async {
    final (root, store) = await storage();
    var gap = true;
    final transport = ScriptedTransport(
      (req) => response(req.method == 'GET' ? syncPage(gap: gap) : state()),
    );
    final engine = SyncEngine(
      ApiClient(Uri.parse('https://im.example'), transport),
      store,
    );
    addTearDown(engine.stop);
    await expectLater(
      engine.syncConversation(context()),
      throwsA(isA<ApiFailure>()),
    );
    expect(transport.requests.where((r) => r.method == 'PUT'), isEmpty);
    expect(await store.committedCursor(conversation), isNull);
    gap = false;
    final db = sqlite3.open('${root.path}/${'a' * 64}.sqlite');
    addTearDown(db.close);
    db.execute(
      "CREATE TRIGGER fail_receipt BEFORE INSERT ON records WHEN NEW.kind='receipt' BEGIN SELECT RAISE(ABORT,'controlled failure'); END",
    );
    await expectLater(
      engine.syncConversation(context()),
      throwsA(isA<StoreFailure>()),
    );
    expect(transport.requests.where((r) => r.method == 'PUT'), isEmpty);
    db.execute('DROP TRIGGER fail_receipt');
    final result = await engine.syncConversation(context());
    expect(result.deliveredSeqs, [Seq.parse('1')]);
    expect(await store.pendingReceipts(), isEmpty);
    expect((await store.committedCursor(conversation)).toString(), '1');
    final request = transport.requests.last;
    expect(request.headers['X-Acting-Membership-ID'], membership);
    expect(request.headers['X-IM-Device-ID'], user);
    expect(request.headers['X-IM-Device-Token'], context().device.deviceToken);
  });
  test('testExpiredProofRefetchKeepsCursor', () async {
    final (_, store) = await storage();
    var gets = 0, puts = 0;
    final first = base64Url.encode(List<int>.filled(32, 1)).replaceAll('=', ''),
        second = base64Url.encode(List<int>.filled(32, 3)).replaceAll('=', '');
    final transport = ScriptedTransport((req) {
      if (req.method == 'GET') {
        gets++;
        expect(req.uri.queryParameters['after_seq'], '0');
        return response(syncPage(proof: gets == 1 ? first : second));
      }
      puts++;
      expect((requestBody(req))['sync_proof'], puts == 1 ? first : second);
      return puts == 1
          ? response({'error_code': 'sync_proof_expired'}, status: 410)
          : response(state());
    });
    final engine = SyncEngine(
      ApiClient(Uri.parse('https://im.example'), transport),
      store,
    );
    addTearDown(engine.stop);
    await expectLater(
      engine.syncConversation(context()),
      throwsA(isA<ApiFailure>()),
    );
    expect((await store.committedCursor(conversation)).toString(), '1');
    expect(await store.pendingReceipts(), hasLength(1));
    final result = await engine.syncConversation(context());
    expect(gets, 2);
    expect(result.deliveredSeqs, [Seq.parse('1')]);
    expect(await store.pendingReceipts(), isEmpty);
  });
  test(
    'unavailable delivery retains the exact pending proof without inventing read facts',
    () async {
      final (_, store) = await storage();
      var gets = 0, puts = 0;
      final proofs = <String>[];
      final transport = ScriptedTransport((req) {
        if (req.method == 'GET') {
          gets++;
          return response(syncPage());
        }
        puts++;
        proofs.add(requestBody(req)['sync_proof']! as String);
        return puts == 1
            ? response({'error_code': 'unavailable'}, status: 503)
            : response(state(read: 9));
      });
      final engine = SyncEngine(
        ApiClient(Uri.parse('https://im.example'), transport),
        store,
      );
      addTearDown(engine.stop);
      await expectLater(
        engine.syncConversation(context()),
        throwsA(isA<ApiFailure>()),
      );
      expect(await store.pendingReceipts(), hasLength(1));
      final result = await engine.syncConversation(context());
      expect(gets, 1);
      expect(proofs[0], proofs[1]);
      expect(result.deliveredSeqs, [Seq.parse('1')]);
      expect(result.locallyReadSeqs, isEmpty);
    },
  );
  test('stop cancels late page before persistence or delivery', () async {
    final (_, store) = await storage();
    final gate = Completer<ApiResponse>(), entered = Completer<void>();
    final transport = ScriptedTransport((req) {
      entered.complete();
      return gate.future;
    });
    final engine = SyncEngine(
      ApiClient(Uri.parse('https://im.example'), transport),
      store,
    );
    final running = engine.syncConversation(context());
    final check = expectLater(running, throwsA(isA<ApiFailure>()));
    await entered.future;
    engine.stop();
    gate.complete(response(syncPage()));
    await check;
    expect(await store.committedCursor(conversation), isNull);
    expect(transport.requests.where((r) => r.method == 'PUT'), isEmpty);
  });
}

Map<String, Object?> requestBody(ApiRequest request) =>
    Map<String, Object?>.from(jsonDecode(utf8.decode(request.body!)) as Map);
