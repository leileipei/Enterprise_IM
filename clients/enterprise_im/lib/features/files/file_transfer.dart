import 'dart:async';
import 'dart:typed_data';
import 'package:cryptography/cryptography.dart';
import '../../core/protocol/api_client.dart';
import '../../core/protocol/models.dart';
import '../../core/protocol/seq.dart';
import '../../core/protocol/client_id.dart';
import '../../core/storage/records.dart';
import '../../core/sync/client_context.dart';
import '../../platform/file_picker.dart';
import '../memberships/membership_controller.dart';
import '../conversations/conversation_controller.dart';
import 'file_wire.dart';

final class FileStatus {
  final String id, conversationId, name, mediaType, state;
  final int size;
  final Seq version;
  final DateTime uploadExpiresAt;
  FileStatus._(
    this.id,
    this.conversationId,
    this.name,
    this.mediaType,
    this.state,
    this.size,
    this.version,
    this.uploadExpiresAt,
  );
  factory FileStatus.fromJson(Map<String, Object?> d) {
    final size = Seq.fromWire(d['declared_size_bytes']).value;
    final state = stringField(d, 'state', max: 16),
        version = Seq.fromWire(d['state_version']);
    final created = wireTime(d['created_at']),
        expiry = wireTime(d['upload_expires_at']);
    if (size < BigInt.one ||
        size > BigInt.from(25 * 1024 * 1024) ||
        (version.value == BigInt.zero && state != 'allocated') ||
        !const [
          'allocated',
          'uploaded',
          'scanning',
          'ready',
          'rejected',
          'scan_failed',
          'delete_pending',
          'deleted',
        ].contains(state) ||
        !expiry.isAfter(created))
      throw const FormatException('文件状态无效');
    return FileStatus._(
      uuidField(d, 'file_id'),
      uuidField(d, 'conversation_id'),
      stringField(d, 'original_filename', max: 255),
      stringField(d, 'declared_media_type', max: 64),
      state,
      size.toInt(),
      version,
      expiry,
    );
  }
}

final class FileTransfer {
  final MembershipController memberships;
  final ConversationController sender;
  final FileWireTransport wire;
  final bool Function()? guard;
  final void Function()? onStateChanged;
  WorkspaceAdmission? _admission;
  ClientContext? _context;
  PendingUpload? _record;
  FileStatus? _status;
  Cancellation? _cancel;
  bool _closed = false, _busy = false;
  int _epoch = 0;
  FileTransfer(
    this.memberships,
    this.sender, {
    FileWireTransport? wire,
    this.guard,
    this.onStateChanged,
  }) : wire = wire ?? HttpFileWireTransport(memberships.owner.auth.api.origin);
  bool get busy => _busy;
  void _changed() {
    if (!_closed) onStateChanged?.call();
  }

  bool get _active =>
      !_closed &&
      _admission != null &&
      identical(memberships.current, _admission) &&
      (guard?.call() ?? true);
  FileStatus? get current => _active ? _status : null;
  void _check(int epoch) {
    if (_closed || epoch != _epoch || !_active)
      throw const ApiFailure('cancelled');
  }

  Future<Map<String, Object?>> _json(
    String method,
    String path, {
    Map<String, Object?>? body,
  }) async {
    final ctx = _context!, epoch = _epoch, c = Cancellation();
    _cancel = c;
    try {
      final d = await memberships.owner.auth.api.json(
        method,
        path,
        body: body,
        bearer: ctx.session.accessToken,
        actingMembershipId: ctx.actingMembershipId,
        cancellation: c,
      );
      _check(epoch);
      return d;
    } finally {
      c.cancel();
      if (identical(_cancel, c)) _cancel = null;
    }
  }

  void _accept(FileStatus next, PendingUpload record) {
    if (next.conversationId != record.conversationId ||
        next.name != record.name ||
        next.mediaType != record.mediaType ||
        next.size != record.size ||
        (record.fileId != null && next.id != record.fileId) ||
        (_status != null && next.version.compareTo(_status!.version) < 0))
      throw const ApiFailure('invalid_response');
    _status = next;
  }

  Future<void> restore(ClientContext ctx) async {
    if (_closed || _busy) throw const ApiFailure('context_in_progress');
    final ready = memberships.current;
    if (ready == null ||
        !identical(ready.client.session, ctx.session) ||
        ready.membership.id != ctx.actingMembershipId ||
        ready.device.deviceId != ctx.device.deviceId ||
        ready.device.deviceToken != ctx.device.deviceToken ||
        ctx.generation != ready.client.store.generation)
      throw const ApiFailure('invalid_context');
    _admission = ready;
    _context = ctx;
    final epoch = ++_epoch;
    final record = await ready.client.store.pendingUpload();
    _check(epoch);
    if (record != null &&
        (record.conversationId != ctx.conversationId ||
            record.actingMembershipId != ctx.actingMembershipId))
      throw const ApiFailure('upload_context_mismatch');
    _record = record;
    _status = null;
    if (record?.fileId != null &&
        (await ready.client.store.pendingSends()).isEmpty)
      await poll();
  }

  void stopRequest() => _cancel?.cancel();
  Future<void> reserveAndUpload(ClientContext ctx, SelectedFile file) async {
    if (_closed || _busy) throw const ApiFailure('context_in_progress');
    final ready = memberships.current;
    if (ready == null ||
        !identical(ready.client.session, ctx.session) ||
        ctx.generation != ready.client.store.generation ||
        ctx.device.deviceId != ready.device.deviceId ||
        ctx.device.deviceToken != ready.device.deviceToken ||
        ctx.actingMembershipId != ready.membership.id ||
        ctx.session.origin != memberships.owner.auth.api.origin)
      throw const ApiFailure('invalid_context');
    _busy = true;
    final epoch = ++_epoch;
    _admission = ready;
    _context = ctx;
    _status = null;
    _changed();
    Uint8List? snapshot;
    try {
      if (file.size < 1 || file.size > 25 * 1024 * 1024)
        throw const ApiFailure('file_too_large');
      final buffer = BytesBuilder(copy: true);
      final c = Cancellation();
      _cancel = c;
      final source = StreamIterator<List<int>>(file.open());
      final timeout = Timer(const Duration(seconds: 60), c.cancel);
      try {
        while (await Future.any([
          source.moveNext(),
          c.whenCancelled.then<bool>(
            (_) => throw const ApiFailure('cancelled'),
          ),
        ])) {
          c.check();
          _check(epoch);
          final chunk = source.current;
          if (buffer.length + chunk.length > file.size)
            throw const ApiFailure('invalid_file_size');
          buffer.add(chunk);
        }
      } finally {
        timeout.cancel();
        await source.cancel();
      }
      snapshot = buffer.takeBytes();
      _check(epoch);
      if (snapshot.length != file.size)
        throw const ApiFailure('invalid_file_size');
      final digest = (await Sha256().hash(
        snapshot,
      )).bytes.map((b) => b.toRadixString(16).padLeft(2, '0')).join();
      _check(epoch);
      final old = await ready.client.store.pendingUpload();
      _check(epoch);
      final record = PendingUpload(
        requestId: old?.requestId ?? ClientMessageId.create(ready.clock),
        conversationId: ctx.conversationId,
        actingMembershipId: ctx.actingMembershipId,
        name: file.name,
        mediaType: file.mediaType,
        size: file.size,
        sha256: digest,
        fileId: old?.fileId,
      );
      if (old != null && old.toJson().toString() != record.toJson().toString())
        throw const ApiFailure('upload_context_mismatch');
      if ((await ready.client.store.pendingSends()).isNotEmpty ||
          await ready.client.store.pendingAction() != null)
        throw const ApiFailure('send_unconfirmed');
      await ready.client.store.putPendingUpload(record);
      _check(epoch);
      _record = record;
      final status = FileStatus.fromJson(
        await _json(
          record.fileId == null ? 'POST' : 'GET',
          record.fileId == null
              ? '/api/v1/conversations/${ctx.conversationId}/files'
              : '/api/v1/files/${record.fileId}',
          body: record.fileId == null ? record.body : null,
        ),
      );
      _accept(status, record);
      if (record.fileId == null) {
        await ready.client.store.saveUploadFile(record, status.id);
        _check(epoch);
        _record = record.withFile(status.id);
      }
      if (status.state == 'allocated') {
        if (!ready.clock.now.isBefore(status.uploadExpiresAt))
          throw const ApiFailure('upload_expired');
        final cancel = Cancellation();
        _cancel = cancel;
        final bytes = snapshot;
        Stream<List<int>> chunks() async* {
          for (var i = 0; i < bytes.length; i += 32768) {
            cancel.check();
            _check(epoch);
            yield bytes.sublist(
              i,
              i + 32768 < bytes.length ? i + 32768 : bytes.length,
            );
          }
        }

        final response = await Future.any([
          wire.upload(
            fileRequest(ctx, 'PUT', status.id),
            chunks(),
            bytes.length,
            cancel,
          ),
          cancel.whenCancelled.then<ApiResponse>(
            (_) => throw const ApiFailure('cancelled'),
          ),
        ]);
        _check(epoch);
        _accept(FileStatus.fromJson(fileJSON(response)), _record!);
      }
    } finally {
      snapshot?.fillRange(0, snapshot.length, 0);
      _cancel?.cancel();
      _cancel = null;
      if (epoch == _epoch) {
        _busy = false;
        _changed();
      }
    }
  }

  Future<void> poll() async {
    final epoch = _epoch;
    _check(epoch);
    if (_busy) throw const ApiFailure('context_in_progress');
    final record = _record;
    if (record?.fileId == null) throw const ApiFailure('upload_unconfirmed');
    _busy = true;
    _changed();
    try {
      _accept(
        FileStatus.fromJson(
          await _json('GET', '/api/v1/files/${record!.fileId}'),
        ),
        record,
      );
    } finally {
      if (epoch == _epoch) {
        _busy = false;
        _changed();
      }
    }
  }

  Future<MessageACK> bindToMessage(String caption) async {
    final epoch = _epoch;
    _check(epoch);
    if (_busy || _status?.state != 'ready' || _record?.fileId == null)
      throw const ApiFailure('file_not_ready');
    _busy = true;
    _changed();
    try {
      await pollForBinding();
      _check(epoch);
      if (_status?.state != 'ready') throw const ApiFailure('file_not_ready');
      final ack = await sender.sendFile(
        _record!.fileId!,
        caption,
        _record!.requestId,
      );
      _check(epoch);
      _record = null;
      _status = null;
      return ack;
    } finally {
      if (epoch == _epoch) {
        _busy = false;
        _changed();
      }
    }
  }

  Future<void> pollForBinding() async {
    final record = _record!;
    _accept(
      FileStatus.fromJson(await _json('GET', '/api/v1/files/${record.fileId}')),
      record,
    );
  }

  Future<void> cancel() async {
    _cancel?.cancel();
    final epoch = _epoch;
    _check(epoch);
    if (_busy) throw const ApiFailure('context_in_progress');
    final record = await _admission!.client.store.pendingUpload();
    _check(epoch);
    if (record != null) {
      if (record.conversationId != _context!.conversationId ||
          record.actingMembershipId != _context!.actingMembershipId)
        throw const ApiFailure('upload_context_mismatch');
      await _admission!.client.store.finishUpload(record);
      _check(epoch);
    }
    _record = null;
    _status = null;
  }

  Future<void> close() async {
    _closed = true;
    _epoch++;
    _cancel?.cancel();
    _cancel = null;
    _admission = null;
    _context = null;
    _status = null;
    wire.close();
  }
}
