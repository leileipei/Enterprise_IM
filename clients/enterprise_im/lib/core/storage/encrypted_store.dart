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

// This low-level repository is not wired into the product UI until typed page
// transactions, account restore and lease authorization are implemented.
final class EncryptedStore {
  final ReceivePort _responses;
  final SendPort _commands;
  final Isolate _worker;
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
    this.generation,
    this._path,
    this._owner,
  ) {
    _responses.listen((raw) {
      final message = raw as List<Object?>;
      final pending = _waiting.remove(message[0]);
      if (pending == null) return;
      if (message[1] == true) {
        pending.complete(message[2]);
      } else {
        pending.completeError(const StoreFailure());
      }
    });
  }
  static Future<EncryptedStore> open(
    String namespace,
    List<int> key, {
    required Directory directory,
    required int generation,
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
    final errors = ReceivePort();
    Isolate? worker;
    try {
      worker = await Isolate.spawn(
        storeWorkerMain,
        [
          startup.sendPort,
          responses.sendPort,
          path,
          namespace,
          List<int>.from(key),
          generation,
        ],
        onError: errors.sendPort,
        errorsAreFatal: true,
      );
      final result = await Future.any<Object?>([
        startup.first,
        errors.first,
      ]).timeout(const Duration(seconds: 15));
      if (result is! SendPort) throw const StoreFailure();
      return EncryptedStore._(
        responses,
        result,
        worker,
        generation,
        path,
        owner,
      );
    } catch (_) {
      worker?.kill(priority: Isolate.immediate);
      responses.close();
      _activePaths.remove(path);
      throw const StoreFailure();
    } finally {
      startup.close();
      errors.close();
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
    if (_closed && !closing) throw const StoreFailure();
    final id = ++_requestId;
    final reply = Completer<Object?>();
    _waiting[id] = reply;
    _commands.send([id, operation, payload, generation]);
    final result = await reply.future;
    if (_closed && !closing) throw const StoreFailure();
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
    if (!_readingGrants.values.any((value) => identical(value, clock)) ||
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
        await _closing;
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
      try {
        await _request(erase ? 'erase' : 'close', null, closing: true);
      } finally {
        _responses.close();
        for (final pending in _waiting.values) {
          pending.completeError(const StoreFailure());
        }
        _waiting.clear();
        _worker.kill(priority: Isolate.immediate);
        if (identical(_owners[_path], _owner)) _activePaths.remove(_path);
      }
    }();
  }
}
