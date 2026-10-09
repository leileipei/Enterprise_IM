// A reading grant exists only in this process. Persisted cache_until alone is
// never authority to read after restart. The caller must bind a fresh server
// response and capture its request-start monotonic time to deduct network delay.
final class LeaseClock {
  static final Stopwatch _processClock = Stopwatch()..start();
  static Duration monotonicNow() => _processClock.elapsed;
  final Duration Function() _now;
  final Duration _baseline, _budget;
  Duration _last;
  bool _authorized;
  LeaseClock._(this._now, this._baseline, this._budget, this._authorized)
    : _last = _baseline;
  factory LeaseClock.start(
    DateTime serverTime,
    Duration receivedMonotonic,
    DateTime cacheUntil, {
    Duration Function()? now,
  }) {
    final budget = cacheUntil.difference(serverTime);
    if (!serverTime.isUtc ||
        !cacheUntil.isUtc ||
        receivedMonotonic.isNegative ||
        budget.isNegative ||
        budget > const Duration(seconds: 3600)) {
      throw const FormatException('阅读租约基线无效');
    }
    return LeaseClock._(now ?? monotonicNow, receivedMonotonic, budget, true);
  }
  factory LeaseClock.unauthorized() =>
      LeaseClock._(monotonicNow, Duration.zero, Duration.zero, false);
  Duration get remaining {
    if (!_authorized) return Duration.zero;
    final current = _now();
    if (current < _last || current < _baseline) {
      invalidate();
      return Duration.zero;
    }
    _last = current;
    final value = _budget - (current - _baseline);
    if (value <= Duration.zero) {
      invalidate();
      return Duration.zero;
    }
    return value;
  }

  void invalidate() {
    _authorized = false;
  }
}
