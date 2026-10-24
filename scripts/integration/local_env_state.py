"""Private, owner/source-bound state and stop requests for local test environments."""
import copy
import json
import math
import os
from pathlib import Path
import re
import secrets
import stat
import time
from urllib.parse import urlsplit
from .registry import process_fingerprint

SCHEMA='im_local_native_env_state_v1'
STOP_SCHEMA='im_local_native_env_stop_v1'
SHA=re.compile(r'[a-f0-9]{64}')
FIELDS={'schema','owner','source','created_at','expires_at','phase','supervisor','origin','issuer','ca_cert','ca_fingerprint','error','resources'}
PHASES={'preparing','ready','stopping','stopped','failed'}

def _number(value):
 return type(value) in (int,float) and math.isfinite(value)

def _root(root):
 root=Path(root).absolute()
 try:
  if root.resolve(strict=True)!=root:raise ValueError('linked_environment_root')
  info=root.lstat()
  if not stat.S_ISDIR(info.st_mode) or info.st_mode&0o777!=0o700 or info.st_uid!=os.getuid():raise ValueError('private_environment_root_required')
 except OSError:raise ValueError('environment_root_unavailable') from None
 return root

def _json(path,budget=65536):
 try:
  fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
  with os.fdopen(fd,'rb') as stream:
   info=os.fstat(stream.fileno())
   if not stat.S_ISREG(info.st_mode) or info.st_mode&0o777!=0o600 or info.st_uid!=os.getuid() or info.st_nlink!=1 or info.st_size>budget:raise ValueError('private_environment_file_required')
   raw=stream.read(budget+1)
 except OSError:raise ValueError('environment_file_unavailable') from None
 if len(raw)>budget:raise ValueError('environment_file_budget')
 def pairs(items):
  result={}
  for key,value in items:
   if key in result:raise ValueError('ambiguous_environment_json')
   result[key]=value
  return result
 def constant(value):raise ValueError('invalid_environment_number')
 try:return json.loads(raw,object_pairs_hook=pairs,parse_constant=constant)
 except (ValueError,UnicodeError):raise ValueError('invalid_environment_json') from None

def _no_secrets(value,depth=0):
 if depth>8:raise ValueError('environment_state_budget')
 if isinstance(value,dict):
  if len(value)>128:raise ValueError('environment_state_budget')
  for key,item in value.items():
   if not isinstance(key,str) or len(key)>128 or any(x in key.lower() for x in ['password','secret','token','private_key','server_key','ca_key','database_url','dsn']):raise ValueError('private_material_in_public_state')
   _no_secrets(item,depth+1)
 elif isinstance(value,list):
  if len(value)>256:raise ValueError('environment_state_budget')
  for item in value:_no_secrets(item,depth+1)
 elif isinstance(value,str):
  if len(value)>4096 or 'PRIVATE KEY' in value:raise ValueError('private_material_in_public_state')
  if '://' in value:
   try:u=urlsplit(value)
   except ValueError:raise ValueError('invalid_state_url') from None
   if u.username is not None or u.password is not None:raise ValueError('credential_url_in_public_state')
 elif value is not None and not isinstance(value,bool) and not _number(value):raise ValueError('invalid_environment_value')

def _fingerprint(value):
 keys={'pid','uid','start_time','executable_path','executable_sha256','workdir','pgid'}
 if not isinstance(value,dict) or set(value)!=keys:raise ValueError('supervisor_identity_required')
 if type(value['pid']) is not int or value['pid']<=1 or value['uid']!=os.getuid() or type(value['pgid']) is not int or value['pgid']<=0 or not isinstance(value['start_time'],str) or not value['start_time'] or not isinstance(value['executable_sha256'],str) or not SHA.fullmatch(value['executable_sha256']):raise ValueError('supervisor_identity_required')
 if not all(isinstance(value[k],str) and Path(value[k]).is_absolute() for k in ['executable_path','workdir']):raise ValueError('supervisor_identity_required')

def _validate(root,data):
 required={'schema','owner','source','created_at','expires_at','phase','supervisor','resources'}
 if not isinstance(data,dict) or not required<=set(data) or not set(data)<=FIELDS or data['schema']!=SCHEMA or not isinstance(data['owner'],str) or not re.fullmatch('[a-f0-9]{32}',data['owner']) or not isinstance(data['source'],str) or not re.fullmatch('[a-f0-9]{40}',data['source']) or data['phase'] not in PHASES:raise ValueError('invalid_environment_state')
 _no_secrets(data);_fingerprint(data['supervisor'])
 if not _number(data['created_at']) or not _number(data['expires_at']) or not 1<=data['expires_at']-data['created_at']<=14400:raise ValueError('invalid_environment_lease')
 if not isinstance(data['resources'],dict):raise ValueError('invalid_environment_resources')
 if 'origin' in data:
  u=urlsplit(data['origin'])
  try:port=u.port
  except ValueError:raise ValueError('invalid_environment_origin') from None
  if u.scheme!='https' or u.hostname!='127.0.0.1' or port is None or not 1024<=port<=65535 or u.path or u.query or u.fragment or u.username is not None:raise ValueError('invalid_environment_origin')
 if 'issuer' in data and data['issuer']!=data.get('origin','')+'/oidc':raise ValueError('invalid_environment_issuer')
 if 'ca_cert' in data:
  expected=root/data['owner']/'tls/ca.pem'
  if Path(data['ca_cert'])!=expected or expected.resolve()!=expected:raise ValueError('foreign_environment_CA')
 if 'ca_fingerprint' in data and (not isinstance(data['ca_fingerprint'],str) or not SHA.fullmatch(data['ca_fingerprint'])):raise ValueError('invalid_environment_CA')
 if 'error' in data and (not isinstance(data['error'],str) or not re.fullmatch('[a-z_]{1,64}',data['error'])):raise ValueError('invalid_public_environment_error')
 if data['phase']=='ready' and not {'origin','issuer','ca_cert','ca_fingerprint'}<=set(data):raise ValueError('ready_environment_metadata_required')
 return data

def _save(root,path,data,new=False):
 content=json.dumps(data,sort_keys=True).encode()+b'\n'
 if len(content)>65536:raise ValueError('environment_file_budget')
 temporary=path if new else root/('.state-'+secrets.token_hex(16)+'.json')
 try:
  fd=os.open(temporary,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
  with os.fdopen(fd,'wb') as stream:stream.write(content);stream.flush();os.fsync(stream.fileno())
  if not new:os.replace(temporary,path)
 except OSError:raise ValueError('environment_write_failed') from None
 finally:
  if not new and temporary.exists():temporary.unlink()

def create(root,owner,source,lease_seconds,supervisor,now=None):
 root=Path(root).absolute();now=time.time() if now is None else now
 if not isinstance(owner,str) or not re.fullmatch('[a-f0-9]{32}',owner) or not isinstance(source,str) or not re.fullmatch('[a-f0-9]{40}',source) or type(lease_seconds) is not int or not 1<=lease_seconds<=14400 or not _number(now):raise ValueError('invalid_environment_admission')
 _fingerprint(supervisor)
 if root.parent.resolve(strict=True)!=root.parent:raise ValueError('linked_environment_parent')
 try:root.mkdir(mode=0o700)
 except OSError:raise ValueError('new_environment_root_required') from None
 data={'schema':SCHEMA,'owner':owner,'source':source,'created_at':now,'expires_at':now+lease_seconds,'phase':'preparing','supervisor':copy.deepcopy(supervisor),'resources':{}}
 _validate(root,data);_save(root,root/'state.json',data,new=True);return data

def read(root):
 root=_root(root);return _validate(root,_json(root/'state.json'))

def write(root,data):
 root=_root(root);before=read(root);_validate(root,data)
 if any(data[k]!=before[k] for k in ['owner','source','created_at','expires_at','supervisor']):raise ValueError('environment_identity_changed')
 if before['phase'] in ['stopped','failed'] and data['phase'] not in [before['phase'],'stopped']:raise ValueError('terminal_environment_cannot_restart')
 _save(root,root/'state.json',data)

def snapshot(root,now=None,fingerprint_reader=process_fingerprint):
 data=read(root);now=time.time() if now is None else now
 if not _number(now):raise ValueError('invalid_observation_time')
 result=copy.deepcopy(data);result['expired']=now>=data['expires_at'];result['supervisor_live']=fingerprint_reader(data['supervisor']['pid'])==data['supervisor'];return result

def _stop(root,data):
 value=_json(root/'stop.json',8192)
 required={'schema','owner','source','request_id','created_at','supervisor'}
 if not isinstance(value,dict) or set(value)!=required or value['schema']!=STOP_SCHEMA or value['owner']!=data['owner'] or value['source']!=data['source'] or value['supervisor']!=data['supervisor'] or not isinstance(value['request_id'],str) or not re.fullmatch('[a-f0-9]{32}',value['request_id']) or not _number(value['created_at']) or value['created_at']<data['created_at']:raise ValueError('foreign_environment_stop_request')
 return value

def request_stop(root,now=None,fingerprint_reader=process_fingerprint):
 root=_root(root);data=read(root);now=time.time() if now is None else now
 if data['phase']=='stopped':return {'status':'already_stopped'}
 if fingerprint_reader(data['supervisor']['pid'])!=data['supervisor']:return {'status':'recovery_required'}
 if not _number(now) or now<data['created_at']:raise ValueError('invalid_stop_time')
 path=root/'stop.json'
 if path.exists() or path.is_symlink():value=_stop(root,data)
 else:
  value={'schema':STOP_SCHEMA,'owner':data['owner'],'source':data['source'],'request_id':secrets.token_hex(16),'created_at':now,'supervisor':data['supervisor']};_save(root,path,value,new=True)
 return {'status':'requested','request_id':value['request_id']}

def stop_requested(root,data):
 root=_root(root);path=root/'stop.json'
 if not path.exists() and not path.is_symlink():return False
 _stop(root,data);return True
