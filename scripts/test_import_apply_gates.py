import importlib.util
import unittest
from pathlib import Path
spec=importlib.util.spec_from_file_location('apply_gates',Path(__file__).with_name('test-import-apply.py'))
gates=importlib.util.module_from_spec(spec);spec.loader.exec_module(gates)
class ApplyGateTests(unittest.TestCase):
 def events(self):
  rows=[dict(name=k,exit_code=0,counts=dict(top_pass=1,sub_pass=1,fail=0,skip=0),executed_tests=gates.REQUIRED_TESTS[k]) for k in gates.REQUIRED_GATES]
  rows.extend(dict(name=k,exit_code=0) for k in gates.REQUIRED_COMMANDS)
  rows.append(dict(name='cleanup',removed=True))
  return rows
 def test_positive(self):self.assertTrue(gates.validate_required_gates(self.events())['required_gates_passed'])
 def test_missing_skip_failure_zero_duplicate_and_cleanup(self):
  for case in ['missing','skip','fail','zero','duplicate','cleanup','command']:
   rows=self.events()
   if case=='missing':rows.pop(0)
   elif case in ['skip','fail']:rows[0]['counts'][case]=1
   elif case=='zero':rows[0]['counts']['top_pass']=0
   elif case=='duplicate':rows.append(dict(rows[0]))
   elif case=='cleanup':rows[-1]['removed']=False
   elif case=='command':rows=[r for r in rows if r['name']!='vet_all']
   self.assertFalse(gates.validate_required_gates(rows)['required_gates_passed'],case)
 def test_names_and_real_database_evidence(self):
  for name in gates.REQUIRED_GATES:
   rows=self.events();next(r for r in rows if r['name']==name)['executed_tests']=['TestPureFunctionOnly']
   self.assertFalse(gates.validate_required_gates(rows)['required_gates_passed'],name)
 def test_full_suite_independent(self):
  rows=self.events()+[dict(name='full_suite',exit_code=1,counts=dict(top_pass=10,fail=1,skip=1),failed_tests=['legacy'],skipped_tests=['missing-fixture'])]
  result=gates.validate_required_gates(rows);self.assertTrue(result['required_gates_passed']);self.assertFalse(result['full_suite_passed']);self.assertEqual(result['failed_tests'],['legacy'])
if __name__=='__main__':unittest.main()
