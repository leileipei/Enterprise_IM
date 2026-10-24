"""Owned-simulator CA admission and immutable artifact/image contracts."""
import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))

class LocalSimulatorAdmissionTests(unittest.TestCase):
 def setUp(self):
  self.temp=tempfile.TemporaryDirectory(prefix='im-local-sim-');self.addCleanup(self.temp.cleanup);self.root=Path(self.temp.name).resolve();self.private=self.root/('a'*32);self.private.mkdir(mode=0o700);self.base=self.root/'sdk-image';self.base.mkdir();self.source='b'*40
  for name in ['system.img','userdata.img','ramdisk.img','kernel-ranchu','source.properties']:(self.base/name).write_bytes(('original '+name).encode())
 def module(self):
  from integration import local_env_simulators
  return local_env_simulators
 def stage(self):return self.module().stage_private_image(self.base,self.private,'a'*32,self.source)
 def test_stage_is_exclusive_exact_copy_and_does_not_change_base(self):
  before={p.name:p.read_bytes() for p in self.base.iterdir()};result=self.stage();target=self.private/'android-system-image';self.assertEqual(result['owner'],'a'*32);self.assertEqual(result['source'],self.source);self.assertEqual(result['base'],str(self.base));self.assertEqual(result['target'],str(target));self.assertEqual(before,{p.name:p.read_bytes() for p in self.base.iterdir()});self.assertEqual({p.name:p.read_bytes() for p in target.iterdir() if p.is_file() and p.name!='im-private-image.json'},before)
  self.assertNotEqual((target/'system.img').stat().st_ino,(self.base/'system.img').stat().st_ino)
  with self.assertRaises(ValueError):self.stage()
 def test_stage_rejects_foreign_owner_link_and_hardlink_before_copy(self):
  m=self.module()
  with self.assertRaises(ValueError):m.stage_private_image(self.base,self.private,'c'*32,self.source)
  (self.base/'linked').symlink_to(self.base/'system.img')
  with self.assertRaises(ValueError):self.stage()
  (self.base/'linked').unlink();os.link(self.base/'system.img',self.base/'hardlink')
  with self.assertRaises(ValueError):self.stage()
  self.assertFalse((self.private/'android-system-image').exists())
 def test_default_emulator_flags_unchanged_private_flags_bind_manifest(self):
  m=self.module();self.assertEqual(m.private_image_arguments(self.private,None,'a'*32,self.source),[]);self.stage();target=self.private/'android-system-image';flags=m.private_image_arguments(self.private,target,'a'*32,self.source);self.assertEqual(flags,['-sysdir',str(target),'-writable-system'])
  for owner,source,path in [('c'*32,self.source,target),('a'*32,'d'*40,target),('a'*32,self.source,self.base)]:
   with self.assertRaises(ValueError):m.private_image_arguments(self.private,path,owner,source)
 def test_private_image_drift_never_admitted_to_emulator(self):
  m=self.module();self.stage();target=self.private/'android-system-image';(target/'system.img').write_bytes(b'changed')
  with self.assertRaises(ValueError):m.private_image_arguments(self.private,target,'a'*32,self.source)
 def test_artifact_pin_rejects_alias_link_or_changed_bytes(self):
  m=self.module();p=self.root/'ordinary.apk';p.write_bytes(b'ordinary fixed application');expected=hashlib.sha256(p.read_bytes()).hexdigest();self.assertEqual(m.pinned_artifact(p,expected),expected)
  link=self.root/'alias.apk';link.symlink_to(p)
  with self.assertRaises(ValueError):m.pinned_artifact(link,expected)
  p.write_bytes(b'different')
  with self.assertRaises(ValueError):m.pinned_artifact(p,expected)
 def test_ios_new_name_admission_rejects_existing_uuid_or_foreign_runtime(self):
  m=self.module();runtime='com.apple.CoreSimulator.SimRuntime.iOS-26-5';name='IM-Local-'+'a'*32;inventory={'devices':{runtime:[]}};m.admit_new_ios(inventory,runtime,name,None)
  for devices,udid in [({runtime:[{'name':name,'udid':'11111111-1111-4111-8111-111111111111'}]},None),({runtime:[]},'11111111-1111-4111-8111-111111111111'),({'foreign':[]},None)]:
   with self.assertRaises(ValueError):m.admit_new_ios({'devices':devices},runtime,name,udid)
 def test_CA_requires_live_unexpired_environment_and_exact_fingerprint(self):
  import subprocess
  import ssl
  import time
  from integration import local_env_state as state
  from integration.registry import process_fingerprint
  m=self.module();root=self.root/'environment';value=state.create(root,'c'*32,self.source,60,process_fingerprint(os.getpid()));private=root/('c'*32);(private/'tls').mkdir(mode=0o700,parents=True);ca=private/'tls/ca.pem'
  p=subprocess.run(['/usr/bin/openssl','req','-x509','-newkey','rsa:2048','-nodes','-keyout',str(private/'test.key'),'-out',str(ca),'-subj','/CN=local-fixture','-days','1'],capture_output=True);self.assertEqual(p.returncode,0);ca.chmod(0o600);fp=hashlib.sha256(ssl.PEM_cert_to_DER_cert(ca.read_text())).hexdigest();value.update(phase='ready',origin='https://127.0.0.1:18443',issuer='https://127.0.0.1:18443/oidc',ca_cert=str(ca),ca_fingerprint=fp);state.write(root,value);self.assertEqual(m.environment_CA(root,'c'*32,self.source),ca)
  with self.assertRaises(ValueError):m.environment_CA(root,'d'*32,self.source)
  value['ca_fingerprint']='0'*64;state.write(root,value)
  with self.assertRaises(ValueError):m.environment_CA(root,'c'*32,self.source)
  value['ca_fingerprint']=fp;value['phase']='stopped';state.write(root,value)
  with self.assertRaises(ValueError):m.environment_CA(root,'c'*32,self.source)
 def test_owned_android_CA_cannot_use_default_or_usb_transport(self):
  m=self.module();good={'name':'IM-Mobile-Android-'+'a'*32,'serial':'emulator-5554','console_port':5554,'adb_server_port':15037,'properties':{'ro.boot.qemu.avd_name':'IM-Mobile-Android-'+'a'*32,'ro.build.version.sdk':'36','ro.product.cpu.abi':'arm64-v8a','ro.kernel.qemu':'1'}};m.admit_android_transport(good,'a'*32)
  for patch in [{'serial':'R58USB123'},{'adb_server_port':5037},{'name':'Existing'},{'console_port':5556}]:
   with self.assertRaises(ValueError):m.admit_android_transport(dict(good,**patch),'a'*32)

if __name__=='__main__':unittest.main()
