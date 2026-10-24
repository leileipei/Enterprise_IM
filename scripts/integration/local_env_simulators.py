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
