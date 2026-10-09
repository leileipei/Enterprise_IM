import 'dart:io';
import 'dart:isolate';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/storage/encrypted_store.dart';

Future<void> exitOnRequest(List<Object?> args) async {
  final commands = ReceivePort();
  (args[0]! as SendPort).send(commands.sendPort);
  await commands.first;
  commands.close();
  Isolate.exit();
}

Future<void> exitBeforeStartup(List<Object?> args) async => Isolate.exit();
Future<void> rejectClose(List<Object?> args) async {
  final commands = ReceivePort();
  (args[0]! as SendPort).send(commands.sendPort);
  final request = await commands.first as List<Object?>;
  (args[1]! as SendPort).send([request[0], false, null]);
  commands.close();
  Isolate.exit();
}

void main() {
  test(
    'failed close still allows logout erasure after actual worker exit',
    () async {
      final root = await Directory.systemTemp.createTemp(
        'im-worker-exit-test-',
      );
      addTearDown(() => root.delete(recursive: true));
      final original = await EncryptedStore.open(
        'a' * 64,
        List<int>.filled(32, 5),
        directory: root,
        generation: 1,
      );
      await original.close();
      final failed = await EncryptedStore.open(
        'a' * 64,
        List<int>.filled(32, 5),
        directory: root,
        generation: 2,
        workerEntryPoint: rejectClose,
      );
      await expectLater(failed.close(), throwsA(isA<StoreFailure>()));
      expect(root.listSync(), isNotEmpty);
      await failed.closeAndErase();
      expect(root.listSync(), isEmpty);
    },
  );

  test(
    'worker exit fails outstanding requests and logout erases the account',
    () async {
      final root = await Directory.systemTemp.createTemp(
        'im-worker-exit-test-',
      );
      addTearDown(() => root.delete(recursive: true));
      final original = await EncryptedStore.open(
        'a' * 64,
        List<int>.filled(32, 5),
        directory: root,
        generation: 1,
      );
      await original.close();
      final crashed = await EncryptedStore.open(
        'a' * 64,
        List<int>.filled(32, 5),
        directory: root,
        generation: 2,
        workerEntryPoint: exitOnRequest,
      );
      try {
        final outcome = await crashed
            .pendingSends()
            .then<Object?>((value) => value, onError: (Object error) => error)
            .timeout(
              const Duration(seconds: 2),
              onTimeout: () => const StoreHang(),
            );
        expect(outcome, isA<StoreFailure>());
      } finally {
        await crashed.closeAndErase().timeout(const Duration(seconds: 2));
      }
      expect(root.listSync(), isEmpty);
      await expectLater(crashed.pendingSends(), throwsA(isA<StoreFailure>()));
      final restarted = await EncryptedStore.open(
        'a' * 64,
        List<int>.filled(32, 7),
        directory: root,
        generation: 3,
      );
      await restarted.closeAndErase();
      expect(root.listSync(), isEmpty);
    },
  );
  test(
    'worker exit during startup releases the owner without waiting for startup timeout',
    () async {
      final root = await Directory.systemTemp.createTemp(
        'im-worker-exit-test-',
      );
      addTearDown(() => root.delete(recursive: true));
      final outcome =
          await EncryptedStore.open(
                'a' * 64,
                List<int>.filled(32, 5),
                directory: root,
                generation: 1,
                workerEntryPoint: exitBeforeStartup,
              )
              .then<Object?>((value) => value, onError: (Object error) => error)
              .timeout(
                const Duration(seconds: 2),
                onTimeout: () => const StoreHang(),
              );
      expect(outcome, isA<StoreFailure>());
      final restarted = await EncryptedStore.open(
        'a' * 64,
        List<int>.filled(32, 7),
        directory: root,
        generation: 2,
      );
      await restarted.closeAndErase();
    },
  );
}

final class StoreHang {
  const StoreHang();
}
