import 'dart:async';
import 'dart:io';
import 'dart:isolate';
import 'store_worker.dart';
import 'records.dart';
import '../protocol/models.dart';
import '../protocol/seq.dart';

final class StoreFailure implements Exception {
  const StoreFailure();
  @override
  String toString() => '加密本地存储不可用';
}

// Shared storage core. Product UI and actual platform storage integration
// are completed and verified independently in the later client tasks.
final class EncryptedStore {
  final ReceivePort _responses;
  final SendPort _commands;
  final Isolate _worker;
  final Future<void> _exited;
  bool _failed = false;
  final int generation;
  final String _path;
  final Object _owner;
  static final _activePaths = <String>{};
  static final _owners = <String, Object>{};
  final Map<int, Completer<Object?>> _waiting = {};
  final _readingGrants = <String, LeaseClock>{};
  int _grantEpoch = 0;
  int _requestId = 0;
  bool _closed = false;
  Future<void>? _closing;
  Future<void>? _erasing;
  EncryptedStore._(
    this._responses,
    this._commands,
    this._worker,
    this._exited,
    this.generation,
    this._path,
    this._owner,
  );
  void _onReply(List<Object?> message) {
    final pending = _waiting.remove(message[0]);
    if (pending == null) return;
    if (message[1] == true) {
      pending.complete(message[2]);
    } else {
      pending.completeError(const StoreFailure());
    }
  }

  void _onWorkerFailure() {
    _failed = true;
    invalidateReading();
    for (final pending in _waiting.values) {
      pending.completeError(const StoreFailure());
    }
    _waiting.clear();
  }

  static Future<EncryptedStore> open(
    String namespace,
    List<int> key, {
    required Directory directory,
    required int generation,
    Future<void> Function(List<Object?>) workerEntryPoint = storeWorkerMain,
  }) async {
    if (!RegExp(r'^[a-f0-9]{64}$').hasMatch(namespace) ||
        key.length != 32 ||
        key.any((byte) => byte < 0 || byte > 255) ||
        generation < 0) {
      throw const StoreFailure();
    }
    final path = await _accountPath(namespace, directory);
    if (!_activePaths.add(path)) throw const StoreFailure();
    final owner = Object();
    _owners[path] = owner;
    final startup = ReceivePort();
    final responses = ReceivePort();
    final exited = Completer<void>();
    final failed = Completer<void>();
    EncryptedStore? store;
    responses.listen((raw) {
      // Replies, fatal errors and actual exit share one ordered event port, so
      // the worker's close acknowledgment cannot be overtaken by its exit.
      if (raw is List<Object?> &&
          raw.length == 3 &&
          raw[0] is int &&
          raw[1] is bool) {
        store?._onReply(raw);
      } else {
        if (raw == null && !exited.isCompleted) exited.complete();
        if (!failed.isCompleted) failed.complete();
        store?._onWorkerFailure();
      }
    });
    Isolate? worker;
    try {
      worker = await Isolate.spawn(
        workerEntryPoint,
        [
          startup.sendPort,
          responses.sendPort,
          path,
          namespace,
          List<int>.from(key),
          generation,
        ],
        onError: responses.sendPort,
        onExit: responses.sendPort,
        errorsAreFatal: true,
      );
      final result = await Future.any<Object?>([
        startup.first,
        failed.future.then((_) => null),
      ]).timeout(const Duration(seconds: 15));
      if (result is! SendPort || failed.isCompleted) throw const StoreFailure();
      store = EncryptedStore._(
        responses,
        result,
        worker,
        exited.future,
        generation,
        path,
        owner,
      );
      return store;
    } catch (_) {
      if (worker != null) {
        worker.kill(priority: Isolate.immediate);
        // Never release the path for another worker before observing actual
        // exit. A failed wait retains ownership and prevents unsafe reopening.
        try {
          await exited.future.timeout(const Duration(seconds: 15));
        } catch (_) {
          responses.close();
          throw const StoreFailure();
        }
      }
      responses.close();
      if (identical(_owners[path], owner)) _activePaths.remove(path);
      throw const StoreFailure();
    } finally {
      startup.close();
    }
  }

  static Future<String> _accountPath(
    String namespace,
    Directory directory,
  ) async {
    if (!RegExp(r'^[a-f0-9]{64}$').hasMatch(namespace))
      throw const StoreFailure();
    final root = directory.absolute;
    for (Directory? part = root; part != null;) {
      if (await FileSystemEntity.type(part.path, followLinks: false) ==
          FileSystemEntityType.link) {
        throw const StoreFailure();
      }
      final parent = part.parent;
      part = parent.path == part.path ? null : parent;
    }
    await root.create(recursive: true);
    final path = '${root.path}/$namespace.sqlite';
    for (final suffix in ['', '-wal', '-shm']) {
      if (await FileSystemEntity.type('$path$suffix', followLinks: false) ==
          FileSystemEntityType.link) {
        throw const StoreFailure();
      }
    }
    return path;
  }

  // Missing secure key is different from an unavailable vault. Call only after
  // fresh authentication and a successful vault read returning no bundle.
  static Future<void> eraseUnavailableKey(
    String namespace, {
    required Directory directory,
  }) async {
    final path = await _accountPath(namespace, directory);
    if (!_activePaths.add(path)) throw const StoreFailure();
    try {
      for (final suffix in ['', '-wal', '-shm']) {
        final file = File('$path$suffix');
        if (await file.exists()) await file.delete();
      }
    } finally {
      _activePaths.remove(path);
    }
  }

  Future<Object?> _request(
    String operation,
    Object? payload, {
    bool closing = false,
  }) async {
    if (_failed || (_closed && !closing)) throw const StoreFailure();
    final id = ++_requestId;
    final reply = Completer<Object?>();
    _waiting[id] = reply;
    _commands.send([id, operation, payload, generation]);
    final result = await reply.future;
    if (_failed || (_closed && !closing)) throw const StoreFailure();
    return result;
  }

  Future<void> putPendingSend(PendingSend value) async {
    if (value.generation != generation) throw const StoreFailure();
    await _request('put_pending', value.toJson());
  }

  Future<List<PendingSend>> pendingSends() async {
    try {
      final rows = await _request('pending', null) as List<Object?>;
      return List.unmodifiable(
        rows.map(
          (row) => PendingSend.fromJson(Map<String, Object?>.from(row! as Map)),
        ),
      );
    } catch (_) {
      throw const StoreFailure();
    }
  }

  Future<void> commitPage(
    SyncPage page,
    PendingReceipt pending, {
    required String actingMembershipId,
    required LeaseClock clock,
  }) async {
    try {
      uuidField({'id': actingMembershipId}, 'id');
      if (pending.generation != generation ||
          pending.conversationId != page.conversationId ||
          pending.nextAfterSeq != page.nextAfterSeq ||
          pending.syncProof != page.syncProof ||
          pending.proofExpiresAt != page.proofExpiresAt ||
          clock.serverTime != page.serverTime) {
        throw const StoreFailure();
      }
      final epoch = _grantEpoch;
      final rows = <Map<String, Object?>>[];
      for (final message in page.messages) {
        final readable =
            !message.redacted &&
            clock.remainingFor(message.cacheUntil!) > Duration.zero &&
            clock.remainingFor(message.contentExpiresAt!) > Duration.zero;
        // A fresh denial must revoke memory authority even if disk rollback preserves old ciphertext.
        _readingGrants.remove('${page.conversationId}/${message.seq}');
        rows.add({
          'seq': message.seq.toString(),
          'value': readable
              ? {
                  'conversation_id': page.conversationId,
                  'acting_membership_id': actingMembershipId,
                  'generation': generation,
                  'authorization_server_time': page.serverTime
                      .toIso8601String(),
                  'content': messageContent(message),
                }
              : null,
        });
      }
      await _request('commit_page', {
        'conversation_id': page.conversationId,
        'acting_membership_id': actingMembershipId,
        'generation': generation,
        'next_after_seq': page.nextAfterSeq.toString(),
        'after_seq':
            (page.nextAfterSeq.value - BigInt.from(page.messages.length))
                .toString(),
        'receipt_revision': page.receiptRevision.toString(),
        'messages': rows,
        'receipt': pending.toJson(),
      });
      if (epoch != _grantEpoch) return;
      for (final row in rows) {
        final id = '${page.conversationId}/${row['seq']}';
        if (row['value'] == null) {
          _readingGrants.remove(id);
        } else {
          _readingGrants[id] = clock;
        }
      }
    } on StoreFailure {
      rethrow;
    } catch (_) {
      throw const StoreFailure();
    }
  }

  Future<void> clearReceivedState() async {
    invalidateReading();
    await _request('clear_received', null);
  }

  Future<void> purgeUnauthorizedMessages() async {
    final rows = await _request('message_candidates', null) as List<Object?>;
    final deletes = <Map<String, Object?>>[];
    for (final raw in rows) {
      final row = Map<String, Object?>.from(raw! as Map);
      final value = StoredMessage.fromJson(
        Map<String, Object?>.from(row['value']! as Map),
      );
      final grant = _readingGrants['${value.conversationId}/${value.seq}'];
      final valid =
          value.generation == generation &&
          grant != null &&
          grant.serverTime == value.authorizationServerTime &&
          grant.remainingFor(value.message.cacheUntil!) > Duration.zero &&
          grant.remainingFor(value.message.contentExpiresAt!) > Duration.zero;
      if (!valid)
        deletes.add({'opaque_key': row['opaque_key'], 'nonce': row['nonce']});
    }
    // Compare the observed ciphertext nonce too: a concurrently committed new
    // authorization of the same message must not be deleted by an old sweep.
    await _request('purge_messages', deletes);
  }

  Future<void> acknowledgePage(String conversationId, Seq next) async {
    uuidField({'id': conversationId}, 'id');
    await _request('acknowledge_page', {
      'conversation_id': conversationId,
      'next_after_seq': next.toString(),
    });
  }

  Future<Seq?> committedCursor(String conversationId) async {
    try {
      uuidField({'id': conversationId}, 'id');
      final value = await _request('cursor', conversationId);
      return value == null ? null : Seq.fromWire(value);
    } catch (_) {
      throw const StoreFailure();
    }
  }

  Future<List<PendingReceipt>> pendingReceipts() async {
    try {
      final rows = await _request('receipts', null) as List<Object?>;
      final values = rows
          .map(
            (row) =>
                PendingReceipt.fromJson(Map<String, Object?>.from(row! as Map)),
          )
          .toList();
      values.sort((a, b) => a.nextAfterSeq.compareTo(b.nextAfterSeq));
      return List.unmodifiable(values);
    } catch (_) {
      throw const StoreFailure();
    }
  }

  Future<List<StoredMessage>> readAuthorizedMessages(LeaseClock clock) async {
    if (_failed ||
        _closed ||
        !_readingGrants.values.any((value) => identical(value, clock)) ||
        clock.remaining == Duration.zero)
      return [];
    try {
      final rows = await _request('messages', null) as List<Object?>;
      final values = <StoredMessage>[];
      for (final row in rows) {
        final value = StoredMessage.fromJson(
          Map<String, Object?>.from(row! as Map),
        );
        final grant = _readingGrants['${value.conversationId}/${value.seq}'];
        if (value.generation == generation &&
            grant != null &&
            grant.serverTime == value.authorizationServerTime &&
            grant.remainingFor(value.message.cacheUntil!) > Duration.zero &&
            grant.remainingFor(value.message.contentExpiresAt!) > Duration.zero)
          values.add(value);
      }
      values.sort((a, b) => a.seq.compareTo(b.seq));
      return List.unmodifiable(values);
    } catch (_) {
      throw const StoreFailure();
    }
  }

  void invalidateReading() {
    _grantEpoch++;
    for (final clock in _readingGrants.values) {
      clock.invalidate();
    }
    _readingGrants.clear();
  }

  Future<void> close() => _finish(false);
  Future<void> closeAndErase() => _finish(true);
  Future<void> _finish(bool erase) {
    if (_closing != null) {
      if (!erase) return _closing!;
      return _erasing ??= () async {
        try {
          await _closing;
        } catch (_) {
          // An observed exit still permits logout cleanup after a reported
          // checkpoint/close error. An unobserved exit never permits deletion.
          await _exited.timeout(const Duration(seconds: 15));
        }
        if (!identical(_owners[_path], _owner) || !_activePaths.add(_path))
          throw const StoreFailure();
        try {
          for (final suffix in ['', '-wal', '-shm']) {
            final file = File('$_path$suffix');
            if (await file.exists()) await file.delete();
          }
        } finally {
          if (identical(_owners[_path], _owner)) _activePaths.remove(_path);
        }
      }();
    }
    invalidateReading();
    _closed = true;
    return _closing = () async {
      Object? closeError;
      try {
        if (!_failed)
          await _request(erase ? 'erase' : 'close', null, closing: true);
      } catch (error) {
        if (!_failed) closeError = error;
        _worker.kill(priority: Isolate.immediate);
      }
      try {
        await _exited.timeout(const Duration(seconds: 15));
      } catch (_) {
        _worker.kill(priority: Isolate.immediate);
        // If even forced exit is not observed, preserve path ownership and
        // evidence. Do not delete files still possibly held by a live worker.
        try {
          await _exited.timeout(const Duration(seconds: 15));
        } catch (_) {
          throw const StoreFailure();
        }
      }
      try {
        if (erase) {
          if (!identical(_owners[_path], _owner)) throw const StoreFailure();
          for (final suffix in ['', '-wal', '-shm']) {
            final file = File('$_path$suffix');
            if (await file.exists()) await file.delete();
          }
        }
      } finally {
        _responses.close();
        for (final pending in _waiting.values) {
          pending.completeError(const StoreFailure());
        }
        _waiting.clear();
        if (identical(_owners[_path], _owner)) _activePaths.remove(_path);
      }
      if (closeError != null) throw const StoreFailure();
    }();
  }
}
