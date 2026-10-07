"""Disposable local data services. Registry owns every created container."""
import json
import os
from pathlib import Path
import secrets
import subprocess
import time
import urllib.parse
import urllib.request
from .model import FixtureBundle,GateEvent,ResourceRef

DOCKER_HOST='unix:///Users/leo.cui/.orbstack/run/docker.sock'


def docker_environment():
    socket=Path(DOCKER_HOST[7:])
    if not socket.is_socket() or socket.stat().st_uid!=os.getuid():
        raise ValueError('local_docker_socket_unproven')
    return {'PATH':'/usr/local/bin:/usr/bin:/bin','LANG':'en_US.UTF-8','TZ':'UTC','DOCKER_HOST':DOCKER_HOST}


def service_plan(images,private,owner,commit):
    private=Path(private)
    result={}
    for role,image,port in [('postgres','postgres-image','5432'),('redis','redis-image','6379'),('minio','minio-image','9000')]:
        args=['create','--label','im.integration.owner='+owner,'--label','im.integration.source='+commit,
              '--publish','127.0.0.1::'+port,'--name','im-p431-'+role+'-'+owner]
        if role=='postgres':
            args+=['--env-file',str(private/'postgres.env'),'--tmpfs','/var/lib/postgresql/data:rw,mode=0700',
                   '--tmpfs','/tls:rw,mode=0700','--mount','type=bind,src='+str(private/'tls')+',dst=/fixture-tls,readonly',
                   '--entrypoint','/bin/sh',images[image]['reference'],'-c',
                   'cp /fixture-tls/* /tls/ && chown -R postgres:postgres /tls && chmod 600 /tls/* && exec docker-entrypoint.sh "$@"',
                   'p431-tls','postgres','-c','ssl=on','-c','ssl_cert_file=/tls/server.crt','-c','ssl_key_file=/tls/server.key',
                   '-c','ssl_ca_file=/tls/ca.crt']
        elif role=='redis':args+=[images[image]['reference'],'redis-server','--save','','--appendonly','no']
        else:args+=['--env-file',str(private/'minio.env'),'--tmpfs','/data:rw,mode=0700',images[image]['reference'],'server','/data','--console-address',':9001']
        result[role]={'argv':args,'port':port,'image':images[image]['reference']}
    result['postgres'].update(database='enterprise_im_files',product_roles=['p431_api','p431_repair','p431_import_writer'])
    return result


def _private_file(path,text):
    fd=os.open(path,os.O_CREAT|os.O_EXCL|os.O_WRONLY|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,'w') as out:out.write(text)


def _docker(tools,args,deadline,input=None):
    remaining=deadline-time.monotonic()
    if remaining<=0:raise ValueError('service_deadline')
    result=subprocess.run([str(tools.paths['docker']),*args],input=input,env=docker_environment(),
                          capture_output=True,text=True,timeout=min(60,remaining))
    if result.returncode:raise ValueError('service_command_failed:'+args[0])
    return result.stdout


def _generate_tls(tools,private,deadline):
    tls=private/'tls';tls.mkdir(mode=0o700)
    for name in ('ca','wrong-ca'):
        args=['req','-x509','-newkey','rsa:2048','-nodes','-days','2','-subj','/CN=p431-'+name,
              '-keyout',str(tls/(name+'.key')),'-out',str(tls/(name+'.crt'))]
        _openssl(tools,args,deadline)
    _openssl(tools,['req','-newkey','rsa:2048','-nodes','-subj','/CN=127.0.0.1',
                    '-keyout',str(tls/'server.key'),'-out',str(tls/'server.csr')],deadline)
    _private_file(tls/'extensions','subjectAltName=IP:127.0.0.1\nextendedKeyUsage=serverAuth\n')
    _openssl(tools,['x509','-req','-in',str(tls/'server.csr'),'-CA',str(tls/'ca.crt'),
                    '-CAkey',str(tls/'ca.key'),'-CAcreateserial','-days','2',
                    '-extfile',str(tls/'extensions'),'-out',str(tls/'server.crt')],deadline)
    for path in tls.iterdir():path.chmod(0o600)


def _openssl(tools,args,deadline):
    result=subprocess.run([str(tools.paths['openssl']),*args],capture_output=True,
                          env={'PATH':'/usr/bin:/bin','LANG':'C.UTF-8','TZ':'UTC'},
                          timeout=max(0.1,min(60,deadline-time.monotonic())))
    if result.returncode:raise ValueError('fixture_tls_generation_failed')


def prepare_services(snapshot,tools,registry,private,deadline):
    private=Path(private).resolve()
    if not private.is_relative_to(registry.root):raise ValueError('service_private_root_not_owned')
    private.mkdir(mode=0o700,parents=True,exist_ok=True)
    secret_values={'postgres':secrets.token_hex(24),'minio_key':'p431'+secrets.token_hex(12),'minio_secret':secrets.token_hex(24)}
    roles={role:secrets.token_hex(24) for role in ('p431_api','p431_repair','p431_import_writer')}
    try:
        docker_environment();_generate_tls(tools,private,deadline)
        _private_file(private/'postgres.env','POSTGRES_DB=enterprise_im_files\nPOSTGRES_USER=postgres\nPOSTGRES_PASSWORD='+secret_values['postgres']+'\n')
        _private_file(private/'minio.env','MINIO_ROOT_USER='+secret_values['minio_key']+'\nMINIO_ROOT_PASSWORD='+secret_values['minio_secret']+'\n')
        plans=service_plan(tools.images,private,registry.owner,snapshot.commit);ids={};ports={}
        for role,plan in plans.items():
            cid=_docker(tools,plan['argv'],deadline).strip()
            if len(cid)!=64 or any(c not in '0123456789abcdef' for c in cid):raise ValueError('service_identity_invalid')
            registry.add(ResourceRef('container',registry.owner,cid,{'container_id':cid}));ids[role]=cid
            _docker(tools,['start',cid],deadline)
            inspect=json.loads(_docker(tools,['inspect',cid],deadline))[0]
            binding=inspect['NetworkSettings']['Ports'][plan['port']+'/tcp']
            if len(binding)!=1 or binding[0]['HostIp']!='127.0.0.1':raise ValueError('service_not_loopback')
            ports[role]=int(binding[0]['HostPort'])
        while True:
            try:
                _docker(tools,['exec',ids['postgres'],'pg_isready','-U','postgres','-d','enterprise_im_files'],deadline)
                break
            except ValueError:
                if time.monotonic()>=deadline:raise
                time.sleep(0.2)
        sql='CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;\n'
        for role,password in roles.items():
            sql+="CREATE ROLE "+role+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '"+password+"';\nGRANT CONNECT,CREATE ON DATABASE enterprise_im_files TO "+role+";\n"
        _docker(tools,['exec','-i',ids['postgres'],'psql','-X','-v','ON_ERROR_STOP=1','-U','postgres','-d','enterprise_im_files'],deadline,sql)
        dsn='postgresql://postgres:'+secret_values['postgres']+'@127.0.0.1:'+str(ports['postgres'])+'/enterprise_im_files?sslmode=verify-full&sslrootcert='+urllib.parse.quote(str(private/'tls/ca.crt'),safe='')
        env={'IM_TEST_DATABASE_URL':dsn,'IM_TEST_REDIS_URL':'redis://127.0.0.1:'+str(ports['redis']),
             'IM_TEST_S3_ENDPOINT':'http://127.0.0.1:'+str(ports['minio'])}
        metadata={'source_commit':snapshot.commit,'private_root':str(private),'container_ids':ids,'ports':ports,
                  'tls_ca':str(private/'tls/ca.crt'),'tls_wrong_ca':str(private/'tls/wrong-ca.crt'),
                  'database':'enterprise_im_files','product_roles':list(roles),'role_passwords':roles,
                  'minio_admin':(secret_values['minio_key'],secret_values['minio_secret']),
                  'admin_database_url':dsn}
        bundle=FixtureBundle(env,set(secret_values.values())|set(roles.values()),registry.path,{registry.owner},metadata)
        event=probe_services(bundle,tools,deadline)
        if event.exit_code:raise ValueError('service_probe_failed')
        return bundle
    except BaseException:
        cleanup=registry.cleanup(time.monotonic()+30)
        if not cleanup.removed:registry.write('preparation_cleanup_failed',registry.owner,detail={'failures':cleanup.failures})
        raise


def probe_services(bundle,tools,deadline):
    meta=bundle.metadata;ids=meta['container_ids'];log=Path(meta['private_root'])/('service-preflight-'+secrets.token_hex(8)+'.json')
    checks=[];failure=[]
    try:
        for role,cid in ids.items():
            inspect=json.loads(_docker(tools,['inspect',cid],deadline))[0]
            labels=inspect['Config']['Labels']
            if labels['im.integration.source']!=meta['source_commit'] or labels['im.integration.owner'] not in bundle.owners:raise ValueError('service_owner_mismatch')
            if not inspect['State']['Running']:raise ValueError('service_not_running')
            checks.append(role+'_ownership_loopback')
        pgargs=['exec',ids['postgres'],'psql','-X','-v','ON_ERROR_STOP=1',
                'host=127.0.0.1 dbname=enterprise_im_files user=postgres sslmode=verify-full sslrootcert=/tls/ca.crt',
                '-At','-c',"SELECT ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid(); SELECT extname FROM pg_extension WHERE extname='btree_gist'; SELECT count(*) FROM pg_roles WHERE rolname IN ('p431_api','p431_repair','p431_import_writer') AND NOT rolsuper AND NOT rolcreaterole;"]
        # Server-side psql still authenticates using a private root password, supplied over stdin.
        password=bundle.environment['IM_TEST_DATABASE_URL'].split('postgres:',1)[1].split('@',1)[0]
        result=_docker(tools,['exec','-i',ids['postgres'],'/bin/sh','-c','read PGPASSWORD; export PGPASSWORD; exec "$@"','p431-probe',*pgargs[2:]],deadline,password+'\n')
        if result.strip().splitlines()!=['t','btree_gist','3']:raise ValueError('service_pg_tls_or_roles_failed')
        checks+=['postgres_actual_tls','btree_gist','ordinary_product_roles']
        if _docker(tools,['exec',ids['redis'],'redis-cli','PING'],deadline).strip()!='PONG':raise ValueError('service_redis_ping_failed')
        checks.append('redis_ping')
        while True:
            try:
                with urllib.request.urlopen(bundle.environment['IM_TEST_S3_ENDPOINT']+'/minio/health/ready',timeout=2) as response:
                    if response.status!=200:raise ValueError('service_minio_health_failed')
                break
            except OSError:
                if time.monotonic()+0.2>=deadline:raise ValueError('service_minio_not_ready')
                time.sleep(0.2)
        checks.append('minio_live_ready')
    except (ValueError,OSError,KeyError,subprocess.SubprocessError) as exc:failure=[str(exc) if isinstance(exc,ValueError) else 'service_probe_error']
    _private_file(log,json.dumps({'checks':checks,'failures':failure})+'\n')
    return GateEvent('fixture_preflight','check',meta['source_commit'],int(bool(failure)),log,checks=checks,failures=failure)
