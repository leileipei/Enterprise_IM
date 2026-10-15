import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/platform/file_export.dart';

// Real DocumentsUI, MainActivity and FileExport. The app-private test marker
// only coordinates independent host inspection; no export URI/result is injected.
void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  const probe = String.fromEnvironment('IM_ANDROID_EXPORT_PROBE_ID');
  const mode = String.fromEnvironment('IM_ANDROID_PARTIAL_CASE');
  const timeout = Timeout(Duration(minutes: 4));
  const channel = MethodChannel('enterprise_im/file_export');

  Future<void> screen(WidgetTester tester, String phase) async {
    expect(Platform.isAndroid, isTrue);
    expect(probe, matches(RegExp(r'^[a-f0-9]{32}$')));
    expect({'source-error', 'cancel', 'limit'}.contains(mode), isTrue);
    await tester.pumpWidget(
      MaterialApp(
        home: Scaffold(body: Center(child: Text('部分导出验证：$phase'))),
      ),
    );
    await tester.runAsync(
      () => Future<void>.delayed(const Duration(seconds: 1)),
    );
    // ignore: avoid_print
    print(
      'ANDROID_PARTIAL phase=$phase mode=$mode runtime_pid=$pid probe=$probe',
    );
  }

  testWidgets('partial native writes are cleaned after $mode', (tester) async {
    await screen(tester, 'partial');
    await tester.runAsync(() async {
      final control = await Directory.systemTemp.createTemp(
        'im-partial-$probe-',
      );
      final release = File('${control.path}/release');
      var prefixAcknowledged = false;
      var sourceCalls = 0;
      final cancel = Cancellation();
      Future<void> inspect(int bytes) async {
        prefixAcknowledged = true;
        // ignore: avoid_print
        print('ANDROID_PARTIAL_READY bytes=$bytes control=${release.path}');
        final deadline = DateTime.now().add(const Duration(seconds: 60));
        while (!await release.exists() && DateTime.now().isBefore(deadline)) {
          await Future<void>.delayed(const Duration(milliseconds: 50));
        }
        expect(
          await release.exists(),
          isTrue,
          reason: 'host must inspect prefix',
        );
      }

      Stream<List<int>> contents() async* {
        sourceCalls++;
        yield List<int>.generate(40000, (i) => i % 251);
        // await-for pauses the source while each native write is awaited.
        await inspect(40000);
        if (mode == 'cancel') {
          cancel.cancel();
          yield [1];
        } else {
          throw const ApiFailure('partial_source_failure');
        }
      }

      try {
        if (mode == 'limit') {
          final ticket = await channel.invokeMethod<String>('beginExport', {
            'name': 'im-$probe-$mode-partial.bin',
          });
          expect(ticket, isNotNull);
          final chunk = Uint8List(32768);
          for (var i = 0; i < 800; i++) {
            await channel.invokeMethod<void>('writeExport', {
              'ticket': ticket,
              'bytes': chunk,
            });
          }
          await inspect(26214400);
          await expectLater(
            channel.invokeMethod<void>('writeExport', {
              'ticket': ticket,
              'bytes': Uint8List.fromList([1]),
            }),
            throwsA(
              isA<PlatformException>().having(
                (e) => e.code,
                'code',
                'export_unavailable',
              ),
            ),
          );
          await channel.invokeMethod<void>('abortExport', {'ticket': ticket});
          await channel.invokeMethod<void>('abortExport', {'ticket': ticket});
        } else {
          await expectLater(
            FileExport().save('im-$probe-$mode-partial.bin', contents, cancel),
            throwsA(
              isA<ApiFailure>().having(
                (e) => e.code,
                'code',
                mode == 'cancel' ? 'cancelled' : 'partial_source_failure',
              ),
            ),
          );
          expect(sourceCalls, 1);
        }
        expect(prefixAcknowledged, isTrue);
      } finally {
        if (await control.exists()) await control.delete(recursive: true);
      }
    });
  }, timeout: timeout);

  testWidgets('fresh export succeeds after $mode cleanup', (tester) async {
    await screen(tester, 'recovery');
    final saved = await tester.runAsync(
      () => FileExport().save('im-$probe-$mode-recovery.bin', () async* {
        if (mode == 'limit') {
          final chunk = Uint8List(32768);
          for (var i = 0; i < 800; i++) {
            yield chunk;
          }
        } else {
          yield [4, 5, 6];
        }
      }, Cancellation()),
    );
    expect(saved, isTrue);
  }, timeout: timeout);
}
