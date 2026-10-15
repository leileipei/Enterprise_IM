#!/usr/bin/env python3
# Never creates signing material or accepts Apple terms. Dedicated empty OS profile required for native mode.
import argparse,hashlib,re,json,os,platform,secrets,subprocess,sys,tempfile,time,shutil
from pathlib import Path
repo=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(repo/'scripts'))
from integration.registry import Registry
from integration.model import ResourceRef
from integration.tools import _lock
from integration.services import docker_environment
from integration.native_client import signing_choice_available,client_preflight


def main():
 parser=argparse.ArgumentParser(description='Owned production-backend messaging gate; host mode is not native acceptance.')
 parser.add_argument('--target', choices=('macos','host'), default='macos')
 parser.add_argument('--flutter-sdk',type=Path,required=True)
 parser.add_argument('--pub-cache',type=Path,required=True)
 parser.add_argument('--go',type=Path,required=True)
 parser.add_argument('--output',type=Path,required=True)
 parser.add_argument('--isolated-test-account',action='store_true')
 args=parser.parse_args()
 for path in (args.flutter_sdk,args.pub_cache,args.go,args.output):
  if not path.is_absolute():parser.error('all paths must be absolute')
 args.output.mkdir(mode=0o700,parents=False,exist_ok=False)
 label='result'
 argv=[str(args.go),'test','-tags=nativeclientintegration','-count=1','-v','-timeout=15m','./internal/policystore','-run','^TestNativeMessagingUIAgainstProductionAPI$']
 preflight=client_preflight(args)
 (args.output/'preflight.json').write_text(json.dumps(preflight,indent=2)+'\n')
 if preflight['status']!='ready':
  print(json.dumps(preflight));sys.exit(2)
 commit=subprocess.check_output(['git','rev-parse','HEAD'],cwd=repo,text=True).strip()
 owner=secrets.token_hex(16); root=Path(tempfile.mkdtemp(prefix='im-p502-protocol-',dir='/private/tmp')).resolve(); root.chmod(0o700)
 registry=Registry(root/'resources.jsonl',owner,commit); private=root/owner
 secret=secrets.token_hex(24); envfile=private/'postgres.env'
 envfile.write_text('POSTGRES_DB=enterprise_im_p5\nPOSTGRES_USER=postgres\nPOSTGRES_PASSWORD='+secret+'\n'); envfile.chmod(0o600)
 env=docker_environment(); result={'command':argv,'source_commit':commit,'owner':owner,'fixture':'isolated local PostgreSQL 16','label':label}; code=1
 out=args.output
 log=out/(label+'.log')
 redis_secret=secrets.token_hex(24)
 redisconf=private/'redis.conf'
 redisconf.write_text('bind 0.0.0.0\nport 6379\nrequirepass '+redis_secret+'\nappendonly no\nsave ""\n');redisconf.chmod(0o600)
 result['fixture']='isolated local PostgreSQL 16 and Redis 7'
 result['target']=args.target
 result['runner_sha256']=hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
 def docker(args,input=None):
  r=subprocess.run(['/usr/local/bin/docker',*args],input=input,capture_output=True,text=True,env=env,timeout=30)
  if r.returncode: raise RuntimeError('docker_'+args[0]+'_failed')
  return r.stdout.strip()
 def sources():
  files=subprocess.check_output(['git','ls-files','-z','--','clients/enterprise_im'],cwd=repo).split(b'\0')+subprocess.check_output(['git','ls-files','--others','--exclude-standard','-z','--','clients/enterprise_im'],cwd=repo).split(b'\0')
  paths={os.fsdecode(f) for f in files if f};paths.update(['internal/policystore/native_messaging_probe_test.go','internal/policystore/native_files_probe_test.go','internal/policystore/native_backend_launch_helpers_test.go','internal/policystore/native_backend_launch_test.go','internal/policystore/native_receipt_probe_test.go','internal/testfixtures/integration_browser.go','internal/testfixtures/integration_registry.go','scripts/test-client-macos-backend.py','scripts/tests/test_client_macos_backend.py','scripts/integration/native_client.py'])
  return {p:hashlib.sha256((repo/p).read_bytes()).hexdigest() for p in sorted(paths)}
 try:
  result['source_sha256']=sources()
  cid=docker(['create','--label','im.integration.owner='+owner,'--label','im.integration.source='+commit,'--publish','127.0.0.1::5432','--env-file',str(envfile),'--tmpfs','/var/lib/postgresql/data:rw,mode=0700',_lock(repo/'testdata/file-runtime/versions.lock')['postgres-image']])
  registry.add(ResourceRef('container',owner,cid,{'container_id':cid})); docker(['start',cid])
  deadline=time.monotonic()+45
  while True:
   r=subprocess.run(['/usr/local/bin/docker','exec',cid,'pg_isready','-h','127.0.0.1','-U','postgres','-d','enterprise_im_p5'],env=env,capture_output=True,timeout=5)
   if r.returncode==0:break
   if time.monotonic()>deadline:raise RuntimeError('postgres_ready_timeout')
   time.sleep(0.2)
  docker(['exec','-i',cid,'psql','-X','-v','ON_ERROR_STOP=1','-U','postgres','-d','enterprise_im_p5'],input='CREATE EXTENSION btree_gist WITH SCHEMA public;')
  binding=json.loads(docker(['inspect',cid]))[0]['NetworkSettings']['Ports']['5432/tcp']
  if len(binding)!=1 or binding[0]['HostIp']!='127.0.0.1':raise RuntimeError('nonloopback_fixture')
  rcid=docker(['create','--label','im.integration.owner='+owner,'--label','im.integration.source='+commit,'--publish','127.0.0.1::6379','--user','0','--mount','type=bind,source='+str(redisconf)+',target=/run/im-redis.conf,readonly',_lock(repo/'testdata/project-integration/versions.lock')['redis-image'],'redis-server','/run/im-redis.conf'])
  registry.add(ResourceRef('container',owner,rcid,{'container_id':rcid}));docker(['start',rcid])
  deadline=time.monotonic()+30
  while True:
   rr=subprocess.run(['/usr/local/bin/docker','exec',rcid,'redis-cli','ping'],env=env,capture_output=True,text=True,timeout=5)
   if rr.returncode==0 and rr.stdout.startswith('NOAUTH'):break
   if time.monotonic()>deadline:raise RuntimeError('redis_ready_timeout')
   time.sleep(0.2)
  rbinding=json.loads(docker(['inspect',rcid]))[0]['NetworkSettings']['Ports']['6379/tcp']
  if len(rbinding)!=1 or rbinding[0]['HostIp']!='127.0.0.1':raise RuntimeError('nonloopback_redis_fixture')
  testenv={k:os.environ[k] for k in ('HOME','LANG','TZ') if k in os.environ}; testenv.update(IM_TEST_DATABASE_URL='postgresql://postgres:'+secret+'@127.0.0.1:'+binding[0]['HostPort']+'/enterprise_im_p5?sslmode=disable',GOWORK='off',GOFLAGS='-mod=readonly')
  testenv['IM_TEST_REDIS_URL']='redis://:'+redis_secret+'@127.0.0.1:'+rbinding[0]['HostPort']+'/0'
  testenv.update({k:env[k] for k in ('DOCKER_HOST','DOCKER_CONTEXT') if k in env})
  testenv.update(IM_TEST_INTEGRATION_REGISTRY=str(registry.path),IM_TEST_INTEGRATION_OWNER=owner,IM_TEST_INTEGRATION_SOURCE_SHA=commit,IM_TEST_INTEGRATION_GATE='client_messaging',IM_TEST_INTEGRATION_PRIVATE_ROOT=str(private),IM_TEST_RECEIPT_ARTIFACT_PREFIX=str(out/label))
  testenv['TMPDIR']=str(private)
  testenv['IM_TEST_NATIVE_DART']=str(args.flutter_sdk/'bin/cache/dart-sdk/bin/dart')
  testenv['IM_TEST_NATIVE_FLUTTER_SNAPSHOT']=str(args.flutter_sdk/'bin/cache/flutter_tools.snapshot')
  testenv['IM_TEST_NATIVE_TESTER']=str(args.flutter_sdk/'bin/cache/artifacts/engine/darwin-x64/flutter_tester')
  testenv['IM_TEST_NATIVE_PUB_CACHE']=str(args.pub_cache)
  testenv['IM_TEST_NATIVE_TARGET']=args.target
  if args.target=='macos':
   testenv['IM_TEST_NATIVE_ISOLATED_PROFILE']='true'
   for k in ('FLUTTER_XCODE_DEVELOPMENT_TEAM','FLUTTER_XCODE_CODE_SIGN_IDENTITY'):testenv[k]=os.environ[k]
  testenv['PATH']='/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin'
  patch=subprocess.check_output(['git','diff','HEAD'],cwd=repo)
  result['tracked_diff_sha256']=hashlib.sha256(patch).hexdigest()
  files=subprocess.check_output(['git','ls-files','--others','--exclude-standard','-z'],cwd=repo).split(b'\0')
  result['untracked_sha256']={os.fsdecode(f):hashlib.sha256((repo/os.fsdecode(f)).read_bytes()).hexdigest() for f in files if f}
  r=subprocess.run(argv,env=testenv,cwd=repo,capture_output=True,text=True,timeout=2850)
  content=(r.stdout+r.stderr).replace(secret,'[REDACTED]').replace(redis_secret,'[REDACTED]'); log.write_text(content)
  result.update(exit_code=r.returncode,log_sha256=hashlib.sha256(content.encode()).hexdigest(),skipped='--- SKIP:' in content)
  result['owned_vault_recovery_handles']=re.findall(r'NATIVE_MESSAGING_VAULT_NAMESPACE ([a-f0-9]{64})',content)
  result['os_vault_cleanup_verified']=args.target=='macos' and 'NATIVE_MESSAGING_VAULT_CLEANUP os_vault_real=true own_records_absent=true' in content
  records=registry.records()
  children=[row for row in records if row.get('event')=='registered' and row.get('kind')=='process' and row.get('detail',{}).get('test')=='TestNativeMessagingUIAgainstProductionAPI']
  expected_children=5 if args.target=='macos' else 4
  if len(children)!=expected_children:raise RuntimeError('unexpected_product_child_count')
  for child in children:
   def same(row):
    return all(row.get(k)==child.get(k) for k in ('source_commit','owner','kind','identity','fingerprint')) and all(row.get('detail',{}).get(k)==child.get('detail',{}).get(k) for k in ('gate','test'))
   ready=[row for row in records if row.get('event')=='ready' and same(row)]
   exits=[row for row in records if row.get('event')=='exited' and same(row)]
   if len(ready)!=1 or len(exits)!=1:raise RuntimeError('child_lifecycle_missing_or_changed')
   detail=exits[0]['detail']
   if not detail.get('expected_exit') or detail.get('actual_exit')!=detail['expected_exit']:raise RuntimeError('child_actual_exit_mismatch')
  result['native_runtime_lifecycle']={'registered_and_ready':sum(r['event']=='ready' and r.get('detail',{}).get('test')=='TestNativeMessagingUIAgainstProductionAPI/flutter_runtime' for r in records),'absent_after_parent_wait':'runtime_absent_after_parent_wait=true' in content,'os_exit_independently_observed':False}
  if result['native_runtime_lifecycle']['registered_and_ready']!=1 or not result['native_runtime_lifecycle']['absent_after_parent_wait']:raise RuntimeError('native_runtime_identity_or_absence_missing')
  result['child_lifecycle']={'registered':len(children),'ready':expected_children,'matching_actual_exits':expected_children}
  code=r.returncode
  if result['skipped'] or 'no tests to run' in content:code=1
  print(content[-5000:])
  result['acceptance_scope']=('compiled development-signed Debug macOS app, real focus/minimize and OS vault against owned production HTTPS API/PG/Redis; not Release install, interactive OAuth, independent native leaf exit, files or production acceptance' if args.target=='macos' else 'real Flutter widgets and shared controllers against production HTTPS API/PG/Redis; host vault/focus fixtures, not native acceptance')
 except Exception as exc:
  result['error']=type(exc).__name__+':'+str(exc).replace(secret,'[REDACTED]').replace(redis_secret,'[REDACTED]'); print(result['error'])
 finally:
  cleanup=registry.cleanup(time.monotonic()+30)
  result['cleanup']={'removed':cleanup.removed,'failures':cleanup.failures}
  result['registry_sha256']=hashlib.sha256(registry.path.read_bytes()).hexdigest()
  (out/(label+'.registry.jsonl')).write_bytes(registry.path.read_bytes())
  if envfile.exists():envfile.unlink()
  for child in list(private.iterdir()):
   if child.is_dir() and not child.is_symlink():shutil.rmtree(child)
   else:child.unlink()
  private.rmdir(); registry.path.unlink(); root.rmdir()
  result['private_root_removed']=not root.exists()
  if not cleanup.removed:code=1
  result['source_unchanged']=result.get('source_sha256')==sources()
  if not result['source_unchanged']:code=1
  result['runner_exit']=code
  (out/(label+'.json')).write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n')
  print(json.dumps({k:result[k] for k in ('label','exit_code','cleanup','private_root_removed') if k in result}))
 sys.exit(code)

if __name__=='__main__':main()
