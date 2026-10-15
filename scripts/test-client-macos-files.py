#!/usr/bin/env python3
# Prepared tools only. No signing changes or shared tool/runtime replacement.
import argparse,hashlib,json,os,re,secrets,subprocess,sys,tempfile,time,shutil
from pathlib import Path
repo=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(repo/'scripts'))
from integration.model import SourceSnapshot,Toolchain
from integration.registry import Registry
from integration.services import prepare_services
from integration.iam import prepare_iam
from integration.scanner import prepare_scanner,probe_scanner
from integration.tools import _lock,file_digest,probe_version,verify_digest
from integration.native_client import client_preflight,prepared_file_inputs
def run_backend(command,cwd,env,timeout):
 try:
  completed=subprocess.run(command,cwd=cwd,env=env,capture_output=True,text=True,timeout=timeout)
  return completed.returncode,completed.stdout+completed.stderr,False
 except subprocess.TimeoutExpired as exc:
  # subprocess.run killed and waited for its child. Preserve partial recovery
  # handles; its normal product exit was not observed and must stay unknown.
  def decoded(value):return value.decode('utf-8',errors='replace') if isinstance(value,bytes) else value or ''
  return None,decoded(exc.stdout)+decoded(exc.stderr),True

def verify_child_exits(rows,children):
 for child in children:
  def same(row):
   return all(row.get(k)==child.get(k) for k in ('source_commit','owner','kind','identity','fingerprint')) and all(row.get('detail',{}).get(k)==child.get('detail',{}).get(k) for k in ('gate','test'))
  lifecycle=[row for row in rows if same(row)]
  exits=[r for r in lifecycle if r['event']=='exited'];ready=[r for r in lifecycle if r['event']=='ready']
  if len(ready)!=1 or len(exits)!=1:raise ValueError('root_child_actual_exit_missing_or_different')
  detail=exits[0]['detail']
  if not detail.get('expected_exit') or detail.get('actual_exit')!=detail['expected_exit']:raise ValueError('root_child_actual_exit_missing_or_different')

def main():
 parser=argparse.ArgumentParser(description='Owned attachment/device backend gate; native mode requires an empty OS account and existing signing material.')
 parser.add_argument('--target',choices=('macos','host'),default='macos')
 for name in ('flutter-sdk','pub-cache','go','output'):
  parser.add_argument('--'+name,type=Path,required=True)
 parser.add_argument('--isolated-test-account',action='store_true')
 parser.add_argument('--mc',type=Path)
 parser.add_argument('--definition-source',type=Path)
 args=parser.parse_args()
 if any(not p.is_absolute() for p in (args.flutter_sdk,args.pub_cache,args.go,args.output)):
  parser.error('all client paths must be absolute')
 args.output.mkdir(mode=0o700,parents=False,exist_ok=False)
 label='result';out=args.output
 preflight=client_preflight(args)
 pins=_lock(repo/'testdata/file-runtime/versions.lock');pins.update(_lock(repo/'testdata/project-integration/versions.lock'))
 if preflight['status']=='ready':
  try:prepared_file_inputs(args.mc,args.definition_source,pins)
  except (RuntimeError,ValueError) as exc:preflight.update(status='not_run',blocked_reason=str(exc))
 (out/'preflight.json').write_text(json.dumps(preflight,indent=2)+'\n')
 if preflight['status']!='ready':
  print(json.dumps(preflight));sys.exit(2)
 commit=subprocess.check_output(['git','rev-parse','HEAD'],cwd=repo,text=True).strip()
 root=Path(tempfile.mkdtemp(prefix='imnf-',dir='/private/tmp')).resolve();root.chmod(0o700);owner=secrets.token_hex(16);registry=Registry(root/'registry.jsonl',owner,commit);private=root/owner
 bundle=None;code=1;result={'runner_sha256':hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),'label':label,'source_commit':commit,'target':args.target,'scope':('compiled Debug macOS file/device controllers and OS vault; generated picker/memory export, not interactive login or Release acceptance' if args.target=='macos' else 'real Flutter host controllers and production API/scanner/storage; not native UI or OS plugins')}
 def source_hashes():
  files=subprocess.check_output(['git','ls-files','-z','--','clients/enterprise_im'],cwd=repo).split(b'\0')
  files+=subprocess.check_output(['git','ls-files','--others','--exclude-standard','-z','--','clients/enterprise_im'],cwd=repo).split(b'\0')
  paths={os.fsdecode(f) for f in files if f}
  paths.update(['internal/httpserver/devices.go','internal/httpserver/devices_test.go','internal/policystore/native_files_probe_test.go','internal/policystore/native_receipt_probe_test.go','internal/policystore/native_backend_launch_helpers_test.go','internal/testfixtures/integration_registry.go','internal/testfixtures/integration_browser.go','scripts/test-client-macos-files.py','scripts/integration/native_client.py','scripts/integration/services.py','scripts/integration/iam.py','scripts/integration/scanner.py','scripts/integration/tools.py'])
  return {p:hashlib.sha256((repo/p).read_bytes()).hexdigest() for p in sorted(paths)}
 try:
  result['source_sha256']=source_hashes()
  result['tracked_diff_sha256']=hashlib.sha256(subprocess.check_output(['git','diff','HEAD'],cwd=repo)).hexdigest()
  pins=_lock(repo/'testdata/file-runtime/versions.lock');pins.update(_lock(repo/'testdata/project-integration/versions.lock'))
  paths={k:Path(v) for k,v in dict(go=str(args.go),docker='/usr/local/bin/docker',openssl='/usr/bin/openssl',qpdf='/opt/homebrew/bin/qpdf',clamd='/opt/homebrew/sbin/clamd',sigtool='/opt/homebrew/bin/sigtool',freshclam='/opt/homebrew/bin/freshclam',mc=str(args.mc)).items()}
  versions={k:probe_version(paths[k],k,pins['clamav'] if k in ('clamd','sigtool','freshclam') else pins[k]) for k in ('go','qpdf','clamd','sigtool','freshclam')};verify_digest(paths['mc'],pins['mc-test-client-sha256-darwin-arm64'])
  images={k:{'reference':pins[k]} for k in ('postgres-image','redis-image','minio-image')}
  for k,im in images.items():
   from integration.services import docker_environment
   actual=subprocess.check_output([str(paths['docker']),'image','inspect',im['reference'],'--format','{{json .RepoDigests}}|{{.Os}}|{{.Architecture}}'],env=docker_environment(),text=True).strip().split('|')
   assert im['reference'] in json.loads(actual[0]) and actual[1:]==['linux','arm64']
  tools=Toolchain(paths,versions,{k:file_digest(v) for k,v in paths.items()},images,'darwin','arm64');snapshot=SourceSnapshot(commit,repo,repo,root/'unused','')
  print('preparing owned PG/Redis/MinIO/scanner',flush=True)
  bundle=prepare_services(snapshot,tools,registry,private/'services',time.monotonic()+120)
  bundle=prepare_iam(bundle,tools,private/'iam',time.monotonic()+120)
  bundle.metadata['definition_source']=str(args.definition_source)
  bundle=prepare_scanner(bundle,tools,registry,private/'scanner',time.monotonic()+240)
  scan=probe_scanner(bundle,tools,time.monotonic()+120)
  if scan.exit_code:raise ValueError('actual_scanner_probe_failed')
  env={k:os.environ[k] for k in ('HOME','LANG','TZ') if k in os.environ};env.update(bundle.environment)
  env.update(PATH='/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin',GOWORK='off',GOFLAGS='-mod=readonly',CI='true',FLUTTER_SUPPRESS_ANALYTICS='true',PUB_CACHE=str(args.pub_cache),TMPDIR=str(private),IM_TEST_NATIVE_DART=str(args.flutter_sdk/'bin/cache/dart-sdk/bin/dart'),IM_TEST_NATIVE_FLUTTER_SNAPSHOT=str(args.flutter_sdk/'bin/cache/flutter_tools.snapshot'),IM_TEST_NATIVE_TESTER=str(args.flutter_sdk/'bin/cache/artifacts/engine/darwin-x64/flutter_tester'),IM_TEST_NATIVE_PUB_CACHE=str(args.pub_cache),IM_TEST_INTEGRATION_REGISTRY=str(registry.path),IM_TEST_INTEGRATION_OWNER=owner,IM_TEST_INTEGRATION_SOURCE_SHA=commit,IM_TEST_INTEGRATION_GATE='native_files',IM_TEST_INTEGRATION_PRIVATE_ROOT=str(private))
  env['IM_TEST_NATIVE_TARGET']=args.target
  if args.target=='macos':
   env['IM_TEST_NATIVE_ISOLATED_PROFILE']='true'
   for k in ('FLUTTER_XCODE_DEVELOPMENT_TEAM','FLUTTER_XCODE_CODE_SIGN_IDENTITY'):env[k]=os.environ[k]
  env['IM_FILE_S3_ACCESS_KEY']=env['IM_TEST_FILE_UPLOAD_ACCESS_KEY'];env['IM_FILE_S3_SECRET_KEY']=env['IM_TEST_FILE_UPLOAD_SECRET_KEY']
  command=[str(paths['go']),'test','-tags=nativeclientintegration','-count=1','-v','-timeout=15m','./internal/policystore','-run','^TestNativeFilesAndDevicesAgainstProductionAPI$']
  result['command']=command;result['tools_sha256']=tools.hashes;result['scanner_definitions']=bundle.metadata['scanner_definition_info'];result['fixture_checks']=scan.checks
  print('running real native files/devices gate',flush=True)
  actual_exit,content,timed_out=run_backend(command,repo,env,930)
  for secret in sorted(bundle.secrets,key=len,reverse=True):content=content.replace(secret,'[REDACTED]')
  (out/(label+'.log')).write_text(content);result.update(actual_exit=actual_exit,timed_out=timed_out,log_sha256=hashlib.sha256(content.encode()).hexdigest(),skipped='--- SKIP:' in content)
  result['os_vault_cleanup_verified']=args.target=='macos' and 'NATIVE_FILES_VAULT_CLEANUP os_vault_real=true own_records_absent=true native_private_storage_removed=true' in content
  result['owned_vault_recovery_handles']=re.findall(r'NATIVE_FILES_VAULT_NAMESPACE ([a-f0-9]{64})',content)
  result['owned_storage_recovery_handles']=re.findall(r'NATIVE_BACKEND_STORAGE_OWNER ([A-Za-z0-9_-]+)',content)
  print(content[-6500:],flush=True);code=1 if timed_out else actual_exit
  if result['skipped']:code=1
  rows=registry.records();children=[r for r in rows if r['event']=='registered' and r.get('detail',{}).get('test')=='TestNativeFilesAndDevicesAgainstProductionAPI']
  result['children_registered']=len(children)
  verify_child_exits(rows,children)
  expected=5 if args.target=='macos' else 4
  if len(children)!=expected:raise ValueError('unexpected_root_child_count')
  result['native_runtime_lifecycle']={'registered_and_ready':sum(r['event']=='ready' and r.get('detail',{}).get('test')=='TestNativeFilesAndDevicesAgainstProductionAPI/flutter_runtime' for r in rows),'absent_after_parent_wait':'runtime_absent_after_parent_wait=true' in content,'os_exit_independently_observed':False}
  if result['native_runtime_lifecycle']['registered_and_ready']!=1 or not result['native_runtime_lifecycle']['absent_after_parent_wait']:raise ValueError('native_runtime_identity_or_absence_missing')
  result['root_child_lifecycle']={'ready':expected,'matching_actual_exits':expected}
 except BaseException as exc:
  result['error']=type(exc).__name__+':'+str(exc);print(result['error'],flush=True);code=1
 finally:
  cleanup=registry.cleanup(time.monotonic()+45);result['cleanup']={'removed':cleanup.removed,'failures':cleanup.failures}
  if bundle:
   for runtime in bundle.metadata.get('scanner_runtimes',[]):
    try:runtime['process'].wait(timeout=3)
    except Exception:result['cleanup']['failures'].append('scanner_not_reaped');code=1
  (out/(label+'.registry.jsonl')).write_bytes(registry.path.read_bytes())
  if cleanup.removed and not result['cleanup']['failures']:
   shutil.rmtree(root);result['private_root_removed']=not root.exists()
  else:
   code=1;result['cleanup']['removed']=False;result['private_root_removed']=False;result['recovery_root']=str(root)
  if 'source_sha256' in result:
   result['source_unchanged']=result['source_sha256']==source_hashes()
   if not result['source_unchanged']:code=1
  result['registry_sha256']=hashlib.sha256((out/(label+'.registry.jsonl')).read_bytes()).hexdigest()
  result['runner_exit']=code;(out/(label+'.json')).write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n')
  print(json.dumps({k:result.get(k) for k in ('label','actual_exit','runner_exit','cleanup','private_root_removed')}),flush=True)
 sys.exit(code)

if __name__=='__main__':main()
