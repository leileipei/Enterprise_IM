import 'dart:io';
import 'package:flutter/material.dart';
import 'platform/login_callback.dart';
import 'ui/login_page.dart';

Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  AppLinksSource? mobile;
  bool ready = true;
  if (Platform.isAndroid || Platform.isIOS) {
    mobile = AppLinksSource();
    try {
      await mobile.initialize();
    } catch (_) {
      ready = false;
    }
  }
  final platform = Platform.isWindows
      ? 'windows'
      : Platform.isMacOS
      ? 'macos'
      : Platform.isAndroid
      ? 'android'
      : Platform.isIOS
      ? 'ios'
      : 'unsupported';
  runApp(
    EnterpriseIMApp(
      platform: platform,
      mobile: mobile,
      platformReady: ready && platform != 'unsupported',
    ),
  );
}

class EnterpriseIMApp extends StatelessWidget {
  final String platform;
  final AppLinksSource? mobile;
  final bool platformReady;
  const EnterpriseIMApp({
    super.key,
    required this.platform,
    this.mobile,
    this.platformReady = true,
  });
  @override
  Widget build(BuildContext context) => MaterialApp(
    title: '企业 IM',
    debugShowCheckedModeBanner: false,
    theme: ThemeData(
      colorScheme: ColorScheme.fromSeed(seedColor: const Color(0xff245b92)),
      useMaterial3: true,
    ),
    home: LoginPage(
      platform: platform,
      platformReady: platformReady,
      callbackFactory: (name, state) {
        if (name == 'windows' || name == 'macos')
          return LoopbackLoginCallback(state);
        final source = mobile;
        if (source == null) throw StateError('mobile callback unavailable');
        return MobileLoginCallback(state, source);
      },
    ),
  );
}
