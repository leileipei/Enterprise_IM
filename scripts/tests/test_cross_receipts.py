"""Reject incomplete native automatic receipt recovery evidence."""
import importlib,sys,unittest
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
class ReceiptRetryTests(unittest.TestCase):
 def test_retry_requires_repeated_unforwarded_failure_and_same_request_success(self):
  m=importlib.import_module('integration.cross_receipts')
  rows=[{'device':'d','action':'read','status':503,'forwarded':False,'injected':True,'body_sha256':'a'*64,'elapsed_ms':0},{'device':'d','action':'read','status':503,'forwarded':False,'injected':True,'body_sha256':'a'*64,'elapsed_ms':2001},{'device':'d','action':'read','status':200,'forwarded':True,'injected':False,'body_sha256':'a'*64,'elapsed_ms':4002}]
  m.retry_proof(rows,'d','read')
  for change in [{'elapsed_ms':2200},{'body_sha256':'b'*64},{'forwarded':False},{'status':503}]:
   with self.assertRaises(ValueError):m.retry_proof(rows[:-1]+[{**rows[-1],**change}],'d','read')
  with self.assertRaises(ValueError):m.retry_proof(rows[1:],'d','read')
 def test_native_failure_must_not_advance_read_or_discard_pending_page(self):
  m=importlib.import_module('integration.cross_receipts')
  row={'cursor':'1','pending_receipts':1,'pending_next':['1'],'seqs':[],'read_seqs':[],'can_send':False,'network_status':503}
  m.delivery_blocked(row)
  for k,v in [('pending_receipts',0),('pending_next',[]),('cursor','0'),('seqs',['1']),('read_seqs',['1']),('can_send',True),('network_status',None)]:
   with self.assertRaises(ValueError):m.delivery_blocked(dict(row,**{k:v}))
