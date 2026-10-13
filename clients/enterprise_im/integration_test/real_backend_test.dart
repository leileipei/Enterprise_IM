// Compiled desktop-only entry. Both scenarios require freshly owned backend
// fixtures; this does not create fixtures or exercise interactive OAuth login.
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'messaging_test.dart' as messaging;
import 'files_and_devices_test.dart' as files;

void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  const scenario = String.fromEnvironment('IM_BACKEND_SCENARIO');
  const bundled = bool.fromEnvironment('IM_NATIVE_BUNDLED_FIXTURE');
  if (!Platform.isWindows && !Platform.isMacOS) {
    throw StateError('native desktop backend entry requires macOS or Windows');
  }
  switch (scenario) {
    case 'messaging':
      const fixture = String.fromEnvironment('IM_MESSAGING_FIXTURE');
      if (fixture.isEmpty && !bundled)
        throw StateError('owned messaging config required');
      testWidgets(
        'testDesktopMinimizeStopsReadAndRefocusResyncs',
        (tester) => messaging.runMessagingScenario(tester, host: false),
        timeout: const Timeout(Duration(minutes: 6)),
      );
    case 'files':
      const fixture = String.fromEnvironment('IM_FILES_FIXTURE');
      if (fixture.isEmpty && !bundled)
        throw StateError('owned files config required');
      testWidgets('native backend attachments and own-device revocation', (
        tester,
      ) async {
        await tester.runAsync(
          () => files.runFilesAndDevicesScenario(host: false),
        );
      }, timeout: const Timeout(Duration(minutes: 4)));
    default:
      throw StateError(
        'explicit IM_BACKEND_SCENARIO messaging or files required',
      );
  }
}
