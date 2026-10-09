import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:math';
import '../protocol/models.dart';
import '../storage/encrypted_store.dart';
import '../storage/secure_vault.dart';
import 'auth_session.dart';
import 'account_reference.dart';

// Only a session verified by NativeAuth (including /me) may be admitted here.
// Access tokens remain in AuthSession memory. One OS-vault bundle atomically
// binds account metadata, database key and rotated refresh/device credentials.
final class SessionStore {
  final SecureVault vault;
  final Directory directory;
  Future<void> _tail = Future.value();
  int _epoch = 0;
  bool _opening = false;
  String? _namespace;
  AuthSession? _auth;
  EncryptedStore? _store;
  _Bundle? _bundle;
  final _privateClosers = <Future<void> Function()>{};
  SessionStore(this.vault, {required this.directory});
  void Function() ownPrivateFiles(
    AuthSession session,
    Future<void> Function() close,
  ) {
    _check(session, _epoch);
    if (!identical(_auth, session) || _store == null)
      throw const AuthFailure('reauthenticate');
    _privateClosers.add(close);
    return () => _privateClosers.remove(close);
  }

  Future<void> _erasePrivateFiles(String ns) async {
    final path = '${directory.path}/$ns.attachments';
    final kind = await FileSystemEntity.type(path, followLinks: false);
    if (kind == FileSystemEntityType.link)
      await Link(path).delete();
    else if (kind == FileSystemEntityType.directory)
      await Directory(path).delete(recursive: true);
    else if (kind != FileSystemEntityType.notFound)
      throw const StoreFailure();
  }

  Future<T> _serial<T>(Future<T> Function() operation) {
    final result = _tail.then((_) => operation());
    _tail = result.then<void>((_) {}, onError: (Object _, StackTrace __) {});
    return result;
  }

  void _check(AuthSession session, int epoch) {
    if (epoch != _epoch) throw const AuthCancelled();
    if (session.isExpired || session.generation < 0)
      throw const AuthFailure('reauthenticate');
  }

  Future<EncryptedStore> open(AuthSession session) async {
    final epoch = _epoch;
    _check(session, epoch);
    if (_opening || _store != null)
      throw const AuthFailure('session_in_progress');
    _opening = true;
    try {
      return await _serial(() async {
        _check(session, epoch);
        final ns = await accountNamespace(
          session.origin,
          session.tenantId,
          session.userId,
        );
        _check(session, epoch);
        _namespace = ns;
        await _erasePrivateFiles(ns);
        _check(session, epoch);
        final raw = await vault.read(ns, 'session');
        _check(session, epoch);
        _Bundle? bundle;
        try {
          if (raw == null) {
            await EncryptedStore.eraseUnavailableKey(ns, directory: directory);
            _check(session, epoch);
            final random = Random.secure();
            bundle = _Bundle(
              List<int>.generate(32, (_) => random.nextInt(256)),
              null,
              null,
            );
            await vault.write(
              ns,
              'session',
              bundle.encode(AccountReference.fromSession(session)),
            );
            _check(session, epoch);
          } else {
            bundle = _Bundle.decode(raw, AccountReference.fromSession(session));
          }
          final store = await EncryptedStore.open(
            ns,
            bundle.key,
            directory: directory,
            generation: session.generation,
          );
          _store = store;
          try {
            _check(session, epoch);
          } catch (_) {
            await store.close();
            _store = null;
            rethrow;
          }
          _bundle = bundle;
          _auth = session;
          return store;
        } catch (_) {
          bundle?.destroy();
          rethrow;
        }
      });
    } finally {
      _opening = false;
    }
  }

  // Only read an OS-vault credential here. Do not create/open SQLite, create
  // a replacement key, or manufacture an AuthSession from account metadata.
  Future<String?> loadRefresh(AccountReference account) {
    final epoch = _epoch;
    return _serial(() async {
      if (epoch != _epoch) throw const AuthCancelled();
      final ns = await accountNamespace(
        account.origin,
        account.tenantId,
        account.userId,
      );
      if (epoch != _epoch) throw const AuthCancelled();
      final raw = await vault.read(ns, 'session');
      if (epoch != _epoch) throw const AuthCancelled();
      if (raw == null) return null;
      final bundle = _Bundle.decode(raw, account);
      try {
        return bundle.refresh;
      } finally {
        bundle.destroy();
      }
    });
  }

  // Invalid-grant handling removes the refresh credential without opening
  // SQLite or deleting the still-owned key/pending-send data.
  Future<void> forgetRefresh(AccountReference account) {
    final epoch = _epoch;
    return _serial(() async {
      if (epoch != _epoch) throw const AuthCancelled();
      final ns = await accountNamespace(
        account.origin,
        account.tenantId,
        account.userId,
      );
      if (epoch != _epoch) throw const AuthCancelled();
      final raw = await vault.read(ns, 'session');
      if (epoch != _epoch) throw const AuthCancelled();
      if (raw == null) return;
      final bundle = _Bundle.decode(raw, account);
      final changed = _Bundle(bundle.key, null, bundle.device);
      try {
        await vault.write(ns, 'session', changed.encode(account));
        if (epoch != _epoch) throw const AuthCancelled();
      } finally {
        changed.destroy();
        bundle.destroy();
      }
    });
  }

  // Caller verifies fresh rotated tokens and the same principal before saving.
  Future<void> saveCredentials(
    AuthSession session, {
    String? refreshToken,
    DeviceGrant? device,
  }) {
    final epoch = _epoch;
    return _serial(() async {
      _check(session, epoch);
      final previous = _auth, bundle = _bundle, ns = _namespace;
      if (previous == null ||
          bundle == null ||
          ns == null ||
          _store == null ||
          previous.generation != session.generation ||
          previous.origin != session.origin ||
          previous.tenantId != session.tenantId ||
          previous.userId != session.userId) {
        throw const AuthFailure('reauthenticate');
      }
      if (refreshToken != null &&
          (refreshToken.isEmpty || utf8.encode(refreshToken).length > 4096)) {
        throw const VaultFailure();
      }
      final changed = _Bundle(bundle.key, refreshToken, device);
      try {
        await vault.write(
          ns,
          'session',
          changed.encode(AccountReference.fromSession(session)),
        );
        _check(session, epoch);
        bundle.destroy();
        _bundle = changed;
        _auth = session;
      } finally {
        if (!identical(_bundle, changed)) changed.destroy();
      }
    });
  }

  String? get refreshToken =>
      _auth?.isExpired == false ? _bundle?.refresh : null;
  DeviceGrant? get device => _auth?.isExpired == false ? _bundle?.device : null;
  Future<void> close() => _finish(false);
  Future<void> logout({AccountReference? account}) =>
      _finish(true, account: account);
  Future<void> _finish(bool erase, {AccountReference? account}) {
    _epoch++;
    final privateClosers = List<Future<void> Function()>.from(_privateClosers);
    _privateClosers.clear();
    final privateClosing = Future.wait(privateClosers.map((close) => close()))
        .then<Object?>(
          (_) => null,
          onError: (Object e, StackTrace stack) => [e, stack],
        );
    _auth = null;
    // Mark the repository closed synchronously, before a queued vault write
    // can delay teardown. Capture any error immediately to avoid an unhandled
    // asynchronous failure while the serialized vault operation finishes.
    final currentStore = _store;
    final closing = currentStore == null
        ? null
        : (erase ? currentStore.closeAndErase() : currentStore.close())
              .then<Object?>(
                (_) => null,
                onError: (Object error, StackTrace stack) => [error, stack],
              );
    return _serial(() async {
      final store = _store;
      // Logout may interrupt restoration before authenticated DB admission.
      // Resolve the explicit selected account only for an unopened repository.
      final ns = store == null && account != null
          ? await accountNamespace(
              account.origin,
              account.tenantId,
              account.userId,
            )
          : _namespace;
      try {
        final privateFailure = await privateClosing;
        if (privateFailure is List)
          Error.throwWithStackTrace(
            privateFailure[0] as Object,
            privateFailure[1] as StackTrace,
          );
        if (ns != null) await _erasePrivateFiles(ns);
        if (store != null) {
          final failure = await closing;
          if (failure is List)
            Error.throwWithStackTrace(
              failure[0] as Object,
              failure[1] as StackTrace,
            );
        } else if (erase && ns != null) {
          await EncryptedStore.eraseUnavailableKey(ns, directory: directory);
        }
      } finally {
        _store = null;
        _bundle?.destroy();
        _bundle = null;
        if (erase && ns != null) await vault.delete(ns, 'session');
      }
    });
  }
}

final class _Bundle {
  final List<int> key;
  final String? refresh;
  final DeviceGrant? device;
  _Bundle(List<int> key, this.refresh, this.device) : key = List<int>.from(key);
  void destroy() => key.fillRange(0, key.length, 0);
  String encode(AccountReference session) => jsonEncode({
    'version': 1,
    'origin': session.origin.origin,
    'tenant_id': session.tenantId,
    'user_id': session.userId,
    'key': base64Url.encode(key),
    'refresh_token': refresh,
    'device': device == null
        ? null
        : {
            'device_id': device!.deviceId,
            'device_token': device!.deviceToken,
            'expires_at': device!.expiresAt.toIso8601String(),
            'server_time': device!.serverTime.toIso8601String(),
          },
  });
  factory _Bundle.decode(String raw, AccountReference session) {
    try {
      if (utf8.encode(raw).length > 65536) throw const FormatException();
      final value = objectMap(jsonDecode(raw));
      if (value.length != 7 ||
          value['version'] != 1 ||
          value['origin'] != session.origin.origin ||
          value['tenant_id'] != session.tenantId ||
          value['user_id'] != session.userId)
        throw const FormatException();
      final encoded = stringField(value, 'key', max: 44),
          key = base64Url.decode(encoded);
      if (key.length != 32 || base64Url.encode(key) != encoded)
        throw const FormatException();
      final refresh = value['refresh_token'];
      if (refresh != null &&
          (refresh is! String ||
              refresh.isEmpty ||
              utf8.encode(refresh).length > 4096))
        throw const FormatException();
      final device = value['device'] == null
          ? null
          : DeviceGrant.fromJson(objectMap(value['device']));
      return _Bundle(key, refresh as String?, device);
    } catch (_) {
      throw const VaultFailure();
    }
  }
}
