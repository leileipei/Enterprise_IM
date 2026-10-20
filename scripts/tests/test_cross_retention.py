"""Reject incomplete restart and retention evidence independently of actors."""
import copy,importlib,sys,unittest
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
class RetentionTests(unittest.TestCase):
 def test_restart_rejects_replaced_device_store_pid_or_missing_revalidation(self):
  m=importlib.import_module('integration.cross_retention')
  before={'pid':12,'device_id':'d','storage':'s','nonce':'n'}
  after={'pid':13,'device_id':'d','storage':'s','nonce':'n','old_absent':True,'preauth_hidden':True,'cache_erased':True,'vault_reused':True,'renew_before_sync':True,'restored_cursor':'3'}
  m.restart_proof(before,after)
  for key,value in [('pid',12),('device_id','new'),('storage','new'),('nonce','new'),('old_absent',False),('preauth_hidden',False),('cache_erased',False),('vault_reused',False),('renew_before_sync',False),('restored_cursor','0')]:
   with self.assertRaises(ValueError):m.restart_proof(before,dict(after,**{key:value}))
 def test_policy_requires_versioned_existing_content_and_no_physical_purge(self):
  m=importlib.import_module('integration.cross_retention')
  row={'message_days':1,'file_days':'1','message_version':1,'file_version':'1','cleanup_enabled':False,'existing_files':True}
  m.policy_proof(row,1,1)
  for key,value in [('message_days',True),('file_days','01'),('message_version',0),('file_version','0'),('cleanup_enabled',True),('existing_files',False)]:
   with self.assertRaises(ValueError):m.policy_proof(dict(row,**{key:value}),1,1)

 def test_body_and_file_versions_are_independent(self):
  m=importlib.import_module('integration.cross_retention')
  row={'message_days':30,'file_days':'1','message_version':1,'file_version':'2','cleanup_enabled':False,'existing_files':True}
  m.policy_proof(row,1,2,message_days=30,message_version=1)
  for key,value in [('message_days',1),('message_version',2),('file_days','30'),('file_version','1')]:
   with self.assertRaises(ValueError):m.policy_proof(dict(row,**{key:value}),1,2,message_days=30,message_version=1)

 def test_exit_probe_waits_for_kernel_absence_without_decoding_dying_comm(self):
  from unittest.mock import patch,call
  m=importlib.import_module('integration.cross_native')
  with patch.object(m.os,'kill',side_effect=[None,ProcessLookupError]) as probe,patch.object(m.time,'sleep'),patch.object(m,'process_fingerprint',side_effect=ValueError('unknown_process_executable')):
   m.wait_process_absent(88)
  self.assertEqual(probe.call_args_list,[call(88,0),call(88,0)])
 def test_exit_probe_never_accepts_still_alive_or_permission_denial(self):
  from unittest.mock import patch
  m=importlib.import_module('integration.cross_native')
  with patch.object(m.os,'kill',return_value=None):
   with self.assertRaises(TimeoutError):m.wait_process_absent(88,timeout=0)
  with patch.object(m.os,'kill',side_effect=PermissionError):
   with self.assertRaises(PermissionError):m.wait_process_absent(88)
