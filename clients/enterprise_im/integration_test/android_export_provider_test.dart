import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/platform/file_export.dart';

// Standalone controlled DocumentsProvider chosen in real DocumentsUI.
// No channel/URI/Intent/result injection. Provider faults are deliberate fixtures.
void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  const probe = String.fromEnvironment('IM_ANDROID_EXPORT_PROBE_ID');
  const mode = String.fromEnvironment('IM_ANDROID_PROVIDER_CASE');
  const timeout = Timeout(Duration(minutes: 4));
  testWidgets('provider $mode failure never reports successful export', (
    tester,
  ) async {
    expect(Platform.isAndroid, isTrue);
    expect(probe, matches(RegExp(r'^[a-f0-9]{32}$')));
    expect({'write', 'delete', 'write-delete'}, contains(mode));
    await tester.pumpWidget(
      const MaterialApp(home: Scaffold(body: Text('提供程序异常验证'))),
    );
    await tester.runAsync(() async {
      final exporter = FileExport();
      final control = await Directory.systemTemp.createTemp(
        'im-provider-$probe-',
      );
      Future<void> host(String phase) async {
        final marker = File('${control.path}/$phase');
        // ignore: avoid_print
        print('ANDROID_PROVIDER_CONTROL phase=$phase marker=${marker.path}');
        final deadline = DateTime.now().add(const Duration(seconds: 60));
        while (!await marker.exists() && DateTime.now().isBefore(deadline)) {
          await Future<void>.delayed(const Duration(milliseconds: 50));
        }
        expect(
          await marker.exists(),
          isTrue,
          reason: 'independent provider inspection required',
        );
      }

      try {
        // ignore: avoid_print
        print(
          'ANDROID_PROVIDER phase=failure mode=$mode probe=$probe runtime_pid=$pid',
        );
        await expectLater(
          exporter.save('im-$probe-$mode.bin', () {
            if (mode == 'delete')
              return Stream.error(const ApiFailure('source_denied'));
            return Stream.value(List<int>.generate(70000, (i) => i % 251));
          }, Cancellation()),
          throwsA(
            isA<PlatformException>().having(
              (e) => e.code,
              'code',
              mode == 'write' ? 'export_unavailable' : 'export_cleanup_failed',
            ),
          ),
        );
        await host('inspect');
        if (mode != 'write') {
          // Stale callers must not disturb the retained cleanup ticket.
          const channel = MethodChannel('enterprise_im/file_export');
          await expectLater(
            channel.invokeMethod<void>('writeExport', {
              'ticket': 'unrelated-stale-ticket',
              'bytes': Uint8List.fromList([99]),
            }),
            throwsA(
              isA<PlatformException>().having(
                (e) => e.code,
                'code',
                'export_unavailable',
              ),
            ),
          );
          await channel.invokeMethod<void>('abortExport', {
            'ticket': 'unrelated-stale-ticket',
          });
          var sourceOpened = false;
          await expectLater(
            exporter.save('blocked.bin', () {
              sourceOpened = true;
              return Stream.value([99]);
            }, Cancellation()),
            throwsA(
              isA<PlatformException>().having(
                (e) => e.code,
                'code',
                'export_cleanup_failed',
              ),
            ),
          );
          expect(sourceOpened, isFalse);
          await host('release');
        }
        // ignore: avoid_print
        print(
          'ANDROID_PROVIDER phase=recovery mode=$mode probe=$probe runtime_pid=$pid',
        );
        expect(
          await exporter.save(
            'im-$probe-recovery.bin',
            () => Stream.value([7, 11, 13]),
            Cancellation(),
          ),
          isTrue,
        );
        await host('verify');
      } finally {
        if (await control.exists()) await control.delete(recursive: true);
      }
    });
  }, timeout: timeout);
}
