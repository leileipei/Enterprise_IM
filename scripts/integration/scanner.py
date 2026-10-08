"""Native scanner and browser fixtures bound to owned process identities."""
import datetime
import json
import os
from pathlib import Path
import queue
import re
import secrets
import shutil
import socket
import subprocess
import threading
import time
from .model import GateEvent,ResourceRef
from .registry import process_fingerprint
from .services import _private_file
from .tools import file_digest,verify_digest

NAMES=('main.cvd','daily.cvd','bytecode.cvd')
DEFINITION_SOURCE=Path('/private/tmp/enterprise-im-p4-26-runtime/definitions-refresh')


def parse_signature_info(text,now,daily):
    if 'Verification OK.' not in text:raise ValueError('definition_signature_unverified')
    fields={line.split(':',1)[0]:line.split(':',1)[1].strip() for line in text.splitlines() if ':' in line}
    try:
        built=datetime.datetime.strptime(fields['Build time'],'%d %b %Y %H:%M %z')
        version=fields['Version']
    except (ValueError,KeyError) as exc:raise ValueError('definition_metadata_invalid') from exc
    if not re.fullmatch('[0-9]+',version):raise ValueError('definition_metadata_invalid')
    if built>now or (daily and now-built>datetime.timedelta(hours=24)):
        raise ValueError('definition_stale_or_future')
    return {'version':version,'build_time':built.isoformat()}


def check_definitions(directory,tools,deadline):
    result={};now=datetime.datetime.now(datetime.timezone.utc)
    for name in NAMES:
        path=Path(directory)/name
        if path.is_symlink() or not path.is_file():raise ValueError('definition_missing_or_linked')
        if time.monotonic()>=deadline:raise ValueError('scanner_deadline')
        completed=subprocess.run([str(tools.paths['sigtool']),'--info',str(path)],capture_output=True,text=True,
                                 timeout=min(60,deadline-time.monotonic()),env={'PATH':'/usr/bin:/bin','TZ':'UTC','LANG':'C.UTF-8'})
        if completed.returncode:raise ValueError('definition_signature_unverified')
        result[name]=parse_signature_info(completed.stdout,now,name=='daily.cvd')
        result[name]['sha256']=file_digest(path)
    return result


def write_manifest(runtime):
    manifest={'kind':'local-process','pid':runtime['pid'],'clamd_binary_path':runtime['binary'],
              'clamd_binary_sha256':runtime['hashes']['binary'],'config_path':runtime['config'],
              'config_sha256':runtime['hashes']['config'],'clamd_socket_path':runtime['socket'],
              'qpdf_path':runtime['qpdf'],'qpdf_sha256':runtime['hashes']['qpdf'],
              'definition_directory':runtime['definitions'],'definition_hashes':runtime['definition_hashes']}
    _private_file(runtime['manifest'],json.dumps(manifest,sort_keys=True)+'\n')
    Path(runtime['manifest']).chmod(0o400)


def validate_binding(runtime):
    for path in (runtime['manifest'],runtime['config']):
        path=Path(path)
        if path.is_symlink() or path.stat().st_uid!=os.getuid() or path.stat().st_mode&0o777!=0o400:
            raise ValueError('scanner_binding_permissions')
    manifest=json.loads(Path(runtime['manifest']).read_text())
    if manifest['pid']!=runtime['pid'] or manifest['clamd_socket_path']!=runtime['socket']:
        raise ValueError('scanner_manifest_binding_mismatch')
    actual=process_fingerprint(runtime['pid'])
    if actual!=runtime['fingerprint']:raise ValueError('scanner_process_binding_changed')
    sock=Path(runtime['socket'])
    if not sock.is_socket() or sock.stat().st_uid!=os.getuid() or sock.stat().st_mode&0o777!=0o600:
        raise ValueError('scanner_socket_binding_changed')
    for key in ('binary','config','qpdf'):verify_digest(Path(runtime[key]),runtime['hashes'][key])
    for name in NAMES:verify_digest(Path(runtime['definitions'])/name,runtime['definition_hashes'][name])
    # Comparing the entire serialized manifest also rejects redirected paths/hashes/unknown fields.
    expected={'kind':'local-process','pid':runtime['pid'],'clamd_binary_path':runtime['binary'],
              'clamd_binary_sha256':runtime['hashes']['binary'],'config_path':runtime['config'],
              'config_sha256':runtime['hashes']['config'],'clamd_socket_path':runtime['socket'],
              'qpdf_path':runtime['qpdf'],'qpdf_sha256':runtime['hashes']['qpdf'],
              'definition_directory':runtime['definitions'],'definition_hashes':runtime['definition_hashes']}
    if manifest!=expected:raise ValueError('scanner_manifest_binding_mismatch')


def clamd_command(path,command):
    with socket.socket(socket.AF_UNIX) as stream:
        stream.settimeout(3);stream.connect(str(path));stream.sendall(('z'+command+'\0').encode())
        return stream.recv(4096).rstrip(b'\0\n').decode()


def prepare_scanner(bundle,tools,registry,private,deadline):
    if bundle.metadata['source_commit']!=registry.commit:raise ValueError('scanner_source_mismatch')
    private=Path(private).resolve()
    if not private.is_relative_to(registry.root):raise ValueError('scanner_private_root_not_owned')
    # Unix sockets have a platform path length limit; reject before starting a daemon.
    if len(os.fsencode(str(private/'positive.sock')))>=104:raise ValueError('scanner_socket_path_too_long')
    private.mkdir(mode=0o700,parents=True,exist_ok=True)
    definitions=private/'definitions';definitions.mkdir(mode=0o700)
    source=Path(bundle.metadata.get('definition_source',DEFINITION_SOURCE))
    for name in NAMES:
        shutil.copyfile(source/name,definitions/name);(definitions/name).chmod(0o600)
    try:info=check_definitions(definitions,tools,deadline)
    except ValueError as exc:
        if str(exc)!='definition_stale_or_future':raise
        config=private/'freshclam.conf'
        _private_file(config,'DatabaseDirectory "'+str(definitions)+'"\nDatabaseMirror database.clamav.net\nScriptedUpdates no\nForeground yes\nConnectTimeout 10\nReceiveTimeout 30\nMaxAttempts 1\n')
        with (private/'freshclam.log').open('xb') as out:
            result=subprocess.run([str(tools.paths['freshclam']),'--config-file='+str(config),'--stdout'],stdout=out,stderr=subprocess.STDOUT,
                env={'PATH':'/opt/homebrew/bin:/usr/bin:/bin','LANG':'C.UTF-8','TZ':'UTC'},timeout=max(0.1,min(180,deadline-time.monotonic())))
        if result.returncode:raise ValueError('definition_update_failed')
        info=check_definitions(definitions,tools,deadline)
    runtimes=[]
    for role in ('positive','negative'):
        config=private/(role+'.conf');sock=private/(role+'.sock')
        lines=Path(__file__).resolve().parents[2].joinpath('testdata/file-runtime/clamd.conf').read_text().splitlines()
        lines=[('LocalSocket '+str(sock)) if line.startswith('LocalSocket ') else
               ('DatabaseDirectory '+str(definitions)) if line.startswith('DatabaseDirectory ') else
               ('MaxFileSize 26214400') if role=='negative' and line.startswith('MaxFileSize ') else line for line in lines]
        _private_file(config,'\n'.join(lines)+'\n');config.chmod(0o400)
        log=private/(role+'.log');fd=os.open(log,os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
        with os.fdopen(fd,'wb') as stream:
            proc=subprocess.Popen([str(tools.paths['clamd']),'--config-file='+str(config)],cwd=registry.root/registry.owner,
                env={'PATH':'/opt/homebrew/bin:/usr/bin:/bin','LANG':'C.UTF-8','TZ':'UTC'},stdout=stream,stderr=subprocess.STDOUT,start_new_session=True)
        fingerprint=process_fingerprint(proc.pid)
        if not fingerprint:raise ValueError('scanner_process_not_registered')
        registry.add(ResourceRef('process',registry.owner,str(proc.pid),fingerprint))
        while True:
            if proc.poll() is not None:raise ValueError('scanner_start_failed')
            try:
                if clamd_command(sock,'PING')=='PONG':break
            except (OSError,ValueError):pass
            if time.monotonic()>=deadline:raise ValueError('scanner_readiness_deadline')
            time.sleep(0.1)
        runtime={'pid':proc.pid,'binary':str(tools.paths['clamd']),'config':str(config),'socket':str(sock),
                 'qpdf':str(tools.paths['qpdf']),'definitions':str(definitions),'fingerprint':fingerprint,
                 'hashes':{'binary':file_digest(tools.paths['clamd']),'config':file_digest(config),'qpdf':file_digest(tools.paths['qpdf'])},
                 'definition_hashes':{name:info[name]['sha256'] for name in NAMES},'manifest':str(private/(role+'-manifest.json'))}
        write_manifest(runtime);validate_binding(runtime);registry.lifecycle(str(proc.pid),'ready',{'gate':'fixture_preflight','role':role})
        # Keep handles for actual wait/reaping during final cleanup, not for public serialization.
        runtime['process']=proc;runtimes.append(runtime)
    bundle.metadata['scanner_runtimes']=runtimes;bundle.metadata['scanner_definition_info']=info
    bundle.environment.update(IM_TEST_QPDF_PATH=str(tools.paths['qpdf']),IM_TEST_CLAMD_SOCKET=runtimes[0]['socket'],
        IM_TEST_SCANNER_MANIFEST=runtimes[0]['manifest'],IM_TEST_UNPROVEN_CLAMD_SOCKET=runtimes[1]['socket'],IM_TEST_UNPROVEN_SCANNER_MANIFEST=runtimes[1]['manifest'])
    return bundle


def probe_scanner(bundle,tools,deadline):
    runtimes=bundle.metadata['scanner_runtimes'];checks=[]
    for runtime in runtimes:
        validate_binding(runtime);check_definitions(runtime['definitions'],tools,deadline)
        if clamd_command(runtime['socket'],'PING')!='PONG':raise ValueError('scanner_live_ping_failed')
        checks.append('bound_live_scanner_'+str(runtime['pid']))
    log=Path(runtimes[0]['config']).parent/('scanner-preflight-'+secrets.token_hex(8)+'.json')
    _private_file(log,json.dumps({'checks':checks})+'\n')
    return GateEvent('fixture_preflight','check',bundle.metadata['source_commit'],0,log,checks=checks)


def probe_browser(bundle,tools,registry,private,deadline):
    if bundle.metadata['source_commit']!=registry.commit:raise ValueError('browser_source_mismatch')
    private=Path(private).resolve()
    if not private.is_relative_to(registry.root):raise ValueError('browser_private_root_not_owned')
    private.mkdir(mode=0o700,parents=True,exist_ok=True);home=private/'browser-home';home.mkdir(mode=0o700)
    log=private/'browser-probe.jsonl';stderr=private/'browser-stderr.log'
    env={'PATH':str(tools.paths['node'].parent)+':/usr/bin:/bin','NODE_PATH':str(tools.paths['node_modules']),
         'HOME':str(home),'TMPDIR':str(private),'LANG':'en_US.UTF-8','TZ':'UTC'}
    script=Path(__file__).with_name('browser_probe.cjs');messages=queue.Queue();failure=[];checks=[];browser_identity=None;node=None
    fd=os.open(stderr,os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
    with os.fdopen(fd,'wb') as err:
        node=subprocess.Popen([str(tools.paths['node']),str(script)],cwd=registry.root/registry.owner,env=env,
            stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=err,text=True,start_new_session=True)
        fp=process_fingerprint(node.pid)
        if not fp:raise ValueError('browser_node_identity_unproven')
        registry.add(ResourceRef('process',registry.owner,str(node.pid),fp))
        payload={'executable':str(tools.paths['chrome']),'playwright':tools.versions['playwright'],'chrome':tools.versions['chrome'],'private':str(private)}
        node.stdin.write(json.dumps(payload)+'\n');node.stdin.flush()
        def read():
            for line in node.stdout:messages.put(line)
            messages.put(None)
        reader=threading.Thread(target=read,daemon=True);reader.start()
        logfd=os.open(log,os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
        with os.fdopen(logfd,'w') as out:
            try:
                while time.monotonic()<deadline:
                    try:line=messages.get(timeout=min(0.1,max(0.001,deadline-time.monotonic())))
                    except queue.Empty:continue
                    if line is None:break
                    out.write(line);out.flush();row=json.loads(line)
                    if row['event']=='browser_launched':
                        pid=row['pid'];browser_fp=process_fingerprint(pid)
                        if not browser_fp or browser_fp['executable_path']!=str(tools.paths['chrome'].resolve()):raise ValueError('browser_process_identity_unproven')
                        registry.add(ResourceRef('process',registry.owner,str(pid),browser_fp));browser_identity=str(pid)
                        checks.append('browser_launched');node.stdin.write('registered\n');node.stdin.flush()
                    elif row['event']=='browser_ready':
                        if row['version']!=tools.versions['chrome'] or not browser_identity:raise ValueError('browser_version_mismatch')
                        registry.lifecycle(browser_identity,'ready',{'gate':'fixture_preflight'});checks.append('browser_version_verified')
                    elif row['event']=='browser_closed':
                        if str(row['pid'])!=browser_identity or row['exit_code']!=0:raise ValueError('browser_exit_unproven')
                        registry.lifecycle(browser_identity,'exited',{'expected_exit':0,'actual_exit':row['exit_code'],'gate':'fixture_preflight'})
                        checks.append('browser_closed')
                node.wait(timeout=max(0.1,deadline-time.monotonic()))
                if node.returncode or set(checks)!={'browser_launched','browser_version_verified','browser_closed'}:failure=['browser_probe_incomplete']
            except (ValueError,OSError,subprocess.SubprocessError,KeyError):failure=['browser_probe_failed']
            finally:
                if node.poll() is None:
                    if process_fingerprint(node.pid)==fp:node.terminate()
                    try:node.wait(timeout=3)
                    except subprocess.TimeoutExpired:failure.append('browser_node_still_running')
                registry.lifecycle(str(node.pid),'exited',{'exit_code':node.returncode,'gate':'fixture_preflight'})
                node.stdin.close();node.stdout.close()
    shutil.rmtree(home)
    return GateEvent('fixture_preflight','check',bundle.metadata['source_commit'],int(bool(failure)),log,checks=checks,failures=failure)
