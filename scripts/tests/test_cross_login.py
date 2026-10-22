"""Fresh sign-in gate must reject saved-refresh or synthetic callback results."""
import importlib,sys,unittest
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
class LoginCallbackTests(unittest.TestCase):
 def test_admission_requires_native_callback_and_fresh_button_login(self):
  m=importlib.import_module('integration.cross_login')
  row={'fresh_login_button':True,'no_seed_refresh':True,'real_mobile_callback':True,'wrong_state_ignored':True,'callback_via_os':True,'callback_completions':1,'authorize_requests':1,'token_requests':1,'refresh_requests':0,'workspace_entered':True,'vault_saved':True,'same_process':True}
  m.admission(row)
  for key in row:
   wrong=2 if key in ('callback_completions','authorize_requests','token_requests') else (1 if key=='refresh_requests' else False)
   with self.assertRaises(ValueError,msg=key):m.admission(dict(row,**{key:wrong}))
 def test_native_bootstrap_never_contains_refresh(self):
  m=importlib.import_module('integration.cross_login');c=importlib.import_module('integration.cross_device')
  raw={'origin':'https://127.0.0.1:54321','ca_pem':'fixture','tenant':'t','conversation':'c','actors':{'bob_ios':{'platform':'ios','user':'u','membership':'m','refresh':'must-not-seed'}}}
  result=m.bootstrap(raw,'bob_ios','a'*32)
  self.assertNotIn('refresh',result);self.assertIs(result['fresh_login'],True)
  self.assertEqual(c.actor_config(raw,'bob_ios','a'*32)['refresh'],'must-not-seed')
