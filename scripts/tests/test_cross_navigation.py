"""Real logout navigation must survive a fresh widget and killed OS process."""
import importlib,sys,unittest
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
class LogoutNavigationTests(unittest.TestCase):
 def test_logged_out_ui_has_no_session_or_recovery_path(self):
  m=importlib.import_module('integration.cross_navigation')
  row={'login_heading':True,'client_shell_count':0,'message_page_count':0,'workspace_entry':False,'restore_button':False,'account_absent':True,'vault_absent':True,'database_files_absent':True,'login_button_enabled':True}
  m.logged_out_page(row)
  for k,v in [('login_heading',False),('client_shell_count',1),('message_page_count',1),('workspace_entry',True),('restore_button',True),('account_absent',False),('vault_absent',False),('database_files_absent',False),('login_button_enabled',False)]:
   with self.assertRaises(ValueError,msg=k):m.logged_out_page(dict(row,**{k:v}))
 def test_cold_start_requires_actual_new_pid_same_storage_without_seed(self):
  m=importlib.import_module('integration.cross_navigation');before={'pid':11,'device':'d','storage':'s'};after={'pid':12,'device':'d','storage':'s','old_absent':True,'no_seed_refresh':True,'login_api_creations':0}
  m.cold_start(before,after)
  for k,v in [('pid',11),('device','other'),('storage','new'),('old_absent',False),('no_seed_refresh',False),('login_api_creations',1)]:
   with self.assertRaises(ValueError,msg=k):m.cold_start(before,dict(after,**{k:v}))
