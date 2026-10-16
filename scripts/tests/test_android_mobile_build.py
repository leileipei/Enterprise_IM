import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))


class AndroidBuildCliTests(unittest.TestCase):
    def test_android_without_prepared_tools_is_explicitly_blocked(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); out = root / 'evidence'
            p = subprocess.run([sys.executable, str(Path(__file__).resolve().parents[1] / 'build-client-mobile.py'), '--target', 'android', '--flutter-sdk', str(root / 'flutter'), '--pub-cache', str(root / 'pub'), '--output', str(out)], capture_output=True, text=True)
            self.assertEqual(p.returncode, 2)
            result = json.loads((out / 'preflight.json').read_text())
            self.assertEqual(result['blocked_reason'], 'prepared_android_paths_required')
            self.assertFalse(result['fixtures_created'])
            self.assertFalse((out / 'registry.jsonl').exists())

    def test_android_does_not_ignore_ios_simulator_verification_request(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); out = root / 'evidence'
            p = subprocess.run([sys.executable, str(Path(__file__).resolve().parents[1] / 'build-client-mobile.py'), '--target', 'android', '--flutter-sdk', str(root / 'flutter'), '--pub-cache', str(root / 'pub'), '--output', str(out), '--verify-simulator'], capture_output=True, text=True)
            self.assertEqual(p.returncode, 2)
            self.assertEqual(json.loads((out / 'preflight.json').read_text())['blocked_reason'], 'explicit_android_simulator_runtime_and_type_required')

    def test_reused_output_cannot_overwrite_other_evidence(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); out = root / 'evidence'; out.mkdir(); marker = out / 'foreign'; marker.write_text('keep')
            p = subprocess.run([sys.executable, str(Path(__file__).resolve().parents[1] / 'build-client-mobile.py'), '--target', 'android', '--flutter-sdk', str(root / 'flutter'), '--pub-cache', str(root / 'pub'), '--output', str(out)], capture_output=True, text=True)
            self.assertNotEqual(p.returncode, 0)
            self.assertEqual(marker.read_text(), 'keep')
            self.assertEqual(list(out.iterdir()), [marker])


class AndroidArtifactGateTests(unittest.TestCase):
    badging = "package: name='com.enterpriseim.client' compileSdkVersion='36'\nminSdkVersion:'24'\ntargetSdkVersion:'36'\nnative-code: 'armeabi-v7a' 'arm64-v8a' 'x86_64'\n"
    signature = 'Verifies\nVerified using v2 scheme (APK Signature Scheme v2): true\nNumber of signers: 1\nSigner #1 certificate DN: C=US, O=Android, CN=Android Debug\nSigner #1 certificate SHA-256 digest: ' + 'a' * 64 + '\n'

    def test_fixed_jdk_patch_and_host_architecture_are_exact(self):
        from integration.android_build import jdk_release_metadata
        value = 'IMPLEMENTOR="Eclipse Adoptium"\nJAVA_VERSION="21.0.12.1"\nJAVA_RUNTIME_VERSION="21.0.12.1+1-LTS"\nOS_ARCH="aarch64"\nOS_NAME="Darwin"\n'
        self.assertEqual(jdk_release_metadata(value)['version'], '21.0.12.1+1-LTS')
        for invalid in (value.replace('21.0.12.1', '21.0.12'), value.replace('aarch64', 'x86_64'), value.replace('Eclipse Adoptium', 'Other Vendor'), value.replace('Darwin', 'Linux')):
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                jdk_release_metadata(invalid)

    def test_expected_release_is_explicitly_test_signed(self):
        from integration.android_build import apk_metadata
        result = apk_metadata(self.badging, self.signature)
        self.assertEqual(result['minimum_api'], 24)
        self.assertEqual(result['target_api'], 36)
        self.assertEqual(result['abis'], ['arm64-v8a', 'armeabi-v7a', 'x86_64'])
        self.assertEqual(result['signature_kind'], 'temporary_debug_certificate')
        self.assertFalse(result['distribution_acceptance'])

    def test_other_package_baseline_debuggable_or_abi_is_rejected(self):
        from integration.android_build import apk_metadata
        for badging in (self.badging.replace('com.enterpriseim.client', 'foreign.client'), self.badging.replace("minSdkVersion:'24'", "minSdkVersion:'26'"), self.badging.replace("targetSdkVersion:'36'", "targetSdkVersion:'35'"), self.badging.replace("compileSdkVersion='36'", "compileSdkVersion='35'"), self.badging + 'application-debuggable\n', self.badging.replace(" 'x86_64'", '')):
            with self.subTest(badging=badging), self.assertRaises(ValueError):
                apk_metadata(badging, self.signature)

    def test_unsigned_or_unknown_certificate_is_not_misreported(self):
        from integration.android_build import apk_metadata
        for signature in (self.signature.replace('): true', '): false'), self.signature.replace('Number of signers: 1', 'Number of signers: 2'), self.signature.replace('CN=Android Debug', 'CN=Unselected Certificate'), self.signature.replace('a' * 64, 'invalid')):
            with self.subTest(signature=signature), self.assertRaises(ValueError):
                apk_metadata(self.badging, signature)

    def test_export_fixture_or_backend_credentials_cannot_enter_archive(self):
        import zipfile
        from integration.android_build import archive_manifest
        with tempfile.TemporaryDirectory() as tmp:
            apk = Path(tmp) / 'app.apk'
            for name, data in [('assets/flutter_assets/owned_backend.json', b'credentials'), ('classes.dex', b'Lcom/enterpriseim/exportfixture/FaultDocumentsProvider;'), ('classes.dex', b'Lcom/enterpriseim/client/tests/HandoffLifecycleInstrumentation;')]:
                with zipfile.ZipFile(apk, 'w') as z:
                    z.writestr('AndroidManifest.xml', b'manifest')
                    if name != 'classes.dex':
                        z.writestr('classes.dex', b'normal production classes')
                    z.writestr(name, data)
                with self.subTest(name=name), self.assertRaisesRegex(ValueError, 'unexpected_'):
                    archive_manifest(apk)

    def test_private_environment_does_not_inherit_host_java_or_adb_options(self):
        from integration.android_build import android_environment
        from types import SimpleNamespace
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            args = SimpleNamespace(verify_simulator=False, java_home=root / 'jdk', android_sdk=root / 'sdk', gradle_cache=root / 'gradle', pub_cache=root / 'pub')
            with patch.dict(os.environ, {'JAVA_TOOL_OPTIONS': '-Dforeign=true', '_JAVA_OPTIONS': '-Dforeign=true', 'ADB_SERVER_SOCKET': 'tcp:foreign:5037', 'GRADLE_OPTS': '-Dforeign=true'}):
                env = android_environment(root / 'private', args)
            self.assertEqual(env['HOME'], str(root / 'private/home'))
            self.assertNotIn('_JAVA_OPTIONS', env)
            self.assertNotIn('ADB_SERVER_SOCKET', env)
            self.assertNotIn('foreign', json.dumps(env))
            self.assertIn('-Duser.home=' + str(root / 'private/java-user'), env['JAVA_TOOL_OPTIONS'])

class AndroidPreflightToolBindingTests(unittest.TestCase):
    def test_sdk_commit_drift_or_tracked_edit_is_rejected(self):
        from integration.android_build import sdk_source_is_fixed
        from unittest.mock import patch
        for outputs, expected in [(['a' * 40 + '\n', b''], True), (['b' * 40 + '\n', b''], False), (['a' * 40 + '\n', b' M packages/flutter_tools/source.dart'], False)]:
            with self.subTest(outputs=outputs), patch('integration.android_build.subprocess.check_output', side_effect=outputs):
                self.assertEqual(sdk_source_is_fixed(Path('/fixed/sdk'), 'a' * 40), expected)

    def test_dart_is_bound_for_post_build_comparison(self):
        import hashlib
        from types import SimpleNamespace
        from unittest.mock import patch
        from integration.android_build import preflight
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); repo = root / 'repo'; sdk = root / 'sdk'; flutter = root / 'flutter'; java = root / 'java'; pub = root / 'pub'; gradle = root / 'gradle'
            dart = flutter / 'bin/cache/dart-sdk/bin/dart'
            files = [dart, flutter / 'bin/cache/flutter_tools.snapshot', java / 'bin/java', sdk / 'platforms/android-36/android.jar', sdk / 'platforms/android-36/source.properties', sdk / 'licenses/android-sdk-license', sdk / 'build-tools/36.0.0/aapt2', sdk / 'build-tools/36.0.0/apksigner']
            files += [flutter / 'bin/cache/artifacts/engine' / abi / 'flutter.jar' for abi in ('android-arm-release', 'android-arm64-release', 'android-x64-release')]
            for path in files:
                path.parent.mkdir(parents=True, exist_ok=True); path.write_bytes(b'prepared local tool')
            for path, content in [(java / 'release', 'IMPLEMENTOR="Eclipse Adoptium"\nJAVA_VERSION="21.0.12.1"\nJAVA_RUNTIME_VERSION="21.0.12.1+1-LTS"\nOS_ARCH="aarch64"\nOS_NAME="Darwin"\n'), (sdk / 'build-tools/36.0.0/source.properties', 'Pkg.Revision=36.0.0'), (sdk / 'ndk/28.2.13676358/source.properties', 'Pkg.Revision=28.2.13676358'), (repo / 'clients/enterprise_im/android/gradle/wrapper/gradle-wrapper.properties', 'distributionSha256Sum=' + 'a' * 64)]:
                path.parent.mkdir(parents=True, exist_ok=True); path.write_text(content)
            lock = repo / 'docs/verification/p5-02-toolchain-lock.json'; lock.parent.mkdir(parents=True); lock.write_text(json.dumps({'framework_commit': 'b' * 40, 'dart_executable_sha256': hashlib.sha256(dart.read_bytes()).hexdigest(), 'android': {'gradle_distribution_sha256': 'a' * 64}}))
            pub.mkdir(); gradle.mkdir()
            args = SimpleNamespace(verify_simulator=False, simulator_runtime=None, simulator_device_type=None, android_sdk=sdk, java_home=java, gradle_cache=gradle, flutter_sdk=flutter, pub_cache=pub, output=root / 'output')
            with patch('integration.android_build.platform.system', return_value='Darwin'), patch('integration.android_build.platform.machine', return_value='arm64'), patch('integration.android_build.subprocess.check_output', side_effect=['b' * 40 + '\n', b'']), patch('integration.android_build.subprocess.run', return_value=subprocess.CompletedProcess([], 0, stdout='openjdk 21.0.12.1', stderr='')):
                result = preflight(repo, args)
            self.assertEqual(result['tool_sha256'].get(str(dart)), hashlib.sha256(dart.read_bytes()).hexdigest())


class AndroidFailedBuildCleanupTests(unittest.TestCase):
    def test_actual_nonzero_command_is_preserved_and_private_state_removed(self):
        import hashlib
        from types import SimpleNamespace
        from unittest.mock import patch
        from integration.android_build import run
        from integration.commands import run_command
        repo = Path(__file__).resolve().parents[2]
        source = 'clients/enterprise_im/lib/main.dart'
        inputs = {source: hashlib.sha256((repo / source).read_bytes()).hexdigest()}
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); out = root / 'evidence'; out.mkdir(); marker = root / 'tool'; marker.write_text('unchanged')
            args = SimpleNamespace(verify_simulator=False, java_home=root / 'jdk', android_sdk=root / 'sdk', gradle_cache=root / 'gradle', pub_cache=root / 'pub', flutter_sdk=root / 'flutter')
            def fail(name, kind, commit, argv, cwd, env, log, deadline, registry):
                return run_command(name, kind, commit, ['/bin/sh', '-c', 'exit 7'], cwd, env, log, deadline, registry)
            with patch('integration.commands.run_command', side_effect=fail), patch('integration.android_build.sdk_source_is_fixed', return_value=True):
                code = run(repo, args, out, {'sdk_commit': 'b' * 40, 'tool_sha256': {str(marker): hashlib.sha256(marker.read_bytes()).hexdigest()}}, lambda: inputs)
            result = json.loads((out / 'result.json').read_text())
            if Path(result['root']).exists():
                self.addCleanup(__import__('shutil').rmtree, result['root'])
            self.assertEqual(code, 1)
            self.assertEqual(result['commands'][0]['actual_exit'], 7)
            self.assertEqual(result['status'], 'failed')
            self.assertTrue(result['private_root_removed'])
            self.assertFalse(Path(result['root']).exists())
            self.assertTrue(result['cleanup']['removed'])
            self.assertFalse((out / 'android-release-test.apk').exists())
            self.assertEqual(marker.read_text(), 'unchanged')

    def test_source_audit_error_still_writes_result_and_preserves_recovery_root(self):
        import hashlib
        import shutil
        from types import SimpleNamespace
        from unittest.mock import patch
        from integration.android_build import run
        from integration.commands import run_command
        repo = Path(__file__).resolve().parents[2]; source = 'clients/enterprise_im/lib/main.dart'
        inputs = {source: hashlib.sha256((repo / source).read_bytes()).hexdigest()}
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); out = root / 'evidence'; out.mkdir(); marker = root / 'tool'; marker.write_text('unchanged')
            args = SimpleNamespace(verify_simulator=False, java_home=root / 'jdk', android_sdk=root / 'sdk', gradle_cache=root / 'gradle', pub_cache=root / 'pub', flutter_sdk=root / 'flutter')
            def fail(name, kind, commit, argv, cwd, env, log, deadline, registry):
                return run_command(name, kind, commit, ['/bin/sh', '-c', 'exit 7'], cwd, env, log, deadline, registry)
            try:
                with patch('integration.commands.run_command', side_effect=fail), patch('integration.android_build.sdk_source_is_fixed', return_value=True), patch('builtins.print'):
                    inventory = __import__('unittest.mock', fromlist=['Mock']).Mock(side_effect=[inputs, OSError('source unavailable')])
                    code = run(repo, args, out, {'sdk_commit': 'b' * 40, 'tool_sha256': {str(marker): hashlib.sha256(marker.read_bytes()).hexdigest()}}, inventory)
                result = json.loads((out / 'result.json').read_text())
                self.assertEqual(code, 1)
                self.assertFalse(result['source_unchanged'])
                self.assertIn('source_audit:OSError', result['cleanup']['failures'])
                self.assertEqual(result['recovery_root'], result['root'])
                self.assertTrue(Path(result['root']).is_dir())
            finally:
                if (out / 'result.json').exists():
                    result = json.loads((out / 'result.json').read_text()); owned = Path(result['root'])
                    assert owned.parent == Path('/private/tmp') and owned.name.startswith('im-mobile-android-')
                    if owned.exists():shutil.rmtree(owned)

if __name__ == '__main__':
    unittest.main()
