import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/platform/login_callback.dart';

void main() {
  test(
    'real loopback ignores invalid callbacks and closes after completion',
    () async {
      final callback = LoopbackLoginCallback('owned-state');
      addTearDown(callback.close);
      final attempt = await callback.start();
      expect(attempt.redirectUri.host, '127.0.0.1');
      expect(attempt.redirectUri.port, greaterThan(0));
      final client = HttpClient()
        ..connectionTimeout = const Duration(seconds: 1);
      addTearDown(() => client.close(force: true));
      bool completed = false;
      final result = attempt.callback.then((value) {
        completed = true;
        return value;
      });
      for (final query in [
        'code=owned-code&state=wrong',
        'code=owned-code&state=owned-state&state=owned-state',
        'error=access_denied&code=owned-code&state=owned-state',
      ]) {
        final request = await client.getUrl(
          attempt.redirectUri.replace(query: query),
        );
        final response = await request.close();
        expect(response.statusCode, 400);
        await response.drain<void>();
        expect(completed, isFalse);
      }
      final invalid = await client.postUrl(
        attempt.redirectUri.replace(query: 'code=owned-code&state=owned-state'),
      );
      final rejected = await invalid.close();
      expect(rejected.statusCode, 400);
      await rejected.drain<void>();
      expect(completed, isFalse);
      final request = await client.getUrl(
        attempt.redirectUri.replace(query: 'code=owned-code&state=owned-state'),
      );
      final response = await request.close();
      expect(response.statusCode, 200);
      expect(response.headers.value('cache-control'), 'no-store');
      await response.drain<void>();
      final returned = await result;
      expect(returned.queryParameters['code'], 'owned-code');
      final duplicate = await client.getUrl(
        attempt.redirectUri.replace(
          query: 'code=second-code&state=owned-state',
        ),
      );
      final duplicateResponse = await duplicate.close();
      expect(duplicateResponse.statusCode, 400);
      await duplicateResponse.drain<void>();
      await callback.close();
      await expectLater(
        Socket.connect(
          '127.0.0.1',
          attempt.redirectUri.port,
          timeout: const Duration(seconds: 1),
        ),
        throwsA(isA<SocketException>()),
      );
    },
  );
  test(
    'cancellation closes a real loopback listener and callback future',
    () async {
      final callback = LoopbackLoginCallback('owned-state');
      final attempt = await callback.start();
      final rejected = expectLater(attempt.callback, throwsA(isA<Exception>()));
      await callback.close();
      await rejected;
      await expectLater(
        Socket.connect(
          '127.0.0.1',
          attempt.redirectUri.port,
          timeout: const Duration(seconds: 1),
        ),
        throwsA(isA<SocketException>()),
      );
    },
  );
}
