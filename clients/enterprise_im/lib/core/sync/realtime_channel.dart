import 'dart:async';
import 'dart:convert';
import 'dart:io';
import '../protocol/api_client.dart';
import '../protocol/models.dart';
import '../storage/records.dart';
import 'client_context.dart';

abstract interface class RealtimeConnection {
  String? get protocol;
  Stream<Object?> get frames;
  Future<void> close();
}

abstract interface class RealtimeConnector {
  Future<RealtimeConnection> connect(
    Uri uri,
    List<String> protocols,
    Cancellation cancellation,
  );
}

// The pinned Dart SDK calls only openUrl for the upgrade. The adapter binds
// that request to one exact origin/path and disables HTTP redirects before any
// ticket-bearing subprotocol is set. Unsupported methods fail closed.
final class _UpgradeClient implements HttpClient {
  final HttpClient client;
  final Uri expected;
  final Cancellation cancellation;
  _UpgradeClient(this.client, this.expected, this.cancellation);
  @override
  Future<HttpClientRequest> openUrl(String method, Uri url) async {
    cancellation.check();
    if (method != 'GET' ||
        url.scheme != expected.scheme ||
        url.host != expected.host ||
        url.port != expected.port ||
        url.path != expected.path ||
        url.userInfo.isNotEmpty ||
        url.query.isNotEmpty ||
        url.fragment.isNotEmpty) {
      throw const ApiFailure('invalid_context');
    }
    // Dart's upgrade creates explicit empty query/fragment components. Use the
    // previously validated canonical endpoint rather than forwarding those.
    final request = await client.openUrl(method, expected);
    if (cancellation.isCancelled) {
      request.abort();
      cancellation.check();
    }
    request.followRedirects = false;
    request.maxRedirects = 0;
    return request;
  }

  @override
  dynamic noSuchMethod(Invocation invocation) =>
      throw const ApiFailure('invalid_context');
}

final class _SocketConnection implements RealtimeConnection {
  final WebSocket socket;
  final HttpClient client;
  final Completer<void> finished;
  Future<void>? _closing;
  _SocketConnection(this.socket, this.client, this.finished);
  @override
  String? get protocol => socket.protocol;
  @override
  Stream<Object?> get frames => socket;
  @override
  Future<void> close() => _closing ??= _close();
  Future<void> _close() async {
    client.close(force: true);
    try {
      await socket.close(WebSocketStatus.goingAway);
    } finally {
      if (!finished.isCompleted) finished.complete();
    }
  }
}

final class HttpRealtimeConnector implements RealtimeConnector {
  final HttpClient Function() createClient;
  HttpRealtimeConnector({HttpClient Function()? createClient})
    : createClient = createClient ?? HttpClient.new;
  @override
  Future<RealtimeConnection> connect(
    Uri uri,
    List<String> protocols,
    Cancellation cancellation,
  ) async {
    cancellation.check();
    if (uri.scheme != 'wss' ||
        uri.host.isEmpty ||
        uri.userInfo.isNotEmpty ||
        uri.path != '/api/v1/realtime' ||
        uri.hasQuery ||
        uri.hasFragment ||
        protocols.length != 2 ||
        protocols[0] != 'enterprise-im.v1' ||
        !protocols[1].startsWith('ticket.') ||
        !proofValid(protocols[1].substring(7))) {
      throw const ApiFailure('invalid_context');
    }
    final client = createClient()
      ..connectionTimeout = const Duration(seconds: 10);
    final finished = Completer<void>();
    WebSocket? socket;
    unawaited(
      Future.any([cancellation.whenCancelled, finished.future]).then((_) async {
        if (cancellation.isCancelled) {
          client.close(force: true);
          await socket?.close(WebSocketStatus.goingAway);
        }
      }),
    );
    try {
      socket =
          await WebSocket.connect(
            uri.toString(),
            protocols: protocols,
            compression: CompressionOptions.compressionOff,
            maxPayloadLength: 4096,
            customClient: _UpgradeClient(
              client,
              uri.replace(scheme: 'https'),
              cancellation,
            ),
          ).timeout(
            const Duration(seconds: 15),
            onTimeout: () {
              cancellation.cancel();
              throw const ApiFailure('timeout');
            },
          );
      if (cancellation.isCancelled) {
        await socket.close(WebSocketStatus.goingAway);
        cancellation.check();
      }
      socket.pingInterval = const Duration(seconds: 15);
      return _SocketConnection(socket, client, finished);
    } catch (error) {
      client.close(force: true);
      if (!finished.isCompleted) finished.complete();
      if (error is ApiFailure) rethrow;
      final status = error is WebSocketException ? error.httpStatusCode : null;
      throw ApiFailure(
        status != null && status >= 300 && status < 400
            ? 'redirect_rejected'
            : 'unavailable',
        status: status,
      );
    }
  }
}

enum RealtimeSignal { ready, syncRequired, disconnected }

final class RealtimeChannel {
  final ApiClient api;
  final RealtimeConnector connector;
  final Duration Function() now;
  final _signals = StreamController<RealtimeSignal>.broadcast(sync: true);
  final _failures = StreamController<ApiFailure>.broadcast(sync: true);
  Stream<RealtimeSignal> get signals => _signals.stream;
  Stream<ApiFailure> get failures => _failures.stream;
  Cancellation? _cancel;
  RealtimeConnection? _connection;
  StreamSubscription<Object?>? _subscription;
  Timer? _readyTimer;
  int _epoch = 0;
  bool _disposed = false;
  bool connected = false;
  RealtimeChannel(this.api, this.connector, {Duration Function()? now})
    : now = now ?? LeaseClock.monotonicNow;
  void _check(ClientContext context, int epoch) {
    if (_disposed || epoch != _epoch) throw const ApiFailure('cancelled');
    if (context.session.origin != api.origin ||
        context.session.generation != context.generation)
      throw const ApiFailure('invalid_context');
    if (context.session.isExpired)
      throw const ApiFailure('reauthenticate', status: 401);
  }

  Future<void> connect(ClientContext context) async {
    final stopping = stop();
    final epoch = _epoch;
    await stopping;
    _check(context, epoch);
    final cancellation = Cancellation();
    _cancel = cancellation;
    try {
      final began = now();
      final raw = await api.json(
        'POST',
        '/api/v1/realtime/tickets',
        bearer: context.session.accessToken,
        actingMembershipId: context.actingMembershipId,
        cancellation: cancellation,
      );
      _check(context, epoch);
      final ticket = raw['ticket'], ttl = raw['expires_in_seconds'];
      if (ticket is! String ||
          !proofValid(ticket) ||
          ttl is! int ||
          ttl < 1 ||
          ttl > 30)
        throw const ApiFailure('invalid_response');
      final elapsed = now() - began;
      if (elapsed < Duration.zero || elapsed >= Duration(seconds: ttl))
        throw const ApiFailure('ticket_expired');
      final connection = await connector.connect(
        api.origin.replace(scheme: 'wss', path: '/api/v1/realtime'),
        ['enterprise-im.v1', 'ticket.$ticket'],
        cancellation,
      );
      if (epoch != _epoch || _disposed) {
        await connection.close();
        throw const ApiFailure('cancelled');
      }
      _connection = connection;
      _check(context, epoch);
      if (connection.protocol != 'enterprise-im.v1')
        throw const ApiFailure('invalid_response');
      _readyTimer = Timer(
        const Duration(seconds: 5),
        () => _fail(const ApiFailure('timeout'), epoch),
      );
      _subscription = connection.frames.listen(
        (frame) {
          if (epoch != _epoch || _disposed) return;
          try {
            _check(context, epoch);
            if (frame is! String || utf8.encode(frame).length > 4096)
              throw const ApiFailure('invalid_response');
            final value = jsonDecode(frame);
            if (value is! Map<String, Object?>)
              throw const ApiFailure('invalid_response');
            if (!connected &&
                value.length == 2 &&
                value['type'] == 'ready' &&
                value['resync_required'] == true) {
              connected = true;
              _readyTimer?.cancel();
              _signals.add(RealtimeSignal.ready);
            } else if (connected &&
                value.length == 1 &&
                value['type'] == 'sync_required') {
              _signals.add(RealtimeSignal.syncRequired);
            } else {
              throw const ApiFailure('invalid_response');
            }
          } catch (error) {
            _fail(
              error is ApiFailure
                  ? error
                  : const ApiFailure('invalid_response'),
              epoch,
            );
          }
        },
        onError: (Object _) => _fail(const ApiFailure('unavailable'), epoch),
        onDone: () => _fail(const ApiFailure('unavailable'), epoch),
      );
    } catch (error) {
      if (epoch == _epoch) await stop();
      if (error is ApiFailure) rethrow;
      throw const ApiFailure('unavailable');
    }
  }

  void _fail(ApiFailure error, int epoch) {
    if (epoch != _epoch || _disposed) return;
    // stop changes epoch and invalidates connection synchronously before callbacks.
    unawaited(stop());
    _failures.add(error);
  }

  Future<void> stop() async {
    _epoch++;
    _cancel?.cancel();
    _cancel = null;
    _readyTimer?.cancel();
    _readyTimer = null;
    final cancelledSubscription = _subscription?.cancel();
    final connection = _connection;
    _subscription = null;
    _connection = null;
    final wasConnected = connected;
    connected = false;
    if (wasConnected && !_signals.isClosed)
      _signals.add(RealtimeSignal.disconnected);
    await cancelledSubscription;
    await connection?.close();
  }

  Future<void> dispose() async {
    if (_disposed) return;
    _disposed = true;
    await stop();
    await _signals.close();
    await _failures.close();
  }
}
