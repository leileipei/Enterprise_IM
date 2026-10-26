"""Exercise the legacy runner with real journals/files and a Docker boundary double."""
import contextlib
import copy
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT/'scripts'))
from integration.registry import Registry

PG, REDIS, VOLUME = 'c'*64, 'd'*64, 'e'*64


class MacosRunnerRecoveryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='im-macos-recovery-test-')
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name).resolve()
        self.base.chmod(0o700)
        self.runtime = self.base/'runtime'
        self.out = self.base/'output'
        spec = importlib.util.spec_from_file_location('macos_recovery_runner', ROOT/'scripts/test-client-macos-backend.py')
        self.runner = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.runner)
        self.containers, self.tickets_at_start, self.started = {}, {}, []
        self.volume_present = False
        self.other_volumes = set()
        self.keep_volume = self.disappear = self.unproven = self.foreign = False
        self.mount_case = None
        self.original_run = subprocess.run

    def new_root(self, **kwargs):
        self.runtime.mkdir(mode=0o700)
        return str(self.runtime)

    def new_registry(self, path, owner, commit):
        self.registry = Registry(path, owner, commit)
        self.registry.docker = self.registry_docker
        return self.registry

    def registry_docker(self, args):
        if args[0] == 'ps':
            return [cid for cid, row in self.containers.items()
                    if row['Config']['Labels']['im.integration.owner'] == self.registry.owner]
        if args[0] == 'inspect':
            return copy.deepcopy(self.containers.get(args[1]))
        if args[:2] == ['volume', 'inspect']:
            self.assertIn(args[2], {VOLUME, 'f'*64})
            present = self.volume_present if args[2] == VOLUME else args[2] in self.other_volumes
            return {'Name': args[2]} if present else None
        if args[:3] == ['rm', '-f', '-v']:
            row = self.containers.pop(args[3])
            if any(m.get('Name') == VOLUME for m in row['Mounts']) and not self.keep_volume:
                self.volume_present = False
            if not self.keep_volume:
                self.other_volumes.difference_update(m.get('Name') for m in row['Mounts'])
            return None
        raise AssertionError('unexpected registry Docker operation: '+str(args))

    def command(self, argv, **kwargs):
        if argv[0] != '/usr/local/bin/docker':
            return self.original_run(argv, **kwargs)
        args = argv[1:]
        if args[0] == 'create':
            cid = PG if not self.containers else REDIS
            labels = dict(args[i+1].split('=', 1) for i, value in enumerate(args) if value == '--label')
            mounts, configured = [], []
            if cid == REDIS:
                self.volume_present = True
                bind = str(self.registry.root/self.registry.owner/'redis.conf')
                mounts = [{'Type': 'bind', 'Source': bind, 'Destination': '/run/im-redis.conf', 'RW': False},
                          {'Type': 'volume', 'Name': VOLUME, 'Destination': '/data', 'RW': True}]
                configured = [{'Type': 'bind', 'Source': bind, 'Target': '/run/im-redis.conf', 'ReadOnly': True}]
                if self.unproven:
                    mounts = None
                if self.foreign:
                    labels['im.integration.owner'] = 'f'*32
            self.containers[cid] = {'Id': cid, 'Config': {'Labels': labels},
                                    'Mounts': mounts, 'HostConfig': {'Binds': [], 'Mounts': configured, 'VolumesFrom': [],
                                        'Tmpfs': {'/var/lib/postgresql/data': 'rw,mode=0700'} if cid == PG else {}},
                                    'State': {'Status': 'created'},
                                    'NetworkSettings': {'Ports': {'5432/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '29991'}]}}}
            actual = self.containers[cid]
            if cid == PG and self.mount_case == 'pg_bind':
                actual['Mounts'].append({'Type': 'bind', 'Source': '/Users/someone/shared-data', 'Destination': '/data', 'RW': True})
                actual['HostConfig']['Mounts'].append({'Type': 'bind', 'Source': '/Users/someone/shared-data', 'Target': '/data'})
            if cid == REDIS:
                if self.mount_case == 'foreign_bind':
                    mounts[0]['Source'] = configured[0]['Source'] = '/Users/someone/shared-data'
                elif self.mount_case == 'named_volume':
                    mounts[1]['Name'] = 'production-data'
                    configured.append({'Type': 'volume', 'Source': 'production-data', 'Target': '/data'})
                elif self.mount_case == 'unknown_type':
                    mounts[0]['Type'] = configured[0]['Type'] = 'foreign-filesystem'
                elif self.mount_case == 'writable_bind':
                    mounts[0]['RW'] = True
                    configured[0]['ReadOnly'] = False
                elif self.mount_case == 'wrong_volume_destination':
                    mounts[1]['Destination'] = '/other-data'
                elif self.mount_case == 'duplicate_volume':
                    mounts.append({'Type': 'volume', 'Name': 'f'*64, 'Destination': '/data', 'RW': True})
                    self.other_volumes.add('f'*64)
                elif self.mount_case == 'unexpected_tmpfs':
                    actual['HostConfig']['Tmpfs'] = {'/unexpected': 'rw'}
            return subprocess.CompletedProcess(argv, 0, cid+'\n', '')
        if args[0] == 'inspect':
            return subprocess.CompletedProcess(argv, 0, json.dumps([self.containers[args[1]]]), '')
        if args[0] == 'start':
            cid = args[1]
            records = [r for r in self.registry.records() if r['event'] == 'registered' and r['identity'] == cid]
            self.tickets_at_start[cid] = copy.deepcopy(records[-1]['fingerprint'])
            self.started.append(cid)
            self.containers[cid]['State']['Status'] = 'running'
            if cid == REDIS:
                if self.disappear:
                    self.containers.pop(cid)
                    self.volume_present = False
                raise RuntimeError('controlled_stop_after_service_start')
            return subprocess.CompletedProcess(argv, 0, '', '')
        if args[0] == 'exec':
            return subprocess.CompletedProcess(argv, 0, '', '')
        raise AssertionError('unexpected runner Docker operation: '+str(args))

    def execute(self):
        argv = ['test-client-macos-backend.py', '--target', 'host', '--flutter-sdk', str(self.base/'sdk'),
                '--pub-cache', str(self.base/'pub'), '--go', '/opt/homebrew/bin/go', '--output', str(self.out)]
        with patch.object(sys, 'argv', argv), \
                patch.object(self.runner, 'client_preflight', return_value={'status': 'ready', 'target': 'host'}), \
                patch.object(self.runner, 'Registry', side_effect=self.new_registry), \
                patch.object(self.runner.tempfile, 'mkdtemp', side_effect=self.new_root), \
                patch.object(subprocess, 'run', side_effect=self.command), \
                patch('integration.registry._capture', return_value=(0, '')), \
                contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(SystemExit) as stopped:
                self.runner.main()
        self.assertEqual(stopped.exception.code, 1)  # The controlled stop is never a passing business gate.
        return json.loads((self.out/'result.json').read_text())

    def test_mounts_are_durable_before_each_container_start(self):
        self.execute()
        self.assertEqual(self.tickets_at_start[PG].get('anonymous_volumes'), [])
        self.assertEqual(self.tickets_at_start[REDIS].get('anonymous_volumes'), [VOLUME])
        self.assertEqual(self.tickets_at_start[REDIS]['mount_configuration']['Mounts'][0]['Target'], '/run/im-redis.conf')

    def test_unproven_mounts_are_rejected_before_start(self):
        self.unproven = True
        result = self.execute()
        self.assertNotIn(REDIS, self.started)
        self.assertFalse(result['cleanup']['removed'])
        self.assertTrue(self.registry.path.exists())

    def test_foreign_identity_is_rejected_before_start_and_removal(self):
        self.foreign = True
        result = self.execute()
        self.assertNotIn(REDIS, self.started)
        self.assertIn(REDIS, self.containers)
        self.assertFalse(result['cleanup']['removed'])

    def test_failed_cleanup_keeps_journal_and_private_recovery_inputs(self):
        self.keep_volume = True
        result = self.execute()
        self.assertFalse(result['cleanup']['removed'])
        self.assertTrue(self.registry.path.is_file(), 'failed cleanup discarded its durable recovery journal')
        self.assertEqual(result.get('recovery_root'), str(self.runtime))
        self.assertFalse(result['private_root_removed'])
        private = self.runtime/self.registry.owner
        self.assertTrue((private/'postgres.env').is_file())
        self.assertTrue((private/'redis.conf').is_file())
        self.assertEqual(private.stat().st_mode & 0o777, 0o700)
        self.assertEqual((private/'postgres.env').stat().st_mode & 0o777, 0o600)
        retained = self.registry.records()
        exported = [json.loads(line) for line in (self.out/'result.registry.jsonl').read_text().splitlines()]
        self.assertEqual(exported, retained)

    def test_disappeared_container_is_checked_using_its_durable_mount_ticket(self):
        self.disappear = True
        result = self.execute()
        self.assertTrue(result['cleanup']['removed'], result['cleanup']['failures'])
        self.assertTrue(result['private_root_removed'])

    def assert_mount_rejected(self, case, cid=REDIS):
        self.mount_case = case
        self.execute()
        self.assertNotIn(cid, self.started, 'correct container labels must not authorize an unexpected mount')

    def test_foreign_bind_source_with_owned_labels_is_not_started(self):
        self.assert_mount_rejected('foreign_bind')

    def test_explicit_foreign_named_volume_is_not_started(self):
        self.assert_mount_rejected('named_volume')

    def test_unknown_mount_type_is_not_started(self):
        self.assert_mount_rejected('unknown_type')

    def test_redis_config_must_not_be_mounted_writable(self):
        self.assert_mount_rejected('writable_bind')

    def test_unexpected_postgres_bind_is_not_started(self):
        self.assert_mount_rejected('pg_bind', PG)

    def test_anonymous_volume_at_unexpected_destination_is_not_started(self):
        self.assert_mount_rejected('wrong_volume_destination')

    def test_duplicate_volume_destination_is_not_started(self):
        self.assert_mount_rejected('duplicate_volume')

    def test_unexpected_redis_tmpfs_is_not_started(self):
        self.assert_mount_rejected('unexpected_tmpfs')

    def test_proven_cleanup_removes_owned_private_root(self):
        result = self.execute()
        self.assertTrue(result['cleanup']['removed'], result['cleanup']['failures'])
        self.assertFalse(self.runtime.exists())
        self.assertFalse(self.containers)
        self.assertFalse(self.volume_present)


if __name__ == '__main__':
    unittest.main()
