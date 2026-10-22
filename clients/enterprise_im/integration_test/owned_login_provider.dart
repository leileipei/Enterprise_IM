// Synthetic authorization adapter only. The OS dispatch and callback plugin are
// real; there is no system-browser HTTPS page or customer identity provider.
import 'dart:async';
import 'dart:io';
import 'package:enterprise_im/platform/browser_launcher.dart';
import 'package:enterprise_im/platform/login_callback.dart';

final class ObservedMobileCallback implements LoginCallback {
  final MobileLoginCallback inner;
  final void Function() accepted;
  ObservedMobileCallback(this.inner, this.accepted);
  @override
  Future<LoginAttempt> start() async {
    final actual = await inner.start();
    return LoginAttempt(
      actual.redirectUri,
      actual.callback.then((uri) {
        accepted();
        return uri;
      }),
    );
  }

  @override
  Future<void> close() => inner.close();
}

final class OwnedLoginProvider implements BrowserLauncher {
  final Uri origin;
  final String role;
  final AppLinksSource source;
  final bool Function() completed;
  int authorizeRequests = 0;
  bool wrongStateIgnored = false, callbackViaOS = false;
  OwnedLoginProvider(this.origin, this.role, this.source, this.completed);
  @override
  Future<bool> open(Uri uri) async {
    if (uri.scheme != 'https' ||
        uri.host != '127.0.0.1' ||
        uri.path != '/authorize' ||
        uri.hasFragment ||
        uri.userInfo.isNotEmpty)
      throw StateError('unowned authorize URI');
    final client = HttpClient();
    try {
      final request = await client.getUrl(
        origin.resolve('/fixture/authorize').replace(query: uri.query),
      );
      request.followRedirects = false;
      request.headers.set('X-IM-Fixture-Role', role);
      authorizeRequests++;
      final response = await request.close();
      if (response.statusCode != 302 ||
          response.headers.value('cache-control') != 'no-store')
        throw StateError('owned authorization rejected');
      final location = response.headers.value('location');
      await response.drain<void>();
      if (location == null) throw StateError('owned callback missing');
      final returned = Uri.parse(location);
      if (!validCallback(
        returned,
        MobileLoginCallback.redirect,
        uri.queryParameters['state']!,
      ))
        throw StateError('owned provider callback invalid');
      final invalid = returned.replace(
        queryParameters: {
          ...returned.queryParameters,
          'state': 'unrelated-attempt',
        },
      );
      final launcher = ExternalBrowserLauncher();
      if (!await launcher.open(invalid))
        throw StateError('OS callback launch failed');
      final end = Stopwatch()..start();
      while (source.initial != invalid) {
        if (end.elapsed > const Duration(seconds: 15))
          throw StateError('OS invalid callback not received');
        await Future<void>.delayed(const Duration(milliseconds: 30));
      }
      await Future<void>.delayed(const Duration(milliseconds: 200));
      if (completed()) throw StateError('wrong-state callback completed login');
      wrongStateIgnored = true;
      if (!await launcher.open(returned))
        throw StateError('OS valid callback launch failed');
      callbackViaOS = true;
      return true;
    } finally {
      client.close(force: true);
    }
  }
}
