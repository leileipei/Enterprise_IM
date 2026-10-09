import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/sync/retry_policy.dart';

void main() {
  test(
    'authentication scope conflicts and proof expiry have distinct recovery actions',
    () {
      final policy = RetryPolicy(random: () => .5);
      expect(
        policy.next(const ApiFailure('unauthorized', status: 401), 0).action,
        RetryAction.reauthenticate,
      );
      expect(
        policy
            .next(const ApiFailure('invalid_identity', status: 403), 0)
            .action,
        RetryAction.stop,
      );
      expect(
        policy
            .next(const ApiFailure('sync_context_changed', status: 409), 0)
            .action,
        RetryAction.resync,
      );
      expect(
        policy
            .next(const ApiFailure('sync_proof_expired', status: 410), 0)
            .action,
        RetryAction.resync,
      );
      expect(
        policy
            .next(const ApiFailure('version_conflict', status: 409), 0)
            .action,
        RetryAction.conflict,
      );
      expect(
        policy.next(const ApiFailure('expired', status: 410), 0).action,
        RetryAction.expired,
      );
    },
  );
  test(
    '429 respects RetryAfter and503 reconnect stays between1and30seconds',
    () {
      for (final value in [0.0, .5, .999]) {
        final policy = RetryPolicy(random: () => value);
        for (var attempt = 0; attempt < 10; attempt++) {
          final result = policy.next(
            const ApiFailure('unavailable', status: 503),
            attempt,
          );
          expect(result.action, RetryAction.retry);
          expect(result.delay!.inMilliseconds, inInclusiveRange(1000, 30000));
        }
        expect(
          policy
              .next(
                const ApiFailure(
                  'rate_limited',
                  status: 429,
                  retryAfter: Duration(seconds: 17),
                ),
                8,
              )
              .delay,
          const Duration(seconds: 17),
        );
        expect(
          policy.pollInterval(connected: false),
          const Duration(seconds: 5),
        );
        expect(
          policy.pollInterval(connected: true).inMilliseconds,
          inInclusiveRange(30000, 40000),
        );
      }
    },
  );
}
