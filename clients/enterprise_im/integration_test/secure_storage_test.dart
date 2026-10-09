import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:enterprise_im/core/storage/secure_vault.dart';

// Two separately launched native processes are required. The test runner supplies
// a new random 64-hex fixture namespace; no real account namespace is touched.
// write: --dart-define=IM_VAULT_PROBE_PHASE=write
// verify/delete after restart: --dart-define=IM_VAULT_PROBE_PHASE=verify
void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  const namespace = String.fromEnvironment('IM_VAULT_PROBE_ID');
  const phase = String.fromEnvironment('IM_VAULT_PROBE_PHASE');
  testWidgets(
    'real secure vault survives process restart and deletes its own record',
    (tester) async {
      expect(namespace, matches(RegExp(r'^[a-f0-9]{64}$')));
      expect({'write', 'verify'}.contains(phase), isTrue);
      final vault = OsSecureVault();
      final marker = 'enterprise-im-private-fixture/$namespace';
      if (phase == 'write') {
        expect(
          await vault.read(namespace, 'session'),
          isNull,
          reason: 'fixture must be fresh; refuse replacing an existing value',
        );
        await vault.write(namespace, 'session', marker);
        expect(await vault.read(namespace, 'session'), marker);
      } else {
        // Cleanup belongs only to this test's previously written fixture.
        final stored = await vault.read(namespace, 'session');
        expect(stored, marker);
        await vault.delete(namespace, 'session');
        expect(await vault.read(namespace, 'session'), isNull);
      }
    },
  );
}
