"""Strict independent-process group/file/revocation evidence contracts."""
import copy
import importlib
from pathlib import Path
import sys
import unittest
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
def facts(phase):
 value={'messages':1,'devices':3,'groups':1,'memberships':2,'organizations':2,'files_ready':0,'attachments':0,'scan_completed':0,'downloads_completed':0,'downloads_acked':0,'revoked':0,'roles':{'alice':{'delivered':0,'read':0},'bob_ios':{'delivered':1,'read':1},'bob_android':{'delivered':1,'read':1}},'users':{'alice':0,'bob':1}}
 if phase in ('both_downloaded','survivor'):
  value.update(messages=3,files_ready=1,attachments=1,scan_completed=1,downloads_completed=2,downloads_acked=2)
  value['roles']={'alice':{'delivered':1,'read':1},'bob_ios':{'delivered':2,'read':2},'bob_android':{'delivered':2,'read':2}};value['users']={'alice':1,'bob':2}
 if phase=='survivor':
  value.update(messages=4,revoked=1);value['roles']['bob_ios']={'delivered':3,'read':3};value['users']['bob']=3
 return value
class CrossGroupTests(unittest.TestCase):
 def module(self):return importlib.import_module('integration.cross_group')
 def test_group_requires_two_organizations_and_both_bob_devices(self):
  m=self.module();value=facts('both_read');m.checkpoint('both_read',value)
  for path,replacement in [('organizations',1),('devices',2),('memberships',3)]:
   bad=copy.deepcopy(value);bad[path]=replacement
   with self.assertRaises(ValueError):m.checkpoint('both_read',bad)
  bad=copy.deepcopy(value);bad['roles']['bob_android']['read']=0
  with self.assertRaises(ValueError):m.checkpoint('both_read',bad)
 def test_file_requires_real_ready_scan_attachment_and_two_audited_downloads(self):
  m=self.module();value=facts('both_downloaded');m.checkpoint('both_downloaded',value)
  for key in ['files_ready','attachments','scan_completed','downloads_completed','downloads_acked']:
   bad=copy.deepcopy(value);bad[key]-=1
   with self.assertRaises(ValueError):m.checkpoint('both_downloaded',bad)
 def test_revoked_device_cannot_receive_later_message_or_new_registration(self):
  m=self.module();value=facts('survivor');m.checkpoint('survivor',value)
  for key,replacement in [('revoked',0),('devices',4)]:
   bad=copy.deepcopy(value);bad[key]=replacement
   with self.assertRaises(ValueError):m.checkpoint('survivor',bad)
  bad=copy.deepcopy(value);bad['roles']['bob_android']['delivered']=3
  with self.assertRaises(ValueError):m.checkpoint('survivor',bad)
 def test_terminal_requires_all_six_actual_invalid_device_rejections_and_private_erasure(self):
  m=self.module();d={'rejected':['sync','delivered','read','messages','download','renew'],'terminal':True,'private_artifacts_absent':True,'received_cleared':True,'can_send':False};m.terminal(d)
  for key,value in [('rejected',['sync']),('terminal',False),('private_artifacts_absent',False),('received_cleared',False),('can_send',True)]:
   with self.assertRaises(ValueError):m.terminal(dict(d,**{key:value}))
 def test_attachment_hash_size_and_device_must_match(self):
  m=self.module();d={'sha256':m.PAYLOAD_SHA,'size':len(m.PAYLOAD),'device_id':'db'};m.download(d,'db')
  for key,value in [('sha256','0'*64),('size',0),('device_id','da')]:
   with self.assertRaises(ValueError):m.download(dict(d,**{key:value}),'db')

 def test_counts_reject_boolean_equivalence(self):
  m=self.module();value=facts('both_read');value['messages']=True
  with self.assertRaises(ValueError):m.checkpoint('both_read',value)

 def test_owner_roster_and_member_denial_are_distinct(self):
  m=self.module();m.selection('alice',{'group_role':'owner','organizations':['A','A2']})
  d={'group_role':'member','roster_denial':{'status':403,'code':'group_permission_denied'}};m.selection('bob_ios',d)
  for bad in [dict(d,organizations=['A','A2']),dict(d,roster_denial={'status':200,'code':'group_permission_denied'}),dict(d,roster_denial={'status':403,'code':'unavailable'})]:
   with self.assertRaises(ValueError):m.selection('bob_android',bad)
