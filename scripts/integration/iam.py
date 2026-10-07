"""Separate S3 identities and version-specific cleanup; credentials stay private."""
import json
from pathlib import Path
import secrets
import subprocess
import time
from .services import _private_file


def iam_policy(role,bucket,negative):
    if not bucket.startswith('p426-p431-') or negative!=bucket+'-policy':raise ValueError('iam_bucket_not_owned')
    if role not in ('upload','worker','download','cleanup','bootstrap'):raise ValueError('iam_role_unknown')
    path=Path(__file__).resolve().parents[2]/'testdata/project-integration/iam'/(role+'.json')
    template=path.read_text()
    return json.loads(template.replace('{{bucket}}',bucket).replace('{{policy_bucket}}',negative))



def product_credentials(keys):
    result={}
    for role in ('upload','worker','download','cleanup','bootstrap'):
        prefix='IM_FILE_CLEANUP_S3_' if role=='cleanup' else 'IM_TEST_FILE_'+role.upper()+'_'
        for suffix,value in zip(('ACCESS_KEY','SECRET_KEY'),keys[role]):result[prefix+suffix]=value
    return result


def parse_mc_output(command,output):
    # The pinned mc pipe command writes byte progress even with --json.
    # Only stat supplies a structured result consumed as evidence here.
    if command!='stat':return []
    try:rows=[json.loads(line) for line in output.splitlines() if line.strip()]
    except ValueError as exc:raise ValueError('iam_stat_output_invalid') from exc
    if len(rows)!=1 or not isinstance(rows[0],dict) or rows[0].get('status')!='success':
        raise ValueError('iam_stat_output_invalid')
    return rows


def prepare_iam(bundle,tools,private,deadline):
    private=Path(private).resolve();private.mkdir(mode=0o700,parents=True,exist_ok=True)
    config=private/'mc-config';config.mkdir(mode=0o700)
    def mc(args,input=None):
        if time.monotonic()>=deadline:raise ValueError('iam_deadline')
        result=subprocess.run([str(tools.paths['mc']),'--config-dir',str(config),'--json',*args],input=input,
            capture_output=True,text=True,timeout=min(60,deadline-time.monotonic()),
            env={'PATH':'/usr/bin:/bin','LANG':'C.UTF-8','TZ':'UTC'})
        for path in config.rglob('*'):
            if path.is_file():path.chmod(0o600)
            elif path.is_dir():path.chmod(0o700)
        if result.returncode:
            diagnostic=result.stdout+result.stderr
            for secret in sorted(bundle.secrets,key=len,reverse=True):diagnostic=diagnostic.replace(secret,'[REDACTED]')
            _private_file(private/('failure-'+secrets.token_hex(8)+'.json'),diagnostic)
            raise ValueError('iam_command_failed:'+':'.join(args[:3]))
        return parse_mc_output(args[0],result.stdout)
    admin=bundle.metadata['minio_admin'];mc(['alias','set','fixture',bundle.environment['IM_TEST_S3_ENDPOINT'],*admin])
    owner=next(iter(bundle.owners));bucket='p426-p431-'+owner;negative=bucket+'-policy'
    for name in (bucket,negative):
        mc(['mb','fixture/'+name]);mc(['version','enable','fixture/'+name]);mc(['anonymous','set','none','fixture/'+name])
    keys={'admin':admin}
    for role in ('upload','worker','download','cleanup','bootstrap'):
        key='p431'+role+secrets.token_hex(8);secret=secrets.token_hex(24);keys[role]=(key,secret)
        bundle.secrets.update((key,secret));policy=private/(role+'-policy.json')
        _private_file(policy,json.dumps(iam_policy(role,bucket,negative)))
        mc(['admin','user','add','fixture',key,secret]);mc(['admin','policy','create','fixture','p431-'+role,str(policy)])
        mc(['admin','policy','attach','fixture','p431-'+role,'--user',key])
    mc(['alias','set','bootstrap',bundle.environment['IM_TEST_S3_ENDPOINT'],*keys['bootstrap']])
    mc(['pipe','bootstrap/'+bucket+'/_im_runtime/read-probe/v1'],input='enterprise-im-file-read-probe-v1\n')
    stat=mc(['stat','bootstrap/'+bucket+'/_im_runtime/read-probe/v1'])
    version=stat[0].get('versionID')
    if not version or version=='null':raise ValueError('iam_probe_version_unproven')
    bundle.environment.update(product_credentials(keys))
    bundle.environment.update(IM_TEST_S3_BUCKET=bucket,IM_TEST_S3_POLICY_BUCKET=negative,IM_TEST_INTEGRATION_PROBE_VERSION=version)
    bundle.metadata.update(bucket=bucket,policy_bucket=negative,probe_version=version,iam_roles=list(keys),iam_private_root=str(private))
    return bundle
