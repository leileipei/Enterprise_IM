import ast,copy,hashlib,importlib.util,json,sys,tempfile,unittest
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from integration.model import SourceSnapshot,Inventory

class GateTests(unittest.TestCase):
 def setUp(self):
  try:from integration import gates
  except ImportError:self.fail('gate adapter interfaces missing')
  self.g=gates;self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup);self.root=Path(self.temp.name).resolve()
  self.snapshot=SourceSnapshot('c'*40,self.root/'readonly-repo',Path(__file__).resolve().parents[2],self.root/'source.tar','d'*64)
  self.inventory=Inventory(set(),{},set())
 def specs(self):return {s['name']:s for s in self.g.gate_specs(self.snapshot,self.inventory)}
 def test_archive_script_with_readonly_git_root(self):
  spec=self.specs()['import_append']
  self.assertEqual(set(spec),{'name','kind','argv','cwd','env_group','required'})
  self.assertEqual(spec['cwd'],self.snapshot.repository_root)
  self.assertEqual(Path(spec['argv'][1]),self.snapshot.root/'scripts/test-import-apply.py')
  self.assertIn('--required-only',spec['argv']);self.assertNotIn('--reuse-full-suite',spec['argv'])
 def report(self):
  loader=importlib.util.spec_from_file_location('legacy',self.snapshot.root/'scripts/test-import-apply.py');legacy=importlib.util.module_from_spec(loader);loader.loader.exec_module(legacy)
  (self.root/'source.tar').write_bytes(b'fixed archive');(self.root/'bin').mkdir(exist_ok=True);(self.root/'bin/probe').write_bytes(b'actual binary fixture')
  rows=[]
  for n in legacy.REQUIRED_GATES:
   log=self.root/(n+'.log');names=set(legacy.REQUIRED_TESTS[n]);names.update(x.split('/')[0] for x in names.copy())
   events=[{'Action':'start','Package':'fixture/'+n}]
   for test in sorted(names):events += [{'Action':'run','Package':'fixture/'+n,'Test':test},{'Action':'pass','Package':'fixture/'+n,'Test':test}]
   events.append({'Action':'pass','Package':'fixture/'+n});log.write_text(''.join(json.dumps(e)+'\n' for e in events))
   rows.append(dict(name=n,exit_code=0,counts={'top_pass':sum('/' not in x for x in names),'sub_pass':sum('/' in x for x in names),'fail':0,'skip':0},executed_tests=sorted(names),failed_tests=[],skipped_tests=[],log=log.name,log_sha256=hashlib.sha256(log.read_bytes()).hexdigest()))
  rows.extend(dict(name=n,exit_code=0) for n in legacy.REQUIRED_COMMANDS);rows.append({'name':'cleanup','removed':True})
  return {'source_commit':self.snapshot.commit,'source_archive_sha256':hashlib.sha256((self.root/'source.tar').read_bytes()).hexdigest(),'events':rows,'bin_sha256':{'probe':hashlib.sha256((self.root/'bin/probe').read_bytes()).hexdigest()},'cleanup':{'removed':True},'full_suite_status':'not_executed','full_suite_source_commit':'','full_suite_passed':False,'required_gates_passed':True}
 def validate(self,report):
  path=self.root/'verification.json';path.write_text(json.dumps(report));return self.g.validate_import_child(path,self.snapshot.commit)
 def test_import_required_only_is_not_full_pass(self):
  report=self.report();event=self.validate(report);self.assertFalse(event.failures)
  for change in [{'full_suite_passed':True},{'full_suite_source_commit':self.snapshot.commit},{'full_suite_status':'passed'}]:
   changed=dict(report,**change);self.assertTrue(self.validate(changed).failures)
  changed=copy.deepcopy(report);changed['events'].append({'name':'full_suite','exit_code':0});self.assertTrue(self.validate(changed).failures)
 def test_child_source_and_hash_mismatch(self):
  report=self.report();report['source_commit']='e'*40;self.assertTrue(self.validate(report).failures)
  report=self.report();report['source_archive_sha256']='f'*64;self.assertTrue(self.validate(report).failures)
  report=self.report();(self.root/'unit.log').write_text('changed');self.assertTrue(self.validate(report).failures)
  report=self.report();(self.root/'bin/probe').write_bytes(b'changed');self.assertTrue(self.validate(report).failures)
  report=self.report();report['events'][0]['log']='../outside';self.assertTrue(self.validate(report).failures)
 def test_all_existing_required_names_preserved(self):
  specs=self.specs()
  for gate,n in [('file_messages',8),('file_download_retention',16),('web_files',11)]:self.assertEqual(len(specs[gate]['required']),n)
  for name,script in [('file_messages','test-file-messages.sh'),('file_download_retention','test-file-download-retention.sh'),('web_files','test-web-files.sh')]:
   import re
   literal=re.search(r'^required=(\{[^\n]+\})$',(self.snapshot.root/'scripts'/script).read_text(),re.M).group(1)
   self.assertEqual(specs[name]['required'],ast.literal_eval(literal))
  self.assertEqual(len(specs['file_business_process']['required']),15)
  self.assertIn('TestFileBusinessProcessRP08/slow_client_SIGTERM',specs['file_business_process']['required'])
  self.assertIn('TestRealBrowserLoginRealtimeAndOfflinePull',specs['message_realtime']['required'])
  self.assertIn('./internal/access',specs['message_realtime']['argv']);self.assertIn('./internal/oidcauth',specs['message_realtime']['argv'])
 def test_import_process_is_owned_while_git_root_remains_readonly(self):
  from unittest.mock import patch
  from integration.model import FixtureBundle,Toolchain,GateEvent
  from integration.registry import Registry
  registry=Registry(self.root/'registry.jsonl','b'*32,self.snapshot.commit)
  tools=Toolchain({'python':Path(sys.executable),'node':Path('/bin/echo'),'chrome':Path('/bin/echo')},{},{},{},'darwin','arm64')
  bundle=FixtureBundle({},set(),registry.path,{registry.owner},{'source_commit':self.snapshot.commit})
  calls=[]
  def command(name,kind,commit,argv,cwd,env,log,deadline,registry):
   calls.append((argv,cwd));return GateEvent(name,kind,commit,1,log)
  with patch.object(self.g,'run_command',side_effect=command),patch.object(self.g,'docker_environment',return_value={'DOCKER_HOST':'local','PATH':'/usr/bin'}):
   self.g.run_stage(self.specs()['import_append'],self.snapshot,tools,bundle,registry,1e12)
  argv,cwd=calls[0]
  self.assertEqual(cwd,registry.root/registry.owner)
  self.assertEqual(argv[argv.index('--repository-root')+1],str(self.snapshot.repository_root))

 def test_component_branch_parses_actual_output_without_realtime_import(self):
  from unittest.mock import patch
  from integration.model import FixtureBundle,Toolchain,GateEvent
  from integration.registry import Registry
  from support import go_rows
  registry=Registry(self.root/'registry.jsonl','b'*32,self.snapshot.commit)
  tools=Toolchain({'python':Path(sys.executable),'go':Path('/opt/homebrew/bin/go'),'node':Path('/bin/echo'),'chrome':Path('/bin/echo')},{},{},{},'darwin','arm64')
  bundle=FixtureBundle({},set(),registry.path,{registry.owner},{'source_commit':self.snapshot.commit})
  package='github.com/leileipei/Enterprise_IM/internal/files'
  inventory=Inventory({package},{package:{'TestFileControlled'}},set())
  def command(name,kind,commit,argv,cwd,env,log,deadline,registry):
   import subprocess
   payload=''.join(json.dumps(r)+'\n' for r in go_rows(package,[('TestFileControlled','pass')]))
   with (log.parent/'components.json').open('w') as stream:subprocess.run([sys.executable,'-c','import sys;sys.stdout.write('+repr(payload)+')'],stdout=stream,check=True)
   (log.parent/'assembly.json').write_text('');log.write_text('completed');Path(str(log)+'.stderr').write_text('')
   return GateEvent(name,kind,commit,0,log)
  with patch.object(self.g,'run_command',side_effect=command),patch.object(self.g,'collect_inventory',return_value=inventory),patch.object(self.g,'docker_environment',return_value={'DOCKER_HOST':'local','PATH':'/usr/bin'}):
   event=self.g.run_stage(self.specs()['file_components'],self.snapshot,tools,bundle,registry,1e12)
  self.assertEqual(event.exit_code,0);self.assertFalse(event.failures);self.assertIn(package+'::TestFileControlled',event.passed)

 def test_resource_registration_precedes_container_start(self):
  try:from integration import resource_command
  except ImportError:self.fail('owned resource launcher missing')
  from unittest.mock import patch
  from integration.registry import Registry
  registry=Registry(self.root/'registry.jsonl','b'*32,self.snapshot.commit)
  cid='d'*64;calls=[]
  def docker(args,**kwargs):
   calls.append(args)
   if args[0]=='create':return cid+'\n'
   if args[0]=='inspect':return json.dumps([{'Id':cid,'Config':{'Labels':{'im.integration.owner':'b'*32,'im.integration.source':self.snapshot.commit}},'HostConfig':{'Memory':536870912,'NanoCpus':1000000000,'Tmpfs':{'/limited':'size=1048576,mode=0700'}}}])
   if args[0]=='start':
    self.assertTrue(any(r['identity']==cid and r['event']=='registered' for r in registry.records()))
    return 'PASS\n'
   if args[0]=='rm':return ''
   self.fail(args)
  with patch.object(resource_command,'docker',side_effect=docker),patch.object(registry,'_owned_container',return_value=None),patch('sys.stdout'):
   report=resource_command.launch(['--rm','--memory=512m','--cpus=1','--tmpfs','/limited:size=1048576,mode=0700','alpine@sha256:'+'a'*64,'/fixtures/transfer.test'],registry,self.root/registry.owner/'proof.json')
  self.assertEqual(report['memory'],536870912);self.assertEqual(report['nano_cpus'],1000000000)
  self.assertEqual([a[0] for a in calls][:3],['create','inspect','start'])

 def test_linux_resource_names_not_zero_match(self):
  spec=self.specs()['file_resources'];self.assertIn('TestScannerRealResourceBoundary',spec['required']);self.assertIn('TestFileTransferRealDiskFull',spec['required'])
  event=self.g.parse_resource_logs(self.root,self.snapshot.commit,0)
  self.assertTrue(event.failures);self.assertEqual(len(event.passed),0)

if __name__=='__main__':unittest.main()
