"""Cold recovery must preserve the new grant, not reseed or re-register."""
import importlib,sys,unittest
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
class ReloginRestartTests(unittest.TestCase):
 def test_restart_bootstrap_cannot_reseed_or_enter_fresh_login_mode(self):
  m=importlib.import_module('integration.cross_relogin_restart');original={'role':'bob_ios','origin':'https://127.0.0.1:1234','refresh':'must-not-seed','fresh_login':True,'logged_out_restart':True}
  config=m.restart_config(original)
  self.assertEqual(config,{'role':'bob_ios','origin':'https://127.0.0.1:1234','restart':True})
  self.assertEqual(original['refresh'],'must-not-seed')
 def test_restore_requires_real_new_process_same_account_storage_and_device(self):
  m=importlib.import_module('integration.cross_relogin_restart');before={'role':'bob_ios','platform':'ios','pid':11,'user':'u','nonce':'a'*32,'device_id':'d','storage':'s'}
  after=dict(before,pid=12,old_absent=True,no_seed_refresh=True,preauth_hidden=True,vault_reused=True,login_restore_via_ui=True,same_device=True,login_api_creations=1)
  m.restored(before,after)
  for key,value in [('pid',11),('pid',True),('device_id','other'),('storage','other'),('role','other'),('user','other'),('nonce','b'*32),('platform','android'),('old_absent',False),('no_seed_refresh',False),('preauth_hidden',False),('vault_reused',False),('login_restore_via_ui',False),('same_device',False),('login_api_creations',2)]:
   with self.assertRaises(ValueError,msg=key):m.restored(before,dict(after,**{key:value}))
