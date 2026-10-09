final class AuthFailure implements Exception {
  final String code;
  const AuthFailure(this.code);
  @override
  String toString() => '登录失败（$code）';
}

final class AuthCancelled extends AuthFailure {
  const AuthCancelled() : super('cancelled');
}

final class AuthSession {
  final Uri origin;
  final String tenantId, userId, accessToken, displayName;
  final DateTime expiresAt;
  final int generation;
  final Duration _budget;
  final Stopwatch _clock = Stopwatch()..start();
  AuthSession({
    required this.origin,
    required this.tenantId,
    required this.userId,
    required this.accessToken,
    required this.expiresAt,
    required this.generation,
    this.displayName = '',
    required Duration budget,
  }) : _budget = budget;
  Duration get remaining {
    final value = _budget - _clock.elapsed;
    return value.isNegative ? Duration.zero : value;
  }

  bool get isExpired => remaining == Duration.zero;
  @override
  String toString() => 'AuthSession(generation: $generation)';
}
