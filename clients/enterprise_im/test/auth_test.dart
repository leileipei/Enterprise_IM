import 'dart:async';
import 'dart:convert';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/auth/native_auth.dart';
import 'package:enterprise_im/core/auth/pkce.dart';
import 'package:enterprise_im/platform/browser_launcher.dart';
import 'package:enterprise_im/platform/login_callback.dart';

class FakeTransport implements ApiTransport {
  Future<ApiResponse> Function(ApiRequest, Cancellation) handler;
  FakeTransport(this.handler);
  @override
  Future<ApiResponse> send(ApiRequest request, Cancellation cancel) =>
      handler(request, cancel);
  @override
  void close() {}
}

class FakeCallback implements LoginCallback {
  final result = Completer<Uri>();
  bool closed = false;
  @override
  Future<LoginAttempt> start() async => LoginAttempt(
    Uri.parse('http://127.0.0.1:49152/oauth/callback'),
    result.future,
  );
  @override
  Future<void> close() async {
    closed = true;
    if (!result.isCompleted) result.completeError(const AuthCancelled());
  }
}

class DeferredCallback implements LoginCallback {
  final startResult = Completer<LoginAttempt>();
  final started = Completer<void>();
  bool closed = false;
  @override
  Future<LoginAttempt> start() {
    started.complete();
    return startResult.future;
  }

  @override
  Future<void> close() async {
    closed = true;
  }
}

class FakeBrowser implements BrowserLauncher {
  final Future<bool> Function(Uri) handler;
  FakeBrowser(this.handler);
  @override
  Future<bool> open(Uri uri) => handler(uri);
}

ApiResponse response(Map<String, Object?> json) =>
    ApiResponse(200, utf8.encode(jsonEncode(json)), const {});
Map<String, Object?> config() => {
  'platform': 'macos',
  'issuer': 'https://idp.example.test',
  'authorization_url': 'https://idp.example.test/authorize',
  'client_id': 'im-macos',
  'scope': 'openid profile',
  'redirect_uri': 'http://127.0.0.1/oauth/callback',
  'loopback': true,
  'refresh_enabled': true,
  'server_time': '2026-10-09T00:00:00Z',
};
Map<String, Object?> tokens(String refresh) => {
  'access_token': 'private-access',
  'refresh_token': refresh,
  'token_type': 'Bearer',
  'expires_at': '2026-10-09T01:00:00Z',
  'server_time': '2026-10-09T00:00:00Z',
};
const self = {
  'tenant_id': '11111111-1111-4111-8111-111111111111',
  'user_id': '22222222-2222-4222-8222-222222222222',
  'display_name': '人员',
  'global_employee_no': '1',
  'memberships': <Object?>[],
};
void main() {
  test('testAuthStateNonceCallbackAndCancellation', () async {
    final key = await PKCE.generate();
    expect(key.verifier.length, 43);
    expect(key.challenge.length, 43);
    expect(base64Url.decode(base64Url.normalize(key.state)).length, 32);
    expect(key.nonce, isNot(key.state));
    expect((await PKCE.challengeFor(key.verifier)), key.challenge);
    var callback = FakeCallback();
    Uri? opened;
    String? expectedState;
    int exchanges = 0;
    final transport = FakeTransport((req, cancel) async {
      if (req.uri.path.endsWith('/config')) return response(config());
      if (req.uri.path.endsWith('/token')) {
        exchanges++;
        final body = jsonDecode(utf8.decode(req.body!)) as Map<String, Object?>;
        expect(body['platform'], 'macos');
        expect(body['redirect_uri'], 'http://127.0.0.1:49152/oauth/callback');
        expect(body['nonce'], opened!.queryParameters['nonce']);
        expect(
          await PKCE.challengeFor(body['code_verifier']! as String),
          opened!.queryParameters['code_challenge'],
        );
        return response(tokens('refresh-1'));
      }
      if (req.uri.path.endsWith('/refresh'))
        return response(tokens('refresh-2'));
      expect(req.headers['Authorization'], 'Bearer private-access');
      return response(self);
    });
    final api = ApiClient(Uri.parse('https://im.example.test'), transport);
    final auth = NativeAuth(
      api,
      callbackFactory: (platform, state) {
        expectedState = state;
        callback = FakeCallback();
        return callback;
      },
      browser: FakeBrowser((uri) async {
        opened = uri;
        expect(uri.queryParameters['state'], expectedState);
        expect(uri.queryParameters['code_challenge_method'], 'S256');
        callback.result.complete(
          Uri.parse(
            'http://127.0.0.1:49152/oauth/callback?code=owned-code&state=$expectedState',
          ),
        );
        return true;
      }),
    );
    final session = await auth.signIn('macos');
    expect(session.userId, self['user_id']);
    expect(session.accessToken, 'private-access');
    expect(callback.closed, isTrue);
    expect(exchanges, 1);
    final refreshed = await auth.refresh();
    expect(refreshed.generation, session.generation);
    expect(refreshed.userId, session.userId);
    await auth.cancel();
    expect(auth.session, isNull);
    final bad = NativeAuth(
      api,
      callbackFactory: (platform, state) {
        callback = FakeCallback();
        return callback;
      },
      browser: FakeBrowser((uri) async {
        callback.result.complete(
          Uri.parse(
            'http://127.0.0.1:49152/oauth/callback?code=owned-code&state=wrong',
          ),
        );
        return true;
      }),
    );
    await expectLater(bad.signIn('macos'), throwsA(isA<AuthFailure>()));
    expect(exchanges, 1);
    expect(bad.session, isNull);
    expect(callback.closed, isTrue);
  });
  test('late exchange after cancellation never creates a session', () async {
    final exchange = Completer<ApiResponse>();
    final called = Completer<void>();
    var cb = FakeCallback();
    final api = ApiClient(
      Uri.parse('https://im.example.test'),
      FakeTransport((req, cancel) async {
        if (req.uri.path.endsWith('/config')) return response(config());
        called.complete();
        return exchange.future;
      }),
    );
    final auth = NativeAuth(
      api,
      callbackFactory: (p, s) {
        cb = FakeCallback();
        return cb;
      },
      browser: FakeBrowser((uri) async {
        cb.result.complete(
          Uri.parse(
            'http://127.0.0.1:49152/oauth/callback?code=c&state=${uri.queryParameters['state']}',
          ),
        );
        return true;
      }),
    );
    final login = auth.signIn('macos');
    final rejected = expectLater(login, throwsA(isA<AuthCancelled>()));
    await called.future;
    await auth.cancel();
    await rejected;
    exchange.complete(response(tokens('late-secret')));
    await Future<void>.delayed(Duration.zero);
    expect(auth.session, isNull);
    expect(cb.closed, isTrue);
  });
  test('late callback startup failure cannot erase a newer login', () async {
    final delayed = DeferredCallback();
    var callback = FakeCallback();
    int attempts = 0;
    final api = ApiClient(
      Uri.parse('https://im.example.test'),
      FakeTransport((r, c) async {
        if (r.uri.path.endsWith('/config')) return response(config());
        if (r.uri.path.endsWith('/token'))
          return response(tokens('new-refresh'));
        return response(self);
      }),
    );
    final auth = NativeAuth(
      api,
      callbackFactory: (p, s) {
        attempts++;
        if (attempts == 1) return delayed;
        callback = FakeCallback();
        return callback;
      },
      browser: FakeBrowser((uri) async {
        callback.result.complete(
          Uri.parse(
            'http://127.0.0.1:49152/oauth/callback?code=new-code&state=${uri.queryParameters['state']}',
          ),
        );
        return true;
      }),
    );
    final first = auth.signIn('macos');
    final rejected = expectLater(first, throwsA(isA<AuthCancelled>()));
    await delayed.started.future;
    await auth.cancel();
    final newer = await auth.signIn('macos');
    expect(auth.session, same(newer));
    delayed.startResult.completeError(
      const AuthFailure('callback_unavailable'),
    );
    await rejected;
    expect(auth.session, same(newer));
    expect(delayed.closed, isTrue);
    await auth.cancel();
  });
  test('unsafe service origins and redirects cannot send bearer', () async {
    for (final raw in [
      'http://im.example.test',
      'https://user:pass@im.example.test',
      'https://im.example.test/path',
      'https://im.example.test?token=secret',
      'https://im.example.test#fragment',
    ]) {
      expect(
        () => ApiClient(
          Uri.parse(raw),
          FakeTransport((r, c) async => response({})),
        ),
        throwsFormatException,
      );
    }
    int calls = 0;
    final api = ApiClient(
      Uri.parse('https://im.example.test'),
      FakeTransport((r, c) async {
        calls++;
        return ApiResponse(307, const [], const {
          'location': 'https://other.example.test/stolen',
        });
      }),
    );
    await expectLater(
      api.json('GET', '/api/v1/me', bearer: 'secret'),
      throwsA(isA<ApiFailure>()),
    );
    expect(calls, 1);
    await expectLater(
      api.json('GET', 'https://other.example.test/stolen', bearer: 'secret'),
      throwsA(isA<FormatException>()),
    );
    expect(calls, 1);
  });
  test(
    'full 500-message Go JSON-escaped sync page fits the bounded client response',
    () async {
      final message = {
        'seq': '1',
        'message_id': self['tenant_id'],
        'sender_user_id': self['user_id'],
        'message_type': 'text',
        'text': 'GO_ESCAPED_TEXT',
        'server_time': '2026-10-09T00:00:00Z',
        'content_expires_at': '2026-10-09T01:00:00Z',
        'cache_until': '2026-10-09T00:05:00Z',
      };
      final escaped = r'\u0026' * 16384;
      final raw = jsonEncode({
        'conversation_id': self['tenant_id'],
        'messages': List.generate(500, (i) => {...message, 'seq': '${i + 1}'}),
        'next_after_seq': '500',
        'has_more': false,
        'receipt_revision': '1',
        'sync_proof': base64Url
            .encode(List<int>.filled(32, 7))
            .replaceAll('=', ''),
        'proof_expires_at': '2026-10-09T00:05:00Z',
        'server_time': '2026-10-09T00:00:00Z',
      }).replaceAll('GO_ESCAPED_TEXT', escaped);
      final bytes = utf8.encode(raw);
      final api = ApiClient(
        Uri.parse('https://im.example.test'),
        FakeTransport((r, c) async => ApiResponse(200, bytes, const {})),
      );
      final decoded = await api.json(
        'GET',
        '/api/v1/conversations/${self['tenant_id']}/sync?limit=500',
      );
      expect((decoded['messages']! as List<Object?>).length, 500);
      expect(bytes.length, greaterThan(500 * 16384 * 6));
      await expectLater(
        api.json('GET', '/api/v1/me'),
        throwsA(isA<ApiFailure>()),
      );
    },
  );
}
