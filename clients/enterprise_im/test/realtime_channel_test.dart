import 'dart:async';
import 'dart:convert';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/sync/realtime_channel.dart';
import 'sync_test.dart' as fixture;

final class SocketFixture implements RealtimeConnection {
  final controller = StreamController<Object?>.broadcast();
  bool closed = false;
  Completer<void>? closeGate;
  final closedDone = Completer<void>();
  @override
  String? protocol = 'enterprise-im.v1';
  @override
  Stream<Object?> get frames => controller.stream;
  @override
  Future<void> close() async {
    closed = true;
    await closeGate?.future;
    await controller.close();
    if (!closedDone.isCompleted) closedDone.complete();
  }
}

final class DialFixture implements RealtimeConnector {
  var socket = SocketFixture();
  Uri? uri;
  List<String>? protocols;
  @override
  Future<RealtimeConnection> connect(
    Uri uri,
    List<String> protocols,
    Cancellation cancel,
  ) async {
    if (socket.closed) socket = SocketFixture();
    this.uri = uri;
    this.protocols = protocols;
    return socket;
  }
}

final ticket = base64Url.encode(List<int>.filled(32, 4)).replaceAll('=', '');
void main() {
  test(
    'ticket only enters same origin subprotocol and notices require ready',
    () async {
      final dial = DialFixture();
      final transport = fixture.ScriptedTransport((request) {
        expect(request.method, 'POST');
        expect(
          request.uri.toString(),
          'https://im.example/api/v1/realtime/tickets',
        );
        expect(request.body, isNull);
        expect(request.headers['X-Acting-Membership-ID'], fixture.membership);
        expect(request.headers['Authorization'], 'Bearer memory-access');
        return fixture.response({'ticket': ticket, 'expires_in_seconds': 30});
      });
      final channel = RealtimeChannel(
        ApiClient(Uri.parse('https://im.example'), transport),
        dial,
      );
      final signals = <RealtimeSignal>[];
      final errors = <ApiFailure>[];
      final sub = channel.signals.listen(signals.add);
      final failed = channel.failures.listen(errors.add);
      addTearDown(() async {
        await channel.dispose();
        await sub.cancel();
        await failed.cancel();
      });
      await channel.connect(fixture.context());
      expect(dial.uri.toString(), 'wss://im.example/api/v1/realtime');
      expect(dial.protocols, ['enterprise-im.v1', 'ticket.$ticket']);
      expect(channel.connected, isFalse);
      dial.socket.controller.add('{"type":"ready","resync_required":true}');
      dial.socket.controller.add('{"type":"sync_required"}');
      await Future<void>.delayed(Duration.zero);
      expect(signals, [RealtimeSignal.ready, RealtimeSignal.syncRequired]);
      expect(channel.connected, isTrue);
      expect(errors, isEmpty);
      await channel.stop();
      expect(channel.connected, isFalse);
      expect(dial.socket.closed, isTrue);
    },
  );
  test('cancelled late ticket never connects old generation', () async {
    final pending = Completer<ApiResponse>();
    final dial = DialFixture();
    final channel = RealtimeChannel(
      ApiClient(
        Uri.parse('https://im.example'),
        fixture.ScriptedTransport((_) => pending.future),
      ),
      dial,
    );
    addTearDown(channel.dispose);
    final attempt = channel.connect(fixture.context());
    final rejected = expectLater(attempt, throwsA(isA<ApiFailure>()));
    await channel.stop();
    pending.complete(
      fixture.response({'ticket': ticket, 'expires_in_seconds': 30}),
    );
    await rejected;
    expect(dial.uri, isNull);
    await dial.socket.close();
  });
  test('expired ticket never opens socket', () async {
    var time = Duration.zero;
    final dial = DialFixture();
    final channel = RealtimeChannel(
      ApiClient(
        Uri.parse('https://im.example'),
        fixture.ScriptedTransport((_) {
          time = const Duration(seconds: 30);
          return fixture.response({'ticket': ticket, 'expires_in_seconds': 30});
        }),
      ),
      dial,
      now: () => time,
    );
    addTearDown(channel.dispose);
    await expectLater(
      channel.connect(fixture.context()),
      throwsA(isA<ApiFailure>()),
    );
    expect(dial.uri, isNull);
    await dial.socket.close();
  });
  test('body-bearing or before-ready frames fail closed', () async {
    for (final frame in [
      '{"type":"sync_required"}',
      '{"type":"ready","resync_required":true,"text":"secret"}',
      '{"type":"ready","resync_required":false}',
    ]) {
      final dial = DialFixture();
      final channel = RealtimeChannel(
        ApiClient(
          Uri.parse('https://im.example'),
          fixture.ScriptedTransport(
            (_) =>
                fixture.response({'ticket': ticket, 'expires_in_seconds': 30}),
          ),
        ),
        dial,
      );
      final failed = Completer<ApiFailure>();
      final sub = channel.failures.listen((e) {
        if (!failed.isCompleted) failed.complete(e);
      });
      await channel.connect(fixture.context());
      dial.socket.controller.add(frame);
      expect((await failed.future).code, 'invalid_response');
      expect(channel.connected, isFalse);
      await channel.dispose();
      await sub.cancel();
    }
  });
}
