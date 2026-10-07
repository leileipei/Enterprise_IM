"""Owned Linux resource launcher used by the unchanged run-all entry point."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time
from .model import ResourceRef
from .registry import Registry
from .services import docker_environment


def docker(args):
    cli=shutil.which('docker')
    if not cli:raise ValueError('resource_docker_missing')
    result=subprocess.run([cli,*args],env=docker_environment(),capture_output=True,text=True,timeout=1800)
    if result.returncode:raise ValueError('resource_docker_command_failed:'+args[0])
    return result.stdout


def launch(args,registry,proof):
    proof=Path(proof)
    if not proof.resolve().is_relative_to(registry.root/registry.owner) or proof.is_symlink():raise ValueError('resource_proof_not_owned')
    rows=registry.records()
    if not any(r['event']=='reserve' and r['owner']==registry.owner for r in rows):raise ValueError('resource_owner_not_reserved')
    cid=None;report={'source_commit':registry.commit,'owner':registry.owner,'exit_code':1,'cleanup':False}
    try:
        options=[a for a in args if a!='--rm']
        cid=docker(['create','--label','im.integration.owner='+registry.owner,'--label','im.integration.source='+registry.commit,*options]).strip()
        if len(cid)!=64 or any(x not in '0123456789abcdef' for x in cid):raise ValueError('resource_invalid_container_id')
        registry.add(ResourceRef('container',registry.owner,cid,{'container_id':cid}))
        actual=json.loads(docker(['inspect',cid]))[0]
        if actual.get('Id')!=cid or actual.get('Config',{}).get('Labels',{}).get('im.integration.source')!=registry.commit or actual['Config']['Labels'].get('im.integration.owner')!=registry.owner:
            raise ValueError('resource_container_identity_mismatch')
        host=actual['HostConfig'];report.update(container_id=cid,image_id=actual.get('Image',''),memory=host.get('Memory'),nano_cpus=host.get('NanoCpus'),tmpfs=host.get('Tmpfs',{}))
        output=docker(['start','--attach',cid]);sys.stdout.write(output);sys.stdout.flush()
        # start --attach status is the actual contained test binary status.
        report['exit_code']=0
    finally:
        if cid:
            actual=registry._owned_container(cid,registry.owner)
            if actual and actual.get('State',{}).get('Status')!='removing':docker(['rm','-f',cid])
            deadline=time.monotonic()+15
            while registry._owned_container(cid,registry.owner) is not None and time.monotonic()<deadline:time.sleep(.1)
            report['cleanup']=registry._owned_container(cid,registry.owner) is None
        fd=os.open(proof,os.O_CREAT|os.O_EXCL|os.O_WRONLY|os.O_NOFOLLOW,0o600)
        with os.fdopen(fd,'w') as stream:json.dump(report,stream,indent=2);stream.write('\n')
    if not report['cleanup']:raise ValueError('resource_cleanup_unconfirmed')
    return report


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--proof',type=Path,required=True);parser.add_argument('docker_args',nargs=argparse.REMAINDER)
    args=parser.parse_args(argv)
    try:
        registry_path=Path(os.environ['IM_TEST_INTEGRATION_REGISTRY'])
        if not registry_path.is_file():raise ValueError('parent_resource_registry_missing')
        registry=Registry(registry_path,os.environ['IM_TEST_INTEGRATION_OWNER'],os.environ['IM_TEST_INTEGRATION_SOURCE_SHA'])
        return launch(args.docker_args[1:] if args.docker_args[:1]==['--'] else args.docker_args,registry,args.proof)['exit_code']
    except (KeyError,ValueError,OSError,subprocess.SubprocessError):print('owned_resource_command_failed',file=sys.stderr);return 1


if __name__=='__main__':raise SystemExit(main())
