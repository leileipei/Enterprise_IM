// Integration-test resources only. Native sandbox runs read their temporary
// bundled fixture and create a fresh private subdirectory, never host /tmp.
import 'dart:convert';
import 'dart:io';
import 'package:flutter/services.dart';
import 'package:enterprise_im/platform/native_storage.dart';
import 'package:enterprise_im/core/protocol/models.dart';

Future<Map<String, dynamic>> readOwnedBackendFixture({
  required bool host,
  required String path,
}) async {
  if (host) {
    if (path.isEmpty)
      throw StateError('explicit owned backend config required; no skip');
    return objectMap(jsonDecode(await File(path).readAsString()));
  }
  const bundled = bool.fromEnvironment('IM_NATIVE_BUNDLED_FIXTURE');
  if (!bundled)
    throw StateError('native sandbox requires the owned bundled test fixture');
  return objectMap(
    jsonDecode(await rootBundle.loadString('assets/owned_backend.json')),
  );
}

SecurityContext ownedBackendTrust(
  Map<String, dynamic> config, {
  required bool host,
}) {
  final trust = SecurityContext(withTrustedRoots: false);
  if (host) {
    trust.setTrustedCertificates(config['ca'] as String);
  } else {
    trust.setTrustedCertificatesBytes(utf8.encode(config['ca_pem'] as String));
  }
  return trust;
}

Future<Directory?> createOwnedNativeStorage({required bool host}) async {
  if (host) return null;
  final support = await nativeStorageRoot();
  await support.create(recursive: true);
  final root = await support.createTemp('im-native-backend-');
  // Opaque recovery handle, no credentials or user filesystem path.
  // ignore: avoid_print
  print(
    'NATIVE_BACKEND_STORAGE_OWNER ${root.path.split(Platform.pathSeparator).last}',
  );
  return root;
}
