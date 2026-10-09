import 'dart:convert';
import 'package:cryptography/cryptography.dart';
import 'package:flutter_secure_storage/flutter_secure_storage.dart';
import '../protocol/api_client.dart';
import '../protocol/models.dart';

final class VaultFailure implements Exception {
  const VaultFailure();
  @override
  String toString() => '系统安全存储不可用';
}

Future<String> accountNamespace(
  Uri origin,
  String tenantId,
  String userId,
) async {
  final service = ApiClient.validateOrigin(origin);
  final ids = {'tenant_id': tenantId, 'user_id': userId};
  uuidField(ids, 'tenant_id');
  uuidField(ids, 'user_id');
  final hash = await Sha256().hash(
    utf8.encode(
      jsonEncode([
        'enterprise-im-account-v1',
        service.origin,
        tenantId,
        userId,
      ]),
    ),
  );
  return hash.bytes
      .map((byte) => byte.toRadixString(16).padLeft(2, '0'))
      .join();
}

abstract interface class SecureVault {
  Future<String?> read(String namespace, String key);
  Future<void> write(String namespace, String key, String value);
  Future<void> delete(String namespace, String key);
}

final class OsSecureVault implements SecureVault {
  final FlutterSecureStorage _storage;
  OsSecureVault({FlutterSecureStorage? storage})
    : _storage =
          storage ??
          const FlutterSecureStorage(
            aOptions: AndroidOptions(
              resetOnError: false,
              migrateOnAlgorithmChange: false,
            ),
            iOptions: IOSOptions(
              accessibility: KeychainAccessibility.unlocked_this_device,
              synchronizable: false,
            ),
            mOptions: MacOsOptions(
              accessibility: KeychainAccessibility.unlocked_this_device,
              synchronizable: false,
            ),
          );
  String _key(String namespace, String key) {
    if (!RegExp(r'^[a-f0-9]{64}$').hasMatch(namespace) ||
        !{'session', 'key', 'refresh', 'device'}.contains(key)) {
      throw const FormatException('安全存储账号或记录类型无效');
    }
    return 'enterprise-im-v1/$namespace/$key';
  }

  @override
  Future<String?> read(String namespace, String key) async {
    final qualified = _key(namespace, key);
    try {
      return await _storage.read(key: qualified);
    } catch (_) {
      throw const VaultFailure();
    }
  }

  @override
  Future<void> write(String namespace, String key, String value) async {
    final qualified = _key(namespace, key);
    if (value.isEmpty || utf8.encode(value).length > 65536) {
      throw const FormatException('安全存储记录无效');
    }
    try {
      await _storage.write(key: qualified, value: value);
    } catch (_) {
      throw const VaultFailure();
    }
  }

  @override
  Future<void> delete(String namespace, String key) async {
    final qualified = _key(namespace, key);
    try {
      await _storage.delete(key: qualified);
    } catch (_) {
      throw const VaultFailure();
    }
  }
}
