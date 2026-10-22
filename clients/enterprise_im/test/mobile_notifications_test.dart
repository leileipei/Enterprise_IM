import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:flutter_local_notifications/flutter_local_notifications.dart';
import 'package:enterprise_im/platform/notifications.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  for (final platform in [TargetPlatform.android, TargetPlatform.iOS]) {
    for (final granted in [true, false]) {
      test(
        '$platform explicitly requests notification permission: $granted',
        () async {
          debugDefaultTargetPlatformOverride = platform;
          if (platform == TargetPlatform.android) {
            AndroidFlutterLocalNotificationsPlugin.registerWith();
          } else {
            IOSFlutterLocalNotificationsPlugin.registerWith();
          }
          addTearDown(() => debugDefaultTargetPlatformOverride = null);
          const channel = MethodChannel(
            'dexterous.com/flutter/local_notifications',
          );
          final calls = <MethodCall>[];
          TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
              .setMockMethodCallHandler(channel, (call) async {
                calls.add(call);
                if (call.method.startsWith('request')) return granted;
                return true;
              });
          addTearDown(
            () => TestDefaultBinaryMessengerBinding
                .instance
                .defaultBinaryMessenger
                .setMockMethodCallHandler(channel, null),
          );
          final port = NativeNotificationPort(
            platform == TargetPlatform.android ? 'android' : 'ios',
          );
          expect(
            calls,
            isEmpty,
            reason: 'constructing the port must never request permission',
          );
          expect(await port.enable((_) {}), granted);
          expect(
            calls,
            isNotEmpty,
            reason:
                'an explicit request must reach the native adapter even when permission is denied',
          );
          expect(calls.first.method, 'initialize');
          if (platform == TargetPlatform.iOS) {
            final init = calls.first.arguments as Map;
            expect(init['requestAlertPermission'], false);
            expect(init['requestBadgePermission'], false);
            expect(init['requestSoundPermission'], false);
            final permissions = calls.last.arguments as Map;
            expect(permissions['alert'], true);
            expect(permissions['badge'], false);
            expect(permissions['sound'], false);
          } else {
            expect(
              (calls.first.arguments as Map)['defaultIcon'],
              'im_notification',
            );
            expect(calls.last.method, 'requestNotificationsPermission');
          }
          if (granted) {
            await port.show(body: '收到新消息', payload: 'opaque-reference');
            final shown = calls.last.arguments as Map;
            expect(shown['title'], '企业 IM');
            expect(shown['body'], '收到新消息');
            expect(shown['payload'], 'opaque-reference');
            final config = shown['platformSpecifics'] as Map;
            if (platform == TargetPlatform.android) {
              expect(config['channelId'], 'enterprise_im_messages');
              expect(config['playSound'], false);
              expect(config['enableVibration'], false);
              expect(
                config['visibility'],
                NotificationVisibility.private.index,
              );
            } else {
              expect(config['presentBanner'], true);
              expect(config['presentList'], true);
              expect(config['presentSound'], false);
              expect(config['presentBadge'], false);
            }
          }
          await port.cancel();
          expect(
            calls.last.method,
            'cancel',
            reason: 'even a denied workspace owns its cleanup',
          );
        },
      );
    }
  }
}
