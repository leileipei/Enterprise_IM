"""Opt-in native clients against owned production API/PG/Redis processes.
Synthetic refresh IdP only; never edits global trust or normal application assets.
"""
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import threading
import time
from .commands import run_command
from .model import ResourceRef
from .services import docker_environment
from .tools import _lock

TARGET = 'integration_test/mobile_backend_test.dart'
PHASES = ['sent', 'delivered', 'read', 'revoked', 'terminal', 'done']
REJECTIONS = ['sync', 'delivered', 'read', 'messages']

def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def preflight(go):
    if go is None or not go.is_absolute() or not go.is_file():
        raise ValueError('prepared_go_required_for_real_backend')
    p = subprocess.run([str(go), 'version'], capture_output=True, text=True, timeout=10)
    if p.returncode or p.stdout.strip() != 'go version go1.27.1 darwin/arm64':
        raise ValueError('fixed_go_version_required')
    env = docker_environment()
    p = subprocess.run(['/usr/local/bin/docker', 'version', '--format', '{{.Server.Version}}'], env=env, capture_output=True, timeout=10)
    if p.returncode:raise ValueError('local_docker_unavailable')
    return {'go': str(go), 'go_sha256': digest(go), 'go_version': '1.27.1', 'docker_local': True}


@contextmanager
def bundled_fixture(client, config):
    client = Path(client)
    pub = client / 'pubspec.yaml'
    original = pub.read_bytes()
    text = original.decode()
    assets = client / 'assets'
    if assets.exists() or assets.is_symlink() or len(re.findall(r'^flutter:\n',text,re.M)) != 1 or '  assets:' in text:
        raise ValueError('owned_backend_asset_path_ambiguous')
    assets.mkdir(mode=0o700)
    try:
        file = assets / 'owned_backend.json'
        file.write_text(json.dumps(config)+'\n');file.chmod(0o600)
        pub.write_text(re.sub(r'^flutter:\n',lambda _: 'flutter:\n  assets:\n    - assets/owned_backend.json\n',text,count=1,flags=re.M))
        yield
    finally:
        pub.write_bytes(original)
        if file.exists():file.unlink()
        assets.rmdir()
        if pub.read_bytes()!=original or assets.exists():raise ValueError('fixture_asset_cleanup_failed')


def verify_report(report, facts, code, driver, platform):
    expected = {'status':'passed','platform':platform,'messages':3,'delivered':3,'read':3,'revoked_rejections':REJECTIONS,'terminal_stopped':True,'received_cleared':True,'os_vault_real':True,'own_records_absent':True,'storage_removed':True}
    database = {'runtime_pid':report.get('runtime_pid'),'automatic_request_delta':0,'platform':platform,'phases':PHASES,'messages':3,'device_delivered':3,'device_read':3,'user_read':3,'read_cursor':3,'bob_revoked':1,'device_count':2,'old_device_rejections':REJECTIONS}
    pid=report.get('runtime_pid')
    if any(report.get(k)!=v for k,v in expected.items()) or not isinstance(pid,int) or isinstance(pid,bool) or pid<=0 or any(facts.get(k)!=v for k,v in database.items()) or code!=0 or 'All tests passed.' not in driver or 'Failure Details:' in driver:
        raise ValueError('real_mobile_backend_unverified')


class Backend:
    fixture_test = 'TestMobileBackendFixture'
    expected_product_children = 3

    def runtime_environment(self):
        return {}
    def __init__(self, repo, args, private, out, registry, platform, state, save):
        self.repo=Path(repo);self.args=args;self.out=Path(out);self.registry=registry
        self.root=Path(private)/'mobile-backend';self.root.mkdir(mode=0o700)
        self.platform=platform;self.state=state;self.save=save;self.thread=None;self.event=None;self.error=None
        self.registry.recover_containers=True
        self.sources={};self.config=None
        self.state['go_sha256']=digest(args.go)

    def docker(self, argv, input=None):
        p=subprocess.run(['/usr/local/bin/docker',*argv],input=input,env=docker_environment(),capture_output=True,text=True,timeout=30)
        if p.returncode:raise RuntimeError('owned_backend_docker_' + argv[0] + '_failed')
        return p.stdout.strip()

    def service(self, role, image, port, extra, command=()):
        cid=self.docker(['create','--label','im.integration.owner='+self.registry.owner,'--label','im.integration.source='+self.registry.commit,'--publish','127.0.0.1::'+port,*extra,image,*command])
        if not re.fullmatch('[a-f0-9]{64}',cid):raise ValueError('backend_container_id_invalid')
        self.registry.add(ResourceRef('container',self.registry.owner,cid,{'container_id':cid}))
        self.docker(['start',cid])
        row=json.loads(self.docker(['inspect',cid]))[0]
        binding=row['NetworkSettings']['Ports'][port+'/tcp']
        if len(binding)!=1 or binding[0]['HostIp']!='127.0.0.1':raise ValueError('backend_service_not_loopback')
        self.state.setdefault('services',{})[role]={'id':cid,'port':int(binding[0]['HostPort']),'image_id':row['Image'],'reference':image};self.save()
        return cid,binding[0]['HostPort']

    def start(self):
        self.state.update(scope='production Go API/outbox/receipt workers, owned PostgreSQL/Redis and verified TLS; synthetic refresh IdP, not interactive SSO',platform=self.platform,root=str(self.root))
        password=secrets.token_hex(24)
        envfile=self.root/'postgres.env';envfile.write_text('POSTGRES_DB=enterprise_im_p5\nPOSTGRES_USER=postgres\nPOSTGRES_PASSWORD='+password+'\n');envfile.chmod(0o600)
        pg,port=self.service('postgres',_lock(self.repo/'testdata/file-runtime/versions.lock')['postgres-image'],'5432',['--env-file',str(envfile),'--tmpfs','/var/lib/postgresql/data:rw,mode=0700'])
        # Redis has a one-use private password file, never a public argv or result.
        redis_secret=secrets.token_hex(24)
        conf=self.root/'redis.conf';conf.write_text('bind 0.0.0.0\nport 6379\nrequirepass '+redis_secret+'\nappendonly no\nsave ""\n');conf.chmod(0o600)
        image=_lock(self.repo/'testdata/project-integration/versions.lock')['redis-image']
        redis,rport=self.service('redis',image,'6379',['--user','0','--mount','type=bind,source='+str(conf)+',target=/run/im-redis.conf,readonly'],['redis-server','/run/im-redis.conf'])
        end=time.monotonic()+45
        while True:
            ready=subprocess.run(['/usr/local/bin/docker','exec',pg,'pg_isready','-h','127.0.0.1','-U','postgres','-d','enterprise_im_p5'],env=docker_environment(),capture_output=True,timeout=5)
            rr=subprocess.run(['/usr/local/bin/docker','exec',redis,'redis-cli','ping'],env=docker_environment(),capture_output=True,text=True,timeout=5)
            if ready.returncode==0 and rr.returncode==0 and rr.stdout.startswith('NOAUTH'):break
            if time.monotonic()>end:raise TimeoutError('backend_service_ready')
            time.sleep(.2)
        self.docker(['exec','-i',pg,'psql','-X','-v','ON_ERROR_STOP=1','-U','postgres','-d','enterprise_im_p5'],input='CREATE EXTENSION btree_gist WITH SCHEMA public;')
        source=self.root/'source';source.mkdir(mode=0o700)
        paths=subprocess.check_output(['git','ls-files','-z','--','cmd','internal','db','testdata','go.mod','go.sum'],cwd=self.repo).split(b'\0')+subprocess.check_output(['git','ls-files','--others','--exclude-standard','-z','--','cmd','internal','db','testdata','go.mod','go.sum'],cwd=self.repo).split(b'\0')
        for value in sorted(set(paths)):
            if not value:continue
            rel=Path(os.fsdecode(value));src=self.repo/rel
            if rel.is_absolute() or '..' in rel.parts or src.is_symlink() or not src.is_file():raise ValueError('backend_source_not_regular')
            target=source/rel;target.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
            target.write_bytes(src.read_bytes());target.chmod(0o600)
            self.sources[str(rel)]=digest(target)
        self.state['source_sha256']=self.sources;self.save()
        (self.root/'tmp').mkdir(mode=0o700)
        env={k:os.environ[k] for k in ('HOME','LANG','TZ') if k in os.environ}
        env.update(PATH=str(self.args.go.parent)+':/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin',TMPDIR=str(self.root/'tmp'),GOWORK='off',GOFLAGS='-mod=readonly',GOPROXY='off',GOTOOLCHAIN='local',GOSUMDB='off',IM_TEST_DATABASE_URL='postgresql://postgres:'+password+'@127.0.0.1:'+port+'/enterprise_im_p5?sslmode=disable',IM_TEST_REDIS_URL='redis://:'+redis_secret+'@127.0.0.1:'+rport+'/0',IM_TEST_INTEGRATION_REGISTRY=str(self.registry.path),IM_TEST_INTEGRATION_OWNER=self.registry.owner,IM_TEST_INTEGRATION_SOURCE_SHA=self.registry.commit,IM_TEST_INTEGRATION_GATE='mobile_backend',IM_TEST_INTEGRATION_PRIVATE_ROOT=str(self.root.parent),IM_TEST_MOBILE_PLATFORM=self.platform,IM_TEST_MOBILE_CONFIG=str(self.root/'input.json'),IM_TEST_MOBILE_FACTS=str(self.out/'backend-facts.json'),IM_TEST_MOBILE_STOP=str(self.root/'stop'))
        env.update(self.runtime_environment())
        argv=[str(self.args.go),'test','-tags=nativeclientintegration','-count=1','-v','-timeout=20m','./internal/policystore','-run','^'+self.fixture_test+'$']
        def execute():
            try:
                self.event=run_command('real-backend','mobile-backend',self.registry.commit,argv,source,env,self.out/'backend.log',time.monotonic()+1230,self.registry)
            except Exception as exc:self.error=type(exc).__name__
        self.thread=threading.Thread(target=execute,name='owned-production-mobile-backend');self.thread.start()
        end=time.monotonic()+180
        while not (self.root/'input.json').exists():
            if not self.thread.is_alive():raise RuntimeError('real_backend_prepare_failed')
            if time.monotonic()>end:raise TimeoutError('real_backend_prepare_timeout')
            time.sleep(.1)
        self.config=json.loads((self.root/'input.json').read_text())
        self.state['origin']=self.config['origin'];self.state['ready']=True;self.save()
        return self.config

    def close(self):
        (self.root/'stop').touch(mode=0o600)
        if self.thread:
            self.thread.join(timeout=45)
            if self.thread.is_alive():
                self.state['join_cleanup']=self.registry.cleanup(time.monotonic()+15).failures
                self.thread.join(timeout=15)
            self.state['thread_joined']=not self.thread.is_alive()
            if self.thread.is_alive():raise RuntimeError('real_backend_thread_still_running')
        self.state['actual_exit']=self.event.exit_code if self.event else None
        self.state['command_failures']=self.event.failures if self.event else [self.error or 'not_started']
        self.state['go_unchanged']=digest(self.args.go)==self.state['go_sha256']
        records=self.registry.records()
        children=[r for r in records if r.get('event')=='registered' and r.get('kind')=='process' and r.get('detail',{}).get('test')==self.fixture_test]
        verified=len(children)==self.expected_product_children
        for child in children:
            def match(r):return all(r.get(k)==child.get(k) for k in ('source_commit','owner','kind','identity','fingerprint'))
            ready=[r for r in records if r.get('event')=='ready' and match(r)]
            exits=[r for r in records if r.get('event')=='exited' and match(r)]
            verified=verified and len(ready)==1 and len(exits)==1 and exits[0]['detail'].get('actual_exit')==exits[0]['detail'].get('expected_exit')=='signal:killed'
        self.state['product_children']={'registered':len(children),'lifecycle_verified':verified}
        self.state['source_unchanged']=all(digest(self.repo/p)==h for p,h in self.sources.items())
        self.save()


def collect(out, state, platform):
    driver=(out/'backend-driver.log').read_text()
    rows=re.findall(r'MOBILE_BACKEND_REPORT (\{[^\n]+\})',driver)
    if len(rows)!=1:raise ValueError('native_backend_report_missing_or_duplicate')
    report=json.loads(rows[0]);facts=json.loads((out/'backend-facts.json').read_text())
    verify_report(report,facts,state['actual_exit'],driver,platform)
    if state.get('go_unchanged') is not True or state.get('product_children',{}).get('lifecycle_verified') is not True or state.get('command_failures') or not state.get('thread_joined') or not state.get('source_unchanged'):raise ValueError('backend_lifecycle_unverified')
    if state.get('native_pid_verified') is not True:raise ValueError('native_backend_pid_not_independently_verified')
    content=(out/'backend.log').read_text()+(out/'backend.log.stderr').read_text()
    if '--- PASS: TestMobileBackendFixture' not in content or '--- SKIP:' in content:raise ValueError('backend_test_did_not_pass')
    state.update(passed=True,native_report=report,database=facts)


def run_android(repo,args,sim,flutter,step,client):
    from urllib.parse import urlparse
    state={};sim.state['real_backend']=state
    backend=Backend(repo,args,sim.private,sim.out,sim.registry,'android',state,sim.save)
    port=None
    try:
        config=backend.start();port=urlparse(config['origin']).port
        sim.adb('backend-reverse',['reverse','tcp:'+str(port),'tcp:'+str(port)])
        with bundled_fixture(client,config):
            step('backend-pub',flutter+['pub','get','--offline','--enforce-lockfile'],120)
            step('backend-driver',flutter+['drive','--driver=test_driver/integration_test.dart','--target='+TARGET,'-d',sim.serial,'--no-pub','--no-dds','--keep-app-running','--dart-define=IM_NATIVE_BUNDLED_FIXTURE=true','-v'],900)
        rows=re.findall(r'MOBILE_BACKEND_REPORT (\{[^\n]+\})',(sim.out/'backend-driver.log').read_text())
        if len(rows)!=1:raise ValueError('native_backend_report_missing')
        native=json.loads(rows[0])
        state['native_pid_verified']=sim.adb('backend-pid',['shell','pidof','com.enterpriseim.client']).stdout.decode().strip()==str(native['runtime_pid'])
        sim.stop_app('backend-final')
    finally:
        try:
            if port is not None:sim.adb('backend-reverse-remove',['reverse','--remove','tcp:'+str(port)])
        finally:
            backend.close();sim.save()
    collect(sim.out,state,'android');sim.save()
    step('backend-pub-restore',flutter+['pub','get','--offline','--enforce-lockfile'],120)


def run_ios(repo,args,udid,private,out,env,step,state,save,registry):
    result={};state['real_backend']=result
    backend=Backend(repo,args,private,out,registry,'ios',result,save)
    try:
        config=backend.start()
        with bundled_fixture(Path(private)/'client',config):
            step('backend-pub',['pub','get','--offline','--enforce-lockfile'],timeout=120)
            step('backend-driver',['drive','--driver=test_driver/integration_test.dart','--target='+TARGET,'-d',udid,'--no-pub','--no-dds','--keep-app-running','--dart-define=IM_NATIVE_BUNDLED_FIXTURE=true','-v'],timeout=900)
        rows=re.findall(r'MOBILE_BACKEND_REPORT (\{[^\n]+\})',(out/'backend-driver.log').read_text())
        if len(rows)!=1:raise ValueError('native_backend_report_missing')
        native=json.loads(rows[0])
        p=subprocess.run(['/usr/bin/xcrun','simctl','spawn',udid,'launchctl','list'],env=env,capture_output=True,timeout=15)
        matches=[line.split() for line in p.stdout.decode().splitlines() if 'UIKitApplication:com.enterpriseim.client[' in line]
        result['native_pid_verified']=p.returncode==0 and len(matches)==1 and matches[0][0]==str(native['runtime_pid'])
    finally:
        try:
            p=subprocess.run(['/usr/bin/xcrun','simctl','terminate',udid,'com.enterpriseim.client'],env=env,capture_output=True,timeout=15)
            result['terminate_actual_exit']=p.returncode
        finally:
            backend.close();save()
    collect(out,result,'ios');save()
    step('backend-pub-restore',['pub','get','--offline','--enforce-lockfile'],timeout=120)
