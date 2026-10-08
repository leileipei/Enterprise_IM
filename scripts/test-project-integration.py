#!/usr/bin/env python3
"""Standard-library bootstrap: load the runner only from a verified Git archive."""
import argparse
import hashlib
import importlib.util
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys
import tarfile
from typing import Callable, Dict


def digest(path):
    h=hashlib.sha256()
    with Path(path).open('rb') as f:
        for block in iter(lambda:f.read(1024*1024),b''): h.update(block)
    return h.hexdigest()


def members(tar):
    result=tar.getmembers();seen=set()
    for member in result:
        path=PurePosixPath(member.name)
        if (path.is_absolute() or not path.parts or '..' in path.parts or
            '\\' in member.name or not (member.isfile() or member.isdir()) or
            str(path) in seen):
            raise ValueError('unsafe_archive_member')
        seen.add(str(path))
    return result


def extract_archive(archive: Path, root: Path) -> None:
    with tarfile.open(archive,'r:') as tar:
        entries=members(tar)  # Validate every member before writing any.
        root.mkdir(mode=0o700)
        for member in entries:
            target=root.joinpath(*PurePosixPath(member.name).parts)
            if member.isdir(): target.mkdir(mode=0o700,parents=True,exist_ok=True)
            else:
                target.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
                with tar.extractfile(member) as src, target.open('xb') as dst:
                    for block in iter(lambda:src.read(1024*1024),b''): dst.write(block)
                target.chmod(0o700 if member.mode & 0o111 else 0o600)


def validate_export(data: Dict[str,str]) -> None:
    archive=Path(data['archive']);root=Path(data['root'])
    if digest(archive)!=data['archive_sha256']: raise ValueError('archive_hash_mismatch')
    expected=set()
    with tarfile.open(archive,'r:') as tar:
        for member in members(tar):
            target=root.joinpath(*PurePosixPath(member.name).parts)
            if target.is_symlink(): raise ValueError('export_link')
            if member.isfile():
                expected.add(str(target.relative_to(root)))
                if not target.is_file(): raise ValueError('export_file_missing')
                with tar.extractfile(member) as source:
                    h=hashlib.sha256()
                    for block in iter(lambda:source.read(1024*1024),b''):h.update(block)
                if digest(target)!=h.hexdigest(): raise ValueError('export_file_hash_mismatch')
    actual=set()
    for target in root.rglob('*'):
        if target.is_symlink(): raise ValueError('export_link')
        if target.is_file(): actual.add(str(target.relative_to(root)))
    if actual!=expected: raise ValueError('export_file_inventory_mismatch')


def bootstrap(repository_root: Path, commit: str, out: Path) -> Dict[str,str]:
    if not re.fullmatch('[a-f0-9]{40}',commit):raise ValueError('full_commit_required')
    repository_root=Path(repository_root).resolve();out=Path(out)
    if not out.is_absolute() or any(p.is_symlink() for p in (out,*out.parents)):
        raise ValueError('output_requires_absolute_nonlink_path')
    if out.exists():raise ValueError('output_exists')
    git_env={'PATH':'/usr/bin:/bin','TZ':'UTC','LANG':'en_US.UTF-8',
             'GIT_NO_REPLACE_OBJECTS':'1','GIT_CONFIG_NOSYSTEM':'1','GIT_CONFIG_GLOBAL':os.devnull}
    try:
        actual=subprocess.check_output(['/usr/bin/git','-C',str(repository_root),'rev-parse','--show-toplevel'],env=git_env,stderr=subprocess.DEVNULL).decode().strip()
        resolved=subprocess.check_output(['/usr/bin/git','-C',str(repository_root),'rev-parse','--verify',commit+'^{commit}'],env=git_env,stderr=subprocess.DEVNULL).decode().strip()
    except subprocess.CalledProcessError as exc:raise ValueError('invalid_repository_or_commit') from exc
    if Path(actual).resolve()!=repository_root or resolved!=commit:raise ValueError('repository_root_mismatch')
    out.mkdir(mode=0o700,parents=True)
    archive=out/'source.tar'
    with archive.open('xb') as f:
        subprocess.run(['/usr/bin/git','-C',str(repository_root),'archive','--format=tar',commit],env=git_env,stdout=f,stderr=subprocess.DEVNULL,check=True)
    archive.chmod(0o600)
    root=out/'source'
    extract_archive(archive,root)
    entry=root/'scripts/test-project-integration.py'
    if not entry.is_file() or digest(entry)!=digest(Path(__file__)):
        raise ValueError('entry_hash_mismatch')
    data=dict(commit=commit,repository_root=str(repository_root),root=str(root),
              archive=str(archive),archive_sha256=digest(archive),entry_sha256=digest(entry))
    validate_export(data)
    return data


def load_archived_runner(data: Dict[str,str]) -> Callable[[Dict[str,str],Path],int]:
    validate_export(data)
    root=Path(data['root'])/'scripts/integration'
    # A unique package avoids an already-imported working-tree integration module.
    name='_enterprise_im_snapshot_'+hashlib.sha256(str(root).encode()).hexdigest()
    spec=importlib.util.spec_from_file_location(name,root/'__init__.py',submodule_search_locations=[str(root)])
    if spec is None or spec.loader is None:raise ValueError('missing_archived_package')
    module=importlib.util.module_from_spec(spec);sys.modules[name]=module
    previous=sys.dont_write_bytecode
    sys.dont_write_bytecode=True
    try:
        spec.loader.exec_module(module)
        runner=__import__(name+'.run',fromlist=['execute_bootstrap'])
        for key,loaded in list(sys.modules.items()):
            if key==name or key.startswith(name+'.'):
                origin=Path(loaded.__file__).resolve()
                if not origin.is_relative_to(root.resolve()):raise ValueError('module_origin_mismatch')
        return runner.execute_bootstrap
    finally:
        sys.dont_write_bytecode=previous


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-commit',required=True)
    parser.add_argument('--output-dir',required=True,type=Path)
    args=parser.parse_args(argv)
    try:
        data=bootstrap(Path(__file__).resolve().parents[1],args.source_commit,args.output_dir)
        return load_archived_runner(data)(data,args.output_dir)
    except (ValueError,OSError,ImportError,subprocess.SubprocessError):
        # Detailed errors belong to the private report, never to an inherited environment dump.
        print('integration_bootstrap_failed',file=sys.stderr)
        return 1


if __name__=='__main__':sys.exit(main())
