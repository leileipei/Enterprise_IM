"""Real exit/signal/log probes verify orchestration; no business acceptance here."""
import copy,hashlib,importlib.util,json,os,signal,subprocess,sys,tempfile,time,unittest
from pathlib import Path
from unittest.mock import patch
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from integration.model import SourceSnapshot,Toolchain,Inventory,GateEvent,CleanupResult,ResourceRef,FixtureBundle
from integration.results import REQUIRED_GATES,CHECK_GATES,parse_go
from support import go_rows,write_json

class RunTests(unittest.TestCase):
 def setUp(self):
  try:from integration import run,evidence
  except ImportError:self.fail('unified runner and evidence interfaces missing')
  self.run=run;self.evidence=evidence
  self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.base=Path(self.tmp.name).resolve();self.base.chmod(0o700)
  repo=self.base/'repo';repo.mkdir();(repo/'scripts/integration').mkdir(parents=True)
  entry=Path(__file__).resolve().parents[1]/'test-project-integration.py';(repo/'scripts/test-project-integration.py').write_bytes(entry.read_bytes())
  (repo/'scripts/integration/__init__.py').write_text('')
  (repo/'scripts/integration/run.py').write_text("from pathlib import Path\ndef execute_bootstrap(data,output):\n Path(output/'archived-marker').write_text(data['commit'])\n return 1\n")
  env={'PATH':'/usr/bin:/bin','GIT_CONFIG_NOSYSTEM':'1','GIT_CONFIG_GLOBAL':os.devnull}
  for cmd in [['init','-q'],['add','.'],['-c','user.name=Fixture','-c','user.email=fixture@example.invalid','commit','-qm','fixture']]:subprocess.run(['/usr/bin/git',*cmd],cwd=repo,env=env,check=True,capture_output=True)
  commit=subprocess.check_output(['/usr/bin/git','rev-parse','HEAD'],cwd=repo,env=env,text=True).strip()
  self.output=self.base/'attempt';bootstrap_spec=importlib.util.spec_from_file_location('fixture_bootstrap',repo/'scripts/test-project-integration.py');module=importlib.util.module_from_spec(bootstrap_spec);bootstrap_spec.loader.exec_module(module)
  data=module.bootstrap(repo,commit,self.output)
  self.snapshot=SourceSnapshot(commit,repo,Path(data['root']),Path(data['archive']),data['archive_sha256'])
  self.tools=Toolchain({'python':Path(sys.executable)},{},{},{},'darwin','arm64')
 def contract(self,spec,snapshot,tools,bundle,registry,deadline):
  # A short actual process produces a proper unittest protocol for control-flow tests.
  from integration.commands import run_command
  from integration.results import parse_unittest
  log=self.output/'contract.log'
  command=[sys.executable,'-c','import sys,time; print("test_fixture (fixture.Case) ... ok\\n\\nRan 1 test in 0.01s\\n\\nOK",file=sys.stderr,flush=True); time.sleep(0.1)']
  outer=run_command('orchestrator_contract','test',snapshot.commit,command,registry.root/registry.owner,{'PATH':'/usr/bin:/bin'},log,deadline,registry)
  event=parse_unittest(Path(str(log)+'.stderr'),'orchestrator_contract',snapshot.commit,outer.exit_code)
  event.inventory=Inventory({'unittest'},{'unittest':{'fixture.Case.test_fixture'}},set());return event
 def patches(self,prepare):
  return patch.multiple(self.run,discover_toolchain=lambda *a:self.tools,run_stage=self.contract,prepare_services=prepare,gate_specs=lambda *a:[{'name':'orchestrator_contract'}])
 def test_missing_fixture_and_partial_green_never_success(self):
  with self.patches(lambda *a:(_ for _ in ()).throw(ValueError('missing_fixture'))):rc=self.run.execute(self.snapshot,self.output)
  self.assertNotEqual(rc,0);report=json.loads((self.output/'verification.json').read_text())
  self.assertFalse(report['required_gates_passed']);self.assertFalse(report['full_suite_passed']);self.assertTrue(report['cleanup']['removed'])
 def test_cancelled_setup_preserves_report_and_cleanup(self):
  processes=[]
  def prepare(snapshot,tools,registry,private,deadline):
   p=subprocess.Popen(['/bin/sleep','30'],cwd=registry.root/registry.owner,start_new_session=True);processes.append(p)
   from integration.registry import process_fingerprint
   registry.add(ResourceRef('process',registry.owner,str(p.pid),process_fingerprint(p.pid)))
   raise KeyboardInterrupt()
  with self.patches(prepare):self.assertNotEqual(self.run.execute(self.snapshot,self.output),0)
  for p in processes:p.wait(timeout=2)
  report=json.loads((self.output/'verification.json').read_text());self.assertTrue(report['cleanup']['removed']);self.assertIn('cancelled',report['failures'])
 def test_recovered_process_identity_present_in_final_receipt(self):
  processes=[]
  def prepare(snapshot,tools,registry,private,deadline):
   directory=registry.root/registry.owner/'gap';directory.mkdir(mode=0o700)
   proc=subprocess.Popen(['/bin/sleep','30'],cwd=directory,start_new_session=True);processes.append(proc)
   self.assertIsNone(proc.poll())
   raise KeyboardInterrupt()
  try:
   with self.patches(prepare):self.assertNotEqual(self.run.execute(self.snapshot,self.output),0)
   for proc in processes:proc.wait(timeout=2)
   report=json.loads((self.output/'verification.json').read_text());self.assertTrue(report['cleanup']['removed'])
   self.assertTrue(any(row['event']=='registered' and row['kind']=='process' and row['identity']==str(processes[0].pid) for row in report['resource_records']),'recovered actual process omitted from final receipt')
  finally:
   for proc in processes:
    if proc.poll() is None:proc.kill()
    proc.wait()

 def test_full_inventory_early_child_exit(self):
  path=self.output/'early.jsonl';rows=go_rows('fixture',[('TestFirst','pass')]);payload=''.join(json.dumps(x)+'\n' for x in rows)
  with path.open('w') as f:subprocess.run([sys.executable,'-c','import sys;sys.stdout.write('+repr(payload)+')'],stdout=f,check=True)
  event=parse_go(path,'full_repository',self.snapshot.commit,0);event.inventory=Inventory({'fixture'},{'fixture':{'TestFirst','TestMissing'}},set())
  verdict=self.evidence.write_evidence(self.output,self.snapshot,self.tools,[event],CleanupResult(True,[]),event.inventory,set())
  self.assertFalse(verdict.full_suite_passed);self.assertTrue(any('TestMissing' in e for e in verdict.failures))
 def test_signal_stops_new_gates(self):
  calls=[]
  def prepare(*args):calls.append('prepare');os.kill(os.getpid(),signal.SIGTERM);self.fail('signal did not cancel')
  with self.patches(prepare):self.assertNotEqual(self.run.execute(self.snapshot,self.output),0)
  self.assertEqual(calls,['prepare']);report=json.loads((self.output/'verification.json').read_text());self.assertIn('cancelled',report['failures'])
 def test_raw_secret_is_not_published_or_retained(self):
  secret='private-fixture-password-123';path=self.output/'raw.log';path.write_text(secret+'\npostgresql://reader:uri-password-unregistered@127.0.0.1:45678/db\n-----BEGIN PRIVATE KEY-----\nfixture-secret\n-----END PRIVATE KEY-----\n')
  event=GateEvent('toolchain','check',self.snapshot.commit,0,path,checks=['tool_fixture'])
  self.evidence.write_evidence(self.output,self.snapshot,self.tools,[event],CleanupResult(True,[]),Inventory(set(),{},set()),{secret})
  self.assertFalse(path.exists(),'raw credential-bearing log must be disposed')
  for p in self.output.rglob('*'):
   if p.is_file() and p.suffix in ['.json','.log','.txt']:
    self.assertNotIn(secret,p.read_text());self.assertNotIn('uri-password-unregistered',p.read_text())
  report=json.loads((self.output/'verification.json').read_text());self.assertTrue(report['log_dispositions'][0]['original_disposed']);self.assertNotIn('BEGIN PRIVATE KEY',json.dumps(report))
 def test_foreign_original_is_never_removed_or_published(self):
  path=self.base/'foreign.log';secret='outside-private-value';path.write_text(secret)
  event=GateEvent('toolchain','check',self.snapshot.commit,0,path,checks=['fixture'])
  verdict=self.evidence.write_evidence(self.output,self.snapshot,self.tools,[event],CleanupResult(True,[]),Inventory(set(),{},set()),{secret})
  self.assertFalse(verdict.required_gates_passed)
  self.assertTrue(path.exists(),'foreign log was deleted by redaction')
  self.assertEqual(path.read_text(),secret)
  self.assertFalse((self.output/'share').exists(),'foreign log was published')

 def green_events(self,resource_receipts=True):
  rows=[]
  inventory=Inventory({'fixture'},{'fixture':{'TestProtocol'}},set())
  for name in REQUIRED_GATES:
   path=self.output/(name+'.log')
   if name in CHECK_GATES:
    path.write_text('fixture check\n');event=GateEvent(name,'check',self.snapshot.commit,0,path,checks=['protocol_fixture'])
   else:
    write_json(path,go_rows('fixture',[('TestProtocol','pass')]));event=parse_go(path,name,self.snapshot.commit,0);event.inventory=inventory
   rows.append(event)
  if resource_receipts:
   from test_project_integration_gates import GateTests
   fixture=GateTests();fixture.setUp()
   self.addCleanup(fixture.doCleanups)
   fixture.root=self.output/'linux-resource';fixture.root.mkdir(mode=0o700)
   fixture.snapshot=self.snapshot;fixture.resource_fixture()
   event=fixture.g.parse_resource_logs(fixture.root,self.snapshot.commit,0)
   self.assertFalse(event.failures)
   rows=[event if r.name=='file_resources' else r for r in rows]
   self.tools.images['alpine-resource-image']={'image_id':'sha256:'+'a'*64}
   self.snapshot.resource_records=[dict(kind='container',event='registered',identity=format(n,'064x'),owner='b'*32,source_commit=self.snapshot.commit) for n in range(1,5)]
  return rows,inventory

 def test_resource_delivery_rejects_missing_bound_artifacts(self):
  events,inventory=self.green_events(resource_receipts=False)
  self.evidence.write_evidence(self.output,self.snapshot,self.tools,events,CleanupResult(True,[]),inventory,set())
  with self.assertRaisesRegex(ValueError,'resource'):
   self.evidence.validate_delivery(self.snapshot.commit,self.output/'verification.json',self.snapshot.repository_root)

 def test_resource_rehashed_claims_cannot_hide_wrong_limits_or_binary(self):
  events,inventory=self.green_events()
  self.evidence.write_evidence(self.output,self.snapshot,self.tools,events,CleanupResult(True,[]),inventory,set())
  report=self.output/'verification.json';baseline=json.loads(report.read_text())
  for name,change in [('resource.log.proof.json',lambda d:d.update(memory=1)),
                      ('resource.log.proof.json',lambda d:d.update(environment={'unexpected':'synthetic-not-a-credential'})),
                      ('resource-build.json',lambda d:d['binaries'][0].update(sha256='f'*64))]:
   with self.subTest(artifact=name):
    data=copy.deepcopy(baseline);event=next(e for e in data['events'] if e['name']=='file_resources')
    artifact=next(a for a in event['resource_evidence']['artifacts'] if a['name']==name)
    row=next(r for r in data['log_dispositions'] if r['original_path']==artifact['original_path'])
    original=Path(row['original_path']);published=self.output/row['published_path'];raw=original.read_bytes()
    value=json.loads(raw);change(value);changed=json.dumps(value).encode()
    original.write_bytes(changed);published.write_bytes(changed);digest=hashlib.sha256(changed).hexdigest()
    row['original_sha256']=digest;row['published_sha256']=digest;artifact['sha256']=digest
    report.write_text(json.dumps(data))
    try:
     with self.assertRaisesRegex(ValueError,'resource'):
      self.evidence.validate_delivery(self.snapshot.commit,report,self.snapshot.repository_root)
    finally:original.write_bytes(raw);published.write_bytes(raw)
  report.write_text(json.dumps(baseline))
  self.evidence.validate_delivery(self.snapshot.commit,report,self.snapshot.repository_root)

 def test_changed_hash_and_false_cleanup_override_green(self):
  events,inventory=self.green_events();verdict=self.evidence.write_evidence(self.output,self.snapshot,self.tools,events,CleanupResult(True,[]),inventory,set())
  self.assertTrue(verdict.required_gates_passed,'synthetic all-gate protocol fixture only')
  report=self.output/'verification.json';self.evidence.validate_delivery(self.snapshot.commit,report,self.snapshot.repository_root)
  data=json.loads(report.read_text());log=self.output/data['log_dispositions'][0]['published_path'];log.write_text('tampered')
  with self.assertRaises(ValueError):self.evidence.validate_delivery(self.snapshot.commit,report,self.snapshot.repository_root)
  data['cleanup']['removed']=False;report.write_text(json.dumps(data))
  with self.assertRaises(ValueError):self.evidence.validate_delivery(self.snapshot.commit,report,self.snapshot.repository_root)
 def test_unexpected_stage_error_keeps_raw_log_and_runs_next_gate(self):
  tools=copy.deepcopy(self.tools);tools.paths['go']=Path(sys.executable);calls=[]
  def specs(*args):return [{'name':'orchestrator_contract','kind':'test','env_group':'orchestrator_contract','argv':[]},{'name':'file_messages','kind':'test','env_group':'file_messages','argv':[]},{'name':'web_files','kind':'test','env_group':'web_files','argv':[]}]
  def probe(bundle,*args):
   path=self.output/next(iter(bundle.owners))/'probe.log';path.write_text('controlled fixture protocol')
   return GateEvent('fixture_preflight','check',self.snapshot.commit,0,path,checks=['protocol_fixture'])
  def stage(spec,snapshot,tools,bundle,registry,deadline):
   calls.append(spec['name'])
   if spec['name']=='orchestrator_contract':return self.contract(spec,snapshot,tools,bundle,registry,deadline)
   path=registry.root/registry.owner/'gates'/spec['env_group'];path.mkdir(mode=0o700,parents=True,exist_ok=True)
   raw=path/'business.jsonl';write_json(raw,go_rows('fixture',[('TestProtocol','pass')]))
   if spec['name']=='file_messages':raise UnboundLocalError('controlled parser regression')
   event=parse_go(raw,spec['name'],snapshot.commit,0);event.inventory=Inventory({'fixture'},{'fixture':{'TestProtocol'}},set());return event
  from integration.commands import run_command as actual_command
  def iam_command(name,kind,commit,argv,cwd,env,log,deadline,registry):
   if name=='orchestrator_contract':return actual_command(name,kind,commit,argv,cwd,env,log,deadline,registry)
   write_json(log,go_rows('github.com/leileipei/Enterprise_IM/internal/testfixtures',[( 'TestIntegrationFixtureIAM/'+name,'pass') for name in self.run.IAM_SUBCASES]+[('TestIntegrationFixtureIAM','pass')]));Path(str(log)+'.stderr').write_text('')
   return GateEvent(name,kind,commit,0,log)
  with patch.multiple(self.run,discover_toolchain=lambda *a:tools,gate_specs=specs,run_stage=stage,prepare_services=lambda *a:FixtureBundle({},set(),a[2].path,{a[2].owner},{'source_commit':self.snapshot.commit}),prepare_iam=lambda b,*a:b,prepare_scanner=lambda b,*a:b,complete_profile=lambda *a:None,probe_services=probe,probe_scanner=probe,probe_browser=probe,collect_inventory=lambda *a:Inventory({'fixture'},{'fixture':{'TestProtocol'}},set())),patch('integration.commands.run_command',side_effect=iam_command):
   self.assertNotEqual(self.run.execute(self.snapshot,self.output),0)
  self.assertEqual(calls,['orchestrator_contract','file_messages','web_files'])
  report=json.loads((self.output/'verification.json').read_text());self.assertTrue(report['cleanup']['removed'])
  self.assertTrue(any(r['original_path'].endswith('business.jsonl') and r['published_path'] for r in report['log_dispositions']))
  self.assertTrue(any('UnboundLocalError' in x for x in report['failures']))

 def test_iam_top_only_cannot_start_business_gates(self):
  tools=copy.deepcopy(self.tools);tools.paths['go']=Path(sys.executable);calls=[]
  def specs(*args):return [{'name':'orchestrator_contract','kind':'test','env_group':'orchestrator_contract','argv':[]},{'name':'file_messages','kind':'test','env_group':'file_messages','argv':[]}]
  def probe(bundle,*args):
   path=self.output/next(iter(bundle.owners))/'probe.log';path.write_text('controlled fixture protocol')
   return GateEvent('fixture_preflight','check',self.snapshot.commit,0,path,checks=['protocol_fixture'])
  def stage(spec,snapshot,tools,bundle,registry,deadline):
   calls.append(spec['name'])
   if spec['name']=='orchestrator_contract':return self.contract(spec,snapshot,tools,bundle,registry,deadline)
   path=registry.root/registry.owner/'unexpected.jsonl';write_json(path,go_rows('fixture',[('TestProtocol','pass')]))
   return parse_go(path,spec['name'],snapshot.commit,0)
  from integration.commands import run_command as actual_command
  def iam_command(name,kind,commit,argv,cwd,env,log,deadline,registry):
   if name=='orchestrator_contract':return actual_command(name,kind,commit,argv,cwd,env,log,deadline,registry)
   write_json(log,go_rows('github.com/leileipei/Enterprise_IM/internal/testfixtures',[('TestIntegrationFixtureIAM','pass')]));Path(str(log)+'.stderr').write_text('')
   return GateEvent(name,kind,commit,0,log)
  with patch.multiple(self.run,discover_toolchain=lambda *a:tools,gate_specs=specs,run_stage=stage,prepare_services=lambda *a:FixtureBundle({},set(),a[2].path,{a[2].owner},{'source_commit':self.snapshot.commit}),prepare_iam=lambda b,*a:b,prepare_scanner=lambda b,*a:b,complete_profile=lambda *a:None,probe_services=probe,probe_scanner=probe,probe_browser=probe,collect_inventory=lambda *a:Inventory({'fixture'},{'fixture':{'TestProtocol'}},set())),patch('integration.commands.run_command',side_effect=iam_command):
   self.assertNotEqual(self.run.execute(self.snapshot,self.output),0)
  self.assertEqual(calls,['orchestrator_contract'],'missing IAM matrix was accepted before business execution')
  report=json.loads((self.output/'verification.json').read_text())
  self.assertTrue(any('missing_required_subtest' in x or 'missing_required_pass' in x for x in report['failures']))

 def test_global_deadline_interrupts_blocked_setup(self):
  self.assertTrue(hasattr(self.run,'TOTAL_SECONDS'),'global deadline timer missing')
  def prepare(*args):time.sleep(5);self.fail('deadline failed to interrupt blocked setup')
  with self.patches(prepare),patch.object(self.run,'TOTAL_SECONDS',1),patch.object(self.run,'CLEANUP_SECONDS',0.2):
   start=time.monotonic();self.assertNotEqual(self.run.execute(self.snapshot,self.output),0)
  self.assertLess(time.monotonic()-start,3)
  report=json.loads((self.output/'verification.json').read_text());self.assertTrue(report['cleanup']['removed']);self.assertTrue(any('attempt_deadline' in x for x in report['failures']))

 def test_complete_profile_has_explicit_writer_and_tls_paths(self):
  from integration.model import FixtureBundle
  complete=getattr(self.run,'complete_profile',None)
  self.assertIsNotNone(complete,'full-suite import profile missing')
  bundle=FixtureBundle({'IM_TEST_DATABASE_URL':'postgresql://postgres:admin@127.0.0.1:45678/enterprise_im_files?sslmode=verify-full'}, {'admin','writer'}, self.output/'registry.jsonl',set(),{'tls_ca':'/private/ca.crt','tls_wrong_ca':'/private/wrong-ca.crt','role_passwords':{'p431_import_writer':'writer'},'minio_admin':('admin-key','admin-secret')})
  complete(bundle,self.output)
  values=bundle.environment
  self.assertIn('p431_import_writer:writer@127.0.0.1:45678',values['IM_IMPORT_APPLY_TEST_DATABASE_URL'])
  self.assertEqual(values['IM_COMPARE_TEST_CA'],'/private/ca.crt');self.assertEqual(values['IM_COMPARE_TEST_WRONG_CA'],'/private/wrong-ca.crt')
  self.assertEqual(Path(values['IM_COMPARE_TEST_BINARY']),self.output/'bin/im-import-compare')
  self.assertEqual(values['IM_TEST_S3_ADMIN_ACCESS_KEY'],'admin-key')

 def test_publication_error_still_cleans_owned_process(self):
  processes=[]
  def prepare(snapshot,tools,registry,private,deadline):
   process=subprocess.Popen(['/bin/sleep','30'],cwd=registry.root/registry.owner,start_new_session=True);processes.append(process)
   from integration.registry import process_fingerprint
   registry.add(ResourceRef('process',registry.owner,str(process.pid),process_fingerprint(process.pid)))
   raise KeyboardInterrupt()
  actual=self.evidence.write_evidence;attempt=[0]
  def write(*args):
   attempt[0]+=1
   if attempt[0]==1:raise OSError('controlled publication error')
   return actual(*args)
  try:
   with self.patches(prepare),patch.object(self.run,'write_evidence',side_effect=write):self.assertNotEqual(self.run.execute(self.snapshot,self.output),0)
   for process in processes:process.wait(timeout=2)
   self.assertTrue(json.loads((self.output/'verification.json').read_text())['cleanup']['removed'])
  finally:
   for process in processes:
    if process.poll() is None:process.kill()
    process.wait()

 def test_python_inventory_is_discovered_without_run_events(self):
  from integration import source
  discover=getattr(source,'collect_python_inventory',None)
  self.assertIsNotNone(discover,'independent Python discovery missing')
  tests=self.snapshot.root/'scripts/tests';tests.mkdir()
  (tests/'test_project_integration_minimal.py').write_text('import unittest\nclass Cases(unittest.TestCase):\n def test_first(self):pass\n def test_missing(self):pass\n')
  inventory=discover(self.snapshot,self.tools,{'PATH':'/usr/bin:/bin','PYTHONDONTWRITEBYTECODE':'1'})
  self.assertEqual(inventory.tests['unittest'],{'test_project_integration_minimal.Cases.test_first','test_project_integration_minimal.Cases.test_missing'})

 def test_cli_loads_archived_runner_and_missing_parameters_fail(self):
  entry=self.snapshot.repository_root/'scripts/test-project-integration.py'
  (self.snapshot.repository_root/'scripts/integration/run.py').write_text("raise RuntimeError('working_tree_must_not_load')\n")
  out=self.base/'cli-attempt';env={'PATH':'/usr/bin:/bin','PYTHONDONTWRITEBYTECODE':'1','TRAP_PASSWORD':'do-not-print-private-value'}
  result=subprocess.run([sys.executable,str(entry),'--source-commit',self.snapshot.commit,'--output-dir',str(out)],env=env,capture_output=True,text=True)
  self.assertEqual(result.returncode,1);self.assertEqual((out/'archived-marker').read_text(),self.snapshot.commit)
  result=subprocess.run([sys.executable,str(entry)],env=env,capture_output=True,text=True);self.assertNotEqual(result.returncode,0);self.assertNotIn(env['TRAP_PASSWORD'],result.stdout+result.stderr)

if __name__=='__main__':unittest.main()
