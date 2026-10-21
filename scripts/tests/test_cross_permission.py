"""Reject permission-denial evidence that loses unrelated memberships/devices."""
import copy,importlib,sys,unittest
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
class PermissionChangeTests(unittest.TestCase):
 def test_native_terminal_requires_actual_403_and_erasure(self):
  m=importlib.import_module('integration.cross_permission')
  row={'latest_network_status':403,'latest_network_code':'invalid_identity','context_cleared':True,'can_send':False,'seqs':[],'read_seqs':[],'cursor':None,'pending_receipts':0,'pending_sends':0,'ui_body_absent':True,'proof_was_real':True,'account_active':True,'remaining_memberships':['other'],'same_device':True,'rejections':{a:{'status':403,'code':'invalid_identity'} for a in ['sync','history','delivered','read','send']}}
  m.terminal_proof(row,'ended','other')
  for key,value in [('latest_network_status',401),('context_cleared',False),('can_send',True),('seqs',['1']),('read_seqs',['1']),('cursor','1'),('pending_receipts',1),('pending_sends',1),('ui_body_absent',False),('proof_was_real',False),('account_active',False),('remaining_memberships',['ended','other']),('same_device',False)]:
   with self.assertRaises(ValueError,msg=key):m.terminal_proof(dict(row,**{key:value}),'ended','other')
  bad=copy.deepcopy(row);bad['rejections']['delivered']['status']=401
  with self.assertRaises(ValueError):m.terminal_proof(bad,'ended','other')
 def test_database_end_is_scoped_and_audited(self):
  m=importlib.import_module('integration.cross_permission')
  facts={'messages':2,'devices':3,'revoked':0,'delivered':3,'read':3,'user_read':2,'ended_status':'ended','other_status':'active','user_status':'active','end_audits':1}
  m.database_proof(facts)
  for key,value in [('messages',3),('devices',4),('revoked',1),('delivered',4),('read',4),('user_read',3),('ended_status','active'),('other_status','ended'),('user_status','ended'),('end_audits',0)]:
   with self.assertRaises(ValueError,msg=key):m.database_proof(dict(facts,**{key:value}))
