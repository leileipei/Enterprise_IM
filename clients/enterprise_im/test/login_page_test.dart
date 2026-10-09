import 'dart:io';
import 'package:enterprise_im/core/auth/session_store.dart';
import 'session_store_test.dart' show MemoryVault;
import 'login_session_test.dart' show pumpUntil;
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/ui/login_page.dart';
import 'package:enterprise_im/platform/login_callback.dart';

void main() {
  testWidgets(
    'Chinese login page rejects unsafe service before opening a callback',
    (tester) async {
      int callbacks = 0;
      await tester.pumpWidget(
        MaterialApp(
          home: LoginPage(
            platform: 'macos',
            storeFactory: () async =>
                SessionStore(MemoryVault(), directory: Directory.systemTemp),
            callbackFactory: (platform, state) {
              callbacks++;
              return LoopbackLoginCallback(state);
            },
          ),
        ),
      );
      expect(find.text('登录集团工作空间'), findsOneWidget);
      expect(find.text('服务地址'), findsOneWidget);
      await pumpUntil(
        tester,
        () =>
            tester.widget<FilledButton>(find.byType(FilledButton)).onPressed !=
            null,
      );
      await tester.enterText(find.byType(TextField), 'http://im.example.test');
      await tester.tap(find.text('使用企业账号登录'));
      await tester.pump();
      expect(
        find.text('请输入有效的 HTTPS 服务根地址，例如 https://im.example.com。'),
        findsOneWidget,
      );
      expect(callbacks, 0);
      expect(find.text('登录成功'), findsNothing);
      await tester.pumpWidget(const SizedBox());
    },
  );
  testWidgets(
    'missing native callback preparation does not offer a working login',
    (tester) async {
      await tester.pumpWidget(
        MaterialApp(
          home: LoginPage(
            platform: 'android',
            storeFactory: () async =>
                SessionStore(MemoryVault(), directory: Directory.systemTemp),
            platformReady: false,
            callbackFactory: (platform, state) => LoopbackLoginCallback(state),
          ),
        ),
      );
      expect(find.text('无法接收登录回调，请检查应用安装配置。'), findsOneWidget);
      expect(
        tester.widget<FilledButton>(find.byType(FilledButton)).onPressed,
        isNull,
      );
      await tester.pumpWidget(const SizedBox());
    },
  );
}
