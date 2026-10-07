import copy
import importlib.util
import unittest
from pathlib import Path
spec=importlib.util.spec_from_file_location('compare_gates',Path(__file__).with_name('test-import-compare.py'))
gates=importlib.util.module_from_spec(spec);spec.loader.exec_module(gates)
class GateTests(unittest.TestCase):
 def events(self):
  rows=[{'name':k,'exit_code':0,'counts':{'top_pass':1,'sub_pass':0,'fail':0,'skip':0},'executed_tests':gates.REQUIRED_TESTS.get(k,[])} for k in gates.REQUIRED_GATES]
  rows.extend({'name':k,'exit_code':0} for k in gates.REQUIRED_COMMANDS)
  rows.append({'name':'cleanup','removed':True})
  keys=['tenants','legal_entities','organizations','departments','users','user_organizations','user_departments','external_identities','admin_grants','total']
  report={'counts':{k:1 if k in ['tenants','total'] else 0 for k in keys},'classification_counts':{k:{'new':0,'identical':1 if k in ['tenants','total'] else 0,'conflict':0} for k in keys}}
  for row in rows:
   if row['name']=='comparison_database':row['reports']=[report]
  return rows
 def test_positive(self):self.assertTrue(gates.validate_required_gates(self.events())['required_gates_passed'])
 def test_fail_skip_zero_missing_or_cleanup(self):
  for kind in ['fail','skip','zero','missing','command_missing','cleanup']:
   rows=self.events()
   if kind in ['fail','skip']:rows[0]['counts'][kind]=1
   elif kind=='zero':rows[0]['counts']['top_pass']=0
   elif kind=='missing':rows.pop(0)
   elif kind=='command_missing':rows=[r for r in rows if r['name']!='vet_all']
   else:rows[-1]['removed']=False
   self.assertFalse(gates.validate_required_gates(rows)['required_gates_passed'],kind)
 def test_classification_or_required_test_missing(self):
  for mutate in ['count','tests','empty_report']:
   rows=self.events();row=next(r for r in rows if r['name']=='comparison_database')
   if mutate=='count':row['reports'][0]['classification_counts']['total']['new']=1
   elif mutate=='tests':row['executed_tests']=[]
   else:row['reports']=[]
   self.assertFalse(gates.validate_required_gates(rows)['required_gates_passed'])
 def test_full_suite_stays_false(self):
  rows=self.events()+[{'name':'full_suite','exit_code':1,'counts':{'fail':1,'skip':1}}]
  result=gates.validate_required_gates(rows)
  self.assertTrue(result['required_gates_passed']);self.assertFalse(result['full_suite_passed'])
if __name__=='__main__':unittest.main()
