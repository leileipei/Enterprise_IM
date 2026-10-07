"""Reject incomplete evidence rather than treating missing failures as success."""
import json
import re
from pathlib import Path
from typing import List, Optional

from .model import CleanupResult, GateEvent, Inventory, Verdict

REQUIRED_GATES = (
    'orchestrator_contract', 'toolchain', 'fixture_preflight', 'build_all',
    'vet_all', 'message_realtime', 'file_components', 'file_resources',
    'file_messages', 'file_download_retention', 'web_files',
    'file_business_process', 'import_append', 'full_repository',
    'race_repository', 'evidence_integrity', 'resource_cleanup',
)
CHECK_GATES = {'toolchain', 'fixture_preflight', 'build_all', 'vet_all',
               'evidence_integrity', 'resource_cleanup'}
HELPER = 'github.com/leileipei/Enterprise_IM/internal/policystore::TestRealtimeAPIChild'
CALLER = 'github.com/leileipei/Enterprise_IM/internal/policystore::TestMultiProcessRealtimeWorkerFanoutAndReconnect'
TERMINALS = {'pass', 'fail', 'skip'}


def _event(log, name, commit, rc):
    counts = {scope+'_'+status: 0 for scope in ('top','sub','package')
              for status in TERMINALS}
    counts.update(ordinary_skip=0, helper_skip=0)
    return GateEvent(name, 'test', commit, rc, Path(log), counts=counts)


def _terminal(event, package, test, action):
    if test:
        identity = package+'::'+test
        if identity in event.executed:
            event.failures.append('duplicate_test_terminal:'+identity)
        event.executed.add(identity)
        event.counts[('sub' if '/' in test else 'top')+'_'+action] += 1
        if action == 'pass':
            event.passed.add(identity)
        elif action == 'fail':
            event.failures.append(identity)
        else:
            event.skips.append(identity)
    else:
        if package in event.package_status:
            event.failures.append('duplicate_package_terminal:'+package)
        event.package_status[package] = action
        event.counts['package_'+action] += 1
        if action == 'fail':
            event.failures.append('package_failed:'+package)


def parse_go(log: Path, name: str, source_commit: str, exit_code: int) -> GateEvent:
    event = _event(log, name, source_commit, exit_code)
    no_tests = set()
    try:
        lines = Path(log).read_text().splitlines()
    except (OSError, UnicodeError):
        event.failures.append('unreadable_log')
        return event
    for n, line in enumerate(lines, 1):
        if not line.strip():
            continue
        try:
            row = json.loads(line)
        except (ValueError, TypeError):
            # Compiler diagnostics can precede JSON; never discard malformed JSON.
            if not line.startswith('# '):
                event.failures.append('invalid_json_line:'+str(n))
            continue
        if not isinstance(row, dict):
            event.failures.append('invalid_event:'+str(n))
            continue
        action, package, test = row.get('Action'), row.get('Package'), row.get('Test','')
        output = row.get('Output','')
        if not isinstance(output,str) or not isinstance(test,str):
            event.failures.append('invalid_event_fields:'+str(n)); continue
        if 'WARNING: DATA RACE' in output:
            event.failures.append('race_report')
        if action in {'build-output','build-fail'}:
            if action == 'build-fail': event.failures.append('build_failed')
            continue
        if not isinstance(package,str) or not package or action not in {
                'start','run','pause','cont','output','bench','pass','fail','skip'}:
            event.failures.append('invalid_event_fields:'+str(n)); continue
        if action == 'run' and test:
            event.started.add(package+'::'+test)
        elif action in TERMINALS:
            _terminal(event,package,test,action)
        if '[no test files]' in output and not test:
            no_tests.add(package)
    for package in no_tests:
        if event.package_status.get(package) == 'skip':
            event.package_status[package] = 'no_tests'
    event.counts['ordinary_skip'] = len(event.skips)
    return event


def parse_verbose(log: Path, name: str, source_commit: str, exit_code: int) -> GateEvent:
    event = _event(log,name,source_commit,exit_code)
    # Go verbose test events precede their package summary. Bind them only there.
    pending = []
    run = re.compile(r'^\s*=== RUN\s+(\S+)\s*$')
    terminal = re.compile(r'^\s*--- (PASS|FAIL|SKIP): (\S+)(?:\s|$)')
    summary = re.compile(r'^(ok|FAIL|\?)\s+(\S+)\s*(.*)$')
    try:
        lines = Path(log).read_text().splitlines()
    except (OSError,UnicodeError):
        event.failures.append('unreadable_log'); return event
    for line in lines:
        if 'WARNING: DATA RACE' in line: event.failures.append('race_report')
        m=run.match(line)
        if m: pending.append(('run',m.group(1))); continue
        m=terminal.match(line)
        if m: pending.append((m.group(1).lower(),m.group(2))); continue
        m=summary.match(line)
        if m:
            status, package, detail=m.groups()
            for action,test in pending:
                if action=='run': event.started.add(package+'::'+test)
                else: _terminal(event,package,test,action)
            pending=[]
            _terminal(event,package,'', 'pass' if status=='ok' else 'fail' if status=='FAIL' else 'skip')
            if status=='?' and '[no test files]' in detail:
                event.package_status[package]='no_tests'
    if pending: event.failures.append('missing_verbose_package_summary')
    event.counts['ordinary_skip']=len(event.skips)
    return event


def parse_unittest(log: Path,name: str,source_commit: str,exit_code: int) -> GateEvent:
    event=_event(log,name,source_commit,exit_code)
    pattern=re.compile(r'^(\S+) \(([^)]+)\) \.\.\. (ok|FAIL|ERROR|skipped\b.*)$')
    total=None
    summary=None
    try: lines=Path(log).read_text().splitlines()
    except (OSError,UnicodeError):
        event.failures.append('unreadable_log'); return event
    for line in lines:
        m=pattern.match(line)
        if m:
            test,case,status=m.groups()
            identity=case+'.'+test
            event.started.add('unittest::'+identity)
            _terminal(event,'unittest',identity,
                      'pass' if status=='ok' else 'skip' if status.startswith('skipped') else 'fail')
        m=re.match(r'^Ran (\d+) tests? in ',line)
        if m:
            if total is not None: event.failures.append('duplicate_unittest_total')
            total=int(m.group(1))
        if re.match(r'^(OK(?: \(.*\))?|FAILED \(.*\))$',line):
            if summary is not None: event.failures.append('duplicate_unittest_summary')
            summary=line
    if total is None or total!=len(event.executed) or total==0:
        event.failures.append('incomplete_unittest_total')
    if summary is None:
        event.failures.append('missing_unittest_summary')
    else:
        _terminal(event,'unittest','', 'pass' if summary=='OK' else 'fail')
    event.counts['ordinary_skip']=len(event.skips)
    return event


def _helper_valid(event):
    if event.name not in {'full_repository','race_repository'} or CALLER not in event.passed:
        return False
    proofs=event.helper_proofs
    if len(proofs)!=2: return False
    ids=set(); pids=set()
    for p in proofs:
        if not isinstance(p,dict): return False
        if (p.get('gate')!=event.name or p.get('test')!=CALLER or
            p.get('source_commit')!=event.source_commit or
            any(p.get(k) is not True for k in ('registered','ready','exited')) or
            not isinstance(p.get('pid'),int) or isinstance(p.get('pid'),bool) or p['pid']<=0 or
            not isinstance(p.get('uid'),int) or p['uid']<0 or
            not p.get('start_time') or not p.get('identity') or
            not re.fullmatch('[a-f0-9]{32}', str(p.get('owner',''))) or
            not re.fullmatch('[a-f0-9]{64}', str(p.get('executable_sha256',''))) or
            not p.get('expected_exit') or p.get('expected_exit')!=p.get('actual_exit')):
            return False
        ids.add(p['identity']); pids.add(p['pid'])
    return len(ids)==len(pids)==2


def validate_gate(event: GateEvent, inventory: Inventory,
                  allowed_helper: Optional[str]) -> List[str]:
    errors=list(event.failures)
    if event.exit_code!=0: errors.append('command_exit_nonzero')
    if event.kind=='check':
        if not event.checks: errors.append('missing_checks')
        return errors
    if event.kind!='test': return errors+['unknown_gate_kind']
    if not any('/' not in name.split('::',1)[-1] for name in event.passed):
        errors.append('zero_named_top_pass')
    helper = (allowed_helper==HELPER and HELPER in event.skips and _helper_valid(event))
    exempt={HELPER} if helper else set()
    ordinary=set(event.skips)-exempt
    event.counts['helper_skip']=len(exempt)
    event.counts['ordinary_skip']=len(ordinary)
    if ordinary: errors.extend('ordinary_skip:'+name for name in sorted(ordinary))
    if event.started-event.executed:
        errors.append('missing_test_terminal')
    if event.executed-event.started:
        errors.append('test_terminal_without_start')
    required=set(inventory.required_subtests)
    for package in inventory.packages:
        if package not in inventory.tests:
            errors.append('missing_inventory_tests:'+package); continue
        required.update(package+'::'+test for test in inventory.tests[package])
        state=event.package_status.get(package)
        if state!='pass' and not (state=='no_tests' and not inventory.tests[package]):
            errors.append('missing_successful_package_terminal:'+package)
    if not inventory.packages: errors.append('empty_inventory')
    if set(event.package_status)-inventory.packages:
        errors.append('unexpected_package')
    for name in sorted(required-event.passed-exempt):
        errors.append('missing_required_pass:'+name)
    return errors


def evaluate(events: List[GateEvent], cleanup: CleanupResult, commit: str) -> Verdict:
    errors=[]; accepted={}; seen=set()
    if not re.fullmatch('[a-f0-9]{40}',commit): errors.append('invalid_source_commit')
    for event in events:
        if event.name in seen:
            errors.append('duplicate_gate:'+event.name)
            accepted[event.name]=False; continue
        seen.add(event.name)
        problems=[]
        if event.name not in REQUIRED_GATES: problems.append('unknown_gate')
        if event.source_commit!=commit: problems.append('different_source')
        expected='check' if event.name in CHECK_GATES else 'test'
        if event.kind!=expected: problems.append('unexpected_gate_kind')
        if event.kind=='test' and event.inventory is None:
            problems.append('missing_discovered_inventory')
        else:
            problems.extend(validate_gate(event,event.inventory or Inventory(set(),{},set()),
                HELPER if event.name in {'full_repository','race_repository'} else None))
        accepted[event.name]=not problems
        errors.extend(event.name+':'+problem for problem in problems)
    errors.extend('missing_gate:'+name for name in REQUIRED_GATES if name not in seen)
    if cleanup.removed is not True or cleanup.failures:
        errors.extend(['cleanup_not_confirmed']+list(cleanup.failures))
    return Verdict(not errors and all(accepted.get(n,False) for n in REQUIRED_GATES),
                   accepted.get('full_repository',False),
                   accepted.get('race_repository',False), errors)
