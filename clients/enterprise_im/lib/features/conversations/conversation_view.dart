import 'dart:async';
import 'package:flutter/foundation.dart';
import '../../core/protocol/api_client.dart';
import '../../core/protocol/models.dart';
import '../../core/protocol/seq.dart';
import '../../core/storage/records.dart';
import '../../core/sync/sync_engine.dart';
import '../../core/sync/sync_coordinator.dart';
import '../../core/sync/receipt_queue.dart';
import '../../core/sync/realtime_channel.dart';
import '../../platform/app_activity.dart';
import '../memberships/membership_controller.dart';
import 'conversation_controller.dart';
import '../files/file_transfer.dart';
import '../files/file_wire.dart';

final class ConversationView extends ChangeNotifier {
  final MembershipController memberships;
  final AppActivity activity;
  final bool sendAllowed;
  final void Function(SyncResult)? onAuthorizedPage;
  late final ConversationController sender;
  late final FileTransfer files;
  PendingUpload? _upload;
  late final SyncEngine engine;
  late final ReceiptQueue receipts;
  late final RealtimeChannel channel;
  late final SyncCoordinator coordinator;
  late final StreamSubscription<void> _activity;
  final _results = <Seq, SyncResult>{}, _redacted = <Seq, MessageDTO>{};
  final _summaries = <Seq, ReceiptSummary>{};
  List<PendingSend> _pending = const [];
  ClientContext? _context;
  WorkspaceAdmission? _admission;
  UnreadState? _unread;
  ApiFailure? _failure, _readFailure;
  Timer? _expiry, _metadataRetry;
  Cancellation? _metadataCancel;
  Future<void>? _metadataFlight, _closing;
  bool _metadataDirty = false,
      _disposed = false,
      _loading = false,
      _sending = false,
      _synced = false;
  int _epoch = 0, _metadataAuthority = 0;
  ConversationView(
    this.memberships,
    this.activity, {
    RealtimeConnector? connector,
    FileWireTransport? fileWire,
    this.sendAllowed = true,
    this.onAuthorizedPage,
  }) {
    final ready = memberships.current;
    if (ready == null) throw const ApiFailure('reauthenticate');
    final api = memberships.owner.auth.api;
    sender = ConversationController(memberships);
    files = FileTransfer(
      memberships,
      sender,
      wire: fileWire,
      onStateChanged: () {
        if (!_disposed) notifyListeners();
      },
      guard: () => _active && _synced && activity.foreground,
    );
    engine = SyncEngine(api, ready.client.store);
    receipts = ReceiptQueue(
      api,
      activity,
      onState: (_) {
        if (_active) {
          // A successful read ACK resolves only that read attempt's warning.
          // Preserve a newer failure from metadata or another operation.
          if (_readFailure != null && identical(_failure, _readFailure)) {
            _failure = null;
          }
          _readFailure = null;
          notifyListeners();
        }
        unawaited(refreshMetadata());
      },
      onFailure: (error) {
        if (!_active) return;
        _readFailure = error;
        _failed(error);
      },
    );
    channel = RealtimeChannel(api, connector ?? HttpRealtimeConnector());
    coordinator = SyncCoordinator(
      engine,
      channel,
      receipts,
      activity,
      onResult: _received,
      onFailure: (error) {
        // The coordinator has synchronously stopped page authority for these
        // terminal outcomes. A send/metadata error has different semantics.
        if (error.status == 401 ||
            error.status == 403 ||
            error.status == 404 ||
            error.status == 409 ||
            error.status == 410 ||
            const [
              'hard_deny',
              'invalid_context',
              'invalid_response',
              'cancelled',
            ].contains(error.code)) {
          _synced = false;
          _metadataAuthority++;
          _metadataCancel?.cancel();
          _summaries.clear();
          _unread = null;
          _prune();
          _armExpiry();
        }
        _failed(error);
      },
    );
    _activity = activity.changes.listen((_) {
      if (_disposed) return;
      if (!activity.foreground) {
        files.stopRequest();
        _synced = false;
        _metadataAuthority++;
        _metadataCancel?.cancel();
        _summaries.clear();
        _unread = null;
      }
      _prune();
      _armExpiry();
      notifyListeners();
    });
  }
  ClientContext? get context => _active ? _context : null;
  bool get _active =>
      !_disposed &&
      _context != null &&
      identical(memberships.current, _admission);
  bool get loading => _loading;
  bool get sending => _sending || files.busy;
  PendingUpload? get pendingUpload => _active ? _upload : null;
  bool get canManageFiles =>
      _active &&
      _synced &&
      activity.foreground &&
      !activity.blocked &&
      sendAllowed &&
      !_sending &&
      _pending.isEmpty;
  bool get canSend =>
      _active &&
      _synced &&
      activity.foreground &&
      !activity.blocked &&
      sendAllowed &&
      !_sending &&
      _pending.isEmpty &&
      _upload == null &&
      !files.busy;
  ApiFailure? get failure => _failure;
  UnreadState? get unread => _active ? _unread : null;
  Map<Seq, ReceiptSummary> get summaries =>
      _active ? Map.unmodifiable(_summaries) : const {};
  List<PendingSend> get pending =>
      _active ? List.unmodifiable(_pending) : const [];
  List<MessageDTO> get messages {
    if (!_active) return const [];
    final values = <Seq, MessageDTO>{..._redacted};
    for (final entry in _results.entries) {
      final row = entry.value.messages.where(
        (message) => message.seq == entry.key,
      );
      if (row.isNotEmpty) values[entry.key] = row.single;
    }
    final ordered = values.values.toList()
      ..sort((a, b) => a.seq.compareTo(b.seq));
    return List.unmodifiable(ordered);
  }

  Future<void> select(
    ClientContext context,
    ConversationSummary conversation,
  ) async {
    if (_disposed) throw const ApiFailure('cancelled');
    final epoch = ++_epoch;
    _context = null;
    _clear();
    _loading = true;
    notifyListeners();
    await coordinator.stop();
    if (_disposed || epoch != _epoch) return;
    await sender.select(context, conversation);
    if (_disposed || epoch != _epoch) return;
    _admission = memberships.current;
    _context = context;
    activity.update(
      foreground: activity.foreground,
      focused: activity.focused,
      activeConversation: conversation.id,
      blocked: activity.blocked,
    );
    await _loadPending(epoch);
    if (!_active || epoch != _epoch) return;
    try {
      await coordinator.start(context);
    } finally {
      if (!_disposed && epoch == _epoch) {
        _loading = false;
        notifyListeners();
      }
    }
  }

  Future<void> _loadPending(int epoch) async {
    final ready = _admission, context = _context;
    if (ready == null || context == null) return;
    final rows = await ready.client.store.pendingSends();
    final upload = await ready.client.store.pendingUpload();
    if (!_active || epoch != _epoch) return;
    _upload = upload;
    if (upload == null && _synced && activity.foreground && !files.busy) {
      await files.restore(context);
      if (!_active || epoch != _epoch) return;
    }
    _pending = List.unmodifiable(
      rows.where(
        (p) =>
            p.conversationId == context.conversationId &&
            p.actingMembershipId == context.actingMembershipId,
      ),
    );
  }

  void _received(SyncResult result) {
    final context = _context;
    if (!_active || context == null || !result.matchesContext(context)) return;
    for (final message in result.messages) {
      _redacted.remove(message.seq);
      _results[message.seq] = result;
    }
    _synced = true;
    _loading = false;
    _prune();
    _armExpiry();
    notifyListeners();
    unawaited(refreshMetadata());
    // Optional notification/UI decoration never changes sync or send authority.
    try {
      onAuthorizedPage?.call(result);
    } catch (_) {}
  }

  void _prune() {
    for (final entry in _results.entries.toList()) {
      final messages = entry.value.messages.where((m) => m.seq == entry.key);
      if (messages.isEmpty || messages.single.redacted) {
        _redacted[entry.key] = MessageDTO.fromJson({
          'seq': entry.key.toString(),
          'redacted': true,
        });
        _results.remove(entry.key);
        _summaries.remove(entry.key);
        receipts.forget(entry.key);
      }
    }
  }

  void _armExpiry() {
    _expiry?.cancel();
    _expiry = null;
    if (!_active) return;
    Duration? earliest;
    for (final result in _results.values.toSet()) {
      final remaining = result.contentAuthorizationRemaining;
      if (remaining != null && (earliest == null || remaining < earliest))
        earliest = remaining;
    }
    if (earliest == null) return;
    final epoch = _epoch;
    _expiry = Timer(earliest, () {
      if (!_active || epoch != _epoch) return;
      _prune();
      _armExpiry();
      notifyListeners();
    });
  }

  Future<void> refreshMetadata() {
    if (!_active ||
        !_synced ||
        !activity.foreground ||
        _metadataRetry?.isActive == true)
      return Future.value();
    if (_metadataFlight != null) {
      _metadataDirty = true;
      return _metadataFlight!;
    }
    final epoch = _epoch;
    final operation = _metadata(epoch);
    _metadataFlight = operation;
    unawaited(
      operation.whenComplete(() {
        if (identical(_metadataFlight, operation)) {
          _metadataFlight = null;
          if (_metadataDirty && _active) unawaited(refreshMetadata());
        }
      }),
    );
    return operation;
  }

  Future<void> _metadata(int epoch) async {
    do {
      _metadataDirty = false;
      final context = _context, authority = _metadataAuthority;
      if (!_active || !_synced || !activity.foreground || context == null)
        return;
      _summaries.clear();
      _unread = null;
      notifyListeners();
      final cancel = Cancellation();
      _metadataCancel = cancel;
      try {
        final api = memberships.owner.auth.api;
        Future<Map<String, Object?>> get(String suffix) => api.json(
          'GET',
          '/api/v1/conversations/${context.conversationId}/$suffix',
          bearer: context.session.accessToken,
          actingMembershipId: context.actingMembershipId,
          device: context.device,
          cancellation: cancel,
        );
        final seqs =
            messages
                // Receipt summaries are authorized only for the sender.
                .where(
                  (m) =>
                      !m.redacted && m.senderUserId == context.session.userId,
                )
                .map((m) => m.seq)
                .toList()
              ..sort();
        final requested = seqs.length > 100
            ? seqs.sublist(seqs.length - 100)
            : seqs;
        final values = await Future.wait([
          get('unread'),
          if (requested.isNotEmpty) get('receipts?seqs=${requested.join(',')}'),
        ]);
        if (!_active ||
            epoch != _epoch ||
            authority != _metadataAuthority ||
            !activity.foreground)
          return;
        final unread = UnreadState.fromJson(values[0]),
            summaries = <Seq, ReceiptSummary>{};
        if (requested.isNotEmpty) {
          final rows = values[1]['receipts'];
          if (rows is! List || rows.length != requested.length)
            throw const FormatException('回执列表无效');
          for (final raw in rows) {
            final summary = ReceiptSummary.fromJson(objectMap(raw));
            if (!requested.contains(summary.seq) ||
                summaries.containsKey(summary.seq))
              throw const FormatException('回执范围无效');
            summaries[summary.seq] = summary;
          }
        }
        _unread = unread;
        final live = messages
            .where((m) => !m.redacted)
            .map((m) => m.seq)
            .toSet();
        _summaries.addAll(
          Map.fromEntries(summaries.entries.where((e) => live.contains(e.key))),
        );
        notifyListeners();
      } on ApiFailure catch (error) {
        if (!_active ||
            epoch != _epoch ||
            authority != _metadataAuthority ||
            !activity.foreground)
          return;
        _failed(error);
        if (error.status == 429 && _active) {
          _metadataRetry?.cancel();
          _metadataRetry = Timer(
            error.retryAfter ?? const Duration(seconds: 1),
            () {
              _metadataRetry = null;
              unawaited(refreshMetadata());
            },
          );
        }
        return;
      } catch (_) {
        if (_active && epoch == _epoch && authority == _metadataAuthority)
          _failed(const ApiFailure('invalid_response'));
        return;
      } finally {
        cancel.cancel();
        if (identical(_metadataCancel, cancel)) _metadataCancel = null;
      }
    } while (_active && epoch == _epoch && _metadataDirty);
  }

  void _failed(ApiFailure error) {
    if (!_active) return;
    _failure = error;
    if (error.status == 401 ||
        error.status == 403 ||
        error.status == 404 ||
        error.code == 'hard_deny' ||
        error.code == 'invalid_device') {
      _epoch++;
      _context = null;
      sender.close();
      unawaited(files.close());
      _clear();
      engine.stop();
      receipts.contextChanged();
      // Queue the reset before another consumer can reopen this account scope.
      final reset = engine.store.clearReceivedState();
      unawaited(
        Future.wait([
          coordinator.stop(),
          reset,
        ]).catchError((Object _) => <void>[]),
      );
    }
    if (!_disposed) notifyListeners();
  }

  Duration fileAuthorizationRemaining(MessageDTO message) {
    if (!_active ||
        !activity.foreground ||
        !messages.any((m) => identical(m, message)) ||
        message.attachment?['available'] != true)
      return Duration.zero;
    return _results[message.seq]?.contentAuthorizationRemaining ??
        Duration.zero;
  }

  Future<void> reloadPending() async {
    final epoch = _epoch;
    await _loadPending(epoch);
    if (_active && epoch == _epoch) notifyListeners();
  }

  Future<void> bindFile(String caption) =>
      _send(() => files.bindToMessage(caption));
  Future<void> sendText(String text) => _send(() => sender.sendText(text));
  Future<void> retryPending(String id) => _send(() => sender.retryPending(id));
  Future<void> _send(Future<MessageACK> Function() operation) async {
    if (!_active ||
        !_synced ||
        !activity.foreground ||
        activity.blocked ||
        !sendAllowed ||
        _sending)
      throw const ApiFailure('invalid_context');
    final epoch = _epoch;
    _sending = true;
    _failure = null;
    notifyListeners();
    try {
      await operation();
      if (!_active || epoch != _epoch) throw const ApiFailure('cancelled');
      await coordinator.resume();
      unawaited(refreshMetadata());
    } on ApiFailure catch (error) {
      if (_active && epoch == _epoch) _failed(error);
      rethrow;
    } finally {
      if (_active && epoch == _epoch) {
        try {
          await _loadPending(epoch);
        } finally {
          if (!_disposed) {
            _sending = false;
            notifyListeners();
          }
        }
      }
    }
  }

  Future<void> abandonPending(String id) async {
    if (!_active || _sending) throw const ApiFailure('invalid_context');
    final epoch = _epoch;
    await sender.abandonPending(id);
    await _loadPending(epoch);
    if (_active && epoch == _epoch) notifyListeners();
  }

  Future<void> refresh() => coordinator.resume();
  void _clear() {
    _readFailure = null;
    _expiry?.cancel();
    _expiry = null;
    _metadataRetry?.cancel();
    _metadataRetry = null;
    _metadataAuthority++;
    _metadataDirty = false;
    _metadataCancel?.cancel();
    _loading = _sending = false;
    _results.clear();
    _redacted.clear();
    _summaries.clear();
    _pending = const [];
    _upload = null;
    _unread = null;
    _synced = false;
  }

  Future<void> close() {
    if (_closing != null) return _closing!;
    _disposed = true;
    _epoch++;
    _context = null;
    _clear();
    sender.close();
    final stoppedFiles = files.close();
    engine.stop();
    receipts.dispose();
    notifyListeners();
    super.dispose();
    return _closing = () async {
      await stoppedFiles;
      await coordinator.dispose();
      await channel.dispose();
      await _activity.cancel();
    }();
  }
}
