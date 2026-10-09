import 'dart:async';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/platform/file_export.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  const channel = MethodChannel('test/file_export');
  test(
    'Android export chooses destination before authorized bytes and finishes exact stream',
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
        () => TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
            .setMockMethodCallHandler(channel, null),
      );
      final export = FileExport(channel: channel, platform: 'android');
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
    'Android export respects bridge chunk bound for a large file block',
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
        () => TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
            .setMockMethodCallHandler(channel, null),
      );
      expect(
        await FileExport(
          channel: channel,
          platform: 'android',
        ).save('说明.txt', () => Stream.value(original), Cancellation()),
        isTrue,
      );
      expect(copied, original);
    },
  );
  test(
    'Android failed authorization aborts selected document without publishing bytes',
    () async {
      final calls = <String>[];
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(channel, (call) async {
            calls.add(call.method);
            if (call.method == 'beginExport') return 'ticket';
            return null;
          });
      addTearDown(
        () => TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
            .setMockMethodCallHandler(channel, null),
      );
      final export = FileExport(channel: channel, platform: 'android');
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
}
