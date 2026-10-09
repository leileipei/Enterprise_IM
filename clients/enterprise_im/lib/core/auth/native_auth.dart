import 'dart:async';
import '../protocol/api_client.dart';
import '../protocol/models.dart';
import '../../platform/browser_launcher.dart';
import '../../platform/login_callback.dart';
import 'auth_session.dart';
import 'pkce.dart';
export 'auth_session.dart';

final class NativeAuth {
  final ApiClient api;
  final LoginCallback Function(String, String) callbackFactory;
  final BrowserLauncher browser;
  final Duration loginTimeout;
  int _generation = 0;
  bool _busy = false;
  Cancellation? _cancel;
  LoginCallback? _callback;
  AuthSession? _session;
  String? _refreshToken;
  String? _platform;
  bool _refreshEnabled = false;
  NativeAuth(
    this.api, {
    required this.callbackFactory,
    required this.browser,
    this.loginTimeout = const Duration(seconds: 120),
  });
  bool get canRefresh => _refreshEnabled && _refreshToken != null;
  AuthSession? get session => _session?.isExpired == false ? _session : null;
  void _check(int generation) {
    if (generation != _generation || _cancel?.isCancelled == true)
      throw const AuthCancelled();
  }

  void _clear() {
    _session = null;
    _refreshToken = null;
    _platform = null;
    _refreshEnabled = false;
  }

  Future<AuthSession> signIn(String platform) async {
    if (_busy) throw const AuthFailure('login_in_progress');
    if (!const ['macos', 'windows', 'ios', 'android'].contains(platform))
      throw const AuthFailure('invalid_platform');
    _busy = true;
    _clear();
    final generation = ++_generation;
    final cancellation = Cancellation();
    _cancel = cancellation;
    final overall = Stopwatch()..start();
    LoginCallback? callback;
    try {
      final config = await api.json(
        'GET',
        '/api/v1/native/config?platform=$platform',
        cancellation: cancellation,
        timeout: _remaining(overall),
      );
      _check(generation);
      final desktop = platform == 'macos' || platform == 'windows';
      final template = desktop
          ? 'http://127.0.0.1/oauth/callback'
          : 'com.enterpriseim.client:/oauth/callback';
      if (stringField(config, 'platform', max: 16) != platform ||
          boolField(config, 'loopback') != desktop ||
          stringField(config, 'redirect_uri', max: 256) != template)
        throw const AuthFailure('invalid_configuration');
      final issuer = _httpsEndpoint(stringField(config, 'issuer', max: 2048));
      final authorize = _httpsEndpoint(
        stringField(config, 'authorization_url', max: 2048),
      );
      if (issuer.host.isEmpty) throw const AuthFailure('invalid_configuration');
      final client = stringField(config, 'client_id', max: 256);
      final scope = stringField(config, 'scope', max: 512);
      if (!scope.split(RegExp(r'\s+')).contains('openid'))
        throw const AuthFailure('invalid_configuration');
      final refreshEnabled = boolField(config, 'refresh_enabled');
      wireTime(config['server_time']);
      final pkce = await PKCE.generate().timeout(_remaining(overall));
      _check(generation);
      callback = callbackFactory(platform, pkce.state);
      _callback = callback;
      final attempt =
          await Future.any<LoginAttempt>([
            callback.start(),
            cancellation.whenCancelled.then<LoginAttempt>(
              (_) => throw const AuthCancelled(),
            ),
          ]).timeout(
            _remaining(overall),
            onTimeout: () => throw const AuthFailure('timeout'),
          );
      _check(generation);
      if (!desktop && attempt.redirectUri.toString() != template)
        throw const AuthFailure('invalid_callback');
      if (desktop &&
          (attempt.redirectUri.scheme != 'http' ||
              attempt.redirectUri.host != '127.0.0.1' ||
              attempt.redirectUri.port < 1 ||
              attempt.redirectUri.port > 65535 ||
              attempt.redirectUri.path != '/oauth/callback' ||
              attempt.redirectUri.hasQuery ||
              attempt.redirectUri.hasFragment ||
              attempt.redirectUri.userInfo.isNotEmpty))
        throw const AuthFailure('invalid_callback');
      final uri = authorize.replace(
        queryParameters: {
          'response_type': 'code',
          'client_id': client,
          'scope': scope,
          'redirect_uri': attempt.redirectUri.toString(),
          'code_challenge': pkce.challenge,
          'code_challenge_method': 'S256',
          'state': pkce.state,
          'nonce': pkce.nonce,
        },
      );
      final remaining = loginTimeout - overall.elapsed;
      if (remaining <= Duration.zero) throw const AuthFailure('timeout');
      final results = await Future.any<List<Object?>>(
        [
          Future.wait<Object?>([
            browser.open(uri).then<Object?>((opened) {
              if (!opened) throw const AuthFailure('browser_unavailable');
              return opened;
            }),
            attempt.callback,
          ], eagerError: true),
          cancellation.whenCancelled.then<List<Object?>>(
            (_) => throw const AuthCancelled(),
          ),
        ],
      ).timeout(remaining, onTimeout: () => throw const AuthFailure('timeout'));
      _check(generation);
      final returned = results[1]! as Uri;
      if (!validCallback(returned, attempt.redirectUri, pkce.state))
        throw const AuthFailure('invalid_callback');
      if (returned.queryParameters.containsKey('error'))
        throw const AuthFailure('provider_rejected');
      await callback.close();
      _callback = null;
      _check(generation);
      final tokenClock = Stopwatch()..start();
      final tokens = await api.json(
        'POST',
        '/api/v1/native/oauth/token',
        body: {
          'platform': platform,
          'code': returned.queryParameters['code'],
          'code_verifier': pkce.verifier,
          'redirect_uri': attempt.redirectUri.toString(),
          'nonce': pkce.nonce,
        },
        cancellation: cancellation,
        timeout: _remaining(overall),
      );
      _check(generation);
      final access = stringField(tokens, 'access_token');
      final expiry = wireTime(tokens['expires_at']);
      final server = wireTime(tokens['server_time']);
      if (stringField(tokens, 'token_type', max: 16) != 'Bearer')
        throw const AuthFailure('invalid_token_response');
      final identity = await api.json(
        'GET',
        '/api/v1/me',
        bearer: access,
        cancellation: cancellation,
        timeout: _remaining(overall),
      );
      _check(generation);
      final budget = expiry.difference(server) - tokenClock.elapsed;
      if (budget <= Duration.zero) throw const AuthFailure('expired');
      final result = AuthSession(
        origin: api.origin,
        tenantId: uuidField(identity, 'tenant_id'),
        userId: uuidField(identity, 'user_id'),
        accessToken: access,
        expiresAt: expiry,
        generation: generation,
        displayName: stringField(identity, 'display_name', max: 1024),
        budget: budget,
      );
      final refresh = refreshEnabled && tokens.containsKey('refresh_token')
          ? stringField(tokens, 'refresh_token', max: 4096)
          : null;
      _session = result;
      _refreshToken = refresh;
      _refreshEnabled = refreshEnabled;
      _platform = platform;
      return result;
    } on ApiFailure catch (e) {
      if (cancellation.isCancelled || generation != _generation)
        throw const AuthCancelled();
      _clear();
      throw AuthFailure(e.status == 401 ? 'reauthenticate' : e.code);
    } on AuthFailure {
      if (generation != _generation) throw const AuthCancelled();
      _clear();
      rethrow;
    } catch (_) {
      if (generation != _generation) throw const AuthCancelled();
      _clear();
      throw const AuthFailure('invalid_response');
    } finally {
      cancellation.cancel();
      if (callback != null) await callback.close();
      if (_generation == generation) {
        _cancel = null;
        _callback = null;
        _busy = false;
      }
    }
  }

  Future<AuthSession> refresh() async {
    if (_busy) throw const AuthFailure('login_in_progress');
    final previous = _session;
    final token = _refreshToken;
    final platform = _platform;
    if (previous == null ||
        token == null ||
        platform == null ||
        !_refreshEnabled)
      throw const AuthFailure('reauthenticate');
    _busy = true;
    final generation = _generation;
    final cancellation = Cancellation();
    _cancel = cancellation;
    final clock = Stopwatch()..start();
    try {
      final tokens = await api.json(
        'POST',
        '/api/v1/native/oauth/refresh',
        body: {'platform': platform, 'refresh_token': token},
        cancellation: cancellation,
      );
      _check(generation);
      final access = stringField(tokens, 'access_token');
      final expiry = wireTime(tokens['expires_at']);
      final server = wireTime(tokens['server_time']);
      if (stringField(tokens, 'token_type', max: 16) != 'Bearer')
        throw const AuthFailure('invalid_token_response');
      final identity = await api.json(
        'GET',
        '/api/v1/me',
        bearer: access,
        cancellation: cancellation,
      );
      _check(generation);
      if (uuidField(identity, 'tenant_id') != previous.tenantId ||
          uuidField(identity, 'user_id') != previous.userId)
        throw const AuthFailure('identity_changed');
      final budget = expiry.difference(server) - clock.elapsed;
      if (budget <= Duration.zero) throw const AuthFailure('expired');
      final rotated = tokens.containsKey('refresh_token')
          ? stringField(tokens, 'refresh_token', max: 4096)
          : token;
      final result = AuthSession(
        origin: api.origin,
        tenantId: previous.tenantId,
        userId: previous.userId,
        accessToken: access,
        expiresAt: expiry,
        generation: generation,
        displayName: stringField(identity, 'display_name', max: 1024),
        budget: budget,
      );
      _session = result;
      _refreshToken = rotated;
      return result;
    } on ApiFailure catch (e) {
      if (cancellation.isCancelled || generation != _generation)
        throw const AuthCancelled();
      if (e.status == 401) _clear();
      throw AuthFailure(e.status == 401 ? 'reauthenticate' : e.code);
    } on AuthFailure {
      if (generation != _generation) throw const AuthCancelled();
      _clear();
      rethrow;
    } catch (_) {
      if (generation != _generation) throw const AuthCancelled();
      _clear();
      throw const AuthFailure('invalid_response');
    } finally {
      cancellation.cancel();
      if (_generation == generation) {
        _cancel = null;
        _busy = false;
      }
    }
  }

  Future<void> cancel() async {
    _generation++;
    _cancel?.cancel();
    _cancel = null;
    _clear();
    _busy = false;
    final callback = _callback;
    _callback = null;
    await callback?.close();
  }

  Duration _remaining(Stopwatch clock) {
    final remaining = loginTimeout - clock.elapsed;
    if (remaining <= Duration.zero) throw const AuthFailure('timeout');
    return remaining;
  }

  static Uri _httpsEndpoint(String raw) {
    final uri = Uri.tryParse(raw);
    if (uri == null ||
        uri.scheme != 'https' ||
        uri.host.isEmpty ||
        uri.userInfo.isNotEmpty ||
        uri.hasQuery ||
        uri.hasFragment)
      throw const AuthFailure('invalid_configuration');
    return uri;
  }
}
