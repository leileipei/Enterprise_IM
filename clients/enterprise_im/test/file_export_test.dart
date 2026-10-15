import 'dart:async';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/platform/file_export.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  const channel = MethodChannel('test/file_export');
  for (final platform in ['android', 'ios']) {
    test(
      '$platform export chooses destination before authorized bytes and finishes exact stream',
      () async {
        final calls = <String>[];
        final bytes = <int>[];
        TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
            .setMockMethodCallHandler(channel, (call) async {
              calls.add(call.method);
              if (call.method == 'beginExport') return 'ticket';
              if (call.method == 'writeExport')
                bytes.addAll((call.arguments as Map)['bytes'] as Uint8List);
              return null;
            });
        addTearDown(
          () => TestDefaultBinaryMessengerBinding
              .instance
              .defaultBinaryMessenger
              .setMockMethodCallHandler(channel, null),
        );
        final export = FileExport(channel: channel, platform: platform);
        expect(
          await export.save('说明.txt', () {
            expect(calls, ['beginExport']);
            return Stream.value([97, 98, 99]);
          }, Cancellation()),
          isTrue,
        );
        expect(calls, ['beginExport', 'writeExport', 'finishExport']);
        expect(bytes, [97, 98, 99]);
      },
    );
    test(
      '$platform export respects bridge chunk bound for a large file block',
      () async {
        final original = List<int>.generate(70000, (i) => i % 256),
            copied = <int>[];
        TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
            .setMockMethodCallHandler(channel, (call) async {
              if (call.method == 'beginExport') return 'ticket';
              if (call.method == 'writeExport') {
                final bytes = (call.arguments as Map)['bytes'] as Uint8List;
                if (bytes.length > 32768)
                  throw PlatformException(code: 'export_unavailable');
                copied.addAll(bytes);
              }
              return null;
            });
        addTearDown(
          () => TestDefaultBinaryMessengerBinding
              .instance
              .defaultBinaryMessenger
              .setMockMethodCallHandler(channel, null),
        );
        expect(
          await FileExport(
            channel: channel,
            platform: platform,
          ).save('说明.txt', () => Stream.value(original), Cancellation()),
          isTrue,
        );
        expect(copied, original);
      },
    );
    test(
      '$platform failed authorization aborts selected document without publishing bytes',
      () async {
        final calls = <String>[];
        TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
            .setMockMethodCallHandler(channel, (call) async {
              calls.add(call.method);
              if (call.method == 'beginExport') return 'ticket';
              return null;
            });
        addTearDown(
          () => TestDefaultBinaryMessengerBinding
              .instance
              .defaultBinaryMessenger
              .setMockMethodCallHandler(channel, null),
        );
        final export = FileExport(channel: channel, platform: platform);
        await expectLater(
          export.save(
            '说明.txt',
            () => Stream.error(const ApiFailure('invalid_identity')),
            Cancellation(),
          ),
          throwsA(isA<ApiFailure>()),
        );
        expect(calls, ['beginExport', 'abortExport']);
      },
    );
    test('$platform retries failed cleanup before a new destination', () async {
      var busy = false, canDelete = false, choices = 0;
      final copied = <int>[];
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(channel, (call) async {
            if (call.method == 'beginExport') {
              if (busy) throw PlatformException(code: 'export_unavailable');
              busy = true;
              choices++;
              return 'ticket-$choices';
            }
            if (call.method == 'abortExport') {
              if (!canDelete)
                throw PlatformException(code: 'export_cleanup_failed');
              expect((call.arguments as Map)['ticket'], 'ticket-1');
              busy = false;
            }
            if (call.method == 'writeExport')
              copied.addAll((call.arguments as Map)['bytes'] as Uint8List);
            if (call.method == 'finishExport') busy = false;
            return null;
          });
      addTearDown(
        () => TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
            .setMockMethodCallHandler(channel, null),
      );
      final export = FileExport(channel: channel, platform: platform);
      await expectLater(
        export.save(
          'a.txt',
          () => Stream.error(const ApiFailure('invalid_identity')),
          Cancellation(),
        ),
        throwsA(
          isA<PlatformException>().having(
            (e) => e.code,
            'code',
            'export_cleanup_failed',
          ),
        ),
      );
      canDelete = true;
      expect(
        await export.save('b.txt', () => Stream.value([4, 5]), Cancellation()),
        isTrue,
      );
      expect(copied, [4, 5]);
      expect(choices, 2);
    });
    test(
      '$platform cancellation during cleanup retry does not reopen chooser',
      () async {
        var choices = 0, failCleanup = true;
        final cleaning = Completer<void>(), cleaned = Completer<void>();
        TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
            .setMockMethodCallHandler(channel, (call) async {
              if (call.method == 'beginExport') {
                choices++;
                return 'ticket-$choices';
              }
              if (call.method == 'abortExport' &&
                  (call.arguments as Map)['ticket'] == 'ticket-1') {
                if (failCleanup)
                  throw PlatformException(code: 'export_cleanup_failed');
                cleaning.complete();
                await cleaned.future;
              }
              return null;
            });
        addTearDown(
          () => TestDefaultBinaryMessengerBinding
              .instance
              .defaultBinaryMessenger
              .setMockMethodCallHandler(channel, null),
        );
        final export = FileExport(channel: channel, platform: platform);
        await expectLater(
          export.save(
            'a.txt',
            () => Stream.error(const ApiFailure('invalid_identity')),
            Cancellation(),
          ),
          throwsA(isA<PlatformException>()),
        );
        failCleanup = false;
        final cancel = Cancellation();
        var sourceOpened = false;
        final saving = export.save('b.txt', () {
          sourceOpened = true;
          return Stream.value([1]);
        }, cancel);
        final rejected = expectLater(saving, throwsA(isA<ApiFailure>()));
        await cleaning.future;
        cancel.cancel();
        cleaned.complete();
        await rejected;
        expect(choices, 1);
        expect(sourceOpened, isFalse);
      },
    );
    test('$platform chooser cancellation never requests contents', () async {
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(channel, (call) async => null);
      addTearDown(
        () => TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
            .setMockMethodCallHandler(channel, null),
      );
      var requested = false;
      expect(
        await FileExport(channel: channel, platform: platform).save(
          'a.txt',
          () {
            requested = true;
            return Stream.value([1]);
          },
          Cancellation(),
        ),
        isFalse,
      );
      expect(requested, isFalse);
    });
    test(
      '$platform late selection after cancellation releases ticket without reading',
      () async {
        final selected = Completer<String?>(), aborted = Completer<void>();
        TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
            .setMockMethodCallHandler(channel, (call) async {
              if (call.method == 'beginExport') return selected.future;
              if (call.method == 'abortExport') {
                expect((call.arguments as Map)['ticket'], 'late-ticket');
                if (!aborted.isCompleted) aborted.complete();
              }
              return null;
            });
        addTearDown(
          () => TestDefaultBinaryMessengerBinding
              .instance
              .defaultBinaryMessenger
              .setMockMethodCallHandler(channel, null),
        );
        final cancel = Cancellation();
        var requested = false;
        final saving = FileExport(channel: channel, platform: platform).save(
          'a.txt',
          () {
            requested = true;
            return Stream.value([1]);
          },
          cancel,
        );
        final rejected = expectLater(saving, throwsA(isA<ApiFailure>()));
        cancel.cancel();
        await rejected;
        selected.complete('late-ticket');
        await aborted.future;
        expect(requested, isFalse);
      },
    );
  }
}
