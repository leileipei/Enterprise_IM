import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:path_provider/path_provider.dart';
import 'package:enterprise_im/core/storage/encrypted_store.dart';
import 'package:enterprise_im/platform/native_storage.dart';

void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  testWidgets('native application storage opens from OS support directory', (
    tester,
  ) async {
    await tester.runAsync(() async {
      final rawSupport = await getApplicationSupportDirectory();
      final support = await nativeStorageRoot();
      // ignore: avoid_print
      print(
        'NATIVE_STORAGE_ROOT raw=${rawSupport.path} normalized=${support.path}',
      );
      final root = await support.createTemp('im-storage-native-');
      addTearDown(() => root.delete(recursive: true));
      final chain = <String, String>{};
      for (Directory? part = root.absolute; part != null;) {
        chain[part.path] = (await FileSystemEntity.type(
          part.path,
          followLinks: false,
        )).toString();
        if (await FileSystemEntity.type(part.path, followLinks: false) ==
            FileSystemEntityType.link) {
          chain['${part.path}:target'] = await Link(part.path).target();
        }
        final parent = part.parent;
        part = parent.path == part.path ? null : parent;
      }
      // Synthetic owned app paths only; no credentials or message bodies.
      // ignore: avoid_print
      print('NATIVE_STORAGE_PATH ${jsonEncode(chain)}');
      final store = await EncryptedStore.open(
        'a' * 64,
        List<int>.filled(32, 5),
        directory: root,
        generation: 1,
      );
      addTearDown(store.close);
      expect(
        await store.committedCursor('00000000-0000-4000-8000-000000000001'),
        isNull,
      );
    });
  });
}
