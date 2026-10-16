import 'package:integration_test/integration_test_driver.dart';

// Standard Flutter driver entry for native tests when DDS cannot start.
Future<void> main() => integrationDriver(timeout: const Duration(minutes: 5));
