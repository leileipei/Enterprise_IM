import 'dart:async';
import 'package:flutter_test/flutter_test.dart';
import 'package:sqlite3/sqlite3.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/seq.dart';
import 'package:enterprise_im/core/sync/sync_engine.dart';
import 'sync_test.dart' as fixture;

void main() {
  test('testRevokedDeviceAndIdentityStopQueues', () async {
    final (root, store) = await fixture.storage();
    var revoked = false;
    final transport = fixture.ScriptedTransport(
      (req) => revoked
          ? fixture.response({'error_code': 'invalid_device'}, status: 401)
          : fixture.response(
              req.method == 'GET' ? fixture.syncPage() : fixture.state(),
            ),
    );
    final engine = SyncEngine(
      ApiClient(Uri.parse('https://im.example'), transport),
      store,
    );
    addTearDown(engine.stop);
    final ctx = fixture.context(),
        result = await engine.syncConversation(fixture.context());
    expect(result.messages.single.redacted, false);
    revoked = true;
    await expectLater(engine.syncConversation(ctx), throwsA(isA<ApiFailure>()));
    expect(result.messages.single.redacted, true);
    final db = sqlite3.open('${root.path}/${'a' * 64}.sqlite');
    addTearDown(db.close);
    expect(
      db
          .select(
            "SELECT count(*) AS n FROM records WHERE kind IN ('message','receipt','cursor')",
          )
          .single['n'],
      0,
    );
  });
  test('testClientLeaseExpiryRedactsCachedContent', () async {
    final (root, store) = await fixture.storage();
    var elapsed = Duration.zero;
    final transport = fixture.ScriptedTransport(
      (req) => fixture.response(
        req.method == 'GET' ? fixture.syncPage() : fixture.state(),
      ),
    );
    final engine = SyncEngine(
      ApiClient(Uri.parse('https://im.example'), transport),
      store,
      now: () => elapsed,
    );
    addTearDown(engine.stop);
    final result = await engine.syncConversation(fixture.context());
    expect(result.messages.single.redacted, false);
    elapsed = const Duration(seconds: 300);
    expect(result.messages.single.redacted, true);
    await engine.purgeExpiredContent();
    final db = sqlite3.open('${root.path}/${'a' * 64}.sqlite');
    addTearDown(db.close);
    expect(
      db
          .select("SELECT count(*) AS n FROM records WHERE kind='message'")
          .single['n'],
      0,
    );
    expect((await store.committedCursor(fixture.conversation)).toString(), '1');
  });
  test('testRepeatedOutOfOrderReceiptAndPollingConverge', () async {
    final (_, store) = await fixture.storage();
    final gate = Completer<ApiResponse>(), entered = Completer<void>();
    var gets = 0, puts = 0;
    final transport = fixture.ScriptedTransport((req) {
      if (req.method == 'GET') {
        gets++;
        if (gets == 1) {
          entered.complete();
          return gate.future;
        }
        expect(req.uri.queryParameters['after_seq'], '1');
        return fixture.response(fixture.syncPage(empty: true));
      }
      puts++;
      return fixture.response(fixture.state());
    });
    final engine = SyncEngine(
      ApiClient(Uri.parse('https://im.example'), transport),
      store,
    );
    addTearDown(engine.stop);
    final ctx = fixture.context(),
        first = engine.syncConversation(fixture.context());
    await entered.future;
    final second = engine.syncConversation(ctx);
    expect(identical(first, second), true);
    gate.complete(fixture.response(fixture.syncPage()));
    await Future.wait([first, second]);
    await engine.syncConversation(ctx);
    expect(gets, 2);
    expect(puts, 1);
    expect(await store.pendingReceipts(), isEmpty);
  });
  test(
    'resume reauthorizes cached history without lowering committed cursor',
    () async {
      final (_, store) = await fixture.storage();
      final transport = fixture.ScriptedTransport(
        (request) => fixture.response(
          request.method == 'GET'
              ? fixture.syncPage(
                  empty: request.uri.queryParameters['after_seq'] != '0',
                )
              : fixture.state(),
        ),
      );
      final engine = SyncEngine(
        ApiClient(Uri.parse('https://im.example'), transport),
        store,
      );
      addTearDown(engine.stop);
      final old = await engine.syncConversation(fixture.context());
      expect(old.messages.single.redacted, isFalse);
      await engine.resume();
      final get = transport.requests.where((r) => r.method == 'GET').last;
      expect(get.uri.queryParameters['after_seq'], '0');
      expect(await store.committedCursor(fixture.conversation), Seq.parse('1'));
      expect(transport.requests.where((r) => r.method == 'PUT').length, 2);
      expect(old.messages.single.redacted, isTrue);
    },
  );
}
