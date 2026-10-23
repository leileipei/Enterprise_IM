"""OS input gates must not bypass live permission or accept stale visual proof."""
import importlib,sys,unittest
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))

class NotificationGateTests(unittest.TestCase):
 def test_full_permission_requires_live_dialog_for_both_android_choices(self):
  m=importlib.import_module('integration.cross_notification_scenario')
  class Actor:
   def __init__(self,role):self.role=role;self.sim=self;self.calls=[]
   def adb(self,*args):self.calls.append(('adb',args))
   def command(self,*args,**kwargs):self.calls.append(('command',args,kwargs));return {'ok':True}
  for role in ['alice','bob_android','bob_ios']:
   a=Actor(role);gates=[]
   def gate(actor,kind,action,**kwargs):gates.append((actor.role,kind,action,kwargs));return {'ok':True}
   row,mode=m.permission(a,False,gate)
   self.assertEqual(a.calls,[]);self.assertEqual(row,{'ok':True})
   self.assertEqual(gates,[(role,('ios' if role=='bob_ios' else 'android')+'_notification_permission','notification_permission',{'allowed':role!='alice'})])
   self.assertEqual(mode,'actual_'+('iOS' if role=='bob_ios' else 'Android')+'_system_prompt')
  a=Actor('alice');m.permission(a,True,gate)
  self.assertEqual(len(a.calls),3);self.assertEqual(a.calls[-1][0],'command')
 def test_visual_proof_rejects_other_owner_source_nonce_and_failed_or_missing_test(self):
  m=importlib.import_module('integration.cross_notification_scenario')
  expected={'role':'bob_ios','kind':'ios_logout_visual','owner':'owned','udid':'owned-uuid','nonce':'once','source_commit':'sha'}
  row=dict(expected,status='passed',actual_exit=0,same_product_process=True,xctest_result_bundle='owned.xcresult')
  m.visual_proof(row,expected)
  for key,value in [('role','alice'),('kind','ios_notification_tap'),('owner','foreign'),('udid','other'),('nonce','stale'),('source_commit','old'),('status','failed'),('actual_exit',1),('actual_exit',False),('same_product_process',False),('xctest_result_bundle','')]:
   with self.assertRaises(ValueError,msg=key):m.visual_proof(dict(row,**{key:value}),expected)
  for key in row:
   with self.assertRaises(ValueError,msg=key):m.visual_proof({k:v for k,v in row.items() if k!=key},expected)

class LegacyNotificationGateTests(unittest.TestCase):
 def test_api24_permission_uses_verified_app_state_and_rejects_wrong_api_or_readback(self):
  from types import SimpleNamespace
  m=importlib.import_module('integration.cross_notification_scenario')
  class Actor:
   def __init__(self,role,api=24,reply=None):self.role=role;self.state={'configuration':{'api':api}};self.sim=self;self.calls=[];self.reply=reply
   def adb(self,label,args):
    self.calls.append(args)
    return SimpleNamespace(stdout=(self.reply if self.reply is not None else 'POST_NOTIFICATION: '+('ignore' if self.role=='alice' else 'allow')+'; time=+1s ago\n').encode())
   def command(self,action,**kwargs):self.calls.append((action,kwargs));return {'ok':True}
  def no_dialog(*a,**kw):self.fail('API24 must not wait for nonexistent runtime prompt')
  for role in ['alice','bob_android']:
   a=Actor(role);row,mode=m.permission(a,True,no_dialog,legacy_android=True)
   self.assertEqual(row,{'ok':True});self.assertEqual(mode,'controlled_Android_API24_app_notification_state')
   self.assertFalse(any('pm' in x for x in a.calls));self.assertEqual(a.calls[-1],('notification_permission',{'allowed':role!='alice'}))
  for api in [23,25,36,None]:
   a=Actor('alice',api)
   with self.assertRaises(ValueError):m.permission(a,True,no_dialog,legacy_android=True)
   self.assertEqual(a.calls,[])
  for reply in ['','No operations.','POST_NOTIFICATION: allow;\n','POST_NOTIFICATION: ignore;\nPOST_NOTIFICATION: allow;\n']:
   a=Actor('alice',reply=reply)
   with self.assertRaises(ValueError):m.permission(a,True,no_dialog,legacy_android=True)
   self.assertTrue(all(not isinstance(x,tuple) for x in a.calls))
