"""Reject re-login evidence that reuses identity/storage/callback admission."""
import importlib,sys,unittest
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
class ReloginTests(unittest.TestCase):
 def sample(self):
  before={'role':'bob_ios','platform':'ios','pid':11,'user':'u','nonce':'a'*32,'device_id':'00000000-0000-4000-8000-000000000001'}
  after=dict(before,device_id='00000000-0000-4000-8000-000000000002',details={'old_device_id':before['device_id'],'storage':'s','relogin_from_same_page':True,'fresh_pkce':True,'wrong_state_ignored':True,'callback_via_os':True,'old_owner_stopped':True,'old_view_stopped':True,'vault_saved':True,'workspace_entered':True,'no_seed_refresh':True,'token_requests':2,'refresh_requests':0,'callback_completions':2,'authorize_requests':2})
  return before,after
 def test_relogin_requires_new_device_same_native_owner_and_directory(self):
  m=importlib.import_module('integration.cross_relogin');before,after=self.sample();m.readmission(before,after,'s')
  for key,value in [('pid',12),('role','alice'),('user','other'),('nonce','b'*32),('platform','android'),('device_id',before['device_id'])]:
   with self.assertRaises(ValueError,msg=key):m.readmission(before,dict(after,**{key:value}),'s')
 def test_relogin_rejects_partial_or_refreshed_admission(self):
  m=importlib.import_module('integration.cross_relogin');before,after=self.sample()
  for key,value in after['details'].items():
   bad=False if type(value) is bool else (1 if type(value) is int else 'wrong')
   if key=='refresh_requests':bad=1
   with self.assertRaises(ValueError,msg=key):m.readmission(before,dict(after,details=dict(after['details'],**{key:bad})),'s')
