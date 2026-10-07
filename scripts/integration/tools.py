"""Locked tool discovery; no replacement of a shared installation."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import urllib.request
from .model import SourceSnapshot, Toolchain

RUNTIME=Path('/Users/leo.cui/.cache/codex-runtimes/codex-primary-runtime/dependencies/node')
MC_URL='https://dl.min.io/client/mc/release/darwin-arm64/archive/mc.RELEASE.2024-11-05T11-29-45Z'


def file_digest(path):
    h=hashlib.sha256()
    with Path(path).open('rb') as f:
        for block in iter(lambda:f.read(1024*1024),b''):h.update(block)
    return h.hexdigest()


def verify_digest(path: Path, expected: str) -> None:
    try: actual=file_digest(path)
    except OSError as exc:raise ValueError('tool_missing') from exc
    if actual!=expected:raise ValueError('tool_hash_mismatch')


def python_cli() -> Path:
    # macOS framework launcher re-execs Python.app after Popen returns.
    # Invoke the current interpreter image directly to bind one stable identity.
    result=subprocess.run(['/bin/ps','-p',str(os.getpid()),'-o','comm='],
                          capture_output=True,text=True,timeout=5)
    path=Path(result.stdout.strip())
    if result.returncode or not path.is_absolute() or not path.is_file():
        raise ValueError('python_interpreter_identity_unproven')
    return path


def docker_cli() -> Path:
    path=shutil.which('docker')
    if not path:raise ValueError('docker_missing')
    # OrbStack dispatches its multicall binary using argv[0].
    return Path(path).absolute()


def probe_version(path: Path,key: str,expected: str) -> str:
    probes={'go':(['version'],r'go version go([\d.]+)'),
            'node':(['--version'],r'v([\d.]+)'),
            'chrome':(['--version'],r'Google Chrome ([\d.]+)'),
            'qpdf':(['--version'],r'qpdf version ([\d.]+)'),
            'clamd':(['--help'],r'Clam AntiVirus: Daemon ([\d.]+)'),
            'sigtool':(['--version'],r'ClamAV ([\d.]+)'),
            'freshclam':(['--help'],r'Clam AntiVirus: Database Updater ([\d.]+)'),
            'python':(['--version'],r'Python ([\d.]+)')}
    args,pattern=probes[key]
    result=subprocess.run([str(path),*args],capture_output=True,text=True,timeout=60)
    if result.returncode:raise ValueError('tool_probe_failed:'+key)
    match=re.search(pattern,result.stdout+result.stderr)
    if not match:raise ValueError('unknown_tool_version:'+key)
    version=match.group(1)
    if key=='python':
        if tuple(map(int,version.split('.')[:2]))<(3,9):raise ValueError('python_version_too_old')
    elif not expected or version!=expected:raise ValueError('tool_version_mismatch:'+key)
    return version


def _lock(path):
    result={}
    for line in path.read_text().splitlines():
        if line.strip() and not line.startswith('#'):
            key,value=line.split('=',1);key=key.strip()
            if key in result:raise ValueError('duplicate_tool_lock')
            result[key]=value.strip()
    return result


def discover_toolchain(snapshot: SourceSnapshot,private: Path) -> Toolchain:
    pins=_lock(snapshot.root/'testdata/file-runtime/versions.lock')
    pins.update(_lock(snapshot.root/'testdata/project-integration/versions.lock'))
    private=Path(private).resolve();private.mkdir(mode=0o700,parents=True,exist_ok=True)
    paths=dict(go=Path('/opt/homebrew/bin/go'),python=python_cli(),
               node=RUNTIME/'bin/node',node_modules=RUNTIME/'node_modules',
               chrome=Path('/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'),
               qpdf=Path('/opt/homebrew/bin/qpdf'),clamd=Path('/opt/homebrew/sbin/clamd'),
               sigtool=Path('/opt/homebrew/bin/sigtool'),freshclam=Path('/opt/homebrew/bin/freshclam'),
               openssl=Path('/usr/bin/openssl'))
    paths['docker']=docker_cli()
    for key,path in paths.items():
        if key=='node_modules':
            if not path.is_dir():raise ValueError('node_modules_missing')
        elif not path.is_file():raise ValueError('tool_missing:'+key)
    versions={};hashes={}
    def run(key,args):
        result=subprocess.run([str(paths[key]),*args],capture_output=True,text=True,timeout=60)
        if result.returncode:raise ValueError('tool_probe_failed:'+key)
        return result.stdout+result.stderr
    for key in ('go','node','chrome','qpdf','clamd','sigtool','freshclam','python'):
        expected=pins.get({'clamd':'clamav','sigtool':'clamav','freshclam':'clamav'}.get(key,key))
        versions[key]=probe_version(paths[key],key,expected)
    for key in ('openssl','docker'):versions[key]=run(key,['version'] if key=='openssl' else ['--version']).strip()
    package=paths['node_modules']/'playwright/package.json'
    versions['playwright']=json.loads(package.read_text())['version']
    if versions['playwright']!=pins['playwright']:raise ValueError('playwright_version_mismatch')
    hashes['playwright_package']=file_digest(package)
    mc=private/'mc'
    if not mc.exists():
        try:
            with urllib.request.urlopen(MC_URL,timeout=60) as src,mc.open('xb') as dst:
                shutil.copyfileobj(src,dst)
            verify_digest(mc,pins['mc-test-client-sha256-darwin-arm64'])
            mc.chmod(0o700)
        except (OSError,ValueError) as exc:raise ValueError('locked_mc_unavailable') from exc
    verify_digest(mc,pins['mc-test-client-sha256-darwin-arm64']);paths['mc']=mc
    versions['mc']=run('mc',['--version']).splitlines()[0]
    for key,path in paths.items():
        if key!='node_modules':hashes[key]=file_digest(path)
    go_platform=run('go',['env','GOOS','GOARCH']).splitlines()
    if go_platform!=['darwin','arm64']:raise ValueError('unsupported_integration_platform')
    images={}
    for key in ('minio-image','postgres-image','redis-image','alpine-resource-image'):
        reference=pins[key]
        raw=run('docker',['image','inspect',reference,'--format', '{{json .RepoDigests}}|{{.Id}}|{{.Os}}|{{.Architecture}}'])
        digests,image_id,os_name,architecture=raw.strip().split('|')
        if reference not in json.loads(digests):raise ValueError('image_digest_mismatch:'+key)
        if architecture!='arm64' or os_name!='linux':raise ValueError('image_platform_mismatch:'+key)
        images[key]=dict(reference=reference,image_id=image_id,os=os_name,arch=architecture)
    return Toolchain(paths,versions,hashes,images,*go_platform)
