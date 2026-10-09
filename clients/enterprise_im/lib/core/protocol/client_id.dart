import 'dart:math';
import '../storage/records.dart';

// A server timestamp and process-monotonic elapsed time. Local wall time never
// contributes to a registration/message identity. This is not a reading lease.
final class ServerClock {
  final DateTime _server;
  final Duration _baseline;
  final Duration Function() _elapsed;
  Duration _last;
  ServerClock._(this._server, this._baseline, this._elapsed)
    : _last = _baseline;
  factory ServerClock.start(
    DateTime server,
    Duration requestStarted, {
    Duration Function()? now,
  }) {
    if (!server.isUtc || requestStarted.isNegative)
      throw const FormatException('服务时间无效');
    return ServerClock._(
      server,
      requestStarted,
      now ?? LeaseClock.monotonicNow,
    );
  }
  DateTime get now {
    final value = _elapsed();
    if (value < _baseline || value < _last)
      throw const FormatException('单调时钟无效');
    _last = value;
    return _server.add(value - _baseline);
  }
}

final class ClientMessageId {
  static final Random _random = Random.secure();
  static String create(ServerClock clock, [int Function(int)? secureRandom]) {
    final milliseconds = clock.now.millisecondsSinceEpoch;
    if (milliseconds < 0 || milliseconds > 0xffffffffffff)
      throw const FormatException('服务时间无效');
    final random = secureRandom ?? _random.nextInt;
    final bytes = List<int>.generate(16, (_) => random(256));
    if (bytes.any((b) => b < 0 || b > 255))
      throw const FormatException('随机源无效');
    for (var index = 0; index < 6; index++)
      bytes[index] = (milliseconds >> ((5 - index) * 8)) & 255;
    bytes[6] = (bytes[6] & 15) | 0x70;
    bytes[8] = (bytes[8] & 63) | 0x80;
    final hex = bytes.map((b) => b.toRadixString(16).padLeft(2, '0')).join();
    return '${hex.substring(0, 8)}-${hex.substring(8, 12)}-${hex.substring(12, 16)}-${hex.substring(16, 20)}-${hex.substring(20)}';
  }
}
