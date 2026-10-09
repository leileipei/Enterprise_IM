import 'dart:async';
import 'dart:io';
import 'package:app_links/app_links.dart';
import '../core/auth/auth_session.dart';

final class LoginAttempt {
  final Uri redirectUri;
  final Future<Uri> callback;
  const LoginAttempt(this.redirectUri, this.callback);
}

abstract interface class LoginCallback {
  Future<LoginAttempt> start();
  Future<void> close();
}

bool validCallback(Uri uri, Uri redirect, String state) {
  if (uri.hasFragment ||
      uri.userInfo.isNotEmpty ||
      uri.scheme != redirect.scheme ||
      uri.host != redirect.host ||
      uri.port != redirect.port ||
      uri.path != redirect.path)
    return false;
  if (uri.toString().split('?').first != redirect.toString()) return false;
  final query = uri.queryParametersAll;
  if (query['state']?.length != 1 || query['state']!.single != state)
    return false;
  final code = query['code'];
  final error = query['error'];
  if ((code == null) == (error == null)) return false;
  if (code != null &&
      (code.length != 1 ||
          code.single.isEmpty ||
          code.single.length > 2048 ||
          RegExp(r'[\x00-\x20\x7f]').hasMatch(code.single)))
    return false;
  if (error != null &&
      (error.length != 1 ||
          !RegExp(r'^[a-z][a-z0-9_]{0,63}$').hasMatch(error.single)))
    return false;
  return true;
}

final class LoopbackLoginCallback implements LoginCallback {
  final String state;
  HttpServer? _server;
  StreamSubscription<HttpRequest>? _subscription;
  final _result = Completer<Uri>();
  bool _closed = false;
  LoopbackLoginCallback(this.state) {
    unawaited(
      _result.future.then<void>(
        (_) {},
        onError: (Object error, StackTrace stack) {},
      ),
    );
  }
  @override
  Future<LoginAttempt> start() async {
    if (_closed || _server != null)
      throw const AuthFailure('callback_unavailable');
    final server = await HttpServer.bind(
      InternetAddress.loopbackIPv4,
      0,
      shared: false,
    );
    if (_closed) {
      await server.close(force: true);
      throw const AuthCancelled();
    }
    _server = server;
    final redirect = Uri.parse(
      'http://127.0.0.1:${server.port}/oauth/callback',
    );
    _subscription = server.listen(
      (request) {
        unawaited(_receive(request, redirect));
      },
      onError: (Object error) {
        if (!_result.isCompleted)
          _result.completeError(const AuthFailure('callback_unavailable'));
      },
    );
    return LoginAttempt(redirect, _result.future);
  }

  Future<void> _receive(HttpRequest request, Uri redirect) async {
    request.response.headers.set(HttpHeaders.cacheControlHeader, 'no-store');
    request.response.headers.set('X-Content-Type-Options', 'nosniff');
    request.response.headers.set('Connection', 'close');
    final callback = redirect.resolveUri(request.uri);
    if (_closed ||
        _result.isCompleted ||
        request.method != 'GET' ||
        request.uri.path != '/oauth/callback' ||
        !validCallback(callback, redirect, state)) {
      request.response.statusCode = 400;
    } else {
      request.response.statusCode = 200;
      request.response.headers.contentType = ContentType.text;
      request.response.write('登录回调已收到，可返回企业 IM。');
      _result.complete(callback);
    }
    try {
      await request.response.close();
    } catch (_) {
      /* Request peer may close; no token/body is logged. */
    }
  }

  @override
  Future<void> close() async {
    if (_closed) return;
    _closed = true;
    if (!_result.isCompleted) _result.completeError(const AuthCancelled());
    await _server?.close(force: true);
    await _subscription?.cancel();
    _server = null;
    _subscription = null;
  }
}

/// Subscribe before rendering the mobile login UI, including a cold-start link.
final class AppLinksSource {
  final _links = AppLinks();
  final _controller = StreamController<Uri>.broadcast();
  StreamSubscription<Uri>? _subscription;
  Uri? initial;
  bool _closed = false;
  Stream<Uri> get links => _controller.stream;
  Future<void> initialize() async {
    _subscription = _links.uriLinkStream.listen(
      (uri) {
        if (!_closed) {
          initial = uri;
          _controller.add(uri);
        }
      },
      onError: (Object error) {
        /* Closed errors are handled by login timeout. */
      },
    );
    final first = await _links.getInitialLink();
    if (!_closed && first != null) {
      initial = first;
      _controller.add(first);
    }
  }

  Future<void> close() async {
    _closed = true;
    await _subscription?.cancel();
    await _controller.close();
  }
}

final class MobileLoginCallback implements LoginCallback {
  final String state;
  final AppLinksSource source;
  final _result = Completer<Uri>();
  StreamSubscription<Uri>? _subscription;
  bool _closed = false;
  static final redirect = Uri.parse('com.enterpriseim.client:/oauth/callback');
  MobileLoginCallback(this.state, this.source) {
    unawaited(
      _result.future.then<void>(
        (_) {},
        onError: (Object error, StackTrace stack) {},
      ),
    );
  }
  void _receive(Uri uri) {
    if (!_closed && !_result.isCompleted && validCallback(uri, redirect, state))
      _result.complete(uri);
  }

  @override
  Future<LoginAttempt> start() async {
    if (_closed || _subscription != null)
      throw const AuthFailure('callback_unavailable');
    _subscription = source.links.listen(_receive);
    if (source.initial != null) _receive(source.initial!);
    return LoginAttempt(redirect, _result.future);
  }

  @override
  Future<void> close() async {
    if (_closed) return;
    _closed = true;
    await _subscription?.cancel();
    _subscription = null;
    if (!_result.isCompleted) _result.completeError(const AuthCancelled());
  }
}
