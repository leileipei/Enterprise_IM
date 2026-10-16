import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/platform/file_export.dart';

// The provider holds its real createDocument return after creating an empty file.
// Host lifecycle actions never inject a URI, Intent result, or native journal.
void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  const owner = String.fromEnvironment('IM_ANDROID_EXPORT_PROBE_ID');
  testWidgets('pre-ACK system document result recovery', (tester) async {
    expect(Platform.isAndroid, isTrue);
    expect(owner, matches(RegExp(r'^[a-f0-9]{32}$')));
    await tester.pumpWidget(
      const MaterialApp(home: Scaffold(body: Text('选择结果恢复验证'))),
    );
    await tester.runAsync(() async {
      final prefix = '${Directory.systemTemp.path}/im-handoff-$owner';
      final stage = File('$prefix-stage');
      final phase = (await stage.readAsString()).trim();
      expect({'choose', 'observe', 'observe-loading', 'idle'}, contains(phase));
      var sourceCalls = 0;
      Future<void> save(String name, Map<String, Object> fields) =>
          File('$prefix-$name.json').writeAsString(
            jsonEncode({
              'phase': phase,
              'pid': pid,
              'source_calls': sourceCalls,
              ...fields,
            }),
            flush: true,
          );
      Future<void> waitFor(String name) async {
        final deadline = DateTime.now().add(const Duration(seconds: 100));
        while (!await File('$prefix-$name').exists() &&
            DateTime.now().isBefore(deadline)) {
          await Future<void>.delayed(const Duration(milliseconds: 50));
        }
        expect(await File('$prefix-$name').exists(), isTrue);
      }

      // ignore: avoid_print
      print('ANDROID_HANDOFF phase=$phase pid=$pid control=$prefix');
      try {
        if (phase == 'choose') {
          await save('choose-started', {'status': 'ready'});
          try {
            await FileExport().save('im-$owner-handoff-delete.bin', () {
              sourceCalls++;
              return Stream.value([99]);
            }, Cancellation());
            await save('choose-returned', {'status': 'unexpected_return'});
          } on PlatformException catch (e) {
            expect(sourceCalls, 0);
            expect(e.code, 'export_cancelled');
            await save('choose-cancelled', {'status': 'cancelled'});
          }
        } else if ((phase == 'observe' || phase == 'observe-loading')) {
          await save('observe-ready', {'status': 'ready'});
          await waitFor('assert-return');
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
          await save('blocked-result', {'status': 'passed'});
          if (phase == 'observe-loading') {
            await waitFor('check-loading');
            await expectLater(
              FileExport().save('loading-blocked.bin', () {
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
            await save('loading-result', {'status': 'passed'});
          }
          await waitFor('recover');
          expect(
            await FileExport().save('im-$owner-recovery.bin', () {
              sourceCalls++;
              return Stream.value([7, 11, 13]);
            }, Cancellation()),
            isTrue,
          );
          expect(sourceCalls, 1);
          await save('recovered-result', {'status': 'passed'});
        } else {
          await save('idle-result', {'status': 'passed'});
        }
      } catch (error) {
        await save('failure', {'status': 'failed', 'error': error.toString()});
        rethrow;
      }
    });
  }, timeout: const Timeout(Duration(minutes: 4)));
}
