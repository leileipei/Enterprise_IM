"""Do not accept logout if stale replies can resurrect a native account."""
import importlib,sys,unittest
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
class LogoutRaceTests(unittest.TestCase):
 def test_logout_requires_account_vault_repository_and_visible_body_erasure(self):
  m=importlib.import_module('integration.cross_logout')
  row={'account_absent':True,'vault_absent':True,'database_files_absent':True,'context_cleared':True,'can_send':False,'seqs':[],'read_seqs':[],'ui_body_absent':True,'cursor':None,'same_process':True}
  m.logout_proof(row)
  for k,v in [('account_absent',False),('vault_absent',False),('database_files_absent',False),('context_cleared',False),('can_send',True),('seqs',['2']),('read_seqs',['2']),('ui_body_absent',False),('cursor','2'),('same_process',False)]:
   with self.assertRaises(ValueError,msg=k):m.logout_proof(dict(row,**{k:v}))
 def test_late_actual_success_is_held_then_released_after_cancellation(self):
  m=importlib.import_module('integration.cross_logout')
  rows=[{'device':'d','phase':'held','upstream_status':200,'body_sha256':'a'*64,'next_seq':'2','n':8},{'device':'d','phase':'released','upstream_status':200,'body_sha256':'a'*64,'next_seq':'2','request_cancelled':True,'n':12}]
  m.response_proof(rows,'d','2')
  for key,value in [('request_cancelled',False),('body_sha256','b'*64),('upstream_status',503),('next_seq','1'),('n',7)]:
   with self.assertRaises(ValueError,msg=key):m.response_proof([rows[0],dict(rows[1],**{key:value})],'d','2')
  with self.assertRaises(ValueError):m.response_proof(rows[:1],'d','2')
