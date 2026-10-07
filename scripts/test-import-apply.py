#!/usr/bin/env python3
"""Verify controlled append from immutable source with owned PostgreSQL fixtures."""
import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import tarfile
import tempfile
import time
import uuid
from pathlib import Path

REQUIRED_GATES = ['unit', 'race', 'migration', 'authorization', 'append_database',
 'http_oidc', 'concurrency', 'commit_fault', 'resources', 'readonly_regression',
 'groupdb_regression', 'access_regression', 'ack_regression', 'oidc_regression']
REQUIRED_TESTS = {
 'unit':['TestAppendDecisionParity','TestAppendProtectedEntities','TestAppendPlanDeepTree','TestAppendReceiptStrictDecode','TestAppendHTTPStatusContract','TestAppendHTTPAuthBeforeRead','TestAppendHTTPNoSecrets','TestAppendHTTPIngressLimits','TestAppendRuntimeDefaultOff'],
 'race':['TestAppendPGConcurrentSameBatch','TestAppendPGCommitOutcome','TestAppendHTTPRealOIDCToReceipt'],
 'migration':['TestAppendPGMigration/base1','TestAppendPGMigration/base21'],
 'authorization':['TestAppendPGAuthorization','TestAppendPGAuthorizationExpiry','TestAppendPGAuthNoCommit'],
 'append_database':['TestAppendPGApplyAtomic','TestAppendPGReplayBinding','TestAppendPGSavepointReject','TestAppendPGAuditFailure','TestAppendPGLockCleanup','TestAppendPGWriterProfile','TestAppendPGSnapshotLocks','TestAppendPGStoredLimits','TestAppendRuntimeMissingMigration'],
 'http_oidc':['TestAppendHTTPRealOIDCToReceipt'],
 'concurrency':['TestAppendPGConcurrentSameBatch','TestAppendPGSnapshotAfterSessionLock','TestAppendPGConcurrentMutation','TestAppendPGConcurrentInsertRace','TestAppendPGRetryErrors/serialization','TestAppendPGRetryErrors/deadlock','TestAppendPGRetryErrors/lockTimeout','TestAppendPGAuthorityExpiresDuringWrite','TestAppendPGInventoryMutationBlocked'],
 'commit_fault':['TestAppendPGCommitOutcome/'+x for x in ['beforeCommit','afterCommit','cancelBeforeWrite','processRestart']],
 'resources':['TestAppendHTTPResourceEdges/'+x for x in ['body10m','body10mplus1','rows10000','rows10001','stored20000','stored20001','stored64m','stored64mplus1','string4096','string4097','report200','report201','deepTree','slowBodyExpiry','disconnectAfterCommit']],
 'readonly_regression':['TestComparePGSnapshotReadOnly','TestComparePGIsolation','TestComparePGProfile','TestComparePGColumnWritePermission','TestComparePGStructuralProfile','TestComparePGResourceEdges','TestComparePGUnsupportedTimes','TestComparePGGlobalOccupancy','TestComparePGDDLAndRevoke','TestComparePGQueryBudget','TestCompareProcessEnvIsolation','TestCompareProcessActualSIGTERM','TestComparePGTLS/verify_full','TestCompareProcessTLS/verify_full_home_traps','TestCompareProcessResourceEdges/bytes_plus_one'],
 'groupdb_regression':['TestAllowsParallelOrganizationsAndAdjacentIntervals','TestMigrationCanRollBackAndReapply'],
 'access_regression':['TestAppendPGAuthorization'],
 'ack_regression':['TestMessageACKUsesPersistedTime'],
 'oidc_regression':['TestSignedTokenThroughHTTPToAuditedAdminAndOrdinaryDirectory','TestAppendHTTPRealOIDCToReceipt'],
}
REQUIRED_COMMANDS=['build_all','vet_all','provenance','fixture_cleanup','gate_tests']
BASE='7f5868aa11a3d4d8b758f0e332813502efe7eb6e'
ACK='^TestMessageACKUsesPersistedTime$|^TestSendTextMessage|^TestSendGroupTextMessage|^TestFileMessageDirect|^TestFileMessageGroup|^TestTextSend|^TestTextReplay|^TestFileMessageHistory|^TestFileMessageIdempotency|^TestValidateClientMessageIDWindowAndVersion$|^TestMessageContentBounds$|^TestFileMessageDigestCanonical$'

def validate_required_gates(events):
 groups={};duplicate=False
 for e in events:
  duplicate |= e.get('name') in groups;groups[e.get('name')]=e
 def green(e):
  c=e.get('counts',{})
  return e.get('exit_code')==0 and c.get('fail')==0 and c.get('skip')==0 and type(c.get('top_pass')) is int and c['top_pass']>0
 failed=[name for name in REQUIRED_GATES if name not in groups or not green(groups[name]) or not set(REQUIRED_TESTS[name]).issubset(groups[name].get('executed_tests',[]))]
 failed.extend(name for name in REQUIRED_COMMANDS if groups.get(name,{}).get('exit_code')!=0)
 if duplicate:failed.append('duplicate_gate')
 if not groups.get('cleanup',{}).get('removed'):failed.append('cleanup')
 full=groups.get('full_suite',{})
 return dict(required_gates_passed=not failed,full_suite_passed=green(full),failed_gates=failed,failed_tests=full.get('failed_tests',[]),skipped_tests=full.get('skipped_tests',[]))

def sha(p):return hashlib.sha256(p.read_bytes()).hexdigest()
def checked(args,cwd=None,env=None,input=None,timeout=120):
 return subprocess.check_output(args,cwd=cwd,env=env,input=input,stderr=subprocess.STDOUT,timeout=timeout).decode().strip()
def main():
 parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--source-commit',required=True);parser.add_argument('--output-dir',required=True,type=Path);parser.add_argument('--reuse-full-suite',type=Path)
 args=parser.parse_args();repo=Path(checked(['git','rev-parse','--show-toplevel']))
 if not re.fullmatch('[0-9a-f]{40}',args.source_commit):parser.error('source must be an immutable full commit SHA')
 commit=checked(['git','rev-parse','--verify',args.source_commit+'^{commit}'],repo)
 out=args.output_dir.resolve();out.mkdir(parents=True,exist_ok=False);source=out/'source';source.mkdir();bins=out/'bin';bins.mkdir();archive=out/'source.tar'
 with archive.open('wb') as f:subprocess.run(['git','archive','--format=tar',commit],cwd=repo,stdout=f,check=True)
 with tarfile.open(archive) as tar:
  for m in tar.getmembers():
   if not(m.isfile() or m.isdir()) or Path(m.name).is_absolute() or '..' in Path(m.name).parts:raise RuntimeError('unsafe source archive')
  tar.extractall(source)
 owner=uuid.uuid4().hex;events=[];containers=[];secrets=[];private=Path(tempfile.mkdtemp(prefix='im-append-private-'));private.chmod(0o700)
 result=dict(source_commit=commit,source_archive_sha256=sha(archive),customer_acceptance='not_executed',owner=owner,events=events,full_suite_source_commit=commit,cleanup=dict(removed=False))
 def gate(name,commands,env=None,parse='json',cwd=source):
  if commands and isinstance(commands[0],str):commands=[commands]
  log=out/(name+'.log');start=time.monotonic();code=0
  with log.open('wb') as f:
   for command in commands:
    try:r=subprocess.run(command,cwd=cwd,env=env,stdout=f,stderr=subprocess.STDOUT,timeout=1500);code=r.returncode
    except subprocess.TimeoutExpired:code=124
    if code:break
  text=log.read_text(errors='replace')
  for secret in sorted(secrets,key=len,reverse=True):text=text.replace(secret,'<private fixture value>')
  log.write_text(text);counts=dict(top_pass=0,sub_pass=0,fail=0,skip=0);passed=[];failed=[];skipped=[];outcomes=[]
  if parse=='json':
   for line in text.splitlines():
    try:r=json.loads(line)
    except json.JSONDecodeError:continue
    action,test=r.get('Action'),r.get('Test')
    if action in ['pass','fail','skip'] and (test or action=='fail'):outcomes.append((action,test or '<package>',r.get('Package','')))
  elif parse=='verbose':outcomes=[(a.lower(),t,'') for a,t in re.findall(r'^\s*--- (PASS|FAIL|SKIP): (\S+)',text,re.M)]
  for action,test,package in outcomes:
   if action=='pass':counts['sub_pass' if '/' in test else 'top_pass']+=1;passed.append(test)
   else:counts[action]+=1;(failed if action=='fail' else skipped).append(package+'::'+test)
  event=dict(name=name,exit_code=code,counts=counts,executed_tests=passed,failed_tests=failed,skipped_tests=skipped,seconds=round(time.monotonic()-start,3),log_sha256=sha(log),log=log.name,commands=commands);events.append(event)
  print(name,code,counts,flush=True)
  if name!='full_suite' and(code or name in REQUIRED_GATES and(counts['fail'] or counts['skip'] or not counts['top_pass'] or not set(REQUIRED_TESTS[name]).issubset(passed))):raise RuntimeError('gate failed: '+name)
 def start_container(name,extra):
  cid=checked(['docker','run','-d','--rm','--name',name,'--label','im.append.owner='+owner,'--tmpfs','/var/lib/postgresql/data:rw']+extra+[image['Id']]);containers.append(cid)
  for _ in range(100):
   cp=subprocess.run(['docker','exec',cid,'pg_isready','-h','127.0.0.1','-U','postgres','-d','im_append'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5)
   if cp.returncode==0:return cid
   time.sleep(.1)
  raise RuntimeError('fixture TCP not ready')
 def psql(cid,sql):return checked(['docker','exec','-i',cid,'psql','-v','ON_ERROR_STOP=1','-U','postgres','-d','im_append','-At'],input=sql.encode())
 inventory="select 'schema:'||nspname from pg_namespace union all select 'role:'||rolname from pg_roles order by 1;"
 try:
  env={k:v for k,v in os.environ.items() if not k.startswith('IM_')};env.update(GOFLAGS='-mod=readonly',GOWORK='off')
  node=Path.home()/'.cache/codex-runtimes/codex-primary-runtime/dependencies/node/bin'
  if (node/'node').is_file():env['PATH']=str(node)+os.pathsep+env.get('PATH','')
  result['go_version']=checked(['go','version'],env=env);image=json.loads(checked(['docker','image','inspect','postgres:16-alpine']))[0];result['image_id']=image['Id']
  if image['Architecture']!='arm64':raise RuntimeError('requires cached arm64 PG16 image')
  gate('gate_tests',['python3','scripts/test_import_apply_gates.py'],env,parse='none')
  gate('build_all',['go','build','./...'],env,parse='none');gate('vet_all',['go','vet','./...'],env,parse='none')
  password=uuid.uuid4().hex;writer_password=uuid.uuid4().hex;role='im_import_writer_'+owner[:12];secrets.extend([password,writer_password])
  envfile=private/'postgres.env';envfile.write_text('POSTGRES_DB=im_append\nPOSTGRES_PASSWORD='+password+'\n');envfile.chmod(0o600)
  writer=start_container('im-append-'+owner[:12],['-p','127.0.0.1::5432','--env-file',str(envfile)])
  meta=json.loads(checked(['docker','inspect',writer]))[0];binding=meta['NetworkSettings']['Ports']['5432/tcp'];
  if len(binding)!=1 or binding[0]['HostIp']!='127.0.0.1':raise RuntimeError('writer fixture exposure changed')
  port=binding[0]['HostPort'];psql(writer,"CREATE ROLE "+role+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+writer_password+"';")
  baseline=psql(writer,inventory);result['postgres_version']=psql(writer,'select version();');result['writer_role']=role
  admin_url='postgres://postgres:'+password+'@127.0.0.1:'+port+'/im_append?sslmode=disable';writer_url='postgres://'+role+':'+writer_password+'@127.0.0.1:'+port+'/im_append?sslmode=disable';secrets.extend([admin_url,writer_url])
  db_env=dict(env,IM_TEST_DATABASE_URL=admin_url,IM_IMPORT_APPLY_TEST_ADMIN_URL=admin_url,IM_IMPORT_APPLY_TEST_DATABASE_URL=writer_url)
  def host(name,packages,pattern=None,extra=None):
   cmd=['go','test','-json','-p','1','-timeout=10m','-count=1']+(extra or [])
   if pattern:cmd+=['-run',pattern]
   return gate(name,cmd+packages,db_env)
  offline=['./internal/importpreflight','./internal/importinput','./internal/importcompare','./cmd/im-import-preflight','./cmd/im-import-compare']
  skip='^TestComparePG|^TestPreflightPostgresOracle$|^TestCompareProcessEnvIsolation$|^TestCompareProcessActualSIGTERM$|^TestCompareProcessTLS$|^TestCompareProcessResourceEdges$'
  new_unit='^TestAppend(Decision|Protected|Plan|Receipt|RawBinding|BatchLock|HTTPStatus|HTTPAuth|HTTPNo|HTTPIngress|RuntimeDefault)'
  gate('unit',[
   ['go','test','-json','-p','1','-timeout=5m','-count=1','-skip',skip]+offline,
   ['go','test','-json','-p','1','-count=1','-run',new_unit,'./internal/importapply','./internal/httpserver','./cmd/im-api']],db_env)
  host('race',['./internal/importapply','./internal/httpserver','./internal/oidcauth'],'^TestAppend(Budget|PGConcurrentSameBatch|PGCommitOutcome|HTTPAuthBeforeRead|HTTPIngressLimits|HTTPRealOIDC)', ['-race'])
  host('migration',['./internal/importapply'],'^TestAppendPGMigration$')
  host('authorization',['./internal/access'],'^TestAppendPG(Auth|Authorization)')
  gate('append_database',[
   ['go','test','-json','-count=1','./internal/importapply','-run','^TestAppend(PG(Apply|Replay|Savepoint|Audit|Lock|Snapshot|Writer|Stored)|Budget)'],
   ['go','test','-json','-count=1','./cmd/im-api','-run','^TestAppendRuntime']],db_env)
  host('http_oidc',['./internal/oidcauth'],'^TestAppendHTTPRealOIDCToReceipt$')
  host('concurrency',['./internal/importapply'],'^TestAppendPG(Concurrent|SnapshotAfter|Retry|Authority|Inventory)')
  host('commit_fault',['./internal/importapply'],'^TestAppendPGCommitOutcome$')
  host('resources',['./internal/oidcauth'],'^TestAppendHTTPResourceEdges$')
  for name,pkg in [('groupdb_regression','groupdb'),('access_regression','access'),('oidc_regression','oidcauth')]:host(name,['./internal/'+pkg])
  host('ack_regression',['./internal/policystore'],ACK)
  if psql(writer,inventory)!=baseline:raise RuntimeError('writer fixture schema or role residue')
  # Independent readonly fixture retains P4-29 migrations1..21 and strict role checks.
  linux=dict(env,GOOS='linux',GOARCH='arm64',CGO_ENABLED='0')
  for name,path in [('importcompare','internal/importcompare'),('compare_cli','cmd/im-import-compare')]:gate(name+'_build',['go','test','-c','-o',str(bins/(name+'.test')),'./'+path],linux,parse='none')
  for name in ['im-import-compare','im-import-preflight']:gate(name+'_build',['go','build','-buildvcs=false','-o',str(bins/(name+'-linux-arm64')),'./cmd/'+name],linux,parse='none')
  tls=private/'tls';tls.mkdir()
  for prefix in ['ca','wrong-ca']:
   checked(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-days','2','-subj','/CN=IM append fixture '+prefix,'-keyout',str(tls/(prefix+'.key')),'-out',str(tls/(prefix+'.crt'))])
  checked(['openssl','req','-newkey','rsa:2048','-nodes','-subj','/CN=127.0.0.1','-keyout',str(tls/'server.key'),'-out',str(tls/'server.csr')]);(tls/'server.ext').write_text('subjectAltName=IP:127.0.0.1\nextendedKeyUsage=serverAuth\n')
  checked(['openssl','x509','-req','-in',str(tls/'server.csr'),'-CA',str(tls/'ca.crt'),'-CAkey',str(tls/'ca.key'),'-CAcreateserial','-days','2','-extfile',str(tls/'server.ext'),'-out',str(tls/'server.crt')])
  for path in tls.iterdir():path.chmod(0o600)
  readonly=start_container('im-append-readonly-'+owner[:12],['--network','none','-e','POSTGRES_HOST_AUTH_METHOD=trust','-e','POSTGRES_DB=im_append','--mount','type=bind,src='+str(source)+',dst=/source,readonly','--mount','type=bind,src='+str(bins)+',dst=/bins,readonly','--mount','type=bind,src='+str(tls)+',dst=/tls,readonly'])
  before_readonly=psql(readonly,inventory)
  checked(['docker','exec',readonly,'sh','-c','cp /tls/server.key /tls/server.crt /var/lib/postgresql/data/; chown postgres:postgres /var/lib/postgresql/data/server.*; chmod 600 /var/lib/postgresql/data/server.key'])
  for key,value in [('ssl','on'),('ssl_cert_file','/var/lib/postgresql/data/server.crt'),('ssl_key_file','/var/lib/postgresql/data/server.key')]:psql(readonly,"ALTER SYSTEM SET "+key+" = '"+value+"';")
  psql(readonly,'select pg_reload_conf();')
  for _ in range(100):
   if psql(readonly,'show ssl;')=='on':break
   time.sleep(.1)
  else:raise RuntimeError('TLS fixture reload failed')
  readonly_env=['PATH=/usr/local/bin:/usr/bin:/bin','TZ=UTC','IM_TEST_DATABASE_URL=host=/var/run/postgresql port=5432 user=postgres dbname=im_append sslmode=disable','IM_COMPARE_TEST_BINARY=/bins/im-import-compare-linux-arm64','IM_PREFLIGHT_TEST_BINARY=/bins/im-import-preflight-linux-arm64','IM_COMPARE_TEST_CA=/tls/ca.crt','IM_COMPARE_TEST_WRONG_CA=/tls/wrong-ca.crt']
  commands=[]
  for name,path in [('importcompare','internal/importcompare'),('compare_cli','cmd/im-import-compare')]:commands.append(['docker','exec','-w','/source/'+path,readonly,'env','-i']+readonly_env+['/bins/'+name+'.test','-test.v','-test.timeout=10m','-test.count=1'])
  gate('readonly_regression',commands,parse='verbose')
  if psql(readonly,inventory)!=before_readonly:raise RuntimeError('readonly fixture schema or role residue')
  events.append(dict(name='fixture_cleanup',exit_code=0,schema_role_residue=0))
  for locked in ['go.mod','go.sum']:
   previous=subprocess.check_output(['git','show',BASE+':'+locked],cwd=repo)
   if sha(source/locked)!=hashlib.sha256(previous).hexdigest():raise RuntimeError('dependency lock changed')
  migrations=checked(['git','diff','--name-only',BASE,commit,'--','db/migrations'],repo).splitlines()
  if set(migrations)!={'db/migrations/000022_import_batches.up.sql','db/migrations/000022_import_batches.down.sql'}:raise RuntimeError('unexpected production migration changes')
  events.append(dict(name='provenance',exit_code=0,migration_changes=migrations,locks_unchanged=True))
  result['bin_sha256']={p.name:sha(p) for p in bins.iterdir()}
  if args.reuse_full_suite:
   prior=json.loads(args.reuse_full_suite.read_text());full=next(e for e in prior['events'] if e['name']=='full_suite');full=dict(full);old_log=args.reuse_full_suite.parent/full['log'];shutil.copyfile(old_log,out/'full_suite.log');full['log']='full_suite.log';full['log_sha256']=sha(out/'full_suite.log');events.append(full);result['full_suite_source_commit']=prior['full_suite_source_commit'];result['full_suite_reused']=True
  else:host('full_suite',['./...'])
 except Exception as e:
  result['failure']=str(e)
  for secret in sorted(secrets,key=len,reverse=True):result['failure']=result['failure'].replace(secret,'<private fixture value>')
 finally:
  removed=True
  for cid in containers:
   cp=subprocess.run(['docker','inspect',cid],capture_output=True,text=True,timeout=10)
   if cp.returncode==0:
    meta=json.loads(cp.stdout)[0]
    if meta['Config']['Labels'].get('im.append.owner')!=owner:removed=False;continue
    subprocess.run(['docker','stop','-t','3',cid],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=15)
    for _ in range(100):
     if subprocess.run(['docker','inspect',cid],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5).returncode!=0:break
     time.sleep(.1)
    else:removed=False
  shutil.rmtree(private);result['cleanup']['removed']=removed;events.append(dict(name='cleanup',removed=removed));result.update(validate_required_gates(events))
  if result['full_suite_source_commit']!=commit:result['full_suite_passed']=False
  if 'failure' in result:result['required_gates_passed']=False
  (out/'verification.json').write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n');print('required_gates_passed',result['required_gates_passed'],'full_suite_passed',result['full_suite_passed'],'cleanup',removed,flush=True)
 return 0 if result['required_gates_passed'] else 1
if __name__=='__main__':raise SystemExit(main())
