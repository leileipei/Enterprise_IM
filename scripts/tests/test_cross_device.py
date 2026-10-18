"""Contracts for independently observed native cross-process messaging."""
import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import unittest
SCRIPTS=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(SCRIPTS))

class CrossDeviceTests(unittest.TestCase):
    def module(self):
        path=SCRIPTS/'integration/cross_device.py';self.assertTrue(path.exists(),'independent cross-device coordinator missing')
        spec=importlib.util.spec_from_file_location('integration.cross_under_test',path);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m);return m
    def test_role_bootstrap_excludes_other_account_tokens_and_database_schema(self):
        m=self.module();config={'origin':'https://127.0.0.1:12345','ca_pem':'own-ca','tenant':'tenant','conversation':'chat','schema':'private_schema','actors':{'alice':{'platform':'android','user':'u1','membership':'m1','refresh':'alice-secret'},'bob_ios':{'platform':'ios','user':'u2','membership':'m2','refresh':'bob-secret'},'bob_android':{'platform':'android','user':'u2','membership':'m2','refresh':'bob2-secret'}}}
        actor=m.actor_config(config,'bob_android','a'*32);self.assertEqual(actor,{'origin':'https://127.0.0.1:12345','ca_pem':'own-ca','tenant':'tenant','conversation':'chat','platform':'android','user':'u2','membership':'m2','refresh':'bob2-secret','role':'bob_android','nonce':'a'*32});self.assertNotIn('alice-secret',json.dumps(actor));self.assertNotIn('bob-secret',json.dumps(actor))
        for role in ['unknown','../alice']:
            with self.assertRaises(ValueError):m.actor_config(config,role,'a'*32)
    def test_network_cut_rejects_non_application_uid_or_unowned_chain(self):
        m=self.module();self.assertEqual(m.network_rule(10123,'IMCROSS_A1','insert'),['shell','su','0','iptables','-I','OUTPUT','1','-m','owner','--uid-owner','10123','-m','comment','--comment','IMCROSS_A1','-j','REJECT'])
        for uid in [0,1000,9999,True,'10123']:
            with self.assertRaises(ValueError):m.network_rule(uid,'IMCROSS_A1','insert')
        for chain in ['OUTPUT','IMCROSS_;rm','']:
            with self.assertRaises(ValueError):m.network_rule(10123,chain,'delete')
    def test_same_user_two_devices_cannot_be_collapsed_into_one_process(self):
        m=self.module();states={'alice':{'role':'alice','platform':'android','nonce':'a'*32,'pid':111,'device_id':'da','user':'ua'},'bob_ios':{'role':'bob_ios','platform':'ios','nonce':'b'*32,'pid':222,'device_id':'db','user':'ub'},'bob_android':{'role':'bob_android','platform':'android','nonce':'c'*32,'pid':111,'device_id':'dc','user':'ub'}}
        m.validate_participants(states)
        for field,value in [('device_id','db'),('nonce','b'*32),('user','ua')]:
            changed=copy.deepcopy(states);changed['bob_android'][field]=value
            with self.assertRaises(ValueError):m.validate_participants(changed)
    def test_device_delivery_is_not_inferred_from_another_device_read(self):
        m=self.module();facts={'messages':2,'devices':3,'roles':{'alice':{'delivered':0,'read':0},'bob_ios':{'delivered':2,'read':2},'bob_android':{'delivered':1,'read':0}},'users':{'alice':0,'bob':2}}
        m.checkpoint('offline_sent',facts)
        for key,value in [('delivered',2),('read',1)]:
            bad=copy.deepcopy(facts);bad['roles']['bob_android'][key]=value
            with self.assertRaises(ValueError):m.checkpoint('offline_sent',bad)
    def test_recovered_delivery_must_still_be_unread_on_that_device(self):
        m=self.module();facts={'messages':2,'devices':3,'roles':{'alice':{'delivered':0,'read':0},'bob_ios':{'delivered':2,'read':2},'bob_android':{'delivered':2,'read':0}},'users':{'alice':0,'bob':2}}
        m.checkpoint('recovered_delivered',facts);facts['roles']['bob_android']['read']=2
        with self.assertRaises(ValueError):m.checkpoint('recovered_delivered',facts)
    def test_final_reply_requires_both_directions_and_per_device_read(self):
        m=self.module();facts={'messages':3,'devices':3,'roles':{'alice':{'delivered':1,'read':1},'bob_ios':{'delivered':2,'read':2},'bob_android':{'delivered':2,'read':2}},'users':{'alice':1,'bob':2}}
        m.checkpoint('reply_read',facts)
        for field,value in [('messages',4),('devices',2)]:
            bad=copy.deepcopy(facts);bad[field]=value
            with self.assertRaises(ValueError):m.checkpoint('reply_read',bad)
    def test_actor_ack_must_match_role_nonce_process_and_exact_command(self):
        m=self.module();expected={'role':'bob_android','nonce':'a'*32,'pid':111};state=dict(expected,id=4,status='ok')
        m.validate_ack(state,expected,4)
        for key,value in [('id',3),('pid',222),('nonce','b'*32),('role','alice'),('status','failed')]:
            with self.assertRaises(ValueError):m.validate_ack(dict(state,**{key:value}),expected,4)
