"""Adapt the existing mandatory gates without reducing their test selection."""
import hashlib
import importlib.util
import json
from pathlib import Path
import re
from .model import GateEvent,Inventory
from .results import parse_go,parse_verbose,validate_gate
from .commands import run_command
from .environment import test_environment
from .source import collect_inventory,collect_python_inventory
from .services import docker_environment

PREFIX='github.com/leileipei/Enterprise_IM/'
FILE_MESSAGES={'TestFileMessageRealScanSendPull','TestFileMessageRealRealtime','TestFileMessageRealBrowserLegacy','TestFileMessageProductionClosed','TestFileMessageProductionClosedConfiguration','TestFileMessageRetiredHTTP','TestFileScanRealTrustedRuntime','TestFileScanRealUnprovenRuntime'}
DOWNLOAD={'TestFileDownloadRealOIDCScan','TestFileDownloadRealTokenExpiryBlockedWrite','TestFileDownloadRealRevocation','TestFileDownloadRealAuditRepair','TestFileDownloadRealBrowserLegacy','TestFileDeleteRealVersionsIAM','TestFileDeleteRealUnknownDelete','TestFileDeleteRealHoldOrdering','TestFileDeleteRealOrphanQuarantine','TestFileDownloadProductionClosed','TestFileDownloadRealTCPRevocation','TestFileDownloadRealTotalDeadline','TestFileDeleteRealMarker403','TestFileCleanerDefaultClosed','TestFileScanRealTrustedRuntime','TestFileScanRealUnprovenRuntime'}
WEB={'TestWebFileReal'+x for x in ['Lifecycle','RejectedScan','Settings','ProductionClosed','Search','SearchFinalBoundary','ContextIsolation','UnknownUploadSend','DownloadFaults','Revocation','PolicyConflict']}
BUSINESS={f'TestFileBusinessProcessRP{i:02d}' for i in range(1,15)}|{'TestFileBusinessProcessRP08/slow_client_SIGTERM'}
REALTIME={'TestTwoDeviceRealtimeFromCommittedMessageThroughRedisAndReconnect','TestMultiProcessRealtimeWorkerFanoutAndReconnect','TestProductionAPIWithOIDCAndRealtimeProcesses','TestRealBrowserLoginRealtimeAndOfflinePull'}
RESOURCES={'TestScannerRealResourceBoundary','TestFileTransferRealDiskFull'}
COMPONENTS='TestFile|TestS3|TestClamd|TestScanner|TestUploadPolicy'


def _legacy(root):
    path=root/'scripts/test-import-apply.py'
    spec=importlib.util.spec_from_file_location('_import_contract',path)
    module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
    return module


def gate_specs(snapshot,inventory):
    root=snapshot.root;legacy=_legacy(root)
    def spec(name,argv,required=(),kind='test',group=None,cwd=None):
        return dict(name=name,kind=kind,argv=argv,cwd=cwd or root,env_group=group or name,required=set(required))
    rows=[spec('orchestrator_contract',['python','-m','unittest','discover','-s','scripts/tests','-p','test_project_integration_*.py','-v']),
          spec('build_all',['go','build','./...'],kind='check'),spec('vet_all',['go','vet','./...'],kind='check')]
    realtime=legacy.ACK+'|^'+'$|^'.join(sorted(REALTIME))+'$'
    rows.append(spec('message_realtime',['go','test','-json','-p','1','-timeout=30m','-count=1','./internal/access','./internal/oidcauth','./internal/policystore','-run',realtime],REALTIME))
    # access and OIDC must run fully; run_stage separates their command from selected policystore.
    for name,script,required,group in [
        ('file_components','test-file-runtime.sh',(),'file_runtime'),
        ('file_resources',None,RESOURCES,'file_runtime'),
        ('file_messages','test-file-messages.sh',FILE_MESSAGES,None),
        ('file_download_retention','test-file-download-retention.sh',DOWNLOAD,None),
        ('web_files','test-web-files.sh',WEB,None),
        ('file_business_process','test-file-business-runtime.sh',BUSINESS,None)]:
        rows.append(spec(name,['bash',str(root/'scripts'/script),'run-all'] if script else ['resource-evidence'],required,group=group))
    rows.append(spec('import_append',['python',str(root/'scripts/test-import-apply.py'),'--source-commit',snapshot.commit,'--required-only'],legacy.REQUIRED_GATES,cwd=snapshot.repository_root))
    rows += [spec('full_repository',['go','test','-json','-p','1','-timeout=30m','-count=1','./...']),
             spec('race_repository',['go','test','-race','-json','-p','1','-timeout=30m','-count=1','./...'])]
    return rows


def _private_path(root,relative):
    if not isinstance(relative,str) or not relative or Path(relative).is_absolute() or '..' in Path(relative).parts:
        raise ValueError('invalid_child_evidence_path')
    path=root/relative
    if any(p.is_symlink() for p in (path,*path.parents)) or not path.is_file():raise ValueError('missing_child_evidence')
    return path


def _digest(path):return hashlib.sha256(path.read_bytes()).hexdigest()


def validate_import_child(report,source_commit):
    event=GateEvent('import_append','test',source_commit,1,Path(report))
    try:
        report=Path(report);root=report.parent;data=json.loads(report.read_text());legacy=_legacy(Path(__file__).resolve().parents[2])
        if (data.get('source_commit')!=source_commit or not re.fullmatch('[a-f0-9]{40}',source_commit) or
            data.get('full_suite_status')!='not_executed' or data.get('full_suite_passed') is not False or
            data.get('full_suite_source_commit')!='' or data.get('full_suite_reused') or
            any(r.get('name')=='full_suite' for r in data['events'])):raise ValueError('child_source_or_full_mode_mismatch')
        if _digest(_private_path(root,'source.tar'))!=data.get('source_archive_sha256'):raise ValueError('child_archive_hash_mismatch')
        if data.get('cleanup',{}).get('removed') is not True or data.get('required_gates_passed') is not True:raise ValueError('child_not_passed_or_cleaned')
        verdict=legacy.validate_required_gates(data['events'])
        if not verdict['required_gates_passed']:raise ValueError('child_required_gate_failed')
        groups={r['name']:r for r in data['events']}
        for name in legacy.REQUIRED_GATES:
            row=groups[name];path=_private_path(root,row['log'])
            if _digest(path)!=row['log_sha256']:raise ValueError('child_log_hash_mismatch')
            # Child fields are claims; the original logs must independently establish them.
            parsed=(parse_verbose if row.get('parse')=='verbose' else parse_go)(path,name,source_commit,row['exit_code'])
            if parsed.failures or parsed.skips or not parsed.passed:raise ValueError('child_raw_log_failed')
            names={n.split('::',1)[-1] for n in parsed.passed}
            if not set(legacy.REQUIRED_TESTS[name])<=names:raise ValueError('child_required_name_missing')
            if set(row['executed_tests'])!=names:raise ValueError('child_log_claim_mismatch')
            if parsed.started!=parsed.executed:raise ValueError('child_incomplete_test_terminals')
            if any(s!='pass' for s in parsed.package_status.values()) or not parsed.package_status:raise ValueError('child_missing_package_terminal')
            event.passed.update(parsed.passed);event.executed.update(parsed.executed);event.started.update(parsed.started)
            event.package_status.update(parsed.package_status)
            for key,value in parsed.counts.items():event.counts[key]=event.counts.get(key,0)+value
            if row.get('stderr'):
                if _digest(_private_path(root,row['stderr']))!=row.get('stderr_sha256'):raise ValueError('child_stderr_hash_mismatch')
        if not isinstance(data.get('bin_sha256'),dict) or not data['bin_sha256']:raise ValueError('missing_child_binary_hashes')
        for name,value in data['bin_sha256'].items():
            if _digest(_private_path(root,'bin/'+name))!=value:raise ValueError('child_binary_hash_mismatch')
        # Required child tests are a separate contract; the parent full/race inventory is independent.
        packages=set(event.package_status)
        event.inventory=Inventory(packages,{p:set() for p in packages},set(event.passed))
        event.exit_code=0;event.checks=['fourteen_required_gates','same_source','archive_logs_binaries_hashed','child_owned_cleanup','full_suite_not_executed']
    except (ValueError,KeyError,TypeError,OSError,AttributeError) as exc:
        event.failures.append(str(exc) if isinstance(exc,ValueError) else 'invalid_child_report')
    return event


def parse_resource_logs(root,source_commit,exit_code):
    root=Path(root);event=GateEvent('file_resources','test',source_commit,exit_code,root/'resource.log')
    tests={};required=set()
    for log,list_log,package,mandatory in [
        ('resource.log','resource-list.log','internal/filescanner','TestScannerRealResourceBoundary'),
        ('disk-full.log','disk-full-list.log','internal/filetransfer','TestFileTransferRealDiskFull')]:
        pkg=PREFIX+package
        try:
            listed=_private_path(root,list_log).read_text().splitlines()
            names={s for s in listed if re.fullmatch('Test[A-Za-z0-9_]+',s)}
            if mandatory not in names:raise ValueError('resource_list_missing_required_name')
            proof=json.loads(_private_path(root,log+'.proof.json').read_text())
            if proof.get('source_commit')!=source_commit or proof.get('exit_code')!=0 or proof.get('cleanup') is not True or proof.get('memory')!=536870912 or proof.get('nano_cpus')!=1000000000:
                raise ValueError('resource_limits_unproven')
            if log=='disk-full.log' and proof.get('tmpfs',{}).get('/limited')!='size=1048576,mode=0700':raise ValueError('disk_full_limit_unproven')
            raw=_private_path(root,log);text=raw.read_text()
            # Standalone Go test binaries emit PASS, not a package summary.
            if not re.search(r'^PASS\s*$',text,re.M):raise ValueError('resource_binary_not_passed')
            normalized=root/(log+'.normalized');normalized.write_text(text+'\nok '+pkg+'\n');normalized.chmod(0o600)
            parsed=parse_verbose(normalized,'file_resources',source_commit,exit_code)
            event.failures.extend(parsed.failures);event.skips.extend(parsed.skips)
            for attr in ['passed','started','executed']:getattr(event,attr).update(getattr(parsed,attr))
            event.package_status.update(parsed.package_status)
            for key,value in parsed.counts.items():event.counts[key]=event.counts.get(key,0)+value
            tests[pkg]=names;required.add(pkg+'::'+mandatory)
        except (ValueError,KeyError,OSError,TypeError) as exc:event.failures.append(str(exc) if isinstance(exc,ValueError) else 'missing_resource_evidence')
    event.inventory=Inventory(set(tests),tests,required)
    event.failures.extend(validate_gate(event,event.inventory,None));return event


def _selection(spec,inventory):
    name=spec['name']
    if name in {'full_repository','race_repository'}:return None
    if name=='message_realtime':
        return {PREFIX+'internal/access':'.',PREFIX+'internal/oidcauth':'.',PREFIX+'internal/policystore':spec['argv'][-1]}
    if name=='file_components':return {PREFIX+p:COMPONENTS if p.startswith('internal/') else 'TestFileAPIAssembly|TestFileWorkerConfig' for p in ['cmd/im-api','cmd/im-file-worker','internal/files','internal/objectstore','internal/filescanner','internal/filetransfer','internal/httpserver','internal/policystore']}
    patterns={'file_messages':(['internal/policystore','cmd/im-api'],'^TestFileMessage(Real|Production|RetiredHTTP)|^TestFileScanReal(Trusted|Unproven)Runtime$'),
              'file_download_retention':(['internal/policystore','cmd/im-api','cmd/im-file-cleaner'],'^TestFileDownload(Real|Production)|^TestFileDeleteReal|^TestFileCleanerDefaultClosed$|^TestFileScanReal(Trusted|Unproven)Runtime$'),
              'web_files':(['internal/policystore'],'^TestWebFileReal(Lifecycle|RejectedScan|Settings|ProductionClosed|Search|SearchFinalBoundary|ContextIsolation|UnknownUploadSend|DownloadFaults|Revocation|PolicyConflict)$'),
              'file_business_process':(['internal/policystore'],'^TestFileBusinessProcessRP(0[1-9]|1[0-4])$')}
    pkgs,regex=patterns[name];return {PREFIX+p:regex for p in pkgs}


def run_stage(spec,snapshot,tools,bundle,registry,deadline):
    if snapshot.commit!=registry.commit or bundle.metadata.get('source_commit')!=snapshot.commit:raise ValueError('gate_source_mismatch')
    name=spec['name'];private=registry.root/registry.owner/'gates';private.mkdir(mode=0o700,parents=True,exist_ok=True)
    root=private/spec['env_group'];root.mkdir(mode=0o700,exist_ok=True)
    if name=='file_resources':
        event=parse_resource_logs(root,snapshot.commit,bundle.metadata.get('file_runtime_exit',125));return event
    values=dict(bundle.environment)
    values.update(IM_TEST_INTEGRATION_REGISTRY=str(registry.path),IM_TEST_INTEGRATION_OWNER=registry.owner,
                  IM_TEST_INTEGRATION_SOURCE_SHA=snapshot.commit,IM_TEST_INTEGRATION_GATE=name)
    values.update(IM_TEST_BROWSER_NODE=str(tools.paths['node']),CHROMIUM_EXECUTABLE=str(tools.paths['chrome']))
    if 'IM_TEST_FILE_UPLOAD_ACCESS_KEY' in values:
        values['IM_FILE_S3_ACCESS_KEY']=values['IM_TEST_FILE_UPLOAD_ACCESS_KEY'];values['IM_FILE_S3_SECRET_KEY']=values['IM_TEST_FILE_UPLOAD_SECRET_KEY']
    output_keys={'file_components':'IM_TEST_FILE_RUNTIME_OUTPUT_DIR','file_messages':'IM_TEST_FILE_MESSAGE_OUTPUT_DIR','file_download_retention':'IM_TEST_FILE_DOWNLOAD_OUTPUT_DIR','web_files':'IM_TEST_WEB_FILE_OUTPUT_DIR','file_business_process':'IM_TEST_FILE_BUSINESS_OUTPUT_DIR'}
    if name in output_keys:values[output_keys[name]]=str(root)
    values['IM_TEST_FILE_BUSINESS_BUILD_SHA']=snapshot.commit
    if 'IM_TEST_FILE_BUSINESS_OUTPUT_DIR' not in values:
        business_root=root/'business-fixtures';business_root.mkdir(mode=0o700,exist_ok=True)
        values['IM_TEST_FILE_BUSINESS_OUTPUT_DIR']=str(business_root)
    env=test_environment(tools,registry.root/registry.owner/'env',values);env.update(docker_environment())
    # docker_environment PATH is for Docker only; restore the locked tool search path.
    env['PATH']=test_environment(tools,registry.root/registry.owner/'env',values)['PATH']
    argv=[str(tools.paths[a]) if a in tools.paths else '/bin/bash' if a=='bash' else a for a in spec['argv']]
    log=root/(name+'.command.log')
    if name=='import_append':
        child=registry.root/registry.owner/'import-child'
        argv+=['--repository-root',str(snapshot.repository_root),'--output-dir',str(child),'--resource-registry',str(registry.path),'--resource-owner',registry.owner]
        outer=run_command(name,'test',snapshot.commit,argv,registry.root/registry.owner,env,log,deadline,registry)
        event=validate_import_child(child/'verification.json',snapshot.commit);event.failures.extend(outer.failures)
        if (child/'source.tar').is_file() and _digest(child/'source.tar')!=snapshot.archive_sha256:event.failures.append('child_archive_differs_from_parent')
        if outer.exit_code:event.exit_code=outer.exit_code
        event.argv_template=argv
        if (child/'verification.json').is_file():
            child_data=json.loads((child/'verification.json').read_text())
            event.related_logs=[child/r[k] for r in child_data.get('events',[]) for k in ['log','stderr'] if r.get(k)]
        event.related_logs=list(getattr(event,'related_logs',[]))+[outer.log,Path(str(outer.log)+'.stderr')]
        return event
    python_inventory=collect_python_inventory(snapshot,tools,env) if name=='orchestrator_contract' else None
    if name=='message_realtime':
        first=[str(tools.paths['go']),'test','-json','-p','1','-timeout=30m','-count=1','./internal/access','./internal/oidcauth']
        a=run_command(name,'test',snapshot.commit,first,snapshot.root,env,root/'auth.jsonl',deadline,registry)
        b=run_command(name,'test',snapshot.commit,[str(tools.paths['go']),'test','-json','-p','1','-timeout=30m','-count=1','./internal/policystore','-run',spec['argv'][-1]],snapshot.root,env,root/'realtime.jsonl',deadline,registry)
        log=root/'message_realtime.jsonl';log.write_bytes(a.log.read_bytes()+b.log.read_bytes());log.chmod(0o600)
        outer=GateEvent(name,'test',snapshot.commit,a.exit_code or b.exit_code,log,failures=a.failures+b.failures)
        outer.argv_template=[first,[str(tools.paths['go']),'test','-json','-p','1','-timeout=30m','-count=1','./internal/policystore','-run',spec['argv'][-1]]]
    else:outer=run_command(name,spec['kind'],snapshot.commit,argv,spec['cwd'],env,log,deadline,registry)
    if spec['kind']=='check':
        outer.checks=[name+'_command_completed'] if outer.exit_code==0 and not outer.failures else []
        if name=='build_all' and outer.exit_code==0 and not outer.failures:
            related=[]
            for program,key in [('im-import-compare','IM_COMPARE_TEST_BINARY'),('im-import-preflight','IM_PREFLIGHT_TEST_BINARY')]:
                target=Path(bundle.environment[key])
                built=run_command(name,'check',snapshot.commit,[str(tools.paths['go']),'build','-buildvcs=false','-o',str(target),'./cmd/'+program],snapshot.root,env,root/(program+'.build.log'),deadline,registry)
                related += [built.log,Path(str(built.log)+'.stderr')]
                outer.failures.extend(built.failures)
                if built.exit_code or not target.is_file():outer.exit_code=built.exit_code or 1;outer.failures.append('import_program_build_failed:'+program)
                else:outer.checks.append(program+'_binary_sha256:'+_digest(target))
            outer.related_logs=related
        return outer
    if name=='orchestrator_contract':
        from .results import parse_unittest
        # unittest reports to stderr; its dedicated command diagnostic is the authoritative stream.
        event=parse_unittest(outer.log.with_suffix(outer.log.suffix+'.stderr'),name,snapshot.commit,outer.exit_code)
        event.inventory=python_inventory
    else:
        if name=='file_components':
            bundle.metadata['file_runtime_exit']=outer.exit_code
            log=root/'file-components.jsonl';log.write_bytes((root/'assembly.json').read_bytes()+(root/'components.json').read_bytes());log.chmod(0o600)
        elif name in output_keys:log=root/({'file_messages':'file-messages.jsonl','file_download_retention':'file-download-retention.jsonl','web_files':'web-files.jsonl','file_business_process':'business-runtime.jsonl'}[name])
        else:log=outer.log
        event=parse_go(log,name,snapshot.commit,outer.exit_code)
        event.inventory=collect_inventory(snapshot,tools,env,_selection(spec,None))
        for required in spec['required']:
            top=required.split('/')[0];matched=[p for p,names in event.inventory.tests.items() if top in names]
            if len(matched)!=1:event.failures.append('required_name_not_unique_in_inventory:'+required)
            else:event.inventory.required_subtests.add(matched[0]+'::'+required)
    event.related_logs=list(getattr(event,'related_logs',[]))+[outer.log]+([Path(str(a.log)+'.stderr'),Path(str(b.log)+'.stderr')] if name=='message_realtime' else [Path(str(outer.log)+'.stderr')])
    event.argv_template=getattr(outer,'argv_template',argv)
    event.failures.extend(outer.failures);return event
