"""Run the import runner's real nested lifecycle against a Docker protocol double."""
import ast
import copy
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from integration.registry import Registry
from integration.model import ResourceRef
from support import COMMIT

OWNER = 'b' * 32
CID = 'd' * 64
VOLUME = 'c' * 64
SCRIPT = Path(__file__).resolve().parents[1]/'test-import-apply.py'


class ImportContainerMountTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='im-import-mount-test-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.root.chmod(0o700)
        self.ledger = Registry(self.root/'registry.jsonl', OWNER, COMMIT,
                               recover_containers=False)
        self.actual = {'Id': CID, 'Config': {'Labels': {
            'im.integration.owner': OWNER, 'im.integration.source': COMMIT}},
            'State': {'Status': 'created'},
            'Mounts': [{'Type': 'volume', 'Name': VOLUME, 'Destination': '/data'}],
            'HostConfig': {'Binds': [], 'Mounts': [], 'VolumesFrom': []}}
        self.present = True
        self.volume_present = True
        self.ticket_at_start = None
        self.started = False
        self.ledger.docker = self.docker
        tree = ast.parse(SCRIPT.read_text())
        self.main = next(n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name == 'main')

    def docker(self, argv):
        if argv[0] == 'inspect':
            if argv[1] != CID:
                raise AssertionError('wrong container identity')
            return copy.deepcopy(self.actual) if self.present else None
        if argv[:2] == ['volume', 'inspect']:
            if argv[2] != VOLUME:
                raise AssertionError('wrong volume identity')
            return {'Name': VOLUME} if self.volume_present else None
        if argv[0] == 'rm':
            if argv[-1] != CID:
                raise AssertionError('wrong removal identity')
            self.present = False
            if '-v' in argv:
                self.volume_present = False
            return None
        raise AssertionError('unexpected Docker request: '+str(argv))

    def checked(self, argv):
        if argv[:2] == ['docker', 'create']:
            return CID
        if argv[:2] == ['docker', 'inspect']:
            return json.dumps([self.actual])
        if argv == ['docker', 'start', CID]:
            rows = [r for r in self.ledger.records() if r['event'] == 'registered']
            self.ticket_at_start = copy.deepcopy(rows[-1]['fingerprint'])
            self.started = True
            return ''
        raise AssertionError('unexpected command: '+str(argv))

    def namespace(self):
        return dict(registry=self.ledger, owner=OWNER, commit=COMMIT, containers=[], checked=self.checked,
                    image={'Id': 'sha256:'+'e'*64}, time=time, json=json,
                    subprocess=subprocess,
                    ResourceRef=ResourceRef)

    def start(self):
        node = next(n for n in self.main.body if isinstance(n, ast.FunctionDef) and n.name == 'start_container')
        namespace = self.namespace()
        exec(compile(ast.Module(body=[node], type_ignores=[]), str(SCRIPT), 'exec'), namespace)
        with patch.object(subprocess, 'run', return_value=subprocess.CompletedProcess([], 0)):
            return namespace['start_container']('owned-import', [])

    def cleanup(self):
        outer = next(n for n in self.main.body if isinstance(n, ast.Try) and n.finalbody)
        loop = next(n for n in outer.finalbody if isinstance(n, ast.For))
        namespace = self.namespace()
        namespace.update(containers=[CID], removed=True)
        exec(compile(ast.Module(body=[loop], type_ignores=[]), str(SCRIPT), 'exec'), namespace)
        return namespace['removed']

    def ticket(self):
        self.ledger.add(ResourceRef('container', OWNER, CID, {'container_id': CID}))
        return self.ledger.record_container_mounts(CID, copy.deepcopy(self.actual))

    def test_start_persists_volume_mapping_before_child_can_auto_remove(self):
        self.assertEqual(self.start(), CID)
        self.assertEqual(self.ticket_at_start.get('anonymous_volumes'), [VOLUME])
        self.assertEqual(self.ticket_at_start.get('mounts'), self.actual['Mounts'])
        self.present = self.volume_present = False
        result = self.ledger.cleanup(time.monotonic()+2)
        self.assertTrue(result.removed, result.failures)

    def test_unproven_mounts_are_rejected_before_start(self):
        self.actual['Mounts'] = None
        with self.assertRaisesRegex(ValueError, 'container_mounts_unproven'):
            self.start()
        self.assertFalse(self.started)

    def test_cleanup_rejects_missing_container_with_known_volume_still_present(self):
        self.ticket()
        self.present = False
        self.assertFalse(self.cleanup(), 'container absence must not hide a retained anonymous volume')
        self.assertTrue(self.volume_present)

    def test_cleanup_rejects_changed_mapping_without_discarding_ticket(self):
        original = self.ticket()
        self.actual['Mounts'][0]['Name'] = 'f'*64
        self.assertFalse(self.cleanup(), 'changed mounts must block removal')
        self.assertTrue(self.present)
        latest = [r for r in self.ledger.records() if r['event'] == 'registered'][-1]
        self.assertEqual(latest['fingerprint'], original)

    def test_cleanup_removes_owned_container_and_exact_anonymous_volume(self):
        self.ticket()
        self.assertTrue(self.cleanup())
        self.assertFalse(self.present)
        self.assertFalse(self.volume_present)


if __name__ == '__main__':
    unittest.main()
