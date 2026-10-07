"""Synthetic protocol fixtures; these are not business integration evidence."""
import json
from pathlib import Path

COMMIT = 'a' * 40
PACKAGE = 'github.com/leileipei/Enterprise_IM/internal/policystore'
HELPER = PACKAGE + '::TestRealtimeAPIChild'
CALLER = PACKAGE + '::TestMultiProcessRealtimeWorkerFanoutAndReconnect'


def go_rows(package, tests, package_status='pass'):
    rows = [{'Action': 'start', 'Package': package}]
    for test, status in tests:
        rows.extend(({'Action': 'run', 'Package': package, 'Test': test},
                     {'Action': status, 'Package': package, 'Test': test}))
    rows.append({'Action': package_status, 'Package': package})
    return rows


def write_json(path, rows):
    path.write_text(''.join(json.dumps(row) + '\n' for row in rows))
    return path


def helper_proofs(gate):
    return [dict(gate=gate, test=CALLER, source_commit=COMMIT,
                 identity=str(pid), pid=pid, uid=501,
                 start_time='2026-10-07T00:00:00Z', owner='b' * 32,
                 executable_sha256='c' * 64, registered=True, ready=True,
                 exited=True, expected_exit='exit:0', actual_exit='exit:0')
            for pid in (101, 102)]
