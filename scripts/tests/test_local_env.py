"""Local environment ownership/state contracts; no external service side effects."""
import copy
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from integration import local_env_state as state
from integration.registry import process_fingerprint

class LocalEnvironmentStateTests(unittest.TestCase):
 def setUp(self):
  self.temp=tempfile.TemporaryDirectory(prefix='im-local-state-');self.addCleanup(self.temp.cleanup);self.parent=Path(self.temp.name).resolve();self.root=self.parent/'environment';self.owner='a'*32;self.source='b'*40;self.fp=process_fingerprint(os.getpid());self.assertIsNotNone(self.fp)
 def create(self):return state.create(self.root,self.owner,self.source,60,self.fp,now=1000)
 def test_new_private_state_and_read_only_status(self):
  value=self.create();self.assertEqual(value['phase'],'preparing');self.assertEqual(value['expires_at'],1060);self.assertEqual(self.root.stat().st_mode&0o777,0o700);self.assertEqual((self.root/'state.json').stat().st_mode&0o777,0o600)
  before=(self.root/'state.json').read_bytes();result=state.snapshot(self.root,now=1001,fingerprint_reader=lambda pid:self.fp);self.assertEqual(result['supervisor_live'],True);self.assertEqual(result['expired'],False);self.assertEqual((self.root/'state.json').read_bytes(),before)
 def test_reject_existing_linked_or_wide_state(self):
  self.create()
  with self.assertRaises(ValueError):state.create(self.root,self.owner,self.source,60,self.fp,now=1000)
  linked=self.parent/'linked';linked.symlink_to(self.root,target_is_directory=True)
  with self.assertRaises(ValueError):state.read(linked)
  os.chmod(self.root,0o755)
  with self.assertRaises(ValueError):state.read(self.root)
  os.chmod(self.root,0o700);p=self.root/'state.json';original=p.read_bytes();p.unlink();target=self.parent/'outside';target.write_bytes(original);os.chmod(target,0o600);p.symlink_to(target)
  with self.assertRaises(ValueError):state.read(self.root)
 def test_stop_request_binds_owner_source_and_live_incarnation(self):
  self.create();result=state.request_stop(self.root,now=1002,fingerprint_reader=lambda pid:self.fp);self.assertEqual(result['status'],'requested');self.assertTrue(state.stop_requested(self.root,state.read(self.root)))
  request=json.loads((self.root/'stop.json').read_text());request['owner']='c'*32;(self.root/'stop.json').write_text(json.dumps(request))
  with self.assertRaises(ValueError):state.stop_requested(self.root,state.read(self.root))
 def test_pid_reuse_or_absence_never_writes_stop(self):
  self.create();other=dict(self.fp,start_time='changed')
  for observed in [None,other]:
   with self.subTest(observed=observed):
    result=state.request_stop(self.root,now=1002,fingerprint_reader=lambda pid:observed);self.assertEqual(result['status'],'recovery_required');self.assertFalse((self.root/'stop.json').exists())
 def test_reject_nested_secrets_unsafe_origins_and_identity_changes(self):
  original=self.create()
  for patch in [{'resources':{'password':'secret'}},{'origin':'https://user:password@127.0.0.1:18443'},{'origin':'https://customer.invalid:18443'},{'owner':'c'*32},{'source':'c'*40},{'phase':'unknown'}]:
   with self.subTest(patch=patch):
    with self.assertRaises(ValueError):state.write(self.root,dict(original,**patch))
  self.assertEqual(state.read(self.root),original)
 def test_lease_expiry_and_private_permissions_fail_closed(self):
  value=self.create();result=state.snapshot(self.root,now=1061,fingerprint_reader=lambda pid:self.fp);self.assertTrue(result['expired']);self.assertEqual(result['phase'],'preparing')
  os.chmod(self.root/'state.json',0o644)
  with self.assertRaises(ValueError):state.read(self.root)
 def test_stop_is_idempotent_and_terminal_state_not_restarted(self):
  value=self.create();first=state.request_stop(self.root,now=1001,fingerprint_reader=lambda pid:self.fp);second=state.request_stop(self.root,now=1002,fingerprint_reader=lambda pid:self.fp);self.assertEqual(first['request_id'],second['request_id'])
  value['phase']='stopped';state.write(self.root,value);self.assertEqual(state.request_stop(self.root,now=1003,fingerprint_reader=lambda pid:self.fp)['status'],'already_stopped')
 def test_reject_unbounded_lease_and_malformed_owner(self):
  for owner,lease in [('a'*32,0),('a'*32,14401),('other',60)]:
   with self.assertRaises(ValueError):state.create(self.root,owner,self.source,lease,self.fp,now=1000)
  self.assertFalse(self.root.exists())


class LocalEnvironmentRuntimeContractTests(unittest.TestCase):
 def setUp(self):
  import subprocess
  self.temp=tempfile.TemporaryDirectory(prefix='im-local-runtime-');self.addCleanup(self.temp.cleanup);self.parent=Path(self.temp.name).resolve();self.repo=self.parent/'source';self.repo.mkdir();subprocess.run(['git','init','-q',str(self.repo)],check=True);(self.repo/'go.mod').write_text('module local-fixture\n');subprocess.run(['git','-C',str(self.repo),'add','.'],check=True);subprocess.run(['git','-C',str(self.repo),'-c','user.name=Fixture','-c','user.email=fixture@invalid.test','commit','-qm','fixture'],check=True);self.ca=self.parent/'ca.pem';self.ca.write_text('fixture CA path only');os.chmod(self.ca,0o600)
 def module(self):
  from integration import local_env
  return local_env
 def test_start_requires_clean_frozen_source_and_new_private_root(self):
  from types import SimpleNamespace
  m=self.module();args=SimpleNamespace(go=Path('/opt/homebrew/bin/go'),state_dir=self.parent/'environment',lease_seconds=60)
  source=m.validate_start(self.repo,args);self.assertEqual(len(source),40);(self.repo/'dirty').write_text('keep')
  with self.assertRaises(ValueError):m.validate_start(self.repo,args)
  self.assertFalse(args.state_dir.exists())
 def test_start_rejects_existing_state_invalid_lease_and_relative_paths(self):
  from types import SimpleNamespace
  m=self.module()
  for root,go,lease in [(self.parent,Path('/opt/homebrew/bin/go'),60),(self.parent/'new',Path('go'),60),(Path('relative'),Path('/opt/homebrew/bin/go'),60),(self.parent/'new',Path('/opt/homebrew/bin/go'),0)]:
   with self.assertRaises(ValueError):m.validate_start(self.repo,SimpleNamespace(state_dir=root,go=go,lease_seconds=lease))
 def test_private_backend_environment_exact_platforms_and_same_origin(self):
  m=self.module();env=m.native_environment('https://127.0.0.1:18443',self.ca,'postgresql://local:fixture@127.0.0.1:15432/local','redis://:fixture@127.0.0.1:16379/0','im:local:messages','im:local:receipts')
  self.assertEqual(env['IM_OIDC_ISSUER'],'https://127.0.0.1:18443/oidc');self.assertEqual(env['IM_NATIVE_TOKEN_URL'],'https://127.0.0.1:18443/oidc/token');self.assertEqual(set(env['IM_OIDC_ALLOWED_CLIENT_IDS'].split(',')),{'im-windows','im-macos','im-android','im-ios'});self.assertEqual(env['IM_NATIVE_ANDROID_REDIRECT_URI'],'com.enterpriseim.client:/oauth/callback');self.assertEqual(env['IM_NATIVE_MACOS_REDIRECT_URI'],'http://127.0.0.1/oauth/callback');self.assertEqual(env['SSL_CERT_FILE'],str(self.ca));self.assertNotIn('GODEBUG',env)
 def test_native_environment_rejects_external_targets_or_mixed_streams(self):
  m=self.module()
  for origin,db,redis,stream in [('http://127.0.0.1:18443','postgresql://u:p@127.0.0.1:15432/local','redis://:p@127.0.0.1:16379/0','im:other'),('https://customer.invalid:18443','postgresql://u:p@127.0.0.1:15432/local','redis://:p@127.0.0.1:16379/0','im:other'),('https://127.0.0.1:18443','postgresql://u:p@external.invalid:15432/local','redis://:p@127.0.0.1:16379/0','im:other'),('https://127.0.0.1:18443','postgresql://u:p@127.0.0.1:15432/local','redis://:p@external.invalid:16379/0','im:other'),('https://127.0.0.1:18443','postgresql://u:p@127.0.0.1:15432/local','redis://:p@127.0.0.1:16379/0','im:local:messages')]:
   with self.assertRaises(ValueError):m.native_environment(origin,self.ca,db,redis,'im:local:messages',stream)
 def test_ready_metadata_never_imports_secrets_or_foreign_owner(self):
  m=self.module();ready={'owner':'a'*32,'source':'b'*40,'origin':'https://127.0.0.1:18443','issuer':'https://127.0.0.1:18443/oidc','ca_cert':str(self.ca),'ca_fingerprint':'c'*64,'expires_at':'2026-10-25T00:00:00Z'};m.ready_metadata(ready,'a'*32,'b'*40)
  for value in [dict(ready,owner='d'*32),dict(ready,access_token='secret'),dict(ready,ca_fingerprint='short')]:
   with self.assertRaises(ValueError):m.ready_metadata(value,'a'*32,'b'*40)

class LocalEnvironmentLifecycleTests(unittest.TestCase):
 def test_SIGTERM_is_controlled_stop_and_restores_handler(self):
  import subprocess
  code="""import os,signal
from integration.local_env import StopSignals,ControlledStop
before=signal.getsignal(signal.SIGTERM)
try:
 with StopSignals() as stop:
  os.kill(os.getpid(),signal.SIGTERM)
  stop.check()
except ControlledStop:
 assert signal.getsignal(signal.SIGTERM)==before
 print('controlled-stop')
else:raise AssertionError('signal ignored')
"""
  env=dict(os.environ,PYTHONPATH=str(Path(__file__).resolve().parents[1]));p=subprocess.run([sys.executable,'-c',code],env=env,capture_output=True,text=True,timeout=10);self.assertEqual(p.returncode,0,p.stderr);self.assertEqual(p.stdout.strip(),'controlled-stop')
 def test_partial_cleanup_preserves_private_root_if_process_identity_changed(self):
  import subprocess
  import time
  from integration.local_env import Environment
  from integration.registry import Registry
  from integration.model import ResourceRef
  with tempfile.TemporaryDirectory(prefix='im-local-partial-') as parent:
   root=Path(parent).resolve()/'environment';owner='a'*32;source='b'*40
   value=state.create(root,owner,source,60,process_fingerprint(os.getpid()));registry=Registry(root/'registry.jsonl',owner,source,recover_containers=False);private=root/owner
   child=subprocess.Popen([sys.executable,'-c',"import time;from pathlib import Path;Path('child-ready').write_text('ready');time.sleep(30)"],cwd=private,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
   try:
    end=time.monotonic()+3
    while not (private/'child-ready').exists() and time.monotonic()<end:time.sleep(.01)
    self.assertTrue((private/'child-ready').exists());fp=process_fingerprint(child.pid);self.assertIsNotNone(fp);registry.add(ResourceRef('process',owner,str(child.pid),dict(fp,start_time='changed')))
    (private/'runtime-secret').write_text('must retain on failed cleanup');env=Environment.__new__(Environment);env.root=root;env.owner=owner;env.source=source;env.public=value;env.registry=registry;env.private=private;env.metadata={};env.secret_values=[];env.processes=[]
    failures=env.close();self.assertTrue(failures);self.assertTrue(private.exists(),'failed ownership cleanup must preserve recovery material');self.assertIsNone(child.poll());self.assertEqual(state.read(root)['phase'],'failed')
   finally:child.terminate();child.wait(timeout=5)

if __name__=='__main__':unittest.main()
