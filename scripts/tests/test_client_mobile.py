import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from integration.registry import Registry


class MobileRegistryTests(unittest.TestCase):
    def test_process_only_cleanup_does_not_require_docker(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            root.chmod(0o700)
            registry = Registry(root / 'registry.jsonl', 'a' * 32, 'b' * 40, recover_containers=False)
            marker = root / 'docker-was-required'
            def unavailable_docker(args):
                marker.touch()
                return []
            with patch.object(registry, 'docker', side_effect=unavailable_docker):
                result = registry.cleanup(__import__('time').monotonic() + 5)
            self.assertTrue(result.removed, result.failures)
            self.assertFalse(marker.exists(), 'process-only cleanup must work without a Docker service')


from integration.mobile import simulator_choice, owned_simulator, copy_client, xcrun_environment, simulator_id


class MobileOwnershipTests(unittest.TestCase):
    def test_unavailable_or_unlisted_runtime_cannot_start_device(self):
        inventory = {'runtimes': [{'identifier': 'wanted', 'isAvailable': False, 'supportedDeviceTypes': [{'identifier': 'phone'}]}]}
        for runtime in ('wanted', 'missing'):
            with self.subTest(runtime=runtime), self.assertRaisesRegex(ValueError, 'simulator_runtime_unavailable'):
                simulator_choice(inventory, runtime, 'phone')

    def test_runtime_must_support_exact_requested_device_type(self):
        inventory = {'runtimes': [{'identifier': 'wanted', 'isAvailable': True, 'supportedDeviceTypes': [{'identifier': 'phone'}]}]}
        with self.assertRaisesRegex(ValueError, 'simulator_device_type_unavailable'):
            simulator_choice(inventory, 'wanted', 'different')
        self.assertEqual(simulator_choice(inventory, 'wanted', 'phone'), {'runtime': 'wanted', 'device_type': 'phone'})

    def test_existing_same_udid_with_different_name_is_not_owned(self):
        inventory = {'devices': {'runtime': [{'udid': 'fixture-id', 'name': 'foreign'}]}}
        with self.assertRaisesRegex(ValueError, 'simulator_identity_mismatch'):
            owned_simulator(inventory, 'runtime', 'owned', 'fixture-id')

    def test_recovery_rejects_ambiguous_reserved_names(self):
        inventory = {'devices': {'runtime': [{'udid': 'one', 'name': 'owned'}, {'udid': 'two', 'name': 'owned'}]}}
        with self.assertRaisesRegex(ValueError, 'simulator_identity_ambiguous'):
            owned_simulator(inventory, 'runtime', 'owned')

    def test_copy_rejects_foreign_link_before_creating_destination(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); repo = root / 'repo'; source = repo / 'clients/enterprise_im'
            source.mkdir(parents=True); foreign = root / 'foreign'; foreign.write_text('foreign'); foreign.chmod(0o400)
            (source / 'pubspec.yaml').symlink_to(foreign); dest = root / 'copy'
            with self.assertRaisesRegex(ValueError, 'client_source_not_regular'):
                copy_client(repo, dest, {'clients/enterprise_im/pubspec.yaml': '0' * 64})
            self.assertFalse(dest.exists()); self.assertEqual(foreign.read_text(), 'foreign')
            self.assertEqual(foreign.stat().st_mode & 0o777, 0o400)

    def test_copy_rejects_changed_content_before_creating_destination(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); repo = root / 'repo'; source = repo / 'clients/enterprise_im'
            source.mkdir(parents=True); (source / 'pubspec.yaml').write_text('changed'); dest = root / 'copy'
            with self.assertRaisesRegex(ValueError, 'client_source_changed'):
                copy_client(repo, dest, {'clients/enterprise_im/pubspec.yaml': '0' * 64})
            self.assertFalse(dest.exists())


class MobileCliTests(unittest.TestCase):
    def run_cli(self, root, extra):
        output = root / 'output'
        command = [sys.executable, str(Path(__file__).resolve().parents[1] / 'build-client-mobile.py'), '--flutter-sdk', str(root / 'sdk'), '--pub-cache', str(root / 'pub'), '--output', str(output), *extra]
        process = subprocess.run(command, capture_output=True, text=True, env=dict(os.environ, PYTHONDONTWRITEBYTECODE='1'))
        return process, output

    def test_android_cannot_silently_fall_back_to_ios_build(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); process, output = self.run_cli(root, ['--target', 'android'])
            self.assertEqual(process.returncode, 2)
            report = json.loads((output / 'preflight.json').read_text())
            self.assertEqual(report['blocked_reason'], 'prepared_android_paths_required')
            self.assertFalse(report['fixtures_created'])
            self.assertFalse((output / 'registry.jsonl').exists())

    def test_relative_sdk_cannot_start_a_build(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); process, output = self.run_cli(root, ['--flutter-sdk', 'relative'])
            self.assertEqual(process.returncode, 2)
            self.assertEqual(json.loads((output / 'preflight.json').read_text())['blocked_reason'], 'mobile_paths_must_be_absolute')
            self.assertFalse((output / 'registry.jsonl').exists())

    def test_existing_output_is_preserved(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); output = root / 'output'; output.mkdir(); marker = output / 'foreign'; marker.write_text('unchanged')
            process, unused = self.run_cli(root, [])
            self.assertNotEqual(process.returncode, 0)
            self.assertEqual(marker.read_text(), 'unchanged')
            self.assertEqual(list(output.iterdir()), [marker])

    def test_source_copy_contains_exact_verified_bytes(self):
        import hashlib
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); repo = root / 'repo'; source = repo / 'clients/enterprise_im/lib/main.dart'; source.parent.mkdir(parents=True); source.write_bytes(b'owned code')
            copy_client(repo, root / 'copy', {'clients/enterprise_im/lib/main.dart': hashlib.sha256(b'owned code').hexdigest()})
            self.assertEqual((root / 'copy/lib/main.dart').read_bytes(), b'owned code')
            self.assertEqual(source.read_bytes(), b'owned code')

    def test_default_registry_still_recovers_owned_containers(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); root.chmod(0o700); registry = Registry(root / 'registry.jsonl', 'a' * 32, 'b' * 40)
            marker = root / 'docker-checked'
            def docker(args):
                marker.touch(); return []
            with patch.object(registry, 'docker', side_effect=docker):
                result = registry.cleanup(__import__('time').monotonic() + 5)
            self.assertTrue(result.removed); self.assertTrue(marker.exists())


class SimulatorIdTests(unittest.TestCase):
    def test_real_simctl_hyphenated_id_is_accepted(self):
        self.assertEqual(simulator_id('fc37a5b0-f5bd-4696-b91c-33f59f71524c'), 'FC37A5B0-F5BD-4696-B91C-33F59F71524C')

    def test_noncanonical_id_cannot_select_a_device(self):
        for value in ('a' * 36, 'FC37A5B0F5BD4696B91C33F59F71524C', '../foreign'):
            with self.subTest(value=value), self.assertRaises(ValueError):
                simulator_id(value)


if __name__ == '__main__':
    unittest.main()
