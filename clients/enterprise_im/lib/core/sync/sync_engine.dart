import 'dart:async';
import '../protocol/api_client.dart';
import '../protocol/models.dart';
import '../protocol/seq.dart';
import '../storage/encrypted_store.dart';
import '../storage/records.dart';
import 'client_context.dart';
export 'client_context.dart';

final class SyncResult {
  final SyncPage _page;
  final LeaseClock _clock;
  final ClientContext _context;
  final bool Function() _active;
  final List<Seq> deliveredSeqs, locallyReadSeqs;
  final ReceiptState? receiptState;
  bool get hasMore => _page.hasMore;
  Seq get nextAfterSeq => _page.nextAfterSeq;
  SyncResult._(
    this._page,
    this._clock,
    this._context,
    this._active,
    List<Seq> delivered,
    this.receiptState,
  ) : deliveredSeqs = List.unmodifiable(delivered),
      locallyReadSeqs = const [];
  void invalidateReading() => _clock.invalidate();
  bool matchesContext(ClientContext context) =>
      _active() && _context.sameScope(context);
  bool canRead(Seq seq) =>
      messages.any((message) => message.seq == seq && !message.redacted);
  List<MessageDTO> get messages => List.unmodifiable(
    _page.messages.map((message) {
      if (message.redacted) return message;
      if (!_active() || _context.session.isExpired)
        return MessageDTO.fromJson({
          'seq': message.seq.toString(),
          'redacted': true,
        });
      final until = message.cacheUntil == _page.serverTime
          ? message.onlineUntil
          : message.cacheUntil;
      if (until == null ||
          _clock.remainingFor(until) == Duration.zero ||
          _clock.remainingFor(message.contentExpiresAt!) == Duration.zero) {
        return MessageDTO.fromJson({
          'seq': message.seq.toString(),
          'redacted': true,
        });
      }
      return message;
    }),
  );
}

final class SyncEngine {
  final ApiClient api;
  final EncryptedStore store;
  final Duration Function() now;
  ClientContext? _context;
  Cancellation? _cancel;
  Future<SyncResult>? _flight;
  SyncPage? _pendingPage;
  LeaseClock? _pendingClock;
  Seq? _after;
  Future<void>? _reset;
  int _epoch = 0;
  final _delivered = <Seq>{};
  SyncEngine(this.api, this.store, {Duration Function()? now})
    : now = now ?? LeaseClock.monotonicNow;
  void _check(ClientContext context, int epoch) {
    if (epoch != _epoch || _context == null || !_context!.sameScope(context))
      throw const ApiFailure('cancelled');
    if (context.session.isExpired)
      throw const ApiFailure('reauthenticate', status: 401);
    if (context.session.origin != api.origin ||
        context.generation != store.generation)
      throw const ApiFailure('invalid_context');
  }

  Future<SyncResult> syncConversation(ClientContext context) {
    if (_context != null && !_context!.sameScope(context)) stop();
    _context = context;
    if (_flight != null) return _flight!;
    final epoch = _epoch;
    final cancellation = Cancellation();
    _cancel = cancellation;
    final operation = _sync(context, epoch, cancellation);
    _flight = operation;
    unawaited(
      operation.then<void>(
        (_) {
          if (epoch == _epoch) _flight = null;
        },
        onError: (Object _, StackTrace __) {
          if (epoch == _epoch) _flight = null;
        },
      ),
    );
    return operation;
  }

  Future<SyncResult> _sync(
    ClientContext context,
    int epoch,
    Cancellation cancel,
  ) async {
    try {
      if (_reset != null) await _reset;
      _check(context, epoch);
      if (_after == null) {
        final pending = await store.pendingReceipts();
        _check(context, epoch);
        // A restarted unconfirmed page must be reauthorized. Replaying from0
        // avoids guessing an old proof's page size or bypassing server gaps.
        _after =
            pending.any(
              (receipt) => receipt.conversationId == context.conversationId,
            )
            ? Seq.parse('0')
            : await store.committedCursor(context.conversationId) ??
                  Seq.parse('0');
        _check(context, epoch);
      }
      var page = _pendingPage, clock = _pendingClock;
      if (page == null ||
          clock == null ||
          clock.remainingFor(page.proofExpiresAt!) == Duration.zero) {
        final began = now();
        final raw = await api.json(
          'GET',
          '/api/v1/conversations/${context.conversationId}/sync?after_seq=$_after&limit=100',
          bearer: context.session.accessToken,
          actingMembershipId: context.actingMembershipId,
          device: context.device,
          cancellation: cancel,
        );
        _check(context, epoch);
        try {
          page = SyncPage.fromJson(raw, after: _after!);
          if (page.conversationId != context.conversationId)
            throw const FormatException('会话不匹配');
          var deadline = page.proofExpiresAt ?? page.serverTime;
          for (final message in page.messages) {
            if (!message.redacted && message.cacheUntil!.isAfter(deadline))
              deadline = message.cacheUntil!;
          }
          clock = LeaseClock.start(page.serverTime, began, deadline, now: now);
        } on FormatException {
          throw const ApiFailure('invalid_response');
        }
        if (page.messages.isNotEmpty) {
          final pending = PendingReceipt(
            conversationId: context.conversationId,
            syncProof: page.syncProof!,
            nextAfterSeq: page.nextAfterSeq,
            proofExpiresAt: page.proofExpiresAt!,
            generation: context.generation,
          );
          await store.commitPage(
            page,
            pending,
            actingMembershipId: context.actingMembershipId,
            clock: clock,
          );
          _check(context, epoch);
          _pendingPage = page;
          _pendingClock = clock;
        }
      }
      ReceiptState? state;
      if (page.syncProof != null) {
        final raw = await api.json(
          'PUT',
          '/api/v1/conversations/${context.conversationId}/delivered',
          body: {'sync_proof': page.syncProof},
          bearer: context.session.accessToken,
          actingMembershipId: context.actingMembershipId,
          device: context.device,
          cancellation: cancel,
        );
        _check(context, epoch);
        try {
          state = ReceiptState.fromJson(raw);
          if (state.scannedSeq.compareTo(page.nextAfterSeq) < 0)
            throw const FormatException('扫描不足');
        } on FormatException {
          throw const ApiFailure('invalid_response');
        }
        await store.acknowledgePage(context.conversationId, page.nextAfterSeq);
        _check(context, epoch);
        for (final message in page.messages) {
          if (!message.redacted &&
              message.senderUserId != context.session.userId)
            _delivered.add(message.seq);
        }
      }
      _after = page.nextAfterSeq;
      _pendingPage = null;
      _pendingClock = null;
      return SyncResult._(
        page,
        clock,
        context,
        () => epoch == _epoch && _context?.sameScope(context) == true,
        _delivered.toList()..sort(),
        state,
      );
    } on ApiFailure catch (error) {
      if (epoch == _epoch) {
        if (error.code == 'sync_proof_expired') {
          _pendingPage = null;
          _pendingClock = null;
        }
        if (error.status == 401 ||
            error.status == 403 ||
            error.code == 'invalid_device' ||
            error.code == 'hard_deny' ||
            error.code == 'sync_context_changed' ||
            error.code == 'sync_gap') {
          stop();
          final reset = store.clearReceivedState();
          _reset = reset;
          try {
            await reset;
          } finally {
            if (identical(_reset, reset)) _reset = null;
          }
        }
      }
      rethrow;
    } finally {
      cancel.cancel();
      if (epoch == _epoch && identical(_cancel, cancel)) _cancel = null;
    }
  }

  Future<void> purgeExpiredContent() => store.purgeUnauthorizedMessages();

  Future<void> resume() async {
    final context = _context;
    if (context == null) throw const ApiFailure('invalid_context');
    stop();
    await syncConversation(context);
  }

  void stop() {
    _epoch++;
    _cancel?.cancel();
    _cancel = null;
    _flight = null;
    _context = null;
    _pendingPage = null;
    _pendingClock = null;
    _after = null;
    _delivered.clear();
    store.invalidateReading();
  }
}
