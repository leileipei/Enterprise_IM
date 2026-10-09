final class ApiFailure implements Exception {
  final int? status;
  final String code;
  final Duration? retryAfter;
  const ApiFailure(this.code, {this.status, this.retryAfter});
  @override
  String toString() => '服务请求失败（$code）';
}
