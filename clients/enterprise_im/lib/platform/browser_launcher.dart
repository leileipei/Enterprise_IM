import 'package:url_launcher/url_launcher.dart';

abstract interface class BrowserLauncher {
  Future<bool> open(Uri uri);
}

final class ExternalBrowserLauncher implements BrowserLauncher {
  @override
  Future<bool> open(Uri uri) =>
      launchUrl(uri, mode: LaunchMode.externalApplication);
}
