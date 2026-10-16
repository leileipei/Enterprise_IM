import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/platform/native_storage.dart';
import 'package:enterprise_im/core/storage/encrypted_store.dart';

void main() {
  Future<(Directory, Directory, Directory)> setup() async {
    final root = await Directory.systemTemp.createTemp('im-system-alias-');
    addTearDown(() => root.delete(recursive: true));
    final target = await Directory('${root.path}/data').create();
    final alias = Directory('${root.path}/user-zero');
    await Link(alias.path).create(target.path);
    return (root, target, alias);
  }

  test('known system alias supports native encrypted storage', () async {
    final (_, target, alias) = await setup();
    final original = await Directory(
      '${alias.path}/app/files',
    ).create(recursive: true);
    await expectLater(
      EncryptedStore.open(
        'a' * 64,
        List<int>.filled(32, 5),
        directory: original,
        generation: 1,
      ),
      throwsA(isA<StoreFailure>()),
    );
    final normalized = await normalizeSystemStorageAlias(
      original,
      alias: alias,
      target: target,
    );
    expect(normalized.path, '${target.path}/app/files');
    final store = await EncryptedStore.open(
      'a' * 64,
      List<int>.filled(32, 5),
      directory: normalized,
      generation: 1,
    );
    addTearDown(store.close);
  });
  test(
    'unknown or relative alias targets fail closed and siblings are untouched',
    () async {
      final (root, target, alias) = await setup();
      final other = await Directory('${root.path}/other').create();
      await expectLater(
        normalizeSystemStorageAlias(
          Directory('${alias.path}/app'),
          alias: alias,
          target: other,
        ),
        throwsA(isA<StoreFailure>()),
      );
      await Link(alias.path).delete();
      await Link(alias.path).create('data');
      await expectLater(
        normalizeSystemStorageAlias(
          Directory('${alias.path}/app'),
          alias: alias,
          target: target,
        ),
        throwsA(isA<StoreFailure>()),
      );
      final sibling = Directory('${alias.path}-sibling/app');
      expect(
        (await normalizeSystemStorageAlias(
          sibling,
          alias: alias,
          target: target,
        )).path,
        sibling.path,
      );
    },
  );
  test(
    'normalizing system prefix preserves application and SQLite link guards',
    () async {
      final (root, target, alias) = await setup();
      final outside = await Directory('${root.path}/outside').create();
      final app = await Directory('${target.path}/app').create();
      await Link('${app.path}/accounts').create(outside.path);
      final normalized = await normalizeSystemStorageAlias(
        Directory('${alias.path}/app/accounts'),
        alias: alias,
        target: target,
      );
      Future<void> denied() async => expectLater(
        EncryptedStore.open(
          'a' * 64,
          List<int>.filled(32, 5),
          directory: normalized,
          generation: 1,
        ),
        throwsA(isA<StoreFailure>()),
      );
      await denied();
      await Link(normalized.path).delete();
      await normalized.create();
      final sentinel = File('${outside.path}/sentinel');
      await sentinel.writeAsString('owned sentinel');
      for (final suffix in ['', '-wal', '-shm']) {
        final link = Link('${normalized.path}/${'a' * 64}.sqlite$suffix');
        await link.create(sentinel.path);
        await denied();
        await link.delete();
        expect(await sentinel.readAsString(), 'owned sentinel');
      }
      final store = await EncryptedStore.open(
        'a' * 64,
        List<int>.filled(32, 5),
        directory: normalized,
        generation: 1,
      );
      addTearDown(store.close);
    },
  );
}
