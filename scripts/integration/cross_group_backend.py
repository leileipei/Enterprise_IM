"""Owned MinIO/IAM and signature-bound native scanner for cross-process files."""
import hashlib,json,os,secrets,shutil,subprocess,time
from pathlib import Path
from types import SimpleNamespace
from .mobile_backend import Backend,digest
from .model import FixtureBundle
from .tools import _lock,verify_digest,snapshot_binary,probe_version
from .iam import prepare_iam
from .scanner import prepare_scanner,probe_scanner
class GroupFilesBackend(Backend):
 fixture_test='TestCrossGroupFilesDeviceFixture'
 expected_product_children=5
 def runtime_environment(self):
  pins=_lock(self.repo/'testdata/file-runtime/versions.lock');pins.update(_lock(self.repo/'testdata/project-integration/versions.lock'))
  paths={key:Path(value) for key,value in {'clamd':'/opt/homebrew/sbin/clamd','sigtool':'/opt/homebrew/bin/sigtool','freshclam':'/opt/homebrew/bin/freshclam','qpdf':'/opt/homebrew/bin/qpdf'}.items()}
  versions={key:probe_version(path,key,pins['clamav' if key in ('clamd','sigtool','freshclam') else key]) for key,path in paths.items()}
  mc=Path('/Users/leo.cui/Documents/Codex/.runtime-tools/enterprise-im-files-20261015/mc-build/mc');verify_digest(mc,pins['mc-test-client-sha256-darwin-arm64'])
  retained=self.out/'tools';retained.mkdir(mode=0o700);paths['mc']=snapshot_binary(mc,retained/'mc');paths['go']=self.args.go
  hashes={str(p):digest(p) for p in paths.values()};tools=SimpleNamespace(paths=paths)
  key='imgroup'+secrets.token_hex(12);secret=secrets.token_hex(24);envfile=self.root/'minio.env';envfile.write_text('MINIO_ROOT_USER='+key+'\nMINIO_ROOT_PASSWORD='+secret+'\n');envfile.chmod(0o600)
  ref=pins['minio-image'];image=json.loads(self.docker(['image','inspect',ref]))[0]
  if ref not in image['RepoDigests'] or image['Architecture']!='arm64':raise ValueError('group_minio_image_mismatch')
  cid,port=self.service('minio',ref,'9000',['--env-file',str(envfile),'--tmpfs','/data:rw,mode=0700'],['server','/data','--console-address',':9001'])
  import urllib.request
  deadline=time.monotonic()+240
  while True:
   try:
    with urllib.request.urlopen('http://127.0.0.1:'+port+'/minio/health/ready',timeout=2) as response:
     if response.status==200:break
   except OSError:pass
   if time.monotonic()>deadline:raise TimeoutError('group_minio_ready_timeout')
   time.sleep(.2)
  bundle=FixtureBundle({'IM_TEST_S3_ENDPOINT':'http://127.0.0.1:'+port},{key,secret},self.registry.path,{self.registry.owner},{'source_commit':self.registry.commit,'minio_admin':(key,secret),'definition_source':'/Users/leo.cui/Documents/Codex/.runtime-tools/enterprise-im-files-20261015/definitions'})
  self.bundle=bundle
  prepare_iam(bundle,tools,self.root/'iam',deadline)
  prepare_scanner(bundle,tools,self.registry,self.root/'scan',deadline,required_validity_seconds=1500)
  event=probe_scanner(bundle,tools,deadline)
  if event.exit_code:raise ValueError('group_scanner_probe_failed')
  env=dict(bundle.environment);env.update(IM_FILE_S3_ACCESS_KEY=env['IM_TEST_FILE_UPLOAD_ACCESS_KEY'],IM_FILE_S3_SECRET_KEY=env['IM_TEST_FILE_UPLOAD_SECRET_KEY'])
  self.state['file_runtime']={'tool_sha256':hashes,'versions':versions,'scanner_probe_exit':event.exit_code,'scanner_bound':True,'definition_info':bundle.metadata['scanner_definition_info'],'minio_image':image['Id'],'iam_roles':bundle.metadata['iam_roles']};self.save()
  if self.args.scenario.startswith('retention-restart'):env['IM_TEST_CROSS_SCENARIO']=self.args.scenario
  return env
 def close(self):
  try:super().close()
  finally:
   folder=self.root/'scan'
   if folder.is_dir():
    retained=self.out/'scanner';retained.mkdir(mode=0o700,exist_ok=True)
    for p in folder.iterdir():
     if p.is_file():shutil.copyfile(p,retained/p.name)
    self.state['scanner_diagnostics_retained']=True
   if getattr(self,'bundle',None):
    cleaned=self.registry.cleanup(time.monotonic()+45)
    self.state['scanner_cleanup_failures']=cleaned.failures
    exits=[]
    for runtime in self.bundle.metadata.get('scanner_runtimes',[]):
     try:exits.append({'pid':runtime['pid'],'actual_exit':runtime['process'].wait(timeout=5)})
     except subprocess.TimeoutExpired:raise ValueError('group_scanner_not_reaped')
    self.state['scanner_exits']=exits
   self.state['file_tools_unchanged']=all(Path(p).is_file() and digest(p)==h for p,h in self.state.get('file_runtime',{}).get('tool_sha256',{}).items());self.save()
