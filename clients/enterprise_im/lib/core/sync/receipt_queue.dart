import 'dart:async';
import '../../platform/app_activity.dart';
import '../protocol/api_client.dart';
import '../protocol/models.dart';
import '../protocol/seq.dart';
import 'sync_engine.dart';

final class _Exposure {
  final Seq seq;
  final ClientContext context;
  double ratio;
  bool connected, preview, qualified = false;
  Timer? timer;
  _Exposure(this.seq, this.context, this.ratio, this.connected, this.preview);
}

final class ReceiptQueue {
  final ApiClient api;
  final AppActivity activity;
  final void Function(ReceiptState)? onState;
  final void Function(ApiFailure)? onFailure;
  late final StreamSubscription<void> _activity;
  final _authority = <Seq, SyncResult>{};
  final _observed = <Seq, _Exposure>{};
  final _pending = <Seq>{}, _read = <Seq>{};
  ClientContext? _context;
  int _epoch = 0;
  bool _reading = false;
  Timer? _batch;
  Cancellation? _cancel;
  int _retrySeconds = 1;
  bool _disposed = false;
  ReceiptQueue(this.api, this.activity, {this.onState, this.onFailure}) {
    _activity = activity.changes.listen((_) {
      _interrupt();
      for (final exposure in _observed.values) {
        _arm(exposure);
      }
    });
  }
  List<Seq> get locallyReadSeqs => List.unmodifiable(_read.toList()..sort());
  bool _same(ClientContext context) =>
      !_disposed &&
      _context?.sameScope(context) == true &&
      context.session.origin == api.origin &&
      !context.session.isExpired;
  bool _front(ClientContext context) =>
      _same(context) &&
      activity.foreground &&
      activity.focused &&
      !activity.blocked &&
      activity.activeConversation == context.conversationId;
  bool _eligible(_Exposure exposure) =>
      exposure.connected &&
      !exposure.preview &&
      exposure.ratio >= .5 &&
      _front(exposure.context) &&
      !_read.contains(exposure.seq) &&
      _authority[exposure.seq]?.matchesContext(exposure.context) == true &&
      _authority[exposure.seq]?.canRead(exposure.seq) == true;

  // SyncResult can only be produced after complete page commit and the server's
  // device delivery acknowledgment. A user's read_seq is never per-device proof.
  void acceptDelivered(SyncResult result, ClientContext context) {
    if (!result.matchesContext(context) || context.session.origin != api.origin)
      throw const ApiFailure('invalid_context');
    if (_context != null && !_context!.sameScope(context)) contextChanged();
    _context = context;
    for (final message in result.messages) {
      if (!message.redacted &&
          message.senderUserId != context.session.userId &&
          result.deliveredSeqs.contains(message.seq)) {
        _authority[message.seq] = result;
      } else {
        _authority.remove(message.seq);
        _pending.remove(message.seq);
        final exposure = _observed[message.seq];
        exposure?.timer?.cancel();
        if (exposure != null) exposure.qualified = false;
      }
    }
    for (final exposure in _observed.values) {
      _arm(exposure);
    }
  }

  void observe(
    Seq seq,
    double ratio,
    ClientContext context, {
    bool connected = true,
    bool preview = false,
  }) {
    if (seq.value == BigInt.zero || !ratio.isFinite || ratio < 0 || ratio > 1)
      throw const FormatException('可见区域无效');
    if (!_same(context)) return;
    var exposure = _observed[seq];
    if (exposure == null || !exposure.context.sameScope(context)) {
      exposure?.timer?.cancel();
      exposure = _Exposure(seq, context, ratio, connected, preview);
      _observed[seq] = exposure;
    } else {
      exposure.ratio = ratio;
      exposure.connected = connected;
      exposure.preview = preview;
    }
    _arm(exposure);
  }

  void forget(Seq seq) {
    _observed.remove(seq)?.timer?.cancel();
    _pending.remove(seq);
    _authority.remove(seq);
  }

  void _arm(_Exposure exposure) {
    if (!_eligible(exposure)) {
      exposure.timer?.cancel();
      exposure.timer = null;
      exposure.qualified = false;
      _pending.remove(exposure.seq);
      return;
    }
    if (exposure.timer != null || exposure.qualified) return;
    final epoch = _epoch;
    exposure.timer = Timer(const Duration(milliseconds: 500), () {
      exposure.timer = null;
      if (epoch == _epoch && _eligible(exposure)) {
        exposure.qualified = true;
        confirmVisible([exposure.seq], exposure.context);
      }
    });
  }

  void confirmVisible(List<Seq> seqs, ClientContext context) {
    if (!_front(context)) return;
    for (final seq in seqs) {
      final exposure = _observed[seq];
      if (exposure != null && exposure.qualified && _eligible(exposure))
        _pending.add(seq);
    }
    if (_pending.isNotEmpty && _batch == null && !_reading)
      _schedule(context, const Duration(milliseconds: 300));
  }

  void _schedule(ClientContext context, Duration delay) {
    final epoch = _epoch;
    _batch = Timer(delay, () {
      _batch = null;
      if (epoch == _epoch) unawaited(_flush(context, epoch));
    });
  }

  Future<void> _flush(ClientContext context, int epoch) async {
    if (!_front(context) || _reading) return;
    final seqs = _pending.where((seq) {
      final exposure = _observed[seq];
      return exposure != null && exposure.qualified && _eligible(exposure);
    }).toList()..sort();
    if (seqs.isEmpty) return;
    final batch = seqs.take(500).toList(growable: false),
        cancel = Cancellation();
    _reading = true;
    _cancel = cancel;
    Duration? retry;
    try {
      final raw = await api.json(
        'PUT',
        '/api/v1/conversations/${context.conversationId}/read',
        body: {
          'seqs': batch.map((seq) => seq.toString()).toList(growable: false),
        },
        bearer: context.session.accessToken,
        actingMembershipId: context.actingMembershipId,
        device: context.device,
        cancellation: cancel,
      );
      if (epoch != _epoch || !_same(context)) return;
      final state = ReceiptState.fromJson(raw);
      // No scalar user cursor grants unread page elements. Only the exact
      // successful request batch becomes this device's observed read set.
      for (final seq in batch) {
        _read.add(seq);
        _pending.remove(seq);
      }
      _retrySeconds = 1;
      onState?.call(state);
    } on ApiFailure catch (error) {
      if (epoch != _epoch) return;
      if (error.status == 401 ||
          error.status == 403 ||
          error.status == 404 ||
          error.code == 'invalid_device' ||
          error.code == 'hard_deny') {
        for (final result in _authority.values) {
          result.invalidateReading();
        }
        contextChanged();
        onFailure?.call(error);
        return;
      }
      retry = error.retryAfter ?? Duration(seconds: _retrySeconds);
      _retrySeconds = (_retrySeconds * 2).clamp(1, 30);
      onFailure?.call(error);
    } catch (_) {
      if (epoch != _epoch) return;
      retry = Duration(seconds: _retrySeconds);
      _retrySeconds = (_retrySeconds * 2).clamp(1, 30);
      onFailure?.call(const ApiFailure('invalid_response'));
    } finally {
      cancel.cancel();
      if (epoch == _epoch) {
        _reading = false;
        _cancel = null;
        for (final exposure in _observed.values) {
          _arm(exposure);
        }
        if (_pending.isNotEmpty && _front(context))
          _schedule(context, retry ?? const Duration(milliseconds: 300));
      }
    }
  }

  void _interrupt() {
    _epoch++;
    _cancel?.cancel();
    _cancel = null;
    _reading = false;
    _batch?.cancel();
    _batch = null;
    _pending.clear();
    for (final exposure in _observed.values) {
      exposure.timer?.cancel();
      exposure.timer = null;
      exposure.qualified = false;
    }
  }

  void contextChanged() {
    _interrupt();
    _context = null;
    _authority.clear();
    _observed.clear();
    _read.clear();
    _retrySeconds = 1;
  }

  void dispose() {
    _disposed = true;
    contextChanged();
    unawaited(_activity.cancel());
  }
}
