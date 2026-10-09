import 'dart:async';
import '../../platform/app_activity.dart';
import '../protocol/api_client.dart';
import 'realtime_channel.dart';
import 'receipt_queue.dart';
import 'retry_policy.dart';
import 'sync_engine.dart';

// One active conversation owns all refresh/reconnect/purge timers. Socket
// notifications are hints: every state transition still uses authorized HTTP
// pages and commit-before-delivered. Platform/UI adapters own context admission.
final class SyncCoordinator {
  final SyncEngine engine;
  final RealtimeChannel channel;
  final ReceiptQueue receipts;
  final AppActivity activity;
  final RetryPolicy retry;
  final Timer Function(Duration, void Function()) schedule;
  final void Function(SyncResult) onResult;
  final void Function(ApiFailure)? onFailure;
  late final StreamSubscription<void> _activity;
  late final StreamSubscription<RealtimeSignal> _signals;
  late final StreamSubscription<ApiFailure> _failures;
  ClientContext? _context;
  Timer? _poll, _reconnect, _syncRetry, _purge;
  Future<void>? _refreshFlight, _connectFlight, _reset;
  bool _dirty = false, _suspended = true, _disposed = false, _purging = false;
  int _epoch = 0, _syncAttempt = 0, _connectAttempt = 0;
  SyncCoordinator(
    this.engine,
    this.channel,
    this.receipts,
    this.activity, {
    required this.onResult,
    this.onFailure,
    RetryPolicy? retry,
    Timer Function(Duration, void Function())? schedule,
  }) : retry = retry ?? RetryPolicy(),
       schedule = schedule ?? Timer.new {
    _activity = activity.changes.listen((_) {
      if (_context == null || _disposed) return;
      if (!_foreground) {
        if (!_suspended) unawaited(_pause());
      } else if (_suspended) {
        unawaited(_activate());
      }
    });
    _signals = channel.signals.listen((signal) {
      if (!_running) return;
      if (signal == RealtimeSignal.ready) {
        _connectAttempt = 0;
        _reconnect?.cancel();
        _reconnect = null;
        _armPoll();
        unawaited(_refresh());
      } else if (signal == RealtimeSignal.syncRequired) {
        unawaited(_refresh());
      } else {
        _armPoll();
      }
    });
    _failures = channel.failures.listen((error) {
      if (_running) _handle(error, connecting: true);
    });
  }
  bool get _foreground =>
      activity.foreground &&
      activity.activeConversation == _context?.conversationId;
  bool get _running =>
      !_disposed && !_suspended && _context != null && _foreground;
  bool _active(int epoch) => epoch == _epoch && _running;

  Future<void> start(ClientContext context) async {
    final stopping = stop();
    final epoch = _epoch;
    await stopping;
    if (_disposed || epoch != _epoch) return;
    _context = context;
    _syncAttempt = _connectAttempt = 0;
    if (_foreground) await _activate();
  }

  Future<void> _activate() async {
    if (_disposed || _context == null || !_foreground) return;
    _suspended = false;
    _armPoll();
    _armPurge();
    await Future.wait([_refresh(), _connect()]);
  }

  Future<void> resume() async {
    if (_context == null) throw const ApiFailure('invalid_context');
    if (!_foreground) return;
    if (_suspended) {
      await _activate();
    } else {
      await _refresh();
    }
  }

  Future<void> _refresh() {
    if (!_running || _syncRetry?.isActive == true) return Future.value();
    if (_refreshFlight != null) {
      _dirty = true;
      return _refreshFlight!;
    }
    final epoch = _epoch;
    final operation = _drain(epoch);
    _refreshFlight = operation;
    unawaited(
      operation.then((_) {
        if (epoch == _epoch && identical(_refreshFlight, operation))
          _refreshFlight = null;
      }),
    );
    return operation;
  }

  Future<void> _drain(int epoch) async {
    try {
      if (_reset != null) {
        try {
          await _reset;
        } catch (_) {
          throw const ApiFailure('invalid_response');
        }
      }
      if (!_active(epoch)) return;
      do {
        _dirty = false;
        final context = _context!;
        final result = await engine.syncConversation(context);
        if (!_active(epoch)) return;
        receipts.acceptDelivered(result, context);
        _syncAttempt = 0;
        onResult(result);
        _dirty = _dirty || result.hasMore;
      } while (_active(epoch) && _dirty);
    } on ApiFailure catch (error) {
      if (_active(epoch)) _handle(error, connecting: false);
    } catch (_) {
      if (_active(epoch))
        _handle(const ApiFailure('unavailable'), connecting: false);
    }
  }

  Future<void> _connect() {
    if (!_running) return Future.value();
    if (_connectFlight != null) return _connectFlight!;
    final epoch = _epoch;
    final operation = _open(epoch);
    _connectFlight = operation;
    unawaited(
      operation.then((_) {
        if (epoch == _epoch && identical(_connectFlight, operation))
          _connectFlight = null;
      }),
    );
    return operation;
  }

  Future<void> _open(int epoch) async {
    try {
      await channel.connect(_context!);
    } on ApiFailure catch (error) {
      if (_active(epoch)) _handle(error, connecting: true);
    } catch (_) {
      if (_active(epoch))
        _handle(const ApiFailure('unavailable'), connecting: true);
    }
  }

  void _handle(ApiFailure error, {required bool connecting}) {
    if (!_running) return;
    final decision = retry.next(
      error,
      connecting ? _connectAttempt++ : _syncAttempt++,
    );
    if (decision.action == RetryAction.reauthenticate ||
        decision.action == RetryAction.stop ||
        decision.action == RetryAction.conflict ||
        decision.action == RetryAction.expired) {
      // Invalidate all live page/read authority before invoking user callbacks.
      final stopped = stop();
      // Enqueue the received-state reset now, before any caller can start a
      // fresh generation. A slow socket close must never schedule a late erase.
      final reset = engine.store.clearReceivedState();
      _reset = reset;
      unawaited(
        Future.wait([stopped, reset]).then(
          (_) {
            if (identical(_reset, reset)) _reset = null;
          },
          onError: (Object _) {
            if (!_disposed) onFailure?.call(const ApiFailure('unavailable'));
          },
        ),
      );
      onFailure?.call(error);
      return;
    }
    final epoch = _epoch;
    final delay = decision.delay ?? const Duration(seconds: 1);
    if (connecting) {
      _reconnect?.cancel();
      _reconnect = schedule(delay, () {
        _reconnect = null;
        if (_active(epoch)) unawaited(_connect());
      });
    } else {
      _syncRetry?.cancel();
      _syncRetry = schedule(delay, () {
        _syncRetry = null;
        if (_active(epoch)) unawaited(_refresh());
      });
    }
  }

  void _armPoll() {
    _poll?.cancel();
    if (!_running) return;
    final epoch = _epoch;
    _poll = schedule(retry.pollInterval(connected: channel.connected), () {
      _poll = null;
      if (_active(epoch)) {
        unawaited(_refresh());
        _armPoll();
      }
    });
  }

  void _armPurge() {
    _purge?.cancel();
    if (!_running) return;
    final epoch = _epoch;
    _purge = schedule(const Duration(seconds: 1), () {
      _purge = null;
      if (!_active(epoch)) return;
      if (!_purging) {
        _purging = true;
        unawaited(
          engine.purgeExpiredContent().then(
            (_) {
              _purging = false;
            },
            onError: (Object _) {
              _purging = false;
              if (_active(epoch))
                _handle(
                  const ApiFailure('invalid_response'),
                  connecting: false,
                );
            },
          ),
        );
      }
      _armPurge();
    });
  }

  void _cancelTimers() {
    for (final timer in [_poll, _reconnect, _syncRetry, _purge]) {
      timer?.cancel();
    }
    _poll = _reconnect = _syncRetry = _purge = null;
  }

  Future<void> _pause() async {
    _epoch++;
    _suspended = true;
    _dirty = false;
    _cancelTimers();
    _refreshFlight = _connectFlight = null;
    engine.stop();
    receipts.contextChanged();
    await channel.stop();
  }

  Future<void> stop() {
    _context = null;
    return _pause();
  }

  Future<void> dispose() async {
    if (_disposed) return;
    _disposed = true;
    await stop();
    await _activity.cancel();
    await _signals.cancel();
    await _failures.cancel();
  }
}
