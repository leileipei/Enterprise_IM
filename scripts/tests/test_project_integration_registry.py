"""Owned cleanup exercises real processes; Docker responses are protocol fixtures."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from support import COMMIT

OWNER='b'*32
CHILD='c'*32
CID='d'*64


class RegistryTests(unittest.TestCase):
    def setUp(self):
        try:
            from integration.registry import Registry, process_fingerprint
            from integration.commands import run_command
            from integration.model import ResourceRef
        except ImportError:self.fail('Task 3 registry/commands interfaces are not implemented')
        self.Ref=ResourceRef;self.fingerprint=process_fingerprint;self.run=run_command
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name).resolve();self.root.chmod(0o700)
        self.registry=Registry(self.root/'registry.jsonl',OWNER,COMMIT)

    def sleep(self,cwd):
        proc=subprocess.Popen(['/bin/sleep','30'],cwd=cwd,start_new_session=True)
        def stop():
            if proc.poll() is None:proc.terminate()
            proc.wait()
        self.addCleanup(stop)
        return proc

    def test_cancel_between_create_and_register(self):
        # Reserved child directory and actual process identity allow recovery of the gap.
        self.registry.reserve(CHILD)
        proc=self.sleep(self.root/CHILD)
        result=self.registry.cleanup(time.monotonic()+5)
        self.assertTrue(result.removed,result.failures)
        self.assertIsNotNone(proc.poll())

    def test_pid_reuse_and_foreign_owner(self):
        self.registry.reserve(CHILD)
        proc=self.sleep(self.root/CHILD)
        fingerprint=self.fingerprint(proc.pid)
        fingerprint['start_time']='wrong_start'
        self.registry.add(self.Ref('process',CHILD,str(proc.pid),fingerprint))
        result=self.registry.cleanup(time.monotonic()+1)
        self.assertFalse(result.removed)
        self.assertIsNone(proc.poll(),'identity mismatch must never kill the process')
        other=self.sleep(self.root)
        with self.assertRaises(ValueError):
            self.registry.add(self.Ref('process','e'*32,str(other.pid),self.fingerprint(other.pid)))
        self.assertIsNone(other.poll())

    def test_auto_remove_in_progress(self):
        self.registry.add(self.Ref('container',OWNER,CID,{'container_id':CID}))
        state={'present':True,'calls':[]}
        def docker(args):
            state['calls'].append(args)
            if args[0]=='ps':return [CID] if state['present'] else []
            if args[0]=='inspect':
                if not state['present']:return None
                return {'Id':CID,'Config':{'Labels':{'im.integration.owner':OWNER,'im.integration.source':COMMIT}},
                        'State':{'Status':'removing'},'HostConfig':{'AutoRemove':True}}
            self.fail('must not issue a second rm while auto-removal is in progress')
        with patch.object(self.registry,'docker',side_effect=docker):
            result=self.registry.cleanup(time.monotonic()+0.08)
            self.assertFalse(result.removed)
            state['present']=False
            self.assertTrue(self.registry.cleanup(time.monotonic()+1).removed)

    def test_deadline_preserves_exit_and_runs_cleanup(self):
        log=self.root/'command.log'
        event=self.run('timeout','check',COMMIT,
            [sys.executable,'-c','import time; print("started",flush=True); time.sleep(30)'],
            self.root/OWNER,{'PATH':'/usr/bin:/bin'},log,time.monotonic()+0.2,self.registry)
        self.assertNotEqual(event.exit_code,0)
        self.assertIn('command_deadline',event.failures)
        self.assertIn('started',log.read_text())
        self.assertTrue(self.registry.cleanup(time.monotonic()+3).removed)
        records=[json.loads(line) for line in self.registry.path.read_text().splitlines()]
        self.assertTrue(any(row['event']=='exited' for row in records))
        self.assertEqual(self.registry.path.stat().st_mode & 0o777,0o600)

    def test_json_command_keeps_diagnostics_in_separate_private_file(self):
        log=self.root/'separate.json'
        event=self.run('json','test',COMMIT,[sys.executable,'-c',
            'import sys,time; print("{\\"Action\\":\\"pass\\"}",flush=True); print("diagnostic",file=sys.stderr,flush=True); time.sleep(0.1)'],
            self.root/OWNER,{'PATH':'/usr/bin:/bin'},log,time.monotonic()+3,self.registry)
        self.assertEqual(event.exit_code,0)
        self.assertNotIn('diagnostic',log.read_text())
        diagnostic=Path(str(log)+'.stderr');self.assertIn('diagnostic',diagnostic.read_text())
        self.assertEqual(diagnostic.stat().st_mode & 0o777,0o600)

    def test_non_ascii_workdir_identity_is_exact(self):
        directory=self.root/'企业IM';directory.mkdir(mode=0o700)
        proc=self.sleep(directory)
        self.assertEqual(self.fingerprint(proc.pid)['workdir'],str(directory))

    def test_loaded_foreign_directory_is_not_deleted(self):
        foreign_tmp=tempfile.TemporaryDirectory();self.addCleanup(foreign_tmp.cleanup)
        foreign=Path(foreign_tmp.name).resolve()
        (foreign/'keep').write_text('foreign fixture')
        stat=foreign.stat()
        self.registry.write('registered',OWNER,str(foreign),'directory',
            {'device':stat.st_dev,'inode':stat.st_ino})
        result=self.registry.cleanup(time.monotonic()+1)
        self.assertFalse(result.removed)
        self.assertTrue((foreign/'keep').exists(),'loaded record bypassed directory ownership')

    def test_loaded_foreign_process_is_not_stopped(self):
        foreign_tmp=tempfile.TemporaryDirectory();self.addCleanup(foreign_tmp.cleanup)
        foreign=Path(foreign_tmp.name).resolve()
        proc=self.sleep(foreign)
        self.registry.write('registered',OWNER,str(proc.pid),'process',self.fingerprint(proc.pid))
        result=self.registry.cleanup(time.monotonic()+1)
        self.assertFalse(result.removed)
        self.assertIsNone(proc.poll(),'loaded record bypassed process ownership')

    def test_docker_absence_response_is_case_insensitive_and_verified(self):
        missing=subprocess.CompletedProcess([],1,'[]\n','error: no such object: '+CID+'\n')
        with patch('integration.registry.subprocess.run',return_value=missing):
            try:actual=self.registry.docker(['inspect',CID])
            except ValueError:self.fail('actual Docker absence was reported as cleanup failure')
        self.assertIsNone(actual)
        forbidden=subprocess.CompletedProcess([],1,'','permission denied for '+CID)
        with patch('integration.registry.subprocess.run',return_value=forbidden),self.assertRaises(ValueError):
            self.registry.docker(['inspect',CID])

    def test_cleanup_owned_readonly_module_cache_does_not_follow_links(self):
        directory=self.root/OWNER/'owned-cache';directory.mkdir(mode=0o700)
        module=directory/'module';module.mkdir();(module/'source.go').write_text('package fixture')
        foreign_tmp=tempfile.TemporaryDirectory();self.addCleanup(foreign_tmp.cleanup)
        foreign=Path(foreign_tmp.name).resolve();foreign.chmod(0o500)
        (directory/'outside').symlink_to(foreign,target_is_directory=True)
        module.chmod(0o500)
        def unlock():
            if module.exists():module.chmod(0o700)
            foreign.chmod(0o700)
        self.addCleanup(unlock)
        stat=directory.stat();self.registry.add(self.Ref('directory',OWNER,str(directory),{'device':stat.st_dev,'inode':stat.st_ino}))
        result=self.registry.cleanup(time.monotonic()+3)
        self.assertTrue(result.removed,result.failures)
        self.assertFalse(directory.exists())
        self.assertTrue(foreign.exists())
        self.assertEqual(foreign.stat().st_mode&0o777,0o500)

    def test_process_stops_before_owned_workdir_is_removed(self):
        directory=self.root/OWNER;stat=directory.stat()
        self.registry.add(self.Ref('directory',OWNER,str(directory),{'device':stat.st_dev,'inode':stat.st_ino}))
        import shutil
        executable=directory/'sleep';shutil.copyfile('/bin/sleep',executable);executable.chmod(0o700)
        proc=subprocess.Popen([str(executable),'30'],cwd=directory,start_new_session=True)
        def stop():
            if proc.poll() is None:proc.terminate()
            proc.wait()
        self.addCleanup(stop)
        self.registry.add(self.Ref('process',OWNER,str(proc.pid),self.fingerprint(proc.pid)))
        result=self.registry.cleanup(time.monotonic()+3)
        self.assertTrue(result.removed,result.failures)
        self.assertIsNotNone(proc.poll())
        self.assertFalse(directory.exists())

    def test_process_exits_between_identity_reads(self):
        proc=self.sleep(self.root/OWNER)
        with patch('integration.registry.os.getpgid',side_effect=ProcessLookupError(3,'No such process')):
            try:actual=self.fingerprint(proc.pid)
            except ProcessLookupError:self.fail('process exit between identity reads was treated as cleanup failure')
        self.assertIsNone(actual)

    def test_process_exit_during_workdir_lookup_is_absence_not_unproven(self):
        proc=self.sleep(self.root/OWNER)
        def exit_during_lookup(pid):
            proc.terminate();proc.wait();return None
        with patch('integration.registry._cwd',side_effect=exit_during_lookup):
            try:actual=self.fingerprint(proc.pid)
            except ValueError:self.fail('already exited process was reported as an unproven live workdir')
        self.assertIsNone(actual)
        live=self.sleep(self.root/OWNER)
        with patch('integration.registry._cwd',return_value=None),self.assertRaisesRegex(ValueError,'unproven_process_workdir'):
            self.fingerprint(live.pid)

    def test_process_exit_before_signal_requires_actual_absence(self):
        proc=self.sleep(self.root/OWNER)
        self.registry.add(self.Ref('process',OWNER,str(proc.pid),self.fingerprint(proc.pid)))
        original_kill=os.kill
        def exit_before_signal(pid,sig):
            original_kill(pid,sig);proc.wait();raise ProcessLookupError(3,'No such process')
        with patch('integration.registry.os.kill',side_effect=exit_before_signal):
            result=self.registry.cleanup(time.monotonic()+3)
        self.assertTrue(result.removed,result.failures)

    def test_registry_rejects_malformed_or_symlinked_state(self):
        with self.registry.path.open('a') as f:f.write('{broken\n')
        self.assertFalse(self.registry.cleanup(time.monotonic()+1).removed)


if __name__=='__main__':unittest.main()
