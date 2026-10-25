"""New-owned-only simulator admission for strict local CA testing."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
from .android_simulator import validate_device
from . import local_env_state as state

def digest(path):
 h=hashlib.sha256()
 with Path(path).open('rb') as f:
  for chunk in iter(lambda:f.read(1024*1024),b''):h.update(chunk)
 return h.hexdigest()

def _private(path,owner,source):
 path=Path(path)
 if not re.fullmatch('[a-f0-9]{32}',owner) or not re.fullmatch('[a-f0-9]{40}',source) or not path.is_absolute() or path.resolve()!=path or path.name!=owner:raise ValueError('owned_simulator_private_root_required')
 info=path.lstat()
 if not stat.S_ISDIR(info.st_mode) or info.st_uid!=os.getuid() or info.st_mode&0o777!=0o700:raise ValueError('owned_simulator_private_root_required')
 return path

def _inventory(root,exclude=()):
 root=Path(root)
 if not root.is_absolute() or root.resolve()!=root or not root.is_dir():raise ValueError('canonical_image_required')
 result={}
 for parent,dirs,files in os.walk(root,followlinks=False):
  for name in dirs+files:
   path=Path(parent)/name;info=path.lstat()
   if path.is_symlink() or info.st_uid!=os.getuid() or not (stat.S_ISDIR(info.st_mode) or stat.S_ISREG(info.st_mode)) or (stat.S_ISREG(info.st_mode) and info.st_nlink!=1):raise ValueError('linked_or_foreign_image_member')
  for name in files:
   path=Path(parent)/name;relative=path.relative_to(root).as_posix()
   if relative not in exclude:result[relative]=digest(path)
 if not {'system.img','ramdisk.img','kernel-ranchu','source.properties'}<=set(result) or not ({'userdata.img','data/empty_data_disk'}&set(result)):raise ValueError('incomplete_private_system_image')
 return result

def stage_private_image(base,private,owner,source):
 private=_private(private,owner,source);base=Path(base);before=_inventory(base);target=private/'android-system-image'
 if target.exists() or target.is_symlink():raise ValueError('new_private_image_required')
 target.mkdir(mode=0o700)
 for name in sorted(before):
  original=base/name;path=target/name;path.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
  with original.open('rb') as src:
   fd=os.open(path,os.O_CREAT|os.O_EXCL|os.O_WRONLY|os.O_NOFOLLOW,0o600)
   with os.fdopen(fd,'wb') as dst:shutil.copyfileobj(src,dst,1024*1024)
 if _inventory(base)!=before or _inventory(target)!=before:raise ValueError('private_image_copy_mismatch')
 value={'schema':'im_owned_private_system_image_v1','owner':owner,'source':source,'base':str(base),'target':str(target),'sha256':before};state._save(target,target/'im-private-image.json',value,new=True);return value

def private_image_arguments(private,image,owner,source):
 if image is None:return []
 private=_private(private,owner,source);image=Path(image)
 if image!=private/'android-system-image' or image.resolve()!=image:raise ValueError('foreign_private_system_image')
 value=state._json(image/'im-private-image.json',1048576)
 if set(value)!={'schema','owner','source','base','target','sha256'} or value['schema']!='im_owned_private_system_image_v1' or value['owner']!=owner or value['source']!=source or value['target']!=str(image) or value['sha256']!=_inventory(image,('im-private-image.json',)) or value['sha256']!=_inventory(Path(value['base'])):raise ValueError('private_image_binding_changed')
 return ['-sysdir',str(image),'-writable-system']

def pinned_artifact(path,expected):
 path=Path(path)
 if not isinstance(expected,str) or not re.fullmatch('[a-f0-9]{64}',expected) or not path.is_absolute() or path.resolve()!=path or not path.is_file() or path.stat().st_nlink!=1 or digest(path)!=expected:raise ValueError('ordinary_artifact_pin_mismatch')
 return expected

def admit_new_ios(inventory,runtime,name,udid):
 if runtime!='com.apple.CoreSimulator.SimRuntime.iOS-26-5' or not re.fullmatch('IM-Local-[a-f0-9]{32}',name) or udid is not None or not isinstance(inventory,dict) or not isinstance(inventory.get('devices'),dict) or runtime not in inventory['devices']:raise ValueError('new_owned_ios_required')
 for devices in inventory['devices'].values():
  if not isinstance(devices,list):raise ValueError('invalid_ios_inventory')
  if any(row.get('name')==name for row in devices):raise ValueError('ios_name_already_exists')

def admit_android_transport(value,owner):
 if not re.fullmatch('[a-f0-9]{32}',owner) or value.get('name')!='IM-Mobile-Android-'+owner:raise ValueError('owned_android_transport_required')
 console=value.get('console_port');adb=value.get('adb_server_port')
 if type(console) is not int or not 5554<=console<=5682 or console%2 or type(adb) is not int or not 1024<=adb<=65535 or adb in [5037,console,console+1] or value.get('serial')!='emulator-'+str(console):raise ValueError('owned_android_transport_required')
 validate_device(value.get('properties',{}),value['name'],{'api':36,'abi':'arm64-v8a'})

def environment_CA(root,owner,source):
 import ssl
 observed=state.snapshot(root)
 if observed['owner']!=owner or observed['source']!=source or observed['phase']!='ready' or observed['expired'] or not observed['supervisor_live']:raise ValueError('live_owned_environment_required')
 ca=Path(observed['ca_cert']);info=ca.lstat()
 if ca.resolve()!=ca or not stat.S_ISREG(info.st_mode) or info.st_uid!=os.getuid() or info.st_nlink!=1 or info.st_mode&0o777!=0o600:raise ValueError('exact_private_environment_CA_required')
 try:
  der=ssl.PEM_cert_to_DER_cert(ca.read_text());context=ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT);context.load_verify_locations(cafile=str(ca))
 except (OSError,ValueError,ssl.SSLError):raise ValueError('invalid_environment_CA') from None
 if hashlib.sha256(der).hexdigest()!=observed['ca_fingerprint']:raise ValueError('environment_CA_fingerprint_changed')
 return ca

IOS_ARCHIVE_SHA256='3244f4eda3b4c8af79459164c6d25067db5304abb2be2c505f8aa7c63ecf3ade'
ANDROID_APK_SHA256='eb5aeaf86dd9564bae6790200c1e32aec413e3df5fcaf7e887bd4d2c9c9b6bcf'
PACKAGE='com.enterpriseim.client'

def extract_ios(archive,private):
 import zipfile
 target=private/'ordinary-ios';target.mkdir(mode=0o700)
 with zipfile.ZipFile(archive) as zipped:
  if len(zipped.namelist())!=len(set(zipped.namelist())):raise ValueError('ambiguous_ordinary_archive')
  for member in zipped.infolist():
   path=Path(member.filename)
   if path.is_absolute() or '..' in path.parts or path.parts[0]!='Runner.app' or stat.S_ISLNK(member.external_attr>>16):raise ValueError('invalid_ordinary_archive_path')
   dest=target/path
   if member.is_dir():dest.mkdir(mode=0o700,parents=True,exist_ok=True);continue
   dest.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
   with dest.open('xb') as f:f.write(zipped.read(member))
   dest.chmod(0o700 if member.external_attr>>16&0o111 else 0o600)
 return target/'Runner.app'

def _write_command_log(stream,data):
 stream.write(data);stream.flush();os.fsync(stream.fileno())

class OwnedIOS:
 """Ordinary app and CA only in a newly reserved exact iOS UUID."""
 def __init__(self,env,owner,source,archive,ca):
  from .mobile import simulator_choice
  self.env=env;self.owner=owner;self.source=source;self.private=_private(env['private'],owner,source);self.out=Path(env['out']);self.out.mkdir(mode=0o700,exist_ok=True);self.udid=None;self.reserved=False;self.ca=Path(ca);self.name='IM-Local-'+owner;self.runtime='com.apple.CoreSimulator.SimRuntime.iOS-26-5';self.device_type='com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro';self.state={'owner':owner,'source':source,'name':self.name,'runtime':self.runtime,'device_type':self.device_type,'commands':[],'status':'preparing','artifact_source':'f3d50f65c5e840635a04ccfe9674303ac070aac5','archive_sha256':IOS_ARCHIVE_SHA256}
  self.context();simulator_choice(json.loads(self.call('runtime-inventory',['list','runtimes','-j'])),self.runtime,self.device_type);self.app=extract_ios(archive,self.private)
 def save(self):
  path=self.out/'ios-state.json';temporary=self.out/'.ios-state-new';temporary.write_text(json.dumps(self.state,indent=2)+'\n');temporary.chmod(0o600);temporary.replace(path)
 def context(self):
  if self.ca!=environment_CA(self.env['environment'],self.env['host_owner'],self.env['host_source']):raise ValueError('foreign_simulator_CA')
 def call(self,label,args,timeout=90):
  import subprocess
  from contextlib import ExitStack
  argv=['/usr/bin/xcrun','simctl',*args];expired=None
  index=len(self.state['commands']);file=self.out/('%03d-'%index+label+'.log');error=Path(str(file)+'.stderr')
  if any(p.exists() or p.is_symlink() for p in [file,error]):raise ValueError('owned_ios_evidence_path_exists')
  try:
   with ExitStack() as outputs:
    streams=[]
    for path in [file,error]:
     try:fd=os.open(path,os.O_CREAT|os.O_EXCL|os.O_WRONLY|os.O_NOFOLLOW,0o600)
     except FileExistsError:raise ValueError('owned_ios_evidence_path_exists') from None
     streams.append(outputs.enter_context(os.fdopen(fd,'wb')))
    # Reserve both output slots before a system command can create any resource.
    try:
     p=subprocess.run(argv,env=self.env['process_env'],capture_output=True,timeout=timeout)
     stdout,stderr,code=p.stdout,p.stderr,p.returncode
    except subprocess.TimeoutExpired as exc:
     expired=exc;stdout=exc.stdout or b'';stderr=exc.stderr or b'';code=None
    _write_command_log(streams[0],stdout);_write_command_log(streams[1],stderr)
    record={'label':label,'argv':argv,'actual_exit':code,'stdout_sha256':digest(file),'stderr_sha256':digest(error)}
    if expired is not None:record.update(timed_out=True,timeout_seconds=timeout)
    self.state['commands'].append(record);self.save()
  except (ValueError,OSError) as publication_error:
   if expired is not None:raise expired from publication_error
   raise
  if expired is not None:raise expired
  if code:raise ValueError('owned_ios_'+label.replace('-','_')+'_failed')
  return stdout.decode()
 def inventory(self):return json.loads(self.call('inventory',['list','devices','-j']))
 def owned(self):
  from .mobile import owned_simulator
  row=owned_simulator(self.inventory(),self.runtime,self.name,self.udid)
  if row is not None and row.get('deviceTypeIdentifier')!=self.device_type:raise ValueError('owned_ios_type_changed')
  return row
 def start(self):
  from .mobile import simulator_id
  from .ios_app_launch import bundle_manifest,launched_pid
  from .registry import process_fingerprint
  import time
  self.context();admit_new_ios(self.inventory(),self.runtime,self.name,self.udid);self.reserved=True;self.udid=simulator_id(self.call('create',['create',self.name,self.device_type,self.runtime]).strip());self.state['udid']=self.udid;self.save()
  if self.owned() is None:raise ValueError('new_ios_not_owned')
  self.call('boot',['boot',self.udid]);self.call('bootstatus',['bootstatus',self.udid,'-b'],240);self.context()
  if self.owned().get('state')!='Booted':raise ValueError('new_ios_not_booted')
  self.call('trust-exact-CA',['keychain',self.udid,'add-root-cert',str(self.ca)]);self.state['ca_fingerprint']=state.read(self.env['environment'])['ca_fingerprint'];self.call('install-ordinary',['install',self.udid,str(self.app)]);installed=Path(self.call('installed-app',['get_app_container',self.udid,PACKAGE,'app']).strip());expected=Path.home()/'Library/Developer/CoreSimulator/Devices'/self.udid/'data/Containers/Bundle/Application'
  if installed.parent.parent!=expected or installed.name!='Runner.app' or installed.resolve()!=installed or bundle_manifest(installed)!=bundle_manifest(self.app):raise ValueError('ordinary_ios_installed_manifest_changed')
  self.state['installed_app']=str(installed);self.state['installed_manifest']=bundle_manifest(installed);self.save();pid=launched_pid(self.call('ordinary-launch',['launch',self.udid,PACKAGE]));end=time.monotonic()+3;fp=None
  while time.monotonic()<end:
   try:fp=process_fingerprint(pid)
   except ValueError:fp=None
   if fp and fp['executable_path']==str(installed/'Runner'):break
   time.sleep(.05)
  if not fp or fp['executable_path']!=str(installed/'Runner') or fp['executable_sha256']!=digest(installed/'Runner'):raise ValueError('ordinary_ios_process_unproven')
  self.state.update(pid=pid,process_identity=fp,status='ordinary_ready');self.save();return self
 def close(self):
  import os
  failures=[]
  try:
   if self.reserved:
    row=self.owned()
    if row is not None:
     from .mobile import simulator_id
     self.udid=simulator_id(row['udid'])
     if row.get('state')=='Booted':self.call('shutdown',['shutdown',self.udid])
     self.call('delete',['delete',self.udid])
     if self.owned() is not None:raise ValueError('owned_ios_remains')
    self.state['owned_simulator_removed']=True
  except Exception as exc:failures.append(type(exc).__name__+':'+str(exc))
  self.state['cleanup_failures']=failures;self.save();return failures

def prepare_ios(env,owner,source,archive,ca):
 pinned_artifact(archive,IOS_ARCHIVE_SHA256);actor=OwnedIOS(env,owner,source,archive,ca)
 try:return actor.start()
 except Exception:
  actor.close();raise
