"""Independent negative contracts for real upstream commit and native retries."""
import importlib,sys,unittest
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
class AckTests(unittest.TestCase):
 def test_loss_requires_upstream_commit_but_no_native_ack_and_saved_pending(self):
  m=importlib.import_module('integration.cross_ack')
  v={'upstream_status':200,'downstream_dropped':True,'duplicate':False,'pending_id':'p','db_client_id':'p','native_ack':None,'network_status':None,'network_error':'unavailable'}
  m.lost_ack(v)
  for key,value in [('upstream_status',503),('downstream_dropped',False),('duplicate',True),('pending_id','new'),('native_ack','1'),('network_status',200),('network_error','invalid_response')]:
   with self.assertRaises(ValueError):m.lost_ack(dict(v,**{key:value}))
 def test_recovery_requires_same_pending_device_store_and_explicit_duplicate(self):
  m=importlib.import_module('integration.cross_ack')
  before={'pending_id':'p','device_id':'d','storage':'s','pid':12,'message_id':'m','seq':'1'}
  after={**before,'pid':13,'old_absent':True,'vault_reused':True,'preauth_hidden':True,'duplicate':True,'automatic_posts':0,'pending_after':0,'renew_before_sync':True}
  m.recovered_ack(before,after)
  for key,value in [('pending_id','q'),('device_id','e'),('storage','t'),('pid',12),('message_id','n'),('seq','2'),('old_absent',False),('vault_reused',False),('preauth_hidden',False),('duplicate',False),('automatic_posts',1),('pending_after',1),('renew_before_sync',False)]:
   with self.assertRaises(ValueError):m.recovered_ack(before,dict(after,**{key:value}))
