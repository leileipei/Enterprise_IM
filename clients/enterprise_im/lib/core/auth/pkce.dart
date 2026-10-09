import 'dart:convert';
import 'dart:math';
import 'package:cryptography/cryptography.dart';

final class PKCE {
  final String verifier, challenge, state, nonce;
  PKCE._(this.verifier, this.challenge, this.state, this.nonce);
  static String _random(Random random) => base64Url
      .encode(List<int>.generate(32, (_) => random.nextInt(256)))
      .replaceAll('=', '');
  static Future<String> challengeFor(String verifier) async => base64Url
      .encode((await Sha256().hash(ascii.encode(verifier))).bytes)
      .replaceAll('=', '');
  static Future<PKCE> generate() async {
    final random = Random.secure();
    final verifier = _random(random);
    return PKCE._(
      verifier,
      await challengeFor(verifier),
      _random(random),
      _random(random),
    );
  }
}
