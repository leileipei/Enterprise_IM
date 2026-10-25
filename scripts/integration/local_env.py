"""Owned local native test environment. No production IdP or OS trust mutation."""
import datetime
import hashlib
import io
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import socket
import ssl
import subprocess
import tarfile
import time
from urllib.parse import urlencode,urlsplit
from urllib.request import urlopen
from . import local_env_state as state
from .commands import run_command
from .model import ResourceRef
from .registry import Registry,process_fingerprint
from .services import docker_environment
from .tools import _lock
from .mobile_backend import preflight

def digest(path):return hashlib.sha256(Path(path).read_bytes()).hexdigest()

def validate_start(repo,args):
 repo=Path(repo).resolve();root=Path(args.state_dir);go=Path(args.go)
 if not root.is_absolute() or root.parent.resolve()!=root.parent or not root.parent.is_dir() or root.exists() or root.is_symlink() or not go.is_absolute() or not go.is_file() or type(args.lease_seconds) is not int or not 1<=args.lease_seconds<=14400:raise ValueError('new_private_environment_and_explicit_tools_required')
 p=subprocess.run(['git','status','--porcelain','--untracked-files=all'],cwd=repo,capture_output=True,timeout=15)
 if p.returncode or p.stdout:raise ValueError('clean_frozen_source_required')
 source=subprocess.check_output(['git','rev-parse','HEAD'],cwd=repo,text=True).strip()
 if not re.fullmatch('[a-f0-9]{40}',source):raise ValueError('immutable_source_required')
 return source

def _origin(value):
 try:u=urlsplit(value);port=u.port
 except ValueError:raise ValueError('local_HTTPS_origin_required') from None
 if u.scheme!='https' or u.hostname!='127.0.0.1' or u.username is not None or port is None or not 1024<=port<=65535 or u.path or u.query or u.fragment or value!=('https://127.0.0.1:'+str(port)):raise ValueError('local_HTTPS_origin_required')
 return value

def _connection(value,scheme):
 try:u=urlsplit(value);port=u.port
 except ValueError:raise ValueError('local_connection_required') from None
 if u.scheme!=scheme or u.hostname!='127.0.0.1' or port is None or not 1024<=port<=65535 or not u.password or not u.path or u.fragment:raise ValueError('local_connection_required')
 return value

def native_environment(origin,ca,database,redis,stream,receipts):
 _origin(origin);_connection(database,'postgresql');_connection(redis,'redis');ca=Path(ca)
 if not ca.is_absolute() or ca.resolve()!=ca or ca.is_symlink() or not ca.is_file() or stream==receipts or not all(isinstance(x,str) and re.fullmatch('[A-Za-z0-9:._-]{1,100}',x) for x in [stream,receipts]):raise ValueError('explicit_local_native_config_required')
 env={'IM_DATABASE_URL':database,'IM_OIDC_ENABLED':'true','IM_OIDC_ISSUER':origin+'/oidc','IM_OIDC_AUDIENCE':'enterprise-im-api','IM_OIDC_JWKS_URL':origin+'/oidc/keys','IM_OIDC_ALLOWED_CLIENT_IDS':'im-windows,im-macos,im-android,im-ios','IM_NATIVE_ENABLED':'true','IM_NATIVE_AUTHORIZATION_URL':origin+'/oidc/authorize','IM_NATIVE_TOKEN_URL':origin+'/oidc/token','IM_NATIVE_SCOPE':'openid profile','SSL_CERT_FILE':str(ca),'IM_RECEIPTS_ENABLED':'true','IM_RECEIPT_WRITES_PER_SECOND':'100','IM_DEVICE_REGISTRATIONS_PER_HOUR':'100','IM_REALTIME_REDIS_URL':redis,'IM_REALTIME_STREAM':stream,'IM_RECEIPT_STREAM':receipts}
 for platform in ['windows','macos','android','ios']:
  prefix='IM_NATIVE_'+platform.upper()+'_';env[prefix+'CLIENT_ID']='im-'+platform;env[prefix+'REFRESH_ENABLED']='true';env[prefix+'REDIRECT_URI']='http://127.0.0.1/oauth/callback' if platform in ['windows','macos'] else 'com.enterpriseim.client:/oauth/callback'
 return env

def ready_metadata(value,owner,source):
 keys={'owner','source','origin','issuer','ca_cert','ca_fingerprint','expires_at'}
 if not isinstance(value,dict) or set(value)!=keys or value.get('owner')!=owner or value.get('source')!=source or not isinstance(value.get('ca_fingerprint'),str) or not re.fullmatch('[a-f0-9]{64}',value['ca_fingerprint']):raise ValueError('foreign_identity_ready_metadata')
 _origin(value['origin'])
 if value['issuer']!=value['origin']+'/oidc' or not Path(value['ca_cert']).is_absolute():raise ValueError('invalid_identity_ready_metadata')
 try:
  stamp=value['expires_at'];match=re.fullmatch(r'(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?Z',stamp)
  if match is None:raise ValueError()
  # Go emits RFC3339Nano with 0..9 fraction digits; Python3.9 accepts only 3/6.
  datetime.datetime.fromisoformat(match[1]+'.'+(match[2] or '').ljust(6,'0')[:6]+'+00:00')
 except (TypeError,ValueError):raise ValueError('invalid_identity_ready_expiry') from None
 return {k:value[k] for k in ['origin','issuer','ca_cert','ca_fingerprint']}

class ControlledStop(Exception):pass

class StopSignals:
 def __enter__(self):
  self.requested=False;self.before={key:signal.getsignal(key) for key in [signal.SIGINT,signal.SIGTERM]}
  for key in self.before:signal.signal(key,self.receive)
  return self
 def receive(self,signum,frame):self.requested=True
 def check(self):
  if self.requested:raise ControlledStop()
 def __exit__(self,*args):
  for key,handler in self.before.items():signal.signal(key,handler)


class Environment:
 def __init__(self,repo,args):
  self.repo=Path(repo).resolve();self.args=args;self.source=validate_start(repo,args);self.root=Path(args.state_dir);self.owner=secrets.token_hex(16);self.processes=[];self.secret_values=[];self.public=None;self.registry=None;self.private=None;self.metadata={};self.expected={};self.deadline=None;self.initial_tools=None
 def save(self):
  state.write(self.root,self.public)
 def checkpoint(self):
  if getattr(self,'signals',None):self.signals.check()
  if time.monotonic()>=self.deadline or state.stop_requested(self.root,self.public):raise ControlledStop()
 def docker(self,argv,input=None,check=True):
  p=subprocess.run(['/usr/local/bin/docker',*argv],env=docker_environment(),input=input,capture_output=True,text=True,timeout=40)
  if check and p.returncode:raise RuntimeError('docker_operation_failed')
  return p
 def service(self,role,image,port,extra,command=()):
  self.checkpoint();p=self.docker(['create','--name','im-local-'+role+'-'+self.owner,'--label','im.integration.owner='+self.owner,'--label','im.integration.source='+self.source,'--publish','127.0.0.1::'+port,*extra,image,*command]);cid=p.stdout.strip()
  if not re.fullmatch('[a-f0-9]{64}',cid):raise RuntimeError('container_identity_unproven')
  self.registry.add(ResourceRef('container',self.owner,cid,{'container_id':cid}))
  self.registry.record_container_mounts(cid,json.loads(self.docker(['inspect',cid]).stdout)[0])
  self.docker(['start',cid]);row=json.loads(self.docker(['inspect',cid]).stdout)[0];binding=row['NetworkSettings']['Ports'][port+'/tcp']
  if len(binding)!=1 or binding[0]['HostIp']!='127.0.0.1':raise RuntimeError('container_not_loopback')
  result={'id':cid,'port':int(binding[0]['HostPort']),'image_id':row['Image'],'reference':image};self.public['resources'].setdefault('containers',{})[role]=result;self.save();return result
 def private_file(self,name,value):
  path=self.private/name;fd=os.open(path,os.O_CREAT|os.O_EXCL|os.O_WRONLY|os.O_NOFOLLOW,0o600)
  with os.fdopen(fd,'w') as f:f.write(value);f.flush();os.fsync(f.fileno())
  return path
 def base_env(self):
  env={k:os.environ[k] for k in ['HOME','LANG','TZ'] if k in os.environ};env.update(PATH=str(self.args.go.parent)+':/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin',TMPDIR=str(self.private/'tmp'),GOWORK='off',GOPROXY='off',GOSUMDB='off',GOTOOLCHAIN='local',GOFLAGS='-mod=readonly');return env
 def build(self):
  source=self.private/'source';source.mkdir(mode=0o700);archive=subprocess.check_output(['git','archive',self.source,'cmd','internal','db','testdata','go.mod','go.sum'],cwd=self.repo);inventory={}
  with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
   for member in tar.getmembers():
    if Path(member.name).is_absolute() or '..' in Path(member.name).parts:raise RuntimeError('source_archive_path')
    if member.isdir():continue
    if not member.isfile():raise RuntimeError('source_archive_type')
    content=tar.extractfile(member).read();target=source/member.name;target.parent.mkdir(mode=0o700,parents=True,exist_ok=True);target.write_bytes(content);target.chmod(0o600);inventory[member.name]=hashlib.sha256(content).hexdigest()
  self.metadata.update(source_commit=self.source,archive_sha256=hashlib.sha256(archive).hexdigest(),source_sha256=inventory,commands=[])
  (self.private/'bin').mkdir(mode=0o700)
  for role in ['im-local-test-idp','im-api','im-outbox-worker','im-receipt-worker']:
   self.checkpoint();path=self.private/'bin'/role;argv=[str(self.args.go),'build','-o',str(path),'./cmd/'+role];event=run_command('build-'+role,'local-native-env',self.source,argv,source,self.base_env(),self.private/('build-'+role+'.log'),min(self.deadline,time.monotonic()+120),self.registry);self.metadata['commands'].append({'role':role,'actual_exit':event.exit_code,'failures':event.failures,'argv':argv})
   if event.exit_code or event.failures:
    self.checkpoint();raise RuntimeError('local_binary_build_failed')
   self.expected[role]=digest(path)
  self.metadata['binary_sha256']=dict(self.expected)
 def launch(self,role,extra,argv=None):
  self.checkpoint();binary=self.private/'bin'/role;cmd=argv or [str(binary)];log=(self.private/(role+'.log')).open('xb');p=subprocess.Popen(cmd,cwd=self.private/'source',env=dict(self.base_env(),**extra),stdout=log,stderr=subprocess.STDOUT,start_new_session=True);end=time.monotonic()+2;fp=None
  while fp is None and p.poll() is None and time.monotonic()<end:
   fp=process_fingerprint(p.pid)
   if fp is None:time.sleep(.01)
  entry={'role':role,'process':p,'stream':log,'fingerprint':fp};self.processes.append(entry)
  if fp is None or fp['executable_sha256']!=self.expected[role] or Path(fp['executable_path'])!=binary or not Path(fp['workdir']).is_relative_to(self.private):raise RuntimeError('local_process_identity_unproven')
  self.registry.add(ResourceRef('process',self.owner,str(p.pid),fp));self.public['resources'].setdefault('processes',[]).append({'role':role,'pid':p.pid,'executable_sha256':self.expected[role]});self.save();return entry
 def wait_for(self,condition,seconds=30):
  end=min(self.deadline,time.monotonic()+seconds)
  while time.monotonic()<end:
   self.checkpoint()
   if any(x['process'].poll() is not None for x in self.processes):raise RuntimeError('local_service_exited_before_ready')
   if condition():return
   time.sleep(.1)
  self.checkpoint();raise RuntimeError('local_ready_deadline')
 def mark_ready(self,entry):
  if process_fingerprint(entry['process'].pid)!=entry['fingerprint']:raise RuntimeError('local_ready_process_changed')
  self.registry.lifecycle(str(entry['process'].pid),'ready',{'role':entry['role']})
 def redis(self,command):
  password=self.secret_values[1];cid=self.public['resources']['containers']['redis']['id'];p=self.docker(['exec','-i',cid,'redis-cli','--raw'],input='AUTH '+password+'\n'+command+'\n');values=p.stdout.strip().splitlines()
  if not values or values[0]!='OK':raise RuntimeError('local_redis_auth_failed')
  return values[1:]
 def setup(self):
  self.initial_tools=preflight(self.args.go);self.public=state.create(self.root,self.owner,self.source,self.args.lease_seconds,process_fingerprint(os.getpid()));self.deadline=time.monotonic()+self.args.lease_seconds;self.registry=Registry(self.root/'registry.jsonl',self.owner,self.source);self.private=self.root/self.owner;(self.private/'tmp').mkdir(mode=0o700);self.secret_values=[secrets.token_hex(24) for _ in range(3)];self.private_file('runtime-secrets.json',json.dumps(self.secret_values));self.build()
  pg=_lock(self.repo/'testdata/file-runtime/versions.lock')['postgres-image'];rd=_lock(self.repo/'testdata/project-integration/versions.lock')['redis-image']
  for image in [pg,rd]:self.docker(['image','inspect',image])
  pgfile=self.private_file('postgres.env','POSTGRES_DB=enterprise_im_local\nPOSTGRES_USER=postgres\nPOSTGRES_PASSWORD='+self.secret_values[0]+'\n');rdfile=self.private_file('redis.conf','bind 0.0.0.0\nport 6379\nrequirepass '+self.secret_values[1]+'\nappendonly no\nsave ""\n')
  pgstate=self.service('postgres',pg,'5432',['--env-file',str(pgfile),'--tmpfs','/var/lib/postgresql/data:rw,mode=0700']);rdstate=self.service('redis',rd,'6379',['--user','0','--mount','type=bind,source='+str(rdfile)+',target=/run/im-redis.conf,readonly'],['redis-server','/run/im-redis.conf']);self.wait_for(lambda:self.docker(['exec',pgstate['id'],'pg_isready','-h','127.0.0.1','-U','postgres','-d','enterprise_im_local'],check=False).returncode==0);self.wait_for(lambda:self.redis('PING')==['PONG'])
  with socket.socket() as sock:sock.bind(('127.0.0.1',0));api_port=sock.getsockname()[1]
  cfg={'owner':self.owner,'source':self.source,'private_dir':str(self.private),'listen':'127.0.0.1:0','upstream':'http://127.0.0.1:'+str(api_port),'lease_seconds':self.args.lease_seconds};cfgpath=self.private_file('idp.json',json.dumps(cfg));idp=self.launch('im-local-test-idp',{},[str(self.private/'bin/im-local-test-idp'),'--config',str(cfgpath)])
  ready_path=self.private/'idp-ready.json';self.wait_for(ready_path.exists);ready=ready_metadata(state._json(ready_path),self.owner,self.source)
  if Path(ready['ca_cert'])!=self.private/'tls/ca.pem':raise RuntimeError('local_CA_path_mismatch')
  der=ssl.PEM_cert_to_DER_cert(Path(ready['ca_cert']).read_text())
  if hashlib.sha256(der).hexdigest()!=ready['ca_fingerprint']:raise RuntimeError('local_CA_fingerprint_mismatch')
  self.public.update(ready);self.save();self.tls=ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT);self.tls.load_verify_locations(cafile=ready['ca_cert']);self.wait_for(lambda:self.https('/oidc/keys') is not None);self.mark_ready(idp)
  schema='im_local_'+self.owner;role='im_local_'+self.owner+'_api';self.schema=schema;sql='CREATE EXTENSION btree_gist WITH SCHEMA public; CREATE SCHEMA '+schema+'; SET search_path TO '+schema+',public;\n'
  migrations=sorted((self.private/'source/db/migrations').glob('*.up.sql'))
  if len(migrations)!=26:raise RuntimeError('fixed_local_migration_inventory_changed')
  for migration in migrations:sql+=migration.read_text()+'\n'
  psql=['exec','-i',pgstate['id'],'psql','-X','-v','ON_ERROR_STOP=1','-U','postgres','-d','enterprise_im_local'];self.docker(psql,input=sql)
  seed=(self.private/'source/testdata/local-native-env/seed.sql').read_text();self.docker([*psql,'-v','issuer='+ready['issuer']],input='SET search_path TO '+schema+',public;\n'+seed)
  grants="CREATE ROLE "+role+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '"+self.secret_values[2]+"'; GRANT CONNECT ON DATABASE enterprise_im_local TO "+role+"; GRANT USAGE ON SCHEMA "+schema+" TO "+role+"; GRANT SELECT,INSERT,UPDATE ON ALL TABLES IN SCHEMA "+schema+" TO "+role+"; GRANT USAGE ON ALL SEQUENCES IN SCHEMA "+schema+" TO "+role+"; GRANT DELETE ON "+schema+".native_oauth_request_windows,"+schema+".device_sync_proofs TO "+role+";"
  self.docker(psql,input=grants);dsn='postgresql://'+role+':'+self.secret_values[2]+'@127.0.0.1:'+str(pgstate['port'])+'/enterprise_im_local?'+urlencode({'sslmode':'disable','search_path':schema+',public'});redis='redis://:'+self.secret_values[1]+'@127.0.0.1:'+str(rdstate['port'])+'/0';stream='im:local:'+self.owner+':messages';receipts='im:local:'+self.owner+':receipts'
  outbox=self.launch('im-outbox-worker',{'IM_DATABASE_URL':dsn,'IM_OUTBOX_REDIS_URL':redis,'IM_OUTBOX_STREAM':stream});receipt=self.launch('im-receipt-worker',{'IM_DATABASE_URL':dsn,'IM_RECEIPT_REDIS_URL':redis,'IM_RECEIPT_STREAM':receipts})
  self.wait_for(lambda:self.redis('GET enterprise-im:outbox:publisher:v1:'+hashlib.sha256(stream.encode()).hexdigest())==['active']);self.mark_ready(outbox);self.wait_for(lambda:self.redis('GET enterprise-im:receipt-outbox:publisher:v1:'+hashlib.sha256(receipts.encode()).hexdigest())==['active']);self.mark_ready(receipt)
  env=native_environment(ready['origin'],Path(ready['ca_cert']),dsn,redis,stream,receipts);env['IM_HTTP_ADDR']='127.0.0.1:'+str(api_port);api=self.launch('im-api',env);self.wait_for(lambda:self.https('/health/ready') is not None);self.mark_ready(api)
  for platform in ['android','ios','macos','windows']:
   config=self.https('/api/v1/native/config?platform='+platform)
   if not isinstance(config,dict) or config.get('client_id')!='im-'+platform:raise RuntimeError('native_public_config_mismatch')
  self.public['phase']='ready';self.public['resources']['schema']=schema;self.public['resources']['api_port']=api_port;self.save();self.metadata['ready']=True
 def https(self,path):
  try:
   with urlopen(self.public['origin']+path,context=self.tls,timeout=2) as response:
    data=response.read(65537)
    if response.status!=200 or len(data)>65536:return None
    return json.loads(data)
  except Exception:return None
 def close(self):
  if self.public is None:return []
  failures=[]
  if self.public['phase'] not in ['failed','stopped']:
   self.public['phase']='stopping'
   try:self.save()
   except (ValueError,OSError):failures.append('state_publish_failed')
  for entry in reversed(self.processes):
   p=entry['process']
   try:
    if p.poll() is None:
     if process_fingerprint(p.pid)!=entry['fingerprint']:raise RuntimeError('shutdown_process_identity_changed')
     os.kill(p.pid,signal.SIGTERM)
     try:p.wait(timeout=15)
     except subprocess.TimeoutExpired:raise RuntimeError('shutdown_process_deadline')
    if p.returncode!=0:failures.append('local_process_nonzero_exit')
    if entry['fingerprint']:self.registry.lifecycle(str(p.pid),'exited',{'role':entry['role'],'actual_exit':p.returncode,'expected_exit':0})
   except Exception:failures.append('local_process_cleanup_failed')
   finally:
    try:entry['stream'].close()
    except OSError:failures.append('local_process_log_close_failed')
  try:
   evidence=self.root/'evidence';evidence.mkdir(mode=0o700,exist_ok=True)
   if self.private is not None:
    for path in self.private.glob('*.log*'):
     data=path.read_bytes()
     for value in self.secret_values:data=data.replace(value.encode(),b'[REDACTED]')
     (evidence/path.name).write_bytes(data);(evidence/path.name).chmod(0o600)
    (evidence/'runtime-summary.json').write_text(json.dumps(self.metadata,indent=2)+'\n');(evidence/'runtime-summary.json').chmod(0o600)
  except (ValueError,OSError):failures.append('evidence_publish_failed')
  finally:
   # Evidence publication must never gate independently verified resource cleanup.
   try:
    cleanup=self.registry.cleanup(time.monotonic()+45,preserve_directories=True) if self.registry else None
    if cleanup:failures+=list(cleanup.failures)
   except (ValueError,OSError,subprocess.SubprocessError):failures.append('resource_cleanup_failed')
  # Keep private recovery material unless every process/container is proven gone.
  if not failures and self.private is not None and self.private.exists():
   try:
    info=self.private.stat();self.registry.add(ResourceRef('directory',self.owner,str(self.private),{'device':info.st_dev,'inode':info.st_ino}));cleanup=self.registry.cleanup(time.monotonic()+45);failures+=list(cleanup.failures)
   except (ValueError,OSError,subprocess.SubprocessError):failures.append('private_runtime_cleanup_failed')
  if self.private is not None and self.private.exists():failures.append('private_runtime_root_remains')
  self.public['phase']='failed' if failures or self.public.get('error') else 'stopped'
  if failures:self.public['error']='cleanup_failed'
  try:self.save()
  except (ValueError,OSError):
   failures.append('state_publish_failed');self.public.update(phase='failed',error='cleanup_failed')
  return failures
 def run(self):
  with StopSignals() as self.signals:return self._run()
 def _run(self):
  code=0
  try:
   self.setup();print(json.dumps({'status':'ready',**self.public}),flush=True)
   while True:
    self.checkpoint()
    if any(x['process'].poll() is not None for x in self.processes):raise RuntimeError('local_service_exited')
    time.sleep(.2)
  except ControlledStop:pass
  except Exception as exc:
   code=1;reason=str(exc);self.metadata['error_code']=reason if re.fullmatch('[a-z_]{1,64}',reason) else type(exc).__name__
   if self.public is not None:self.public['phase']='failed';self.public['error']='local_environment_failed';self.save()
  finally:
   if self.close():code=1
  if self.public is not None:print(json.dumps({'status':self.public['phase'],'state_dir':str(self.root),'owner':self.owner,'source':self.source}),flush=True)
  return code
