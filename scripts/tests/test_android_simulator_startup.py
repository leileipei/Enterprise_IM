from pathlib import Path
import os
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))

class StartupTests(unittest.TestCase):
    # Darwin can expose the script basename during exec, before /bin/sh or Java.
    # Returning a missing fingerprint must not silently register an unknown PID.
    def test_transient_exec_name_then_real_owned_process_is_registered(self):
        from integration.android_simulator import AndroidSimulator
        from integration.registry import Registry,process_fingerprint
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);owner='a'*32;reg=Registry(root/'registry.jsonl',owner,'b'*40,recover_containers=False)
            sim=AndroidSimulator.__new__(AndroidSimulator)
            sim.private=root/owner;sim.out=root;sim.registry=reg;sim.env={'PATH':'/usr/bin:/bin'};sim.state={};sim.services=[];sim.save=lambda:None
            launched=[]
            real_popen=subprocess.Popen
            def launch(*a,**kw):
                proc=real_popen(*a,**kw)
                if a[0][0]=='/bin/sleep':launched.append((proc,kw['stdout']))
                return proc
            count=0
            def fingerprint(pid):
                nonlocal count
                count+=1
                if count==1:raise ValueError('unknown_process_executable')
                return process_fingerprint(pid)
            try:
                with patch('integration.android_simulator.subprocess.Popen',side_effect=launch),patch('integration.android_simulator.process_fingerprint',side_effect=fingerprint):
                    entry=sim._launch('startup-test',['/bin/sleep','30'])
                self.assertIsNotNone(entry['fingerprint'])
                self.assertEqual(entry['fingerprint']['pid'],entry['process'].pid)
                self.assertTrue(any(row['event']=='registered' and row['identity']==str(entry['process'].pid) for row in reg.records()))
            finally:
                for proc,stream in launched:
                    if proc.poll() is None:proc.terminate()
                    proc.wait(timeout=5);stream.close()

if __name__=='__main__':unittest.main()
