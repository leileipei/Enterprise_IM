import 'package:flutter_test/flutter_test.dart';
import '../integration_test/messaging_test.dart' as scenario;

void main() {
  const path = String.fromEnvironment('IM_MESSAGING_FIXTURE');
  if (path.isEmpty)
    throw StateError('explicit owned real backend config required; no skip');
  TestWidgetsFlutterBinding.ensureInitialized();
  testWidgets(
    'real backend messaging and group UI',
    (tester) => scenario.runMessagingScenario(tester, host: true),
    timeout: const Timeout(Duration(minutes: 3)),
  );
}
