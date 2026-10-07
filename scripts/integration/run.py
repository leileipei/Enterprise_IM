"""Run one immutable local attempt, with cleanup overriding every green gate."""
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import sys
import time
import urllib.parse
from .model import Toolchain,FixtureBundle,GateEvent,Inventory,CleanupResult,ResourceRef,Verdict
from .source import verify_snapshot,collect_inventory
from .tools import discover_toolchain,python_cli,RUNTIME
from .registry import Registry
from .environment import test_environment
from .services import prepare_services,probe_services
from .iam import prepare_iam
from .scanner import prepare_scanner,probe_scanner,probe_browser
from .gates import gate_specs,run_stage
from .results import validate_gate,HELPER,CALLER
from .evidence import write_evidence,digest


TOTAL_SECONDS=21600
GATE_SECONDS=3600
CLEANUP_SECONDS=60


class Cancelled(KeyboardInterrupt):pass


def _check(name,snapshot,root,checks=(),failures=()):
    path=root/(name+'-'+secrets.token_hex(4)+'.json')
    fd=os.open(path,os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
    with os.fdopen(fd,'w') as f:json.dump({'checks':list(checks),'failures':list(failures)},f);f.write('\n')
    return GateEvent(name,'check',snapshot.commit,int(bool(failures)),path,checks=list(checks),failures=list(failures))


def _helper_proofs(registry,gate):
    rows=registry.records();proofs=[]
    for row in rows:
        detail=row.get('detail',{});fingerprint=row.get('fingerprint',{})
        # Both API children execute the actual compiled policystore test binary; the worker is distinct.
        if row['event']!='registered' or row['kind']!='process' or detail.get('gate')!=gate or detail.get('test')!='TestMultiProcessRealtimeWorkerFanoutAndReconnect' or Path(fingerprint.get('executable_path','')).name!='policystore.test':continue
        lifecycle=[r for r in rows if r['identity']==row['identity'] and r['owner']==row['owner'] and r['source_commit']==registry.commit and r.get('detail',{}).get('gate')==gate and r.get('detail',{}).get('test')==detail['test']]
        ready=[r for r in lifecycle if r['event']=='ready'];exited=[r for r in lifecycle if r['event']=='exited']
        exit_detail=exited[-1]['detail'] if exited else {}
        proofs.append(dict(gate=gate,test=CALLER,source_commit=registry.commit,identity=row['identity'],owner=row['owner'],
            pid=fingerprint.get('pid'),uid=fingerprint.get('uid'),start_time=fingerprint.get('start_time'),executable_sha256=fingerprint.get('executable_sha256'),
            registered=True,ready=len(ready)==1,exited=len(exited)==1,expected_exit=exit_detail.get('expected_exit'),actual_exit=exit_detail.get('actual_exit')))
    return proofs


def execute_bootstrap(data,output):
    sys.dont_write_bytecode=True
    snapshot=verify_snapshot(data)
    return execute(snapshot,output)


def execute(snapshot,output):
    output=Path(output).resolve();owner=secrets.token_hex(16);registry=Registry(output/'registry.jsonl',owner,snapshot.commit)
    private=registry.root/owner;events=[];inventory=Inventory(set(),{},set());bundle=FixtureBundle({},set(),registry.path,{owner},{'source_commit':snapshot.commit})
    # Contract tests precede the actual toolchain gate. These paths cannot count as verified tools.
    tools=Toolchain({'python':python_cli(),'node':RUNTIME/'bin/node','node_modules':RUNTIME/'node_modules',
                    'chrome':Path('/Applications/Google Chrome.app/Contents/MacOS/Google Chrome')},{},{},{},'darwin','arm64')
    cleanup=CleanupResult(False,['not_finished']);total_deadline=time.monotonic()+TOTAL_SECONDS;cancelled=False
    handlers={sig:signal.getsignal(sig) for sig in (signal.SIGINT,signal.SIGTERM,signal.SIGALRM)}
    prior_timer=signal.getitimer(signal.ITIMER_REAL)
    def cancel(signum,frame):raise Cancelled()
    for sig in (signal.SIGINT,signal.SIGTERM):signal.signal(sig,cancel)
    def expire(signum,frame):raise ValueError('attempt_deadline')
    signal.signal(signal.SIGALRM,expire)
    signal.setitimer(signal.ITIMER_REAL,max(.01,TOTAL_SECONDS-CLEANUP_SECONDS))
    def deadline():
        if time.monotonic()>=total_deadline-CLEANUP_SECONDS:raise ValueError('attempt_deadline')
        return min(total_deadline-CLEANUP_SECONDS,time.monotonic()+GATE_SECONDS)
    active='orchestrator_contract'
    try:
        started=time.time();print(active,'START',flush=True)
        first=next(s for s in gate_specs(snapshot,inventory) if s['name']=='orchestrator_contract')
        event=run_stage(first,snapshot,tools,bundle,registry,deadline());event.started_at=started;event.ended_at=time.time();event.seconds=round(event.ended_at-started,3);event.argv_template=first.get('argv',[]);events.append(event)
        if validate_gate(event,event.inventory or inventory,None):raise ValueError('orchestrator_contract_failed')
        active='toolchain';started=time.time();print(active,'START',flush=True)
        tools=discover_toolchain(snapshot,private/'tools')
        # Retain only the approved nonsecret executable, not its private build caches.
        if 'mc' in tools.paths:
            retained=output/'tools';retained.mkdir(mode=0o700,exist_ok=True)
            shutil.copyfile(tools.paths['mc'],retained/'mc');(retained/'mc').chmod(0o700)
            if digest(retained/'mc')!=tools.hashes['mc']:raise ValueError('retained_mc_hash_mismatch')
            tools.paths['mc']=retained/'mc'
        tool_event=_check('toolchain',snapshot,private,['locked_versions','binary_hashes','local_docker','immutable_images','native_platform']);tool_event.started_at=started;tool_event.ended_at=time.time();tool_event.seconds=round(tool_event.ended_at-started,3);events.append(tool_event)
        active='fixture_preflight';started=time.time();print(active,'START',flush=True)
        bundle=prepare_services(snapshot,tools,registry,private/'services',deadline());bundle=prepare_iam(bundle,tools,private/'iam',deadline());bundle=prepare_scanner(bundle,tools,registry,private/'scanner',deadline())
        complete_profile(bundle,private)
        service=probe_services(bundle,tools,deadline());scanner=probe_scanner(bundle,tools,deadline());browser=probe_browser(bundle,tools,registry,private/'browser',deadline())
        env=test_environment(tools,private/'env',bundle.environment)
        from .commands import run_command
        from .results import parse_go
        iam_log=private/'iam-proof.jsonl'
        outer=run_command('fixture_preflight','test',snapshot.commit,[str(tools.paths['go']),'test','-json','-p','1','-count=1','./internal/testfixtures','-run','^TestIntegrationFixtureIAM$'],snapshot.root,env,iam_log,deadline(),registry)
        iam_event=parse_go(iam_log,'fixture_preflight',snapshot.commit,outer.exit_code)
        expected='github.com/leileipei/Enterprise_IM/internal/testfixtures::TestIntegrationFixtureIAM'
        failed=service.failures+scanner.failures+browser.failures+iam_event.failures+outer.failures
        if any(e.exit_code for e in [service,scanner,browser,outer]) or expected not in iam_event.passed or iam_event.skips:failed.append('fixture_actual_checks_failed')
        preflight=_check('fixture_preflight',snapshot,private,service.checks+scanner.checks+browser.checks+(['actual_iam_permission_matrix'] if not failed else []),failed);events.append(preflight)
        # Preserve each underlying fixture proof as evidence, without adding duplicate gates.
        preflight.started_at=started;preflight.ended_at=time.time();preflight.seconds=round(preflight.ended_at-started,3)
        preflight.related_logs=[service.log,scanner.log,browser.log,iam_log,Path(str(iam_log)+'.stderr')]
        if failed:raise ValueError('fixture_preflight_failed')
        inventory=collect_inventory(snapshot,tools,env,None)
        for spec in gate_specs(snapshot,inventory):
            if spec['name']=='orchestrator_contract':continue
            if time.monotonic()>=total_deadline-CLEANUP_SECONDS:raise ValueError('attempt_deadline')
            # Recheck signed freshness/live binding before every business stage.
            probe=probe_scanner(bundle,tools,deadline())
            if probe.failures or probe.exit_code:raise ValueError('scanner_revalidation_failed')
            active=spec['name'];started=time.time();print(active,'START',flush=True)
            try:event=run_stage(spec,snapshot,tools,bundle,registry,deadline())
            except Exception as exc:
                code=str(exc) if isinstance(exc,ValueError) else 'stage_exception:'+type(exc).__name__
                event=GateEvent(spec['name'],spec['kind'],snapshot.commit,1,private/(spec['name']+'-failed.json'),failures=[code])
                # The adapter can fail after its real command has already written evidence.
                # Preserve those streams before the owned directory is removed.
                stage_root=private/'gates'/spec['env_group']
                event.related_logs=[p for p in stage_root.rglob('*') if p.is_file() and not any(q.is_symlink() for q in (p,*p.parents)) and (p.suffix in {'.jsonl','.log','.stderr','.txt'} or p.name in {'assembly.json','components.json'})] if stage_root.is_dir() else []
            if not Path(event.log).exists():
                event.log=_check(spec['name']+'-error',snapshot,private,failures=event.failures).log
            event.argv_template=getattr(event,'argv_template',[str(x) for x in spec['argv']]);event.started_at=started;event.ended_at=time.time();event.seconds=round(event.ended_at-started,3)
            if spec['name'] in {'full_repository','race_repository'}:event.helper_proofs=_helper_proofs(registry,spec['name'])
            events.append(event)
            errors=validate_gate(event,event.inventory or inventory,HELPER if spec['name'] in {'full_repository','race_repository'} else None)
            print(spec['name'],'PASS' if not errors else 'FAIL',event.counts,flush=True)
        # The export must still match the archive after all tests.
        import importlib.util
        entry=snapshot.root/'scripts/test-project-integration.py';loader=importlib.util.spec_from_file_location('_final_snapshot_check',entry);module=importlib.util.module_from_spec(loader);loader.loader.exec_module(module)
        module.validate_export(dict(root=str(snapshot.root),archive=str(snapshot.archive),archive_sha256=snapshot.archive_sha256))
        events.append(_check('evidence_integrity',snapshot,private,['archive_export_unchanged','same_source_complete_gate_evidence']))
    except KeyboardInterrupt:
        cancelled=True
        events.append(_check('evidence_integrity',snapshot,private,failures=['cancelled']))
    except Exception as exc:
        code=str(exc) if isinstance(exc,ValueError) else 'attempt_exception:'+type(exc).__name__
        if active in {'toolchain','fixture_preflight'} and not any(e.name==active for e in events):events.append(_check(active,snapshot,private,failures=[code]))
        events.append(_check('evidence_integrity',snapshot,private,failures=[code]))
    finally:
        # Disable subsequent cancellation only during bounded cleanup; no new gates are started.
        signal.setitimer(signal.ITIMER_REAL,0)
        for sig in handlers:signal.signal(sig,signal.SIG_IGN)
        try:
            verdict,cleanup=_finalize(snapshot,output,tools,bundle,registry,events,inventory,total_deadline)
        finally:
            for sig,handler in handlers.items():signal.signal(sig,handler)
            signal.setitimer(signal.ITIMER_REAL,*prior_timer)
    if cancelled and 'cancelled' not in verdict.failures:
        # Preserve a clear cancellation marker in addition to the gate-qualified failure.
        data=json.loads((output/'verification.json').read_text());data['failures'].append('cancelled')
        from .evidence import _json
        _json(output/'verification.json',data)
    print('required_gates_passed',verdict.required_gates_passed,'full_suite_passed',verdict.full_suite_passed,'race_suite_passed',verdict.race_suite_passed,'cleanup',cleanup.removed,flush=True)
    return 0 if verdict.required_gates_passed else 1


def _finalize(snapshot,output,tools,bundle,registry,events,inventory,total_deadline):
    private=registry.root/registry.owner;publication_failed=False
    try:
        snapshot.resource_records=registry.records()
        snapshot.binary_hashes=[dict(path=r['fingerprint'].get('executable_path'),sha256=r['fingerprint'].get('executable_sha256'),source_commit=r['source_commit']) for r in snapshot.resource_records if r['event']=='registered' and r['kind']=='process']
        write_evidence(output,snapshot,tools,events,CleanupResult(False,['cleanup_pending']),inventory,bundle.secrets)
    except (ValueError,OSError,KeyError,UnicodeError):
        publication_failed=True
    # Publication is optional to cleanup: even a full disk must not leave live children.
    cleanup_deadline=min(total_deadline,time.monotonic()+CLEANUP_SECONDS)
    cleanup=registry.cleanup(cleanup_deadline)
    for runtime in bundle.metadata.get('scanner_runtimes',[]):
        try:runtime['process'].wait(timeout=2)
        except Exception:cleanup=CleanupResult(False,cleanup.failures+['scanner_not_reaped'])
    if cleanup.removed and private.exists():
        stat=private.stat();registry.add(ResourceRef('directory',registry.owner,str(private),{'device':stat.st_dev,'inode':stat.st_ino}))
        cleanup=registry.cleanup(cleanup_deadline)
    if private.exists():cleanup=CleanupResult(False,cleanup.failures+['private_directory_retained'])
    if publication_failed:
        existing=next((e for e in events if e.name=='evidence_integrity'),None)
        if existing:existing.failures.append('evidence_publication_failed')
        else:events.append(_check('evidence_integrity',snapshot,output,failures=['evidence_publication_failed']))
    try:
        events.append(_check('resource_cleanup',snapshot,output,['owned_resources_absent','private_directory_removed'] if cleanup.removed else [],cleanup.failures))
        verdict=write_evidence(output,snapshot,tools,events,cleanup,inventory,bundle.secrets)
    except (ValueError,OSError,KeyError,UnicodeError):
        verdict=Verdict(False,False,False,['evidence_finalization_failed'])
        from .evidence import _json
        try:_json(output/'verification.json',dict(schema_version='project_integration_v1',source_commit=snapshot.commit,required_gates_passed=False,full_suite_passed=False,race_suite_passed=False,cleanup=dict(removed=cleanup.removed,failures=cleanup.failures),failures=verdict.failures,customer_acceptance='not_executed',production_acceptance='not_executed'))
        except OSError:pass  # Cleanup already ran; a full disk cannot store a receipt.
    return verdict,cleanup


def complete_profile(bundle,private):
    private=Path(private);bins=private/'bin';bins.mkdir(mode=0o700,parents=True,exist_ok=True)
    parts=urllib.parse.urlsplit(bundle.environment['IM_TEST_DATABASE_URL'])
    password=bundle.metadata['role_passwords']['p431_import_writer']
    ordinary=parts._replace(netloc='p431_import_writer:'+urllib.parse.quote(password,safe='')+'@'+parts.netloc.rsplit('@',1)[1]).geturl()
    admin_key,admin_secret=bundle.metadata['minio_admin']
    bundle.environment.update(IM_IMPORT_APPLY_TEST_ADMIN_URL=bundle.environment['IM_TEST_DATABASE_URL'],IM_IMPORT_APPLY_TEST_DATABASE_URL=ordinary,
        IM_COMPARE_TEST_CA=bundle.metadata['tls_ca'],IM_COMPARE_TEST_WRONG_CA=bundle.metadata['tls_wrong_ca'],
        IM_COMPARE_TEST_BINARY=str(bins/'im-import-compare'),IM_PREFLIGHT_TEST_BINARY=str(bins/'im-import-preflight'),
        IM_TEST_S3_ADMIN_ACCESS_KEY=admin_key,IM_TEST_S3_ADMIN_SECRET_KEY=admin_secret)
    bundle.secrets.update((ordinary,bundle.environment['IM_TEST_DATABASE_URL'],admin_key,admin_secret))
