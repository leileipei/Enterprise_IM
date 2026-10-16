import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))


class MobileBoundaryTests(unittest.TestCase):
    def test_mobile_boundary_request_requires_owned_android_simulator(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = subprocess.run([sys.executable, str(Path(__file__).resolve().parents[1] / 'build-client-mobile.py'), '--target', 'android', '--flutter-sdk', tmp, '--pub-cache', tmp, '--output', tmp + '/out', '--verify-mobile-boundaries'], capture_output=True, text=True)
            self.assertEqual(p.returncode, 2)
            self.assertTrue((Path(tmp) / 'out/preflight.json').is_file(), p.stderr)
            self.assertEqual(json.loads((Path(tmp) / 'out/preflight.json').read_text())['blocked_reason'], 'mobile_boundaries_require_android_simulator')

    def test_vm_service_only_comes_from_owned_pid_and_loopback(self):
        from integration.android_mobile_boundary import vm_service
        value = 'I/flutter ( 123): The Dart VM service is listening on http://127.0.0.1:43123/abc_123=/\n'
        self.assertEqual(vm_service(value, 123), (43123, '/abc_123=/'))
        for invalid in [value.replace('123)', '124)'), value.replace('127.0.0.1', 'remote.example'), value.replace('43123', '0'), value + value.replace('43123', '43124')]:
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):vm_service(invalid, 123)

    def test_final_gate_rejects_missing_phase_wrong_pid_or_unobserved_pause(self):
        from integration.android_mobile_boundary import verify_report
        phases = ['wrong-state', 'wrong-path', 'duplicate-state', 'warm-valid', 'stale-after-cancel', 'new-valid', 'home-ready', 'background', 'done']
        report = {'status':'passed', 'pid':123, 'cold_initial':True, 'cancelled':True, 'lifecycle':['paused','resumed'], 'background_ms':1100}
        verify_report(report, 123, phases, 'All tests passed.')
        for change in [{'pid':124}, {'cold_initial':False}, {'cancelled':False}, {'lifecycle':['resumed']}, {'background_ms':0}, {'status':'failed'}]:
            with self.subTest(change=change), self.assertRaises(ValueError):verify_report(dict(report, **change), 123, phases, 'All tests passed.')
        with self.assertRaises(ValueError):verify_report(report, 123, phases[:-2], 'All tests passed.')
        with self.assertRaises(ValueError):verify_report(report, 123, phases, 'Failure Details: failed\nAll tests passed.')

if __name__ == '__main__':unittest.main()
