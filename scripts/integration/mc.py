"""Build the approved mc release from hash-verified immutable official source."""
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import tarfile
import urllib.request
from .tools import verify_digest, probe_version, file_digest


def extract_source(archive, destination, commit, digest):
    verify_digest(archive,digest)
    destination=Path(destination)
    if destination.exists() or destination.is_symlink():
        raise ValueError('mc_source_destination_reused')
    if not re.fullmatch('[0-9a-f]{40}',commit):raise ValueError('mc_source_commit_invalid')
    prefix='mc-'+commit
    with tarfile.open(archive,'r:gz') as tar:
        entries=[];seen=set()
        for member in tar.getmembers():
            parts=PurePosixPath(member.name).parts
            if not parts or parts[0]!=prefix or '..' in parts or member.name.startswith('/'):
                raise ValueError('mc_source_path_invalid')
            if not (member.isdir() or member.isfile()):raise ValueError('mc_source_type_invalid')
            relative=Path(*parts[1:])
            if str(relative) in seen:raise ValueError('mc_source_duplicate')
            seen.add(str(relative));entries.append((member,relative))
        if not any(str(relative)=='go.mod' and member.isfile() for member,relative in entries):
            raise ValueError('mc_module_missing')
        destination.mkdir(mode=0o700,parents=True)
        for member,relative in entries:
            path=destination/relative
            if member.isdir():path.mkdir(mode=0o700,parents=True,exist_ok=True)
            else:
                path.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
                with tar.extractfile(member) as src,path.open('xb') as out:shutil.copyfileobj(src,out)
                path.chmod(0o600)


def build_parameters(go,private,commit,release):
    if not re.fullmatch('[0-9a-f]{40}',commit):raise ValueError('mc_source_commit_invalid')
    match=re.fullmatch(r'RELEASE\.(\d{4})-(\d\d)-(\d\d)T(\d\d)-(\d\d)-(\d\d)Z',release)
    if not match:raise ValueError('mc_release_invalid')
    y,mo,d,h,mi,s=match.groups();version=f'{y}-{mo}-{d}T{h}:{mi}:{s}Z'
    values={'Version':version,'CopyrightYear':y,'ReleaseTag':release,
            'CommitID':commit,'ShortCommitID':commit[:12]}
    flags='-s -w '+' '.join('-X github.com/minio/mc/cmd.'+k+'='+v for k,v in values.items())
    private=Path(private).resolve()
    env={'PATH':str(Path(go).parent)+':/usr/bin:/bin','LANG':'C.UTF-8','TZ':'UTC',
         'GOENV':'off','GOWORK':'off','GOTOOLCHAIN':'local','GOOS':'darwin','GOARCH':'arm64',
         'CGO_ENABLED':'0','GOPROXY':'https://proxy.golang.org','GOSUMDB':'sum.golang.org',
         'GOCACHE':str(private/'go-build'),'GOMODCACHE':str(private/'go-mod'),
         'GOPATH':str(private/'go-path'),'TMPDIR':str(private/'tmp')}
    return [str(go),'build','-mod=readonly','-buildvcs=false','-trimpath','-tags','kqueue',
            '-ldflags',flags,'-o',str(private/'mc'),'.'],env


def build_mc(archive,private,pins,go):
    private=Path(private).resolve();private.mkdir(mode=0o700,parents=True,exist_ok=True)
    probe_version(go,'go',pins['go'])
    source=private/'mc-source'
    extract_source(archive,source,pins['mc-source-commit'],pins['mc-source-sha256'])
    argv,env=build_parameters(go,private,pins['mc-source-commit'],pins['mc-test-client'])
    for key in ('GOCACHE','GOMODCACHE','GOPATH','TMPDIR'):
        Path(env[key]).mkdir(mode=0o700,parents=True,exist_ok=True)
    modules={name:file_digest(source/name) for name in ('go.mod','go.sum')}
    log=private/'mc-build.log'
    with log.open('xb') as out:
        log.chmod(0o600)
        result=subprocess.run(argv,cwd=source,env=env,stdout=out,stderr=subprocess.STDOUT,timeout=1800)
    if result.returncode:raise ValueError('mc_source_build_failed')
    if modules!={name:file_digest(source/name) for name in modules}:
        raise ValueError('mc_source_dependencies_changed')
    binary=private/'mc';binary.chmod(0o700)
    receipt={'source_commit':pins['mc-source-commit'],'source_archive_sha256':pins['mc-source-sha256'],
             'release':pins['mc-test-client'],'compiler_version':pins['go'],
             'compiler_sha256':file_digest(go),'binary_sha256':file_digest(binary),
             'module_hashes':modules,'argv':argv,'environment':env,'build_log_sha256':file_digest(log)}
    path=private/'mc-build-receipt.json'
    path.write_text(json.dumps(receipt,indent=2)+'\n');path.chmod(0o600)
    return binary


def provision_mc(private,pins,go):
    private=Path(private)
    if any(path.is_symlink() for path in [private,*private.parents]):
        raise ValueError('mc_private_link_rejected')
    private=private.resolve();binary=private/'mc'
    if binary.is_symlink():raise ValueError('mc_binary_link_rejected')
    if not binary.exists():
        private.mkdir(mode=0o700,parents=True,exist_ok=True)
        archive=private/'mc-source.tar.gz'
        url='https://codeload.github.com/minio/mc/tar.gz/'+pins['mc-source-commit']
        with urllib.request.urlopen(url,timeout=60) as src,archive.open('xb') as out:
            archive.chmod(0o600);shutil.copyfileobj(src,out)
        build_mc(archive,private,pins,go)
    verify_digest(binary,pins['mc-test-client-sha256-darwin-arm64'])
    binary.chmod(0o700)
    result=subprocess.run([str(binary),'--version'],capture_output=True,text=True,timeout=60,
                          env={'PATH':'/usr/bin:/bin','LANG':'C.UTF-8','TZ':'UTC'})
    if result.returncode or not result.stdout.startswith('mc version '+pins['mc-test-client']+' (commit-id='+pins['mc-source-commit']+')'):
        raise ValueError('mc_build_metadata_mismatch')
    return binary
