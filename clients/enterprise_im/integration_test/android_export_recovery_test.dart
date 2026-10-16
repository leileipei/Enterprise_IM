import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/platform/file_export.dart';

// One unchanged compiled APK is relaunched after actual Activity/process death.
// The host controls only a test phase file, never the business URI or native journal.
void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  const probe = String.fromEnvironment('IM_ANDROID_EXPORT_PROBE_ID');
  testWidgets('cleanup recovery before a new export', (tester) async {
    expect(Platform.isAndroid, isTrue);
    expect(probe, matches(RegExp(r'^[a-f0-9]{32}$')));
    await tester.pumpWidget(
      const MaterialApp(home: Scaffold(body: Text('导出清理恢复验证'))),
    );
    await tester.runAsync(() async {
      final prefix = '${Directory.systemTemp.path}/im-recovery-$probe';
      final stageFile = File('$prefix-stage');
      final phase = await stageFile.exists()
          ? (await stageFile.readAsString()).trim()
          : 'prepare';
      expect({
        'prepare',
        'prepare-activity',
        'prepare-same',
        'prepare-active',
        'blocked',
        'recover',
        'idle',
      }, contains(phase));
      final result = File('$prefix-$phase-result.json');
      // ignore: avoid_print
      print('ANDROID_RECOVERY phase=$phase pid=$pid control=$prefix');
      var sourceCalls = 0;
      try {
        if (phase == 'prepare' ||
            phase == 'prepare-activity' ||
            phase == 'prepare-same') {
          await expectLater(
            FileExport().save('im-$probe-write-delete.bin', () {
              sourceCalls++;
              return Stream.value(List<int>.generate(70000, (i) => i % 251));
            }, Cancellation()),
            throwsA(
              isA<PlatformException>().having(
                (e) => e.code,
                'code',
                'export_cleanup_failed',
              ),
            ),
          );
          expect(sourceCalls, 1);
          var blockedSource = 0;
          await expectLater(
            FileExport().save('blocked.bin', () {
              blockedSource++;
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
          expect(blockedSource, 0);
        } else if (phase == 'prepare-active') {
          const channel = MethodChannel('enterprise_im/file_export');
          final ticket = await channel.invokeMethod<String>('beginExport', {
            'name': 'im-$probe-write-delete.bin',
          });
          expect(ticket, isNotEmpty);
          await expectLater(
            channel.invokeMethod<String>('beginExport', {'name': 'busy.bin'}),
            throwsA(
              isA<PlatformException>().having(
                (e) => e.code,
                'code',
                'export_busy',
              ),
            ),
          );
          for (var i = 0; i < 2; i++) {
            await channel.invokeMethod<void>('writeExport', {
              'ticket': ticket,
              'bytes': Uint8List.fromList(List.filled(20000, 77)),
            });
          }
          sourceCalls = 1;
        } else if (phase == 'blocked') {
          await expectLater(
            FileExport().save('blocked.bin', () {
              sourceCalls++;
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
          expect(sourceCalls, 0);
        } else if (phase == 'recover') {
          expect(
            await FileExport().save('im-$probe-recovery.bin', () {
              sourceCalls++;
              return Stream.value([7, 11, 13]);
            }, Cancellation()),
            isTrue,
          );
          expect(sourceCalls, 1);
        }
        await result.writeAsString(
          jsonEncode({
            'phase': phase,
            'pid': pid,
            'status': 'passed',
            'source_calls': sourceCalls,
          }),
          flush: true,
        );
        // ignore: avoid_print
        print('ANDROID_RECOVERY_RESULT phase=$phase status=passed pid=$pid');
        if (phase == 'prepare-same') {
          while ((await stageFile.readAsString()).trim() != 'recover-same') {
            await Future<void>.delayed(const Duration(milliseconds: 50));
          }
          var recoveredSource = 0;
          expect(
            await FileExport().save('im-$probe-recovery.bin', () {
              recoveredSource++;
              return Stream.value([7, 11, 13]);
            }, Cancellation()),
            isTrue,
          );
          expect(recoveredSource, 1);
          await File('$prefix-recover-same-result.json').writeAsString(
            jsonEncode({
              'phase': 'recover-same',
              'pid': pid,
              'status': 'passed',
              'source_calls': recoveredSource,
            }),
            flush: true,
          );
        } else if (phase == 'prepare-activity') {
          // Public Flutter API asks the real Activity to finish. The host waits
          // for Android's wm_on_destroy_called before launching a replacement.
          while ((await stageFile.readAsString()).trim() != 'close-activity') {
            await Future<void>.delayed(const Duration(milliseconds: 50));
          }
          await SystemNavigator.pop();
        } else if (phase == 'prepare-active') {
          await Future<void>.delayed(const Duration(minutes: 2));
        }
      } catch (error) {
        await result.writeAsString(
          jsonEncode({
            'phase': phase,
            'pid': pid,
            'status': 'failed',
            'source_calls': sourceCalls,
            'error': error.toString(),
          }),
          flush: true,
        );
        rethrow;
      }
    });
  }, timeout: const Timeout(Duration(minutes: 3)));
}
