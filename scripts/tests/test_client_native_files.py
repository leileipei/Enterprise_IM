import importlib.util,os,sys,tempfile,unittest
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from integration.native_client import prepared_file_inputs

class FilesInputTests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
  self.root=Path(self.tmp.name);self.mc=self.root/'mc';self.defs=self.root/'definitions';self.defs.mkdir()
  self.marker=self.root/'executed'
  self.mc.write_text('#!/bin/sh\n/usr/bin/touch '+str(self.marker)+'\n');self.mc.chmod(0o700)
  self.pins={'mc-test-client-sha256-darwin-arm64':'0'*64}
  for name in ('main.cvd','daily.cvd','bytecode.cvd'):(self.defs/name).write_bytes(b'owned fixture')
 def test_missing_mc_is_rejected_before_execution(self):
  with self.assertRaisesRegex(RuntimeError,'prepared_mc_missing'):prepared_file_inputs(None,self.defs,self.pins)
  self.assertFalse(self.marker.exists())
 def test_missing_definitions_are_rejected_before_execution(self):
  with self.assertRaisesRegex(RuntimeError,'prepared_scanner_definitions_missing'):prepared_file_inputs(self.mc,None,self.pins)
  self.assertFalse(self.marker.exists())
 def test_wrong_mc_digest_is_rejected_without_executing_it(self):
  with self.assertRaisesRegex(ValueError,'tool_hash_mismatch'):prepared_file_inputs(self.mc,self.defs,self.pins)
  self.assertFalse(self.marker.exists())
 def test_linked_definition_is_rejected_without_touching_target(self):
  foreign=self.root/'foreign';foreign.write_bytes(b'foreign definition');foreign.chmod(0o400)
  (self.defs/'daily.cvd').unlink();(self.defs/'daily.cvd').symlink_to(foreign)
  with self.assertRaisesRegex(RuntimeError,'prepared_scanner_definitions_missing'):prepared_file_inputs(self.mc,self.defs,self.pins)
  self.assertEqual(foreign.read_bytes(),b'foreign definition');self.assertEqual(foreign.stat().st_mode&0o777,0o400)

class FilesRunnerOutputTests(unittest.TestCase):
 def setUp(self):
  path=Path(__file__).resolve().parents[1]/'test-client-macos-files.py'
  spec=importlib.util.spec_from_file_location('native_files_runner',path)
  self.module=importlib.util.module_from_spec(spec);spec.loader.exec_module(self.module)
 def lifecycle(self):
  common={'source_commit':'a'*40,'owner':'b'*32,'kind':'process','identity':'123','fingerprint':{'start':'owned'}}
  rows=[dict(common,event=event,detail=dict(test='owned',gate='native_files',**detail)) for event,detail in [('registered',{}),('ready',{}),('exited',{'expected_exit':'0','actual_exit':'0'})]]
  return rows,[rows[0]]
 def test_null_exits_cannot_be_accepted_as_observed(self):
  rows,children=self.lifecycle();rows[-1]['detail'].update(expected_exit=None,actual_exit=None)
  with self.assertRaises(ValueError):self.module.verify_child_exits(rows,children)
 def test_other_owner_cannot_supply_matching_ready_and_exit(self):
  rows,children=self.lifecycle();rows[1]['owner']='c'*32;rows[2]['owner']='c'*32
  with self.assertRaises(ValueError):self.module.verify_child_exits(rows,children)
 def test_exact_owned_lifecycle_is_accepted(self):
  rows,children=self.lifecycle();self.module.verify_child_exits(rows,children)
 def test_timeout_retains_opaque_cleanup_handles_without_inventing_exit(self):
  command=[sys.executable,'-c','import sys,time; print("NATIVE_FILES_VAULT_NAMESPACE "+"a"*64,flush=True); print("owned stderr",file=sys.stderr,flush=True); time.sleep(30)']
  code,output,timed_out=self.module.run_backend(command,Path.cwd(),dict(os.environ),1)
  self.assertIsNone(code);self.assertTrue(timed_out)
  self.assertIn('NATIVE_FILES_VAULT_NAMESPACE '+'a'*64,output);self.assertIn('owned stderr',output)
 def test_failed_child_retains_actual_exit_and_output(self):
  code,output,timed_out=self.module.run_backend([sys.executable,'-c','print("owned failure"); raise SystemExit(7)'],Path.cwd(),dict(os.environ),5)
  self.assertEqual(code,7);self.assertFalse(timed_out);self.assertIn('owned failure',output)

if __name__=='__main__':unittest.main()
