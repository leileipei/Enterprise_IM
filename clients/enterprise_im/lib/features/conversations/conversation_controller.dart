import '../../core/protocol/api_client.dart';
import '../../core/protocol/models.dart';
import '../../core/protocol/client_id.dart';
import '../../core/sync/client_context.dart';
import '../../core/storage/records.dart';
import '../memberships/membership_controller.dart';

// This controller owns one selected conversation's explicit send attempts.
// It never automatically resubmits a saved pending message after restoration.
final class ConversationController {
  final MembershipController memberships;
  WorkspaceAdmission? _admission;
  ClientContext? _context;
  ConversationSummary? _conversation;
  Cancellation? _cancel;
  MessageACK? _lastACK;
  int _epoch = 0;
  bool _closed = false, _busy = false;
  ConversationController(this.memberships);
  bool get _active =>
      !_closed &&
      _context != null &&
      _admission != null &&
      identical(memberships.current, _admission);
  MessageACK? get lastACK => _active ? _lastACK : null;
  void _check(int epoch) {
    if (epoch != _epoch || !_active) throw const ApiFailure('cancelled');
  }

  Future<void> select(
    ClientContext context,
    ConversationSummary conversation,
  ) async {
    _stop();
    final epoch = _epoch;
    final ready = memberships.current;
    if (_closed ||
        ready == null ||
        !identical(context.session, ready.client.session) ||
        context.actingMembershipId != ready.membership.id ||
        context.conversationId != conversation.id ||
        context.device.deviceId != ready.device.deviceId ||
        context.device.deviceToken != ready.device.deviceToken ||
        context.generation != ready.client.store.generation)
      throw const ApiFailure('invalid_context');
    final pending = await ready.client.store.pendingSends();
    final action = await ready.client.store.pendingAction();
    final upload = await ready.client.store.pendingUpload();
    if (upload != null &&
        (upload.conversationId != context.conversationId ||
            upload.actingMembershipId != context.actingMembershipId))
      throw const ApiFailure('upload_unconfirmed');
    if (_closed || epoch != _epoch || !identical(memberships.current, ready))
      throw const ApiFailure('cancelled');
    if (action != null) throw const ApiFailure('operation_unconfirmed');
    if (pending.any(
      (p) =>
          p.conversationId != context.conversationId ||
          p.actingMembershipId != context.actingMembershipId,
    ))
      throw const ApiFailure('send_unconfirmed');
    _admission = ready;
    _context = context;
    _conversation = conversation;
  }

  Future<MessageACK> sendText(String text) => _sendNew(text);
  Future<MessageACK> sendFile(
    String fileId,
    String caption,
    String uploadRequestId,
  ) => _sendNew(caption, fileId: fileId, uploadRequestId: uploadRequestId);
  Future<MessageACK> _sendNew(
    String text, {
    String? fileId,
    String? uploadRequestId,
  }) async {
    final epoch = _epoch;
    _check(epoch);
    if (_busy) throw const ApiFailure('send_in_progress');
    messageBodyField({'text': text}, 'text', empty: fileId != null);
    _busy = true;
    _lastACK = null;
    try {
      final ready = _admission!, context = _context!;
      final upload = await ready.client.store.pendingUpload();
      if (fileId == null && upload != null)
        throw const ApiFailure('upload_unconfirmed');
      if (fileId != null &&
          (upload == null ||
              upload.fileId != fileId ||
              upload.requestId != uploadRequestId ||
              upload.conversationId != context.conversationId ||
              upload.actingMembershipId != context.actingMembershipId))
        throw const ApiFailure('invalid_context');
      final existing = await ready.client.store.pendingSends();
      final action = await ready.client.store.pendingAction();
      _check(epoch);
      if (action != null) throw const ApiFailure('operation_unconfirmed');
      if (existing.isNotEmpty) throw const ApiFailure('send_unconfirmed');
      final pending = PendingSend(
        clientMsgId: ClientMessageId.create(ready.clock),
        conversationId: context.conversationId,
        actingMembershipId: context.actingMembershipId,
        text: text,
        generation: context.generation,
        createdAt: ready.clock.now,
        fileId: fileId,
        uploadRequestId: uploadRequestId,
      );
      await ready.client.store.putPendingSend(pending);
      _check(epoch);
      return await _send(pending, epoch);
    } finally {
      if (epoch == _epoch) _busy = false;
    }
  }

  Future<MessageACK> retryPending(String clientMsgId) async {
    uuidField({'id': clientMsgId}, 'id');
    final epoch = _epoch;
    _check(epoch);
    if (_busy) throw const ApiFailure('send_in_progress');
    _busy = true;
    _lastACK = null;
    try {
      final store = _admission!.client.store, context = _context!;
      final rows = await store.pendingSends();
      _check(epoch);
      final found = rows.where((p) => p.clientMsgId == clientMsgId).toList();
      if (found.isEmpty) {
        final confirmed = await store.sendConfirmation(clientMsgId);
        _check(epoch);
        if (confirmed == null ||
            confirmed.actingMembershipId != context.actingMembershipId ||
            confirmed.ack.conversationId != context.conversationId)
          throw const ApiFailure('pending_not_found');
        return _lastACK = confirmed.ack;
      }
      final pending = found.single;
      if (pending.conversationId != context.conversationId ||
          pending.actingMembershipId != context.actingMembershipId)
        throw const ApiFailure('invalid_context');
      return await _send(pending, epoch);
    } finally {
      if (epoch == _epoch) _busy = false;
    }
  }

  Future<MessageACK> _send(PendingSend pending, int epoch) async {
    _check(epoch);
    final ready = _admission!,
        conversation = _conversation!,
        context = _context!;
    final cancel = Cancellation();
    _cancel = cancel;
    try {
      final data = await memberships.owner.auth.api.json(
        'POST',
        '/api/v1/${conversation.kind == 'group' ? 'groups' : 'conversations'}/${context.conversationId}/messages',
        bearer: context.session.accessToken,
        actingMembershipId: context.actingMembershipId,
        body: pending.body,
        cancellation: cancel,
      );
      _check(epoch);
      MessageACK ack;
      try {
        ack = MessageACK.fromJson(data);
      } on FormatException {
        throw const ApiFailure('invalid_response');
      }
      if (ack.conversationId != context.conversationId)
        throw const ApiFailure('invalid_response');
      await ready.client.store.acknowledgeSend(pending, ack);
      _check(epoch);
      return _lastACK = ack;
    } finally {
      cancel.cancel();
      if (epoch == _epoch) _cancel = null;
    }
  }

  Future<void> abandonPending(String clientMsgId) async {
    uuidField({'id': clientMsgId}, 'id');
    final epoch = _epoch;
    _check(epoch);
    if (_busy) throw const ApiFailure('send_in_progress');
    _busy = true;
    _lastACK = null;
    try {
      final store = _admission!.client.store;
      final rows = await store.pendingSends();
      _check(epoch);
      final pending = rows.where((p) => p.clientMsgId == clientMsgId).toList();
      if (pending.isNotEmpty) {
        await store.abandonPendingSend(pending.single);
        _check(epoch);
      }
    } finally {
      if (epoch == _epoch) _busy = false;
    }
  }

  void _stop() {
    _epoch++;
    _cancel?.cancel();
    _cancel = null;
    _context = null;
    _admission = null;
    _conversation = null;
    _lastACK = null;
    _busy = false;
  }

  void close() {
    _closed = true;
    _stop();
  }
}
