import copy
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

class ResumeTests(unittest.TestCase):
    def test_flag_requires_owned_simulator_before_preflight(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = subprocess.run([sys.executable, str(Path(__file__).resolve().parents[1] / 'build-client-mobile.py'), '--flutter-sdk', tmp, '--pub-cache', tmp, '--output', tmp + '/out', '--verify-mobile-resume'], capture_output=True, text=True)
            self.assertEqual(p.returncode, 2)
            self.assertTrue((Path(tmp) / 'out/preflight.json').is_file(), p.stderr)
            self.assertEqual(json.loads((Path(tmp) / 'out/preflight.json').read_text())['blocked_reason'], 'mobile_resume_requires_simulator')

    def test_controller_cancel_during_read_or_identity_never_dispatches_os_action(self):
        from integration.mobile_resume import control
        import threading
        for during in ['read', 'identity']:
            with self.subTest(during=during), tempfile.TemporaryDirectory() as tmp:
                stopped = threading.Event()
                result = {'phases':[]}
                actions = []
                def read():
                    if during == 'read':stopped.set()
                    return {'phase':'pause-read','pid':123,'probe':'a'*32}
                def identity(pid):
                    if during == 'identity':stopped.set()
                control(result,Path(tmp),'a'*32,stopped,read,identity,lambda phase:actions.append(phase),lambda phase:actions.append(phase))
                self.assertEqual(actions, [])
                self.assertEqual(result['phases'], [])

    def test_gate_needs_native_assertions_all_phases_and_driver(self):
        from integration.mobile_resume import PHASES, verify_report
        report = {'status':'passed', 'pid':123, 'probe':'a'*32, 'lifecycle':['paused','resumed']*3, 'background_ms':[1100,1100,1100], 'old_read_ack_ignored':True, 'old_page_redacted':True, 'pending_resume_reads':0, 'fresh_after':['0','0','0'], 'fresh_device_headers':True, 'catchup_cursor':'2', 'read_seqs':['2'], 'device_denial':'invalid_device', 'received_state_cleared':True, 'terminal_no_restart':True,'socket_closed_on_pause':True}
        verify_report(report, 123, 'a'*32, PHASES, 'All tests passed.')
        for field, bad in [('status','failed'),('pid',124),('probe','b'*32),('background_ms',[1100,0,1100]),('lifecycle',['resumed']*3),('fresh_after',['0','1','0']),('read_seqs',['1','2']),('device_denial','unavailable'),('catchup_cursor','1'),('old_read_ack_ignored',False),('old_page_redacted',False),('pending_resume_reads',1),('fresh_device_headers',False),('received_state_cleared',False),('terminal_no_restart',False),('socket_closed_on_pause',False)]:
            with self.subTest(field=field), self.assertRaises(ValueError):
                verify_report(dict(report, **{field:bad}), 123, 'a'*32, PHASES, 'All tests passed.')
        for phases, log in [(PHASES[:-1], 'All tests passed.'),(PHASES, 'Failure Details:\nAll tests passed.'),(PHASES, 'tearDownAll')]:
            with self.assertRaises(ValueError):verify_report(report,123,'a'*32,phases,log)

if __name__ == '__main__':unittest.main()
