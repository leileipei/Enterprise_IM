import 'dart:io';
import 'package:path_provider/path_provider.dart';
import '../core/storage/encrypted_store.dart';

// Android's application mount namespace exposes this OS-owned alias even
// when the adb shell namespace reports a directory at the same path. Only
// this verified system prefix is normalized; application paths are not resolved.
Future<Directory> nativeStorageRoot() async {
  final support = await getApplicationSupportDirectory();
  if (!Platform.isAndroid) return support;
  return normalizeSystemStorageAlias(
    support,
    alias: Directory('/data/user/0'),
    target: Directory('/data/data'),
  );
}

// alias and target are trusted platform constants, not account/user input.
// Separate filesystem operation permits real-link security regression tests.
Future<Directory> normalizeSystemStorageAlias(
  Directory directory, {
  required Directory alias,
  required Directory target,
}) async {
  final path = directory.absolute.path;
  final prefix = alias.absolute.path, destination = target.absolute.path;
  if (path != prefix && !path.startsWith('$prefix/')) return directory;
  if (path.split('/').any((part) => part == '.' || part == '..')) {
    throw const StoreFailure();
  }
  if (await FileSystemEntity.type(prefix, followLinks: false) !=
      FileSystemEntityType.link)
    return directory;
  if (await Link(prefix).target() != destination) throw const StoreFailure();
  // Preserve every child component verbatim so EncryptedStore still rejects
  // links at app/files/accounts/database/WAL/SHM or the destination itself.
  return Directory('$destination${path.substring(prefix.length)}');
}
