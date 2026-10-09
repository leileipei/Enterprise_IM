import '../storage/encrypted_store.dart';
import 'account_reference.dart';
import 'account_selector.dart';
import 'native_auth.dart';
import 'session_store.dart';

final class AuthenticatedClient {
  final AuthSession session;
  final EncryptedStore store;
  AuthenticatedClient._(this.session, this.store);
  @override
  String toString() => 'AuthenticatedClient';
}

// UI owns one of these per service. No logged-in client is published until
// authentication, DB admission and OS-vault rotation all succeeded. Shutdown
// revokes current admission synchronously before waiting on native callbacks.
final class ClientSession {
  final NativeAuth auth;
  final SessionStore sessions;
  final String platform;
  final AccountSelector accounts;
  AccountReference? _account;
  AuthenticatedClient? _current;
  int _epoch = 0;
  bool _busy = false;
  ClientSession(this.auth, this.sessions, {required this.platform})
    : accounts = AccountSelector(sessions.vault);
  AuthenticatedClient? get current =>
      _current?.session.isExpired == false ? _current : null;
  void _check(int epoch) {
    if (epoch != _epoch) throw const AuthCancelled();
  }

  Future<AuthenticatedClient> signIn() => _admit(() => auth.signIn(platform));
  Future<AuthenticatedClient> restore(AccountReference account) async {
    // Reject a different service before assigning any credential-cleanup scope.
    if (account.origin != auth.api.origin)
      throw const AuthFailure('reauthenticate');
    if (_busy) throw const AuthFailure('login_in_progress');
    _account = account;
    return _admit(() async {
      final refresh = await sessions.loadRefresh(account);
      if (refresh == null) throw const AuthFailure('reauthenticate');
      return auth.restore(account, platform, refresh);
    }, invalidRefreshAccount: account);
  }

  Future<AuthenticatedClient> refresh() => _admit(
    auth.refresh,
    invalidRefreshAccount: _current == null
        ? null
        : AccountReference.fromSession(_current!.session),
  );

  Future<AuthenticatedClient> _admit(
    Future<AuthSession> Function() authenticate, {
    AccountReference? invalidRefreshAccount,
  }) async {
    if (_busy) throw const AuthFailure('login_in_progress');
    _busy = true;
    final epoch = ++_epoch;
    _current = null;
    try {
      await sessions.close();
      _check(epoch);
      final session = await authenticate();
      _check(epoch);
      if (session.isExpired || !identical(auth.session, session))
        throw const AuthFailure('reauthenticate');
      _account = AccountReference.fromSession(session);
      final store = await sessions.open(session);
      _check(epoch);
      // Preserve an existing device credential; fresh server verification is
      // still required by the context/device controller before message use.
      await sessions.saveCredentials(
        session,
        refreshToken: auth.refreshCredential,
        device: sessions.device,
      );
      _check(epoch);
      await accounts.save(_account!);
      _check(epoch);
      if (session.isExpired) throw const AuthFailure('reauthenticate');
      return _current = AuthenticatedClient._(session, store);
    } catch (error, stack) {
      if (epoch != _epoch) throw const AuthCancelled();
      _current = null;
      // Enqueue cleanup before awaiting any native callback; a late cleanup
      // must never close a newly admitted database or overwrite its credentials.
      final stopped = auth.cancel();
      final closed = sessions.close();
      final forgotten =
          error is AuthFailure &&
              error.code == 'reauthenticate' &&
              invalidRefreshAccount != null
          ? sessions.forgetRefresh(invalidRefreshAccount)
          : Future<void>.value();
      await Future.wait([stopped, closed, forgotten]);
      Error.throwWithStackTrace(error, stack);
    } finally {
      if (epoch == _epoch) _busy = false;
    }
  }

  Future<void> close() => _shutdown(false);
  Future<void> logout() => _shutdown(true);
  Future<void> _shutdown(bool erase) async {
    final epoch = ++_epoch;
    _busy = false;
    _current = null;
    final account = _account;
    final stopped = auth.cancel();
    final closed = erase ? sessions.logout(account: account) : sessions.close();
    final forgotten = erase && account != null
        ? accounts.forget(account)
        : Future<void>.value();
    await Future.wait([stopped, closed, forgotten]);
    if (erase && epoch == _epoch) _account = null;
  }
}
