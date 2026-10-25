"""Fault-boundary regression tests. Docker and simctl are protocol doubles only."""
import errno
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from integration import local_env, local_env_simulators, registry, commands
from integration.model import ResourceRef
from support import COMMIT

OWNER = 'a' * 32
CID = 'd' * 64
VOLUME = 'e' * 64


class RuntimeCleanupRegressionTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='im-cleanup-regression-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.root.chmod(0o700)
        self.ledger = registry.Registry(self.root/'registry.jsonl', OWNER, COMMIT,
                                        recover_containers=False)
        self.private = self.root/OWNER

    def child(self):
        p = subprocess.Popen(['/bin/sleep', '30'], cwd=self.private, start_new_session=True)
        def stop():
            if p.poll() is None:
                p.terminate()
            p.wait(timeout=5)
        self.addCleanup(stop)
        self.ledger.add(ResourceRef('process', OWNER, str(p.pid), registry.process_fingerprint(p.pid)))
        return p

    def environment(self):
        # State and registry use separate names, but both bind the same owner/source.
        env = local_env.Environment.__new__(local_env.Environment)
        env.root = self.root
        env.owner = OWNER
        env.source = COMMIT
        env.public = {'phase': 'ready'}
        env.registry = self.ledger
        env.private = self.private
        env.metadata = {}
        env.secret_values = ['private-secret']
        env.processes = []
        env.save = lambda: (self.root/'observed-state.json').write_text(json.dumps(env.public))
        return env

    def test_evidence_write_failure_still_stops_owned_process_and_keeps_raw_log(self):
        p = self.child()
        raw = self.private/'api.log'
        raw.write_bytes(b'private-secret raw log')
        env = self.environment()
        original = Path.write_bytes
        def fail_evidence(path, data):
            if path.parent == self.root/'evidence':
                raise OSError(errno.ENOSPC, 'injected evidence disk full')
            return original(path, data)
        caught = None
        with patch.object(Path, 'write_bytes', fail_evidence):
            try:
                failures = env.close()
            except OSError as exc:
                caught = exc
        self.assertIsNotNone(p.poll(), 'evidence failure bypassed resource cleanup')
        self.assertIsNone(caught, 'close must report evidence failure without skipping cleanup')
        self.assertIn('evidence_publish_failed', failures)
        self.assertEqual(raw.read_bytes(), b'private-secret raw log')
        self.assertEqual(env.public['phase'], 'failed')

    def test_repeated_close_preserves_unpublished_logs_with_existing_directory_ticket(self):
        env=self.environment();raw=self.private/'api.log';raw.write_bytes(b'old published log')
        original_remove=registry.shutil.rmtree
        def busy(path,*args,**kw):
            if Path(path)==self.private:raise OSError(errno.EBUSY,'injected busy directory')
            return original_remove(path,*args,**kw)
        with patch.object(registry.shutil,'rmtree',side_effect=busy):
            self.assertTrue(env.close())
        self.assertTrue(any(r['kind']=='directory' and r['event']=='registered' for r in self.ledger.records()))
        raw.write_bytes(b'new unpublished raw log')
        original_write=Path.write_bytes
        def disk_full(path,data):
            if path.parent==self.root/'evidence':raise OSError(errno.ENOSPC,'evidence full')
            return original_write(path,data)
        with patch.object(Path,'write_bytes',disk_full):failures=env.close()
        self.assertIn('evidence_publish_failed',failures)
        self.assertTrue(raw.exists(),'an older directory ticket deleted unpublished raw material')
        self.assertEqual(raw.read_bytes(),b'new unpublished raw log')
        self.assertEqual((self.root/'evidence/api.log').read_bytes(),b'old published log')

    def test_state_save_failure_cannot_bypass_registered_process_cleanup(self):
        p = self.child()
        env = self.environment()
        caught = None
        with patch.object(env, 'save', side_effect=OSError(errno.ENOSPC, 'injected state failure')):
            try:
                failures = env.close()
            except OSError as exc:
                caught = exc
        self.assertIsNotNone(p.poll(), 'state publication failure bypassed cleanup')
        self.assertIsNone(caught)
        self.assertIn('state_publish_failed', failures)
        self.assertTrue(self.private.exists(), 'unpublished state must retain recovery material')

    def test_recovery_discovery_failure_still_stops_known_process_and_preserves_directory(self):
        p = self.child()
        info = self.private.stat()
        self.ledger.add(ResourceRef('directory', OWNER, str(self.private),
                                    {'device': info.st_dev, 'inode': info.st_ino}))
        self.ledger.recover_containers = True
        with patch.object(self.ledger, 'docker', side_effect=ValueError('docker_list_failed')):
            result = self.ledger.cleanup(time.monotonic()+3)
        self.assertIsNotNone(p.poll(), 'known process was skipped after Docker discovery failure')
        self.assertFalse(result.removed, 'discovery failure must remain a failed cleanup')
        self.assertIn('docker_list_failed', result.failures)
        self.assertTrue(self.private.exists(), 'retain material when recovery could not complete')

    def test_registration_refusal_preserves_original_error_and_reaps_child(self):
        with tempfile.TemporaryDirectory(prefix='im-foreign-command-') as foreign:
            seen = []
            fingerprint = registry.process_fingerprint
            def observe(pid):
                seen.append(pid)
                return fingerprint(pid)
            with patch.object(commands, 'process_fingerprint', side_effect=observe):
                with self.assertRaisesRegex(ValueError, '^process_outside_owned_root$'):
                    commands.run_command('admission', 'test', COMMIT, ['/bin/sleep', '30'],
                        Path(foreign).resolve(), {'PATH': '/usr/bin:/bin'},
                        self.root/'admission.log', time.monotonic()+3, self.ledger)
            self.assertTrue(seen)
            self.assertIsNone(fingerprint(seen[0]), 'supervised rejected child remained live')
            self.assertEqual([x['event'] for x in self.ledger.records()], ['reserve'])

    def test_registration_io_failure_preserves_original_error(self):
        with patch.object(self.ledger, 'add', side_effect=OSError(errno.ENOSPC, 'ledger full')):
            with self.assertRaisesRegex(OSError, 'ledger full'):
                commands.run_command('ledger', 'test', COMMIT, ['/bin/sleep', '30'],
                    self.private, {'PATH': '/usr/bin:/bin'}, self.root/'ledger.log',
                    time.monotonic()+3, self.ledger)
        self.assertEqual([x['event'] for x in self.ledger.records()], ['reserve'])


class IOSCommandEvidenceRegressionTests(unittest.TestCase):
    def test_timeout_keeps_partial_streams_hashes_and_rethrows_original_exception(self):
        with tempfile.TemporaryDirectory(prefix='im-simctl-timeout-') as tmp:
            actor = local_env_simulators.OwnedIOS.__new__(local_env_simulators.OwnedIOS)
            actor.out = Path(tmp).resolve()
            actor.env = {'process_env': {}}
            actor.state = {'commands': []}
            error = subprocess.TimeoutExpired(['xcrun', 'simctl'], 7,
                                                output=b'partial stdout\xff', stderr=b'partial stderr')
            with patch('subprocess.run', side_effect=error):
                with self.assertRaises(subprocess.TimeoutExpired) as raised:
                    actor.call('create', ['create', 'owned-fixture'], 7)
            self.assertIs(raised.exception, error)
            saved = json.loads((actor.out/'ios-state.json').read_text())
            self.assertEqual(len(saved['commands']), 1)
            row = saved['commands'][0]
            self.assertEqual(row['argv'], ['/usr/bin/xcrun', 'simctl', 'create', 'owned-fixture'])
            self.assertIsNone(row['actual_exit'], 'a timeout has no observed process exit code')
            self.assertTrue(row['timed_out'])
            self.assertEqual(row['timeout_seconds'], 7)
            out = actor.out/'000-create.log'
            err = actor.out/'000-create.log.stderr'
            self.assertEqual(out.read_bytes(), b'partial stdout\xff')
            self.assertEqual(err.read_bytes(), b'partial stderr')
            self.assertEqual(row['stdout_sha256'], hashlib.sha256(b'partial stdout\xff').hexdigest())
            self.assertEqual(row['stderr_sha256'], hashlib.sha256(b'partial stderr').hexdigest())
            self.assertEqual(out.stat().st_mode & 0o777, 0o600)
            self.assertEqual(err.stat().st_mode & 0o777, 0o600)


    def test_retry_cannot_overwrite_partial_log_or_follow_old_log_link(self):
        for existing in ['stdout','stderr','linked-stdout']:
            with self.subTest(existing=existing),tempfile.TemporaryDirectory(prefix='im-ios-log-reuse-') as tmp:
                root=Path(tmp).resolve();out=root/'evidence';out.mkdir(mode=0o700)
                actor=local_env_simulators.OwnedIOS.__new__(local_env_simulators.OwnedIOS)
                actor.out=out;actor.env={'process_env':{}};actor.state={'commands':[]}
                foreign=root/'keep-foreign';foreign.write_bytes(b'foreign original')
                name='000-create.log.stderr' if existing=='stderr' else '000-create.log'
                log=out/name
                if existing=='linked-stdout':log.symlink_to(foreign)
                else:log.write_bytes(b'first failed attempt partial bytes')
                before=log.read_bytes();refusal=None
                with patch('subprocess.run',return_value=subprocess.CompletedProcess([],0,b'retry output',b'retry diagnostic')) as external:
                    try:actor.call('create',['create','owned-fixture'],3)
                    except ValueError as exc:refusal=str(exc)
                self.assertEqual(log.read_bytes(),before,'retry overwrote an earlier failed attempt')
                self.assertEqual(foreign.read_bytes(),b'foreign original')
                self.assertEqual(refusal,'owned_ios_evidence_path_exists')
                external.assert_not_called()
                self.assertEqual(actor.state['commands'],[])

    def test_timeout_stays_primary_when_reserved_log_close_fails(self):
        with tempfile.TemporaryDirectory(prefix='im-ios-close-fault-') as tmp:
            actor=local_env_simulators.OwnedIOS.__new__(local_env_simulators.OwnedIOS)
            actor.out=Path(tmp).resolve();actor.env={'process_env':{}};actor.state={'commands':[]}
            timeout=subprocess.TimeoutExpired(['simctl'],3,output=b'partial',stderr=b'diagnostic')
            original=os.fdopen
            class ClosingFault:
                def __init__(self,stream):self.stream=stream
                def __enter__(self):return self
                def __getattr__(self,name):return getattr(self.stream,name)
                def __exit__(self,*args):
                    self.stream.close()
                    raise OSError(errno.ENOSPC,'reserved log close failed')
            with patch.object(local_env_simulators.os,'fdopen',side_effect=lambda *a,**kw:ClosingFault(original(*a,**kw))), \
                 patch('subprocess.run',side_effect=timeout):
                with self.assertRaises(subprocess.TimeoutExpired) as raised:actor.call('create',['create','owned-fixture'],3)
            self.assertIs(raised.exception,timeout)
            self.assertIsInstance(timeout.__cause__,OSError)
            self.assertEqual((actor.out/'000-create.log').read_bytes(),b'partial')
            self.assertEqual((actor.out/'000-create.log.stderr').read_bytes(),b'diagnostic')

    def test_timeout_remains_primary_when_evidence_write_or_state_save_also_fails(self):
        for stage in ['log','state']:
            with self.subTest(stage=stage),tempfile.TemporaryDirectory(prefix='im-timeout-publish-') as tmp:
                actor=local_env_simulators.OwnedIOS.__new__(local_env_simulators.OwnedIOS)
                actor.out=Path(tmp).resolve();actor.env={'process_env':{}};actor.state={'commands':[]}
                timeout=subprocess.TimeoutExpired(['simctl'],3,output=b'partial',stderr=b'diagnostic')
                context=patch.object(local_env_simulators,'_write_command_log',side_effect=OSError(errno.ENOSPC,'evidence full')) if stage=='log' else patch.object(actor,'save',side_effect=OSError(errno.ENOSPC,'state full'))
                with patch('subprocess.run',side_effect=timeout),context:
                    with self.assertRaises(subprocess.TimeoutExpired) as raised:actor.call('create',['create','owned-fixture'],3)
                self.assertIs(raised.exception,timeout)
                self.assertIsInstance(timeout.__cause__,OSError)
                self.assertEqual(timeout.stdout,b'partial')
                self.assertEqual(timeout.stderr,b'diagnostic')


class ContainerVolumeRegressionTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='im-volume-protocol-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.root.chmod(0o700)
        self.ledger = registry.Registry(self.root/'registry.jsonl', OWNER, COMMIT,
                                        recover_containers=False)
        self.present = True
        self.volumes = {VOLUME, 'keep-named'}
        self.calls = []
        self.keep_volume = False
        self.actual = {'Id': CID, 'Config': {'Labels': {
            'im.integration.owner': OWNER, 'im.integration.source': COMMIT}},
            'State': {'Status': 'created'},
            'Mounts': [{'Type': 'volume', 'Name': VOLUME, 'Destination': '/data'},
                       {'Type': 'volume', 'Name': 'keep-named', 'Destination': '/shared'}],
            'HostConfig': {'Binds': ['keep-named:/shared:rw'], 'Mounts': [], 'VolumesFrom': []}}

    def docker(self, args):
        self.calls.append(args)
        if args[0] == 'ps':
            return [CID] if self.present else []
        if args == ['inspect', CID]:
            return self.actual if self.present else None
        if args[:2] == ['rm', '-f']:
            self.assertEqual(args, ['rm', '-f', '-v', CID], 'anonymous volumes must be removed with their verified container')
            row = [r for r in self.ledger.records() if r['event'] == 'registered' and r['identity'] == CID][-1]
            self.assertEqual(row['fingerprint']['anonymous_volumes'], [VOLUME], 'persist exact volume mapping before deletion')
            self.present = False
            if not self.keep_volume:
                self.volumes.discard(VOLUME)
            return None
        if args[:2] == ['volume', 'inspect']:
            return {'Name': args[2]} if args[2] in self.volumes else None
        self.fail('unexpected Docker operation: '+repr(args))

    def cleanup(self):
        with patch.object(self.ledger, 'docker', side_effect=self.docker):
            return self.ledger.cleanup(time.monotonic()+2)

    def test_container_cleanup_removes_only_its_anonymous_volumes_and_verifies_absence(self):
        self.ledger.add(ResourceRef('container', OWNER, CID, {'container_id': CID}))
        result = self.cleanup()
        self.assertTrue(result.removed, result.failures)
        self.assertFalse(self.present)
        self.assertNotIn(VOLUME, self.volumes)
        self.assertIn('keep-named', self.volumes)
        self.assertIn(['volume', 'inspect', VOLUME], self.calls)
        self.assertNotIn(['volume', 'inspect', 'keep-named'], self.calls)

    def test_remaining_volume_is_failure_and_mapping_survives_reload(self):
        self.keep_volume = True
        self.ledger.add(ResourceRef('container', OWNER, CID, {'container_id': CID}))
        result = self.cleanup()
        self.assertFalse(result.removed)
        self.assertTrue(any('anonymous_volume_still_present' in s for s in result.failures))
        self.ledger = registry.Registry(self.ledger.path, OWNER, COMMIT, recover_containers=False)
        result = self.cleanup()
        self.assertFalse(result.removed, 'container absence must not hide a retained volume')
        self.assertIn(VOLUME, self.volumes)

    def test_absent_legacy_container_without_mount_proof_is_not_cleanup_success(self):
        self.present = False
        self.ledger.add(ResourceRef('container', OWNER, CID, {'container_id': CID}))
        result = self.cleanup()
        self.assertFalse(result.removed, 'unknown historical mounts cannot be assumed empty')
        self.assertTrue(any('container_mounts_unproven' in s for s in result.failures))

    def test_create_register_gap_recovery_records_mounts_before_removal(self):
        self.ledger.recover_containers = True
        result = self.cleanup()
        self.assertTrue(result.removed, result.failures)
        self.assertNotIn(VOLUME, self.volumes)

    def test_missing_mount_inventory_refuses_removal(self):
        del self.actual['Mounts']
        self.ledger.add(ResourceRef('container', OWNER, CID, {'container_id': CID}))
        result = self.cleanup()
        self.assertFalse(result.removed)
        self.assertTrue(self.present)
        self.assertFalse(any(x[0]=='rm' for x in self.calls))

    def test_concurrent_resource_launches_share_one_exclusive_admission(self):
        from concurrent.futures import ThreadPoolExecutor
        import threading
        from integration import resource_command
        binary=self.root/OWNER/'transfer.test';binary.write_bytes(b'owned protocol binary')
        proof=binary.parent/'concurrent-proof.json';barrier=threading.Barrier(2)
        seen=threading.local();original_exists=Path.exists;calls=[]
        def exists(path):
            value=original_exists(path)
            if path==proof and not getattr(seen,'checked',False):
                seen.checked=True;barrier.wait(timeout=5)
            return value
        def docker(args):
            self.assertEqual(args[0],'create');calls.append(args)
            raise ValueError('controlled_fixture_stop')
        def attempt():
            try:resource_command.launch(['fixture-image'],self.ledger,proof,binary)
            except ValueError as exc:return str(exc)
            self.fail('controlled create stop must not be reported successful')
        with patch.object(Path,'exists',exists),patch.object(resource_command,'docker',side_effect=docker):
            with ThreadPoolExecutor(max_workers=2) as workers:
                jobs=[workers.submit(attempt) for _ in range(2)]
                results=[job.result(timeout=10) for job in jobs]
        self.assertEqual(len(calls),1,'two participants crossed the same evidence admission')
        self.assertEqual(sorted(results),['controlled_fixture_stop','resource_evidence_path_exists'])
        reservation=Path(str(proof)+'.reservation')
        ticket=json.loads(reservation.read_text())
        self.assertEqual(ticket['owner'],OWNER);self.assertEqual(ticket['source_commit'],COMMIT)
        self.assertEqual(ticket['proof'],str(proof))
        self.assertEqual(reservation.stat().st_mode&0o777,0o600)

    def test_resource_reused_proof_or_recovery_ticket_refuses_before_docker(self):
        from integration import resource_command
        binary=self.root/OWNER/'transfer.test';binary.write_bytes(b'owned protocol binary')
        for suffix in ['', '.cleanup.json']:
            with self.subTest(existing=suffix):
                proof=binary.parent/('proof-'+('summary' if suffix=='' else 'recovery')+'.json')
                previous=Path(str(proof)+suffix);previous.write_bytes(b'previous failure evidence')
                with patch.object(resource_command,'docker',side_effect=AssertionError('Docker touched before evidence admission')) as external:
                    with self.assertRaisesRegex(ValueError,'^resource_evidence_path_exists$'):
                        resource_command.launch(['fixture-image'],self.ledger,proof,binary)
                self.assertEqual(previous.read_bytes(),b'previous failure evidence')
                external.assert_not_called()
                self.assertEqual([r['event'] for r in self.ledger.records()],['reserve'])

    def test_resource_registration_io_failure_preserves_error_and_exact_recovery_ticket(self):
        from integration import resource_command
        binary=self.root/OWNER/'transfer.test';binary.write_bytes(b'owned protocol binary')
        proof=binary.parent/'proof.json';created=[]
        def docker(args):
            self.assertEqual(args[0],'create');created.append(CID);return CID
        with patch.object(resource_command,'docker',side_effect=docker), \
             patch.object(self.ledger,'add',side_effect=OSError(errno.ENOSPC,'ledger full')), \
             patch.object(self.ledger,'docker',side_effect=self.docker):
            with self.assertRaisesRegex(OSError,'ledger full'):
                resource_command.launch(['fixture-image'],self.ledger,proof,binary)
        self.assertEqual(created,[CID])
        report=json.loads(proof.read_text());self.assertEqual(report['container_id'],CID)
        self.assertFalse(report['cleanup'])
        ticket=json.loads(Path(str(proof)+'.cleanup.json').read_text())
        self.assertEqual(ticket['owner'],OWNER);self.assertEqual(ticket['source_commit'],COMMIT)
        self.assertEqual(ticket['container_id'],CID)
        self.assertEqual(ticket['primary_error'],{'type':'OSError','errno':errno.ENOSPC})
        self.assertEqual(ticket['cleanup_error'],'container_not_registered')
        self.assertEqual(ticket['observed_mount_fingerprint']['anonymous_volumes'],[VOLUME])
        self.assertTrue(self.present,'without durable ownership/mount proof, do not guess-delete')
        self.assertEqual(Path(str(proof)+'.cleanup.json').stat().st_mode&0o777,0o600)

    def test_local_and_mobile_services_snapshot_mounts_before_start(self):
        from integration.mobile_backend import Backend
        for cls in [local_env.Environment, Backend]:
            with self.subTest(service=cls.__name__):
                actor=cls.__new__(cls);actor.registry=self.ledger
                actor.owner=OWNER;actor.source=COMMIT;actor.public={'resources':{}};actor.state={}
                actor.save=lambda:None;actor.checkpoint=lambda:None
                actual=dict(self.actual,Image='sha256:'+'f'*64,
                            NetworkSettings={'Ports':{'6379/tcp':[{'HostIp':'127.0.0.1','HostPort':'16379'}]}})
                def docker(args,*unused,**kw):
                    if args[0]=='create':out=CID
                    elif args[0]=='inspect':out=json.dumps([actual])
                    elif args[0]=='start':
                        row=[r for r in self.ledger.records() if r['event']=='registered' and r['identity']==CID][-1]
                        self.assertIn('mounts',row['fingerprint'],'start ran before the mount ticket was durable')
                        out=''
                    else:self.fail(args)
                    return subprocess.CompletedProcess(args,0,out,'') if cls is local_env.Environment else out
                actor.docker=docker
                actor.service('redis','redis@sha256:'+'f'*64,'6379',[])

    def test_service_preparation_snapshots_mounts_before_start_failure(self):
        from integration import services
        from integration.model import SourceSnapshot,Toolchain
        snapshot=SourceSnapshot(COMMIT,self.root,self.root,self.root/'unused','f'*64)
        tools=Toolchain({}, {}, {}, {k:{'reference':k+'@sha256:'+'f'*64}
            for k in ['postgres-image','redis-image','minio-image']},'darwin','arm64')
        seen=[]
        def command(_tools,args,deadline,input=None):
            if args[0]=='create':return CID
            if args[0]=='inspect':return json.dumps([self.actual])
            if args[0]=='start':
                seen.append([r for r in self.ledger.records() if r['event']=='registered' and r['identity']==CID][-1]['fingerprint'])
                raise ValueError('expected_start_failure')
            self.fail(args)
        with patch.object(services,'_generate_tls'),patch.object(services,'docker_environment',return_value={}), \
             patch.object(services,'_docker',side_effect=command),patch.object(self.ledger,'docker',side_effect=self.docker):
            with self.assertRaisesRegex(ValueError,'expected_start_failure'):
                services.prepare_services(snapshot,tools,self.ledger,self.root/OWNER/'services',time.monotonic()+2)
        self.assertEqual(len(seen),1)
        self.assertEqual(seen[0].get('anonymous_volumes'),[VOLUME], 'failed start must already have an exact volume ticket')
        self.assertNotIn(VOLUME,self.volumes)

    def test_volume_absence_parser_does_not_treat_access_denial_as_absence(self):
        absent = subprocess.CompletedProcess([], 1, '[]\n', 'Error response from daemon: get '+VOLUME+': no such volume\n')
        with patch('integration.registry.shutil.which', return_value='/unused/docker'), \
             patch('integration.services.docker_environment', return_value={}), \
             patch('integration.registry.subprocess.run', return_value=absent):
            self.assertIsNone(self.ledger.docker(['volume', 'inspect', VOLUME]))
        forbidden = subprocess.CompletedProcess([], 1, '', 'permission denied for '+VOLUME)
        with patch('integration.registry.shutil.which', return_value='/unused/docker'), \
             patch('integration.services.docker_environment', return_value={}), \
             patch('integration.registry.subprocess.run', return_value=forbidden):
            with self.assertRaisesRegex(ValueError, 'docker_volume_inspect_failed'):
                self.ledger.docker(['volume', 'inspect', VOLUME])


if __name__ == '__main__':
    unittest.main()
