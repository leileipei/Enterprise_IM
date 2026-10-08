"""Publish redacted evidence with explicit disposition of its private originals."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
from .model import GateEvent,Inventory,CleanupResult
from .results import evaluate,REQUIRED_GATES


def digest(path):
    h=hashlib.sha256()
    with Path(path).open('rb') as stream:
        for block in iter(lambda:stream.read(1024*1024),b''):h.update(block)
    return h.hexdigest()


def sanitize(text,secrets):
    for secret in sorted(secrets,key=len,reverse=True):
        if secret:text=text.replace(secret,'[PRIVATE FIXTURE VALUE]')
    text=re.sub(r'-----BEGIN (?:[A-Z ]+ )?PRIVATE KEY-----.*?-----END (?:[A-Z ]+ )?PRIVATE KEY-----','[PRIVATE KEY DISPOSED]',text,flags=re.S)
    # Dynamically created test roles are not all known to the parent fixture.
    text=re.sub(r'([a-zA-Z][a-zA-Z0-9+.-]*://)([^\s/@]+)@',r'\1[PRIVATE URI USERINFO]@',text)
    text=re.sub(r'\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b','[TOKEN DISPOSED]',text)
    if re.search(r'-----BEGIN [A-Z ]*PRIVATE KEY-----',text):raise ValueError('unsafe_incomplete_private_key')
    return text


def _safe(value,secrets):
    if isinstance(value,str):return sanitize(value,secrets)
    if isinstance(value,list):return [_safe(v,secrets) for v in value]
    if isinstance(value,dict):return {sanitize(str(k),secrets):_safe(v,secrets) for k,v in value.items()}
    return value


def _json(path,data):
    path=Path(path);temporary=path.with_name(path.name+'.new')
    fd=os.open(temporary,os.O_CREAT|os.O_EXCL|os.O_WRONLY|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,'w') as stream:json.dump(data,stream,ensure_ascii=False,indent=2);stream.write('\n');stream.flush();os.fsync(stream.fileno())
    os.replace(temporary,path)


def inventory_dict(inventory):
    if inventory is None:return None
    return dict(packages=sorted(inventory.packages),tests={p:sorted(t) for p,t in sorted(inventory.tests.items())},required_subtests=sorted(inventory.required_subtests))


def event_dict(event):
    return dict(name=event.name,kind=event.kind,source_commit=event.source_commit,exit_code=event.exit_code,
                counts=event.counts,executed=sorted(event.executed),passed=sorted(event.passed),started=sorted(event.started),
                failures=event.failures,skips=event.skips,checks=event.checks,helper_proofs=event.helper_proofs,
                package_status=event.package_status,inventory=inventory_dict(event.inventory),
                argv_template=getattr(event,'argv_template',[]),started_at=getattr(event,'started_at',None),
                ended_at=getattr(event,'ended_at',None),seconds=getattr(event,'seconds',None),
                resource_evidence=getattr(event,'resource_evidence',{}),resource_execution_seconds=getattr(event,'resource_execution_seconds',None))


def _publish_log(output,path,number,secrets,cleanup,cached=None):
    path=Path(path)
    if not path.is_absolute() or not path.resolve().is_relative_to(output.resolve()) or any(p.is_symlink() for p in (path,*path.parents)):
        raise ValueError('foreign_log_path')
    if path.exists():
        if path.stat().st_uid!=os.getuid():raise ValueError('foreign_log_user')
        path.chmod(0o600)
    if cached is not None:
        if Path(cached['original_path'])!=path:raise ValueError('changed_evidence_original')
        if path.exists() and digest(path)!=cached['original_sha256']:raise ValueError('changed_evidence_hash')
        if not path.exists():
            if not cached['original_disposed'] and cleanup.removed is not True:raise ValueError('original_missing_without_cleanup')
            cached=dict(cached,original_disposed=True,disposition='disposed_after_owned_cleanup' if not cached['original_disposed'] else cached['disposition'])
        if cached.get('published_path'):
            published=output/cached['published_path']
            if digest(published)!=cached['published_sha256']:raise ValueError('changed_published_hash')
        return cached
    row={'original_path':str(path),'original_sha256':'','original_mode':None,'original_disposed':False,'published_path':'','published_sha256':'','disposition':'missing'}
    if not path.is_file() or path.is_symlink():raise ValueError('original_log_missing_or_link')
    row.update(original_sha256=digest(path),original_mode=oct(path.stat().st_mode & 0o777))
    raw=path.read_text();published=sanitize(raw,secrets)
    directory=output/'share';directory.mkdir(mode=0o700,exist_ok=True)
    relative='share/'+str(number).zfill(3)+'-'+re.sub('[^a-zA-Z0-9_.-]','_',path.name)
    target=output/relative
    fd=os.open(target,os.O_CREAT|os.O_EXCL|os.O_WRONLY|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,'w') as stream:stream.write(published)
    row.update(published_path=relative,published_sha256=digest(target),disposition='redacted_copy' if published!=raw else 'retained_private_and_identical_share')
    if published!=raw:path.unlink();row.update(original_disposed=True,disposition='redacted_and_original_disposed')
    return row


def write_evidence(output,snapshot,tools,events,cleanup,inventory,secrets):
    output=Path(output);dispositions=[];integrity=[]
    for event in events:
        paths=list(dict.fromkeys([Path(event.log),*[Path(p) for p in getattr(event,'related_logs',[])]]))
        diagnostic=Path(str(event.log)+'.stderr')
        if diagnostic.exists():paths.append(diagnostic)
        paths=list(dict.fromkeys(paths))
        cache=getattr(event,'_published_logs',{})
        for path in paths:
            key=str(path)
            try:
                row=_publish_log(output,path,len(dispositions),secrets,cleanup,cache.get(key));cache[key]=row;dispositions.append(row)
            except (ValueError,OSError,UnicodeError) as exc:
                integrity.append('log_integrity:'+event.name+':'+(str(exc) if isinstance(exc,ValueError) else 'unreadable_log'))
                # A log that cannot be safely shared is disposed; its hash is traceable.
                if path.is_file() and not any(p.is_symlink() for p in (path,*path.parents)) and path.resolve().is_relative_to(output.resolve()) and path.stat().st_uid==os.getuid():
                    row=dict(original_path=str(path),original_sha256=digest(path),original_mode=oct(path.stat().st_mode&0o777),original_disposed=True,published_path='',published_sha256='',disposition='unsafe_original_disposed')
                    path.unlink();cache[key]=row;dispositions.append(row)
        # Cached diagnostics may have been removed by owned directory cleanup.
        for key,row in cache.items():
            if key in {str(p) for p in paths}:continue
            try:dispositions.append(_publish_log(output,Path(key),len(dispositions),secrets,cleanup,row))
            except (ValueError,OSError):integrity.append('cached_diagnostic_integrity:'+event.name)
        event._published_logs=cache
    if integrity:
        matches=[e for e in events if e.name=='evidence_integrity']
        if matches:matches[0].failures.extend(integrity)
        else:events.append(GateEvent('evidence_integrity','check',snapshot.commit,1,output/'integrity-unavailable',failures=integrity))
    verdict=evaluate(events,cleanup,snapshot.commit)
    source=dict(source_commit=snapshot.commit,source_archive_sha256=snapshot.archive_sha256,
                source_archive_path=str(snapshot.archive),source_root=str(snapshot.root),
                orchestrator_sha256=digest(snapshot.root/'scripts/test-project-integration.py'))
    payload=dict(schema_version='project_integration_v1',**source,
        required_gates_passed=verdict.required_gates_passed,full_suite_passed=verdict.full_suite_passed,
        race_suite_passed=verdict.race_suite_passed,failures=verdict.failures,
        cleanup=dict(removed=cleanup.removed,failures=cleanup.failures),
        events=[event_dict(e) for e in events],stage_statuses={name:'executed' if name in {e.name for e in events} else 'not_executed' for name in REQUIRED_GATES},inventory=inventory_dict(inventory),log_dispositions=dispositions,
        tools=dict(paths={k:str(p) for k,p in tools.paths.items()},versions=tools.versions,hashes=tools.hashes,images=tools.images,goos=tools.goos,goarch=tools.goarch),
        binary_hashes=getattr(snapshot,'binary_hashes',[])+[b for e in events for b in getattr(e,'resource_evidence',{}).get('binaries',[])],resource_records=getattr(snapshot,'resource_records',[]),
        customer_acceptance='not_executed',production_acceptance='not_executed')
    try:payload=_safe(payload,secrets)
    except ValueError:
        verdict.required_gates_passed=False;verdict.failures.append('unsafe_report_data')
        payload=dict(schema_version='project_integration_v1',**source,required_gates_passed=False,full_suite_passed=False,race_suite_passed=False,cleanup={'removed':cleanup.removed},failures=['unsafe_report_data'],customer_acceptance='not_executed',production_acceptance='not_executed')
    _json(output/'verification.json',payload);return verdict


def _inventory(data):
    return Inventory(set(data['packages']),{p:set(t) for p,t in data['tests'].items()},set(data['required_subtests']))


def validate_delivery(source_commit,report,repository_root):
    report=Path(report);data=json.loads(report.read_text());root=report.parent
    if data.get('schema_version')!='project_integration_v1' or data.get('source_commit')!=source_commit or not re.fullmatch('[a-f0-9]{40}',source_commit):raise ValueError('delivery_source_mismatch')
    if not all(data.get(k) is True for k in ['required_gates_passed','full_suite_passed','race_suite_passed']) or data.get('cleanup',{}).get('removed') is not True or data['cleanup'].get('failures') or data.get('failures'):raise ValueError('delivery_not_successful')
    rows=[]
    for item in data['events']:
        event=GateEvent(item['name'],item['kind'],item['source_commit'],item['exit_code'],report,
            counts=item['counts'],executed=set(item['executed']),passed=set(item['passed']),started=set(item['started']),
            failures=item['failures'],skips=item['skips'],checks=item['checks'],helper_proofs=item['helper_proofs'],package_status=item['package_status'],
            inventory=_inventory(item['inventory']) if item['inventory'] is not None else None)
        rows.append(event)
    if not evaluate(rows,CleanupResult(True,[]),source_commit).required_gates_passed:raise ValueError('delivery_gate_revalidation_failed')
    if len(rows)!=len(REQUIRED_GATES):raise ValueError('delivery_gate_count_mismatch')
    for event in rows:
        if event.name in {'full_repository','race_repository'} and inventory_dict(event.inventory)!=data['inventory']:raise ValueError('delivery_full_inventory_mismatch')
    for row in data['log_dispositions']:
        relative=Path(row['published_path'])
        if not row['published_path'] or relative.is_absolute() or '..' in relative.parts:raise ValueError('delivery_missing_share_log')
        path=root/relative
        if path.is_symlink() or not path.is_file() or digest(path)!=row['published_sha256']:raise ValueError('delivery_log_hash_mismatch')
        if path.stat().st_mode&0o077:raise ValueError('delivery_log_not_private')
        original=Path(row['original_path'])
        if row['original_disposed']:
            if original.exists():raise ValueError('disposed_original_retained')
            if not re.fullmatch('[a-f0-9]{64}',row['original_sha256']):raise ValueError('disposed_original_hash_missing')
        elif not original.is_file() or digest(original)!=row['original_sha256']:raise ValueError('delivery_original_hash_mismatch')
    from .resource_evidence import validate_delivery_resources
    validate_delivery_resources(data,root)
    archive=Path(data['source_archive_path'])
    if digest(archive)!=data['source_archive_sha256']:raise ValueError('delivery_archive_hash_mismatch')
    env={'PATH':'/usr/bin:/bin','LANG':'en_US.UTF-8','TZ':'UTC','GIT_NO_REPLACE_OBJECTS':'1','GIT_CONFIG_NOSYSTEM':'1','GIT_CONFIG_GLOBAL':os.devnull}
    def git(args):return subprocess.check_output(['/usr/bin/git','-C',str(repository_root),*args],env=env,stderr=subprocess.DEVNULL)
    if git(['rev-parse','--verify',source_commit+'^{commit}']).decode().strip()!=source_commit:raise ValueError('delivery_commit_missing')
    if hashlib.sha256(git(['show',source_commit+':scripts/test-project-integration.py'])).hexdigest()!=data['orchestrator_sha256']:raise ValueError('delivery_entry_hash_mismatch')
    changed=[p for p in git(['diff','--name-only','-z',source_commit,'HEAD']).decode().split('\0') if p]
    if any(not(p.startswith('docs/') or p.startswith('testdata/project-integration/README')) for p in changed):raise ValueError('delivery_product_changed_after_verification')
    for key,value in data['tools']['hashes'].items():
        path=Path(data['tools']['paths']['node_modules'])/'playwright/package.json' if key=='playwright_package' else Path(data['tools']['paths'][key])
        if not path.is_file() or digest(path)!=value:raise ValueError('delivery_tool_hash_mismatch')
