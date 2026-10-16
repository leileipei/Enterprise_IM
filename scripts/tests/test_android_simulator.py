import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))


class AndroidSimulatorContractTests(unittest.TestCase):
    def fixture(self, root, api=36):
        package = 'system-images;android-36;default;arm64-v8a' if api == 36 else 'system-images;android-24;google_apis;arm64-v8a'
        image = root.joinpath(*package.split(';')); image.mkdir(parents=True)
        props = 'AndroidVersion.ApiLevel=' + str(api) + '\nSystemImage.Abi=arm64-v8a\nSystemImage.TagId=' + ('default' if api == 36 else 'google_apis') + '\nPkg.Revision=' + ('2' if api == 36 else '29') + '\n'
        (image / 'source.properties').write_text(props)
        for name in ['system.img', 'ramdisk.img', 'kernel-ranchu']:(image / name).write_bytes(b'prepared image')
        for path in ['emulator/emulator', 'emulator/source.properties', 'platform-tools/adb', 'platform-tools/source.properties', 'cmdline-tools/23.0/bin/avdmanager']:
            p = root / path; p.parent.mkdir(parents=True, exist_ok=True); p.write_text('prepared')
        (root / 'emulator/source.properties').write_text('Pkg.Revision=37.2.12\n')
        (root / 'platform-tools/source.properties').write_text('Pkg.Revision=37.0.1\n')
        return package, image

    def test_only_explicit_prepared_api24_or_api36_arm64_image_can_run(self):
        from integration.android_simulator import simulator_preflight
        for api in [24, 36]:
            with self.subTest(api=api), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp); package, unused = self.fixture(root, api)
                result = simulator_preflight(root, package, 'pixel_7')
                self.assertEqual(result['api'], api)
                self.assertEqual(result['abi'], 'arm64-v8a')
                self.assertEqual(result['package'], package)
                self.assertTrue(result['tool_sha256'])

    def test_unknown_real_device_or_implicit_runtime_cannot_be_selected(self):
        from integration.android_simulator import simulator_preflight
        for package, device in [(None, None), ('system-images;android-35;default;arm64-v8a', 'pixel_7'), ('../../foreign', 'pixel_7'), ('system-images;android-36;default;arm64-v8a', '../foreign'), ('system-images;android-36;default;x86_64', 'pixel_7')]:
            with self.subTest(package=package, device=device), self.assertRaises(ValueError):
                simulator_preflight(Path('/not-used'), package, device)

    def test_mismatched_or_missing_installed_image_stays_not_run(self):
        from integration.android_simulator import simulator_preflight
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); package, image = self.fixture(root)
            (image / 'source.properties').write_text('AndroidVersion.ApiLevel=24\nSystemImage.Abi=x86_64\nPkg.Revision=99\n')
            with self.assertRaises(ValueError):simulator_preflight(root, package, 'pixel_7')
            (image / 'system.img').unlink()
            with self.assertRaises(ValueError):simulator_preflight(root, package, 'pixel_7')

    def test_device_identity_never_accepts_another_avd_or_abi(self):
        from integration.android_simulator import validate_device
        props = {'ro.boot.qemu.avd_name':'IM-Mobile-Android-owned','ro.kernel.qemu.avd_name':'','ro.build.version.sdk':'36','ro.product.cpu.abi':'arm64-v8a','ro.kernel.qemu':'1'}
        validate_device(props, 'IM-Mobile-Android-owned', {'api':36,'abi':'arm64-v8a'})
        for key, value in [('ro.boot.qemu.avd_name','foreign'),('ro.build.version.sdk','24'),('ro.product.cpu.abi','x86_64'),('ro.kernel.qemu','0')]:
            with self.subTest(key=key), self.assertRaises(ValueError):validate_device(dict(props, **{key:value}), 'IM-Mobile-Android-owned', {'api':36,'abi':'arm64-v8a'})

    def test_success_text_without_exact_native_marker_cannot_pass_vault(self):
        from integration.android_simulator import vault_phase
        for text in ['All tests passed.', 'NATIVE_VAULT_PROBE phase=verify runtime_pid=12\nAll tests passed.', 'NATIVE_VAULT_PROBE phase=write runtime_pid=12\nFailure Details: failed', 'NATIVE_VAULT_PROBE phase=write runtime_pid=12\nNATIVE_VAULT_PROBE phase=write runtime_pid=13\nAll tests passed.']:
            with self.subTest(text=text), self.assertRaises(ValueError):vault_phase(text, 'write')
        self.assertEqual(vault_phase('NATIVE_VAULT_PROBE phase=write runtime_pid=12\nAll tests passed.', 'write')['runtime_pid'], 12)

    def test_two_phases_need_distinct_pids_and_retained_installation(self):
        from integration.android_simulator import verify_vault_pair
        a={'runtime_pid':12,'uid':'10079','first_install':'time1'};b={'runtime_pid':13,'uid':'10079','first_install':'time1'}
        verify_vault_pair(a,b)
        for change in [{'runtime_pid':12}, {'uid':'10080'}, {'first_install':'time2'}]:
            with self.subTest(change=change), self.assertRaises(ValueError):verify_vault_pair(a,dict(b,**change))

    def test_debug_plugins_are_prepared_offline_before_three_no_pub_drives(self):
        from integration.android_simulator import AndroidSimulator
        from unittest.mock import Mock
        sim=AndroidSimulator.__new__(AndroidSimulator)
        with tempfile.TemporaryDirectory() as tmp:
            sim.out=Path(tmp); sim.serial='emulator-5554'; sim.state={}; sim.save=lambda:None
            sim.stop_app=Mock(); sim.installation=Mock(return_value={'uid':'10079','first_install':'same'})
            commands=[]
            def step(name,argv,*unused):
                commands.append((name,argv))
                if 'drive' in argv:
                    self.assertIn('--no-pub',argv)
                    phase = name.removeprefix('vault-')
                    marker = '' if name=='activity' else 'NATIVE_VAULT_PROBE phase='+phase+' runtime_pid='+('11' if phase=='write' else '12')+'\n'
                    (sim.out/(name+'.log')).write_text(marker+'All tests passed.\n')
            sim.verify(['dart','flutter.snapshot'],step,Path('/private/client'))
            self.assertEqual(commands[0][0],'debug-pub')
            self.assertEqual(commands[0][1][-4:],['pub','get','--offline','--enforce-lockfile'])
            self.assertEqual([n for n,a in commands if 'drive' in a],['activity','vault-write','vault-verify'])

    def test_installation_parser_covers_actual_api36_appid_and_api24_userid(self):
        from integration.android_simulator import package_installation
        for key in ['appId','userId']:
            text='Packages:\n  Package [com.enterpriseim.client] (abc):\n    '+key+'=10150\n      firstInstallTime=2026-10-16 16:59:34\n'
            self.assertEqual(package_installation(text),{'uid':'10150','first_install':'2026-10-16 16:59:34'})
        for text in ['Packages:\n  Package [foreign.client] (abc):\n    appId=10150\n    firstInstallTime=time1\n', 'Packages:\n  Package [com.enterpriseim.client] (abc):\n    appId=10150\n    userId=10151\n    firstInstallTime=time1\n']:
            with self.assertRaises(ValueError):package_installation(text)

    def test_boot_transport_timeout_is_an_observation_inside_total_deadline(self):
        from integration.android_simulator import boot_probe
        from unittest.mock import patch
        import subprocess
        with patch('integration.android_simulator.subprocess.run', side_effect=subprocess.TimeoutExpired(['owned-adb'],8)):
            result = boot_probe(['owned-adb','connect','127.0.0.1:5555'],{},Path('/private/tmp'))
        self.assertEqual(result.returncode,124)
        self.assertEqual(result.stdout,b'')
        self.assertIn(b'timeout',result.stderr)

    def test_cleanup_never_signals_or_uses_adb_when_service_identity_changes(self):
        from integration.android_simulator import AndroidSimulator
        from types import SimpleNamespace
        from unittest.mock import Mock, patch
        import io
        sim = AndroidSimulator.__new__(AndroidSimulator)
        old = {'pid':12345,'uid':501,'start_time':'original','pgid':12345,'workdir':'/private/tmp/owned/run','executable_path':'/sdk/emulator/emulator'}
        proc = SimpleNamespace(pid=12345, poll=lambda:None)
        sim.private = Path('/private/tmp/owned'); sim.args = SimpleNamespace(android_sdk=Path('/sdk'))
        sim.services = [{'name':'emulator','process':proc,'fingerprint':old,'stream':io.BytesIO()}]
        sim.serial = 'emulator-5554'; sim.state = {'service_exits':[]}; sim.save = lambda:None; sim.adb = Mock()
        with patch('integration.android_simulator.process_fingerprint',return_value=dict(old,start_time='different')), patch('integration.android_simulator.os.kill') as kill:
            failures = sim.close()
        self.assertTrue(failures)
        kill.assert_not_called(); sim.adb.assert_not_called()

if __name__ == '__main__':unittest.main()
