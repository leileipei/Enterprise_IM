import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/storage/records.dart';

void main() {
  test('testRestartAndClockRollbackCannotExtendLease', () {
    final server = DateTime.utc(2026, 10, 9);
    var elapsed = const Duration(seconds: 10);
    final lease = LeaseClock.start(
      server,
      elapsed,
      server.add(const Duration(seconds: 300)),
      now: () => elapsed,
    );
    expect(lease.remaining, const Duration(seconds: 300));
    elapsed = const Duration(seconds: 309);
    expect(lease.remaining, const Duration(seconds: 1));
    elapsed = const Duration(seconds: 5);
    expect(lease.remaining, Duration.zero);
    elapsed = const Duration(seconds: 20);
    expect(lease.remaining, Duration.zero);
    // No saved wall-clock timestamp can restore an authorized monotonic baseline.
    expect(LeaseClock.unauthorized().remaining, Duration.zero);
  });
  test(
    'network delay is deducted and zero-cache never grants offline reading',
    () {
      final server = DateTime.utc(2026, 10, 9);
      final lease = LeaseClock.start(
        server,
        const Duration(seconds: 10),
        server.add(const Duration(seconds: 300)),
        now: () => const Duration(seconds: 15),
      );
      expect(lease.remaining, const Duration(seconds: 295));
      final zero = LeaseClock.start(
        server,
        Duration.zero,
        server,
        now: () => Duration.zero,
      );
      expect(zero.remaining, Duration.zero);
      lease.invalidate();
      expect(lease.remaining, Duration.zero);
    },
  );
  test(
    'lease deadline expires at equality and invalid time contracts are refused',
    () {
      final server = DateTime.utc(2026, 10, 9);
      final expired = LeaseClock.start(
        server,
        Duration.zero,
        server.add(const Duration(seconds: 2)),
        now: () => const Duration(seconds: 2),
      );
      expect(expired.remaining, Duration.zero);
      expect(
        () => LeaseClock.start(
          server,
          Duration.zero,
          server.subtract(const Duration(seconds: 1)),
        ),
        throwsFormatException,
      );
      expect(
        () => LeaseClock.start(
          server,
          Duration.zero,
          server.add(const Duration(seconds: 3601)),
        ),
        throwsFormatException,
      );
    },
  );
}
