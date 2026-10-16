import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/platform/file_export.dart';

// The host changes only phase/gate files and operates real system UI/reboots.
void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  const probe = String.fromEnvironment('IM_ANDROID_EXPORT_PROBE_ID');
  testWidgets('system reboot and original directory reauthorization', (
    tester,
  ) async {
    expect(Platform.isAndroid, isTrue);
    expect(probe, matches(RegExp(r'^[a-f0-9]{32}$')));
    await tester.pumpWidget(
      const MaterialApp(home: Scaffold(body: Text('系统重启导出恢复验证'))),
    );
    await tester.runAsync(() async {
      final prefix = '${Directory.systemTemp.path}/im-reboot-$probe';
      final phase = (await File('$prefix-stage').readAsString()).trim();
      var sourceCalls = 0;
      Future<void> save(
        String status, [
        Map<String, Object?> details = const {},
      ]) => File('$prefix-$phase-result.json').writeAsString(
        jsonEncode({
          'phase': phase,
          'pid': pid,
          'status': status,
          'source_calls': sourceCalls,
          ...details,
        }),
        flush: true,
      );
      try {
        if (phase == 'prepare-preack') {
          await FileExport().save('im-$probe-handoff-delete.bin', () {
            sourceCalls++;
            return Stream.value([99]);
          }, Cancellation());
          throw StateError('pre-ACK operation unexpectedly returned');
        } else if (phase == 'prepare-ram') {
          const channel = MethodChannel('enterprise_im/file_export');
          final ticket = await channel.invokeMethod<String>('beginExport', {
            'name': 'im-$probe-delete.bin',
          });
          expect(ticket, isNotEmpty);
          for (var i = 0; i < 2; i++) {
            await channel.invokeMethod<void>('writeExport', {
              'ticket': ticket,
              'bytes': Uint8List.fromList(List.filled(20000, 77)),
            });
          }
          await save('passed', {'native_ack_bytes': 40000});
          await Future<void>.delayed(const Duration(minutes: 3));
        } else if (phase == 'prepare-partial') {
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
          await save('passed');
        } else if (phase == 'retry-cancel' || phase == 'retry-picker-cancel') {
          expect(
            await FileExport().save('im-$probe-recovery.bin', () {
              sourceCalls++;
              return Stream.value([99]);
            }, Cancellation()),
            isFalse,
          );
          expect(sourceCalls, 0);
          await save('passed');
        } else if (phase == 'retry-wrong' || phase == 'retry-blocked') {
          await expectLater(
            FileExport().save('im-$probe-recovery.bin', () {
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
          await save('passed');
        } else if (phase == 'retry-destroy') {
          await FileExport().save('im-$probe-recovery.bin', () {
            sourceCalls++;
            return Stream.value([99]);
          }, Cancellation());
          throw StateError('destroyed choice unexpectedly returned');
        } else if (phase == 'recover') {
          expect(
            await FileExport().save('im-$probe-recovery.bin', () {
              sourceCalls++;
              return Stream.value([7, 11, 13]);
            }, Cancellation()),
            isTrue,
          );
          expect(sourceCalls, 1);
          await save('passed');
        } else {
          expect(phase, 'idle');
          await save('passed');
        }
      } catch (error) {
        await save('failed', {'error': error.toString()});
        rethrow;
      }
    });
  }, timeout: const Timeout(Duration(minutes: 4)));
}
