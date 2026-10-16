import copy
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

class MobileBackendTests(unittest.TestCase):
    # A missing opt-in check must never create a database or emulator.
    def test_flag_requires_owned_simulator(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = subprocess.run([sys.executable, str(Path(__file__).resolve().parents[1] / 'build-client-mobile.py'), '--flutter-sdk', tmp, '--pub-cache', tmp, '--output', tmp + '/out', '--verify-real-backend'],capture_output=True,text=True)
            self.assertEqual(p.returncode,2)
            self.assertTrue((Path(tmp)/'out/preflight.json').is_file(),p.stderr)
            self.assertEqual(json.loads((Path(tmp)/'out/preflight.json').read_text())['blocked_reason'],'real_backend_requires_simulator')

    # A credentials asset left in the normal build would disclose refresh tokens.
    def test_fixture_bundle_restored_after_failure(self):
        from integration.mobile_backend import bundled_fixture
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp); pub=root/'pubspec.yaml'
            original=b'name: enterprise_im\ndependencies:\n  flutter:\n    sdk: flutter\nflutter:\n  uses-material-design: true\n'
            pub.write_bytes(original)
            with self.assertRaisesRegex(RuntimeError,'probe'):
                with bundled_fixture(root,{'origin':'https://127.0.0.1:1234','refresh':'private-marker'}):
                    self.assertEqual(json.loads((root/'assets/owned_backend.json').read_text())['refresh'],'private-marker')
                    self.assertEqual(pub.read_bytes(),b'name: enterprise_im\ndependencies:\n  flutter:\n    sdk: flutter\nflutter:\n  assets:\n    - assets/owned_backend.json\n  uses-material-design: true\n')
                    raise RuntimeError('probe')
            self.assertEqual(pub.read_bytes(),original)
            self.assertFalse((root/'assets/owned_backend.json').exists())
            self.assertFalse((root/'assets').exists())

    # Prevent a Flutter pass alone, stale DB checkpoint, or skipped backend becoming acceptance.
    def test_requires_independent_facts_native_cleanup_and_backend_exit(self):
        from integration.mobile_backend import verify_report
        report={'status':'passed','platform':'android','runtime_pid':321,'messages':3,'delivered':3,'read':3,'revoked_rejections':['sync','delivered','read','messages'],'terminal_stopped':True,'received_cleared':True,'os_vault_real':True,'own_records_absent':True,'storage_removed':True}
        facts={'runtime_pid':321,'automatic_request_delta':0,'platform':'android','phases':['sent','delivered','read','revoked','terminal','done'],'messages':3,'device_delivered':3,'device_read':3,'user_read':3,'read_cursor':3,'bob_revoked':1,'device_count':2,'old_device_rejections':['sync','delivered','read','messages']}
        verify_report(report,facts,0,'All tests passed.','android')
        for field,bad in [('status','failed'),('platform','macos'),('runtime_pid',0),('messages',2),('delivered',0),('read',0),('revoked_rejections',[]),('terminal_stopped',False),('received_cleared',False),('os_vault_real',False),('own_records_absent',False),('storage_removed',False)]:
            with self.subTest(field=field),self.assertRaises(ValueError):verify_report(dict(report,**{field:bad}),facts,0,'All tests passed.','android')
        for field,bad in [('runtime_pid',322),('automatic_request_delta',1),('platform','ios'),('phases',['done']),('messages',4),('device_delivered',0),('device_read',0),('user_read',6),('read_cursor',0),('bob_revoked',0),('device_count',3),('old_device_rejections',[])]:
            with self.subTest(field=field),self.assertRaises(ValueError):verify_report(report,dict(facts,**{field:bad}),0,'All tests passed.','android')
        for code,log in [(1,'All tests passed.'),(0,'Failure Details: All tests passed.'),(0,'tearDownAll')]:
            with self.assertRaises(ValueError):verify_report(report,facts,code,log,'android')

if __name__=='__main__':unittest.main()
