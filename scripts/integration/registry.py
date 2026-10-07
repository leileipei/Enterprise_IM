"""Private resource ledger; cleanup requires actual identity and ownership."""
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import time
from .model import CleanupResult, ResourceRef


def _capture(argv):
    result=subprocess.run(argv,capture_output=True,text=True,timeout=5,
                          env={'PATH':'/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin','TZ':'UTC','LANG':'en_US.UTF-8','LC_CTYPE':'en_US.UTF-8'})
    return result.returncode,result.stdout


def _cwd(pid):
    proc=Path('/proc')/str(pid)/'cwd'
    if Path('/proc').exists():
        try:return str(proc.resolve(strict=True))
        except OSError:return None
    _,out=_capture(['/usr/sbin/lsof','-a','-p',str(pid),'-d','cwd','-Fn'])
    return next((line[1:] for line in out.splitlines() if line.startswith('n')),None)


def process_fingerprint(pid):
    rc,out=_capture(['/bin/ps','-p',str(pid),'-o','uid=,lstart=,comm='])
    if rc or not out.strip():return None
    rc,state=_capture(['/bin/ps','-p',str(pid),'-o','stat='])
    if rc or state.strip().startswith('Z'):return None
    parts=out.strip().split(None,6)
    if len(parts)!=7:raise ValueError('invalid_process_identity')
    executable=Path(parts[6])
    if not executable.is_absolute():
        found=shutil.which(str(executable))
        if not found:raise ValueError('unknown_process_executable')
        executable=Path(found)
    executable=executable.resolve()
    cwd=_cwd(pid)
    if cwd is None:raise ValueError('unproven_process_workdir')
    return dict(pid=pid,uid=int(parts[0]),start_time=' '.join(parts[1:6]),
                executable_path=str(executable),
                executable_sha256=hashlib.sha256(executable.read_bytes()).hexdigest(),
                workdir=str(Path(cwd).resolve()),pgid=os.getpgid(pid))


class Registry:
    def __init__(self,path: Path,owner: str,commit: str):
        if not re.fullmatch('[a-f0-9]{32}',owner) or not re.fullmatch('[a-f0-9]{40}',commit):
            raise ValueError('invalid_registry_identity')
        self.path=Path(path);self.owner=owner;self.commit=commit
        if self.path.is_symlink() or self.path.parent.is_symlink():raise ValueError('registry_link')
        self.path.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
        if self.path.parent.stat().st_mode & 0o077:raise ValueError('registry_root_not_private')
        self.root=self.path.parent.resolve()
        if self.path.exists():
            if self.path.stat().st_mode & 0o077:raise ValueError('registry_not_private')
            self.records()
        else:
            fd=os.open(self.path,os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600);os.close(fd)
            self.reserve(owner)

    def write(self,event,owner,identity='',kind='',fingerprint=None,detail=None):
        row=dict(schema_version=1,owner=owner,source_commit=self.commit,event=event,
                 identity=identity,kind=kind,fingerprint=fingerprint or {},detail=detail or {},time=time.time())
        fd=os.open(self.path,os.O_WRONLY|os.O_APPEND|os.O_NOFOLLOW)
        try:
            fcntl.flock(fd,fcntl.LOCK_EX)
            payload=(json.dumps(row,sort_keys=True)+'\n').encode()
            with os.fdopen(os.dup(fd),'ab') as stream:stream.write(payload);stream.flush()
            os.fsync(fd)
        finally:os.close(fd)

    def records(self):
        if self.path.is_symlink() or self.path.stat().st_mode & 0o077:raise ValueError('unsafe_registry')
        rows=[]
        with self.path.open() as stream:
            fcntl.flock(stream,fcntl.LOCK_SH)
            for line in stream:
                try:row=json.loads(line)
                except ValueError as exc:raise ValueError('corrupt_registry') from exc
                if (not isinstance(row,dict) or row.get('schema_version')!=1 or
                    row.get('source_commit')!=self.commit or
                    not re.fullmatch('[a-f0-9]{32}',str(row.get('owner','')))):
                    raise ValueError('foreign_registry_record')
                rows.append(row)
        return rows

    def reserve(self,child_owner):
        if not re.fullmatch('[a-f0-9]{32}',child_owner):raise ValueError('invalid_child_owner')
        if any(r['event']=='reserve' and r['owner']==child_owner for r in self.records()):return
        private=self.root/child_owner
        if private.exists() or private.is_symlink():raise ValueError('child_private_path_exists')
        private.mkdir(mode=0o700)
        self.write('reserve',child_owner,kind='directory',fingerprint={'private_root':str(private)})

    def add(self,ref: ResourceRef):
        reserved={r['owner'] for r in self.records() if r['event']=='reserve'}
        if ref.owner not in reserved:raise ValueError('owner_not_reserved')
        if ref.kind not in {'process','container','directory'}:raise ValueError('unknown_resource_kind')
        if ref.kind=='process':
            f=ref.fingerprint
            if f.get('uid')!=os.getuid():raise ValueError('foreign_process_uid')
            if not any(Path(f.get(k,'/')).is_relative_to(self.root) for k in ('workdir','executable_path')):
                raise ValueError('process_outside_owned_root')
        if ref.kind=='directory':
            target=Path(ref.identity)
            if target.is_symlink() or not target.is_relative_to(self.root) or target==self.root:
                raise ValueError('foreign_directory')
        self.write('registered',ref.owner,ref.identity,ref.kind,ref.fingerprint,{'state':ref.state})

    def lifecycle(self,identity,event,detail):
        if event not in {'ready','exited'}:raise ValueError('unknown_process_event')
        rows=self.records()
        matched=next((r for r in reversed(rows) if r['identity']==identity and r['event']=='registered'),None)
        if not matched:raise ValueError('unregistered_lifecycle')
        self.write(event,matched['owner'],identity,matched['kind'],detail=detail)

    def docker(self,args):
        cli=shutil.which('docker')
        if not cli:raise ValueError('docker_unavailable_for_cleanup')
        from .services import docker_environment
        result=subprocess.run([cli,*args],capture_output=True,text=True,timeout=5,env=docker_environment())
        if args[0]=='ps':
            if result.returncode:raise ValueError('docker_list_failed')
            return result.stdout.splitlines()
        if args[0]=='inspect':
            if result.returncode:
                message=result.stderr.strip().lower()
                if (result.stdout.strip() in ('','[]') and len(args)==2 and
                    any(message.endswith(prefix+args[1].lower()) for prefix in
                        ('no such object: ','no such container: '))):return None
                raise ValueError('docker_inspect_failed')
            return json.loads(result.stdout)[0]
        if result.returncode:raise ValueError('docker_cleanup_failed')
        return None

    def _owned_container(self,identity,owner):
        actual=self.docker(['inspect',identity])
        if actual is None:return None
        labels=actual.get('Config',{}).get('Labels',{}) or {}
        cid=actual.get('Id','')
        if (not re.fullmatch('[a-f0-9]{64}',cid) or not cid.startswith(identity) or
            labels.get('im.integration.owner')!=owner or labels.get('im.integration.source')!=self.commit):
            raise ValueError('container_identity_mismatch')
        return actual

    def _recover(self,rows):
        known={r['identity'] for r in rows if r['event']=='registered'}
        # Reserved, private child directories prove process ownership in the create/register gap.
        if Path('/proc').exists():
            candidates=[int(p.name) for p in Path('/proc').iterdir() if p.name.isdigit()]
        else:
            _,out=_capture(['/usr/sbin/lsof','-a','-u',str(os.getuid()),'-d','cwd','-Fpn'])
            candidates=[];pid=None
            private_roots={r['fingerprint']['private_root'] for r in rows if r['event']=='reserve'}
            for line in out.splitlines():
                if line.startswith('p'):pid=int(line[1:])
                elif line.startswith('n') and pid and str(Path(line[1:]).resolve()) in private_roots:
                    candidates.append(pid)
        for reserved in [r for r in rows if r['event']=='reserve']:
            owner=reserved['owner'];directory=Path(reserved['fingerprint']['private_root'])
            for pid in candidates:
                if str(pid) in known:continue
                f=process_fingerprint(pid)
                if not f or f['uid']!=os.getuid() or f['workdir']!=str(directory):continue
                start=datetime.datetime.strptime(f['start_time'],'%a %b %d %H:%M:%S %Y').replace(tzinfo=datetime.timezone.utc).timestamp()
                if start<int(reserved['time']):continue
                self.add(ResourceRef('process',owner,str(pid),f));known.add(str(pid))
            for cid in self.docker(['ps','-a','--filter','label=im.integration.owner='+owner,'--format','{{.ID}}']):
                actual=self._owned_container(cid,owner)
                if actual and actual['Id'] not in known:
                    self.add(ResourceRef('container',owner,actual['Id'],{'container_id':actual['Id']}))
                    known.add(actual['Id'])

    def _stop_process(self,row,deadline):
        pid=int(row['identity']);expected=row['fingerprint'];actual=process_fingerprint(pid)
        if actual is None:return
        if actual!=expected or actual['uid']!=os.getuid():raise ValueError('process_identity_mismatch')
        if not any(Path(actual[k]).resolve().is_relative_to(self.root) for k in ('workdir','executable_path')):
            raise ValueError('process_outside_owned_root')
        # Signal only the verified process; command supervisor owns group cancellation.
        os.kill(pid,signal.SIGTERM)
        while time.monotonic()<deadline:
            current=process_fingerprint(pid)
            if current is None:return
            if current!=expected:raise ValueError('process_identity_changed')
            time.sleep(0.02)
        raise ValueError('process_still_running')

    def cleanup(self,deadline):
        failures=[]
        try:
            rows=self.records();self._recover(rows);rows=self.records()
        except (ValueError,OSError,subprocess.SubprocessError) as exc:
            return CleanupResult(False,[str(exc)])
        reserved={r['owner'] for r in rows if r['event']=='reserve'}
        resources={r['identity']:r for r in rows if r['event']=='registered'}
        for row in resources.values():
            try:
                if row['owner'] not in reserved:raise ValueError('owner_not_reserved')
                if row['kind']=='process':self._stop_process(row,deadline)
                elif row['kind']=='container':
                    actual=self._owned_container(row['identity'],row['owner'])
                    if actual and actual.get('State',{}).get('Status')!='removing':
                        self.docker(['rm','-f',row['identity']])
                    while actual is not None and time.monotonic()<deadline:
                        time.sleep(0.02);actual=self._owned_container(row['identity'],row['owner'])
                    if actual is not None:raise ValueError('container_still_present')
                elif row['kind']=='directory':
                    path=Path(row['identity'])
                    if path.is_symlink() or not path.resolve().is_relative_to(self.root) or path.resolve()==self.root:
                        raise ValueError('directory_outside_owned_root')
                    if path.exists():
                        actual=path.stat()
                        expected=row['fingerprint']
                        if path.is_symlink() or actual.st_uid!=os.getuid() or (actual.st_dev,actual.st_ino)!=(expected.get('device'),expected.get('inode')):
                            raise ValueError('directory_identity_mismatch')
                        shutil.rmtree(path)
            except (ValueError,OSError,subprocess.SubprocessError) as exc:
                failures.append(row['kind']+':'+row['identity']+':'+str(exc))
        return CleanupResult(not failures,failures)
