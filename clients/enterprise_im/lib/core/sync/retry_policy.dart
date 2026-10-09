import 'dart:math';
import '../protocol/api_client.dart';

enum RetryAction { retry, reauthenticate, stop, resync, conflict, expired }

final class RetryDecision {
  final RetryAction action;
  final Duration? delay;
  const RetryDecision(this.action, [this.delay]);
}

final class RetryPolicy {
  final double Function() random;
  RetryPolicy({double Function()? random})
    : random = random ?? Random.secure().nextDouble;
  double _jitter() {
    final value = random();
    if (!value.isFinite || value < 0 || value >= 1)
      throw const FormatException('重试抖动无效');
    return value;
  }

  RetryDecision next(ApiFailure error, int attempt) {
    if (attempt < 0) throw const FormatException('重试次数无效');
    if (error.status == 401)
      return const RetryDecision(RetryAction.reauthenticate);
    if (error.status == 403 ||
        error.status == 404 ||
        error.code == 'hard_deny' ||
        error.code == 'invalid_context' ||
        error.code == 'invalid_response' ||
        error.code == 'cancelled') {
      return const RetryDecision(RetryAction.stop);
    }
    if (error.code == 'sync_proof_expired' ||
        error.code == 'sync_context_changed' ||
        error.code == 'sync_gap')
      return const RetryDecision(RetryAction.resync);
    if (error.status == 409) return const RetryDecision(RetryAction.conflict);
    if (error.status == 410) return const RetryDecision(RetryAction.expired);
    if (error.status == 429 && error.retryAfter != null) {
      final delay = error.retryAfter!;
      if (delay <= Duration.zero || delay > const Duration(days: 1))
        throw const FormatException('重试期限无效');
      return RetryDecision(RetryAction.retry, delay);
    }
    if (error.status != null &&
        error.status! >= 400 &&
        error.status! < 500 &&
        error.status != 429)
      return const RetryDecision(RetryAction.stop);
    final base = attempt < 5 ? (1000 << attempt) : 30000;
    final milliseconds = (base * (.8 + .4 * _jitter())).round().clamp(
      1000,
      30000,
    );
    return RetryDecision(
      RetryAction.retry,
      Duration(milliseconds: milliseconds),
    );
  }

  Duration pollInterval({required bool connected}) => connected
      ? Duration(milliseconds: 30000 + (_jitter() * 10000).round())
      : const Duration(seconds: 5);
}
