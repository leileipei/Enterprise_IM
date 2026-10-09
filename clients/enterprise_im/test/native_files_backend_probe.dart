import 'package:flutter_test/flutter_test.dart';
import '../integration_test/files_and_devices_test.dart' as scenario;

// Explicit host-only entry. Missing fixture is a failure; default shared unit
// suite excludes it because external real services must be owned by the driver.
void main() {
  const fixture = String.fromEnvironment('IM_FILES_FIXTURE');
  if (fixture.isEmpty) throw StateError('explicit owned fixture required');
  TestWidgetsFlutterBinding.ensureInitialized();
  test(
    'real backend attachment and own-device controllers',
    () => scenario.runFilesAndDevicesScenario(host: true),
    timeout: const Timeout(Duration(minutes: 3)),
  );
}
