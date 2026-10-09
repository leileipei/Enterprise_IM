import 'dart:async';
import 'dart:io';
import 'dart:isolate';
import 'store_worker.dart';

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
    // Refuse links before opening any account database. Platform adapters supply
    // the private application support directory, not a user-selected location.
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

  Future<void> putPendingSend(Map<String, Object?> value) async {
    if (value['generation'] != generation ||
        value['client_msg_id'] is! String ||
        (value['client_msg_id']! as String).isEmpty ||
        value['text'] is! String) {
      throw const StoreFailure();
    }
    await _request('put_pending', Map<String, Object?>.from(value));
  }

  Future<List<Map<String, Object?>>> pendingSends() async {
    final rows = await _request('pending', null) as List<Object?>;
    return rows
        .map((row) => Map<String, Object?>.from(row! as Map))
        .toList(growable: false);
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
