/// Canonical signed-64-bit nonnegative wire sequence; never a floating point.
final class Seq implements Comparable<Seq> {
  final BigInt value;
  static final maximum = BigInt.parse('9223372036854775807');
  Seq._(this.value);
  factory Seq.parse(String raw) {
    if (raw.length > 19 || !RegExp(r'^(0|[1-9][0-9]*)$').hasMatch(raw)) {
      throw const FormatException('消息编号无效');
    }
    final value = BigInt.parse(raw);
    if (value > maximum) throw const FormatException('消息编号越界');
    return Seq._(value);
  }
  factory Seq.fromWire(Object? raw, {bool legacyInteger = false}) {
    if (raw is String) return Seq.parse(raw);
    if (legacyInteger && raw is int) return Seq.parse(raw.toString());
    throw const FormatException('消息编号格式无效');
  }
  Seq get next => Seq.parse((value + BigInt.one).toString());
  @override
  int compareTo(Seq other) => value.compareTo(other.value);
  @override
  String toString() => value.toString();
  @override
  bool operator ==(Object other) => other is Seq && value == other.value;
  @override
  int get hashCode => value.hashCode;
}
