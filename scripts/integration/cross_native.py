"""Owned OS actors and private-file control; no customer device selection."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import shlex
import time
from .android_simulator import AndroidSimulator, package_installation
from .android_build import android_environment
from .mobile import simulator_id, owned_simulator
from .ios_app_launch import launched_pid, bundle_manifest
from .native_runner_artifacts import validate_png
from .registry import process_fingerprint
from .cross_device import validate_ack

PACKAGE='com.enterpriseim.client'
FILES={'cross-input.json','cross-command.json','cross-state.json'}


def digest(path):return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def installed_apk_hash(sim,path,expected):
    if sim.state.get('configuration',{}).get('api')==24:
        # This prepared API24 image has no sha256sum and its file-sync stat
        # rejects a path readable by shell. Accept only complete matching bytes.
        for attempt in range(12):
            response=sim.adb('installed-bytes-'+str(attempt),['exec-out','cat',path],required=False,binary=True)
            if response.returncode==0:
                if hashlib.sha256(response.stdout).hexdigest()!=expected:raise ValueError('actor_apk_mismatch')
                boot=sim.adb('installed-boot-'+str(attempt),['shell','cat','/proc/sys/kernel/random/boot_id'],required=False)
                if boot.returncode==0:
                    if boot.stdout.decode().strip()!=sim.state['boot_id']:raise ValueError('actor_vm_boot_replaced')
                    sim.state.update(installed_apk_verification='complete_exec_out_exact_SHA256_same_boot',installed_apk_bytes=len(response.stdout))
                    return expected
            sim.adb('installed-reconnect-'+str(attempt),['reconnect','offline'],required=False)
            time.sleep(.5)
        raise ValueError('actor_installed_apk_transport_unavailable')
    # Hash inside the guest: avoid an unframed large exec-out stream being
    # truncated when the first-boot ADB transport reconnects. Never accept a
    # different checksum or a replacement VM boot as a retry success.
    for attempt in range(12):
        response=sim.adb('installed-sha256-'+str(attempt),['shell','sha256sum',path],required=False)
        if response.returncode==0:
            fields=response.stdout.decode().split()
            if len(fields)!=2 or fields[1]!=path or fields[0]!=expected:raise ValueError('actor_apk_mismatch')
            boot=sim.adb('installed-boot-'+str(attempt),['shell','cat','/proc/sys/kernel/random/boot_id'],required=False)
            if boot.returncode==0:
                if boot.stdout.decode().strip()!=sim.state['boot_id']:raise ValueError('actor_vm_boot_replaced')
                return fields[0]
        sim.adb('installed-reconnect-'+str(attempt),['reconnect','offline'],required=False)
        time.sleep(.5)
    raise ValueError('actor_installed_apk_transport_unavailable')


def wait_process_absent(pid,timeout=15):
    # Identity must be proved by the caller before its one terminating signal.
    # A dying process can temporarily report a non-executable comm string.
    # Kernel existence needs neither path decoding nor another killing signal.
    if type(pid) is not int or pid<=1:raise ValueError('invalid_absence_pid')
    end=time.monotonic()+timeout
    while True:
        try:os.kill(pid,0)
        except ProcessLookupError:return
        if time.monotonic()>=end:raise TimeoutError('old_process_still_exists')
        time.sleep(.05)


class Actor:
    def __init__(self,role,out,state,save):
        self.role=role;self.out=Path(out);self.state=state;self.save=save;self.expected=None;self.number=0
    def await_state(self,number,timeout=100):
        end=time.monotonic()+timeout
        while time.monotonic()<end:
            row=self.read()
            if row and row.get('status')=='failed':
                (self.out/('failed-state-'+str(number)+'.json')).write_text(json.dumps(row,indent=2)+'\n');raise ValueError('native_actor_failed_'+self.role)
            if row and row.get('id')==number:
                if self.expected:validate_ack(row,self.expected,number)
                elif row.get('role')!=self.role or row.get('status')!='ok':raise ValueError('actor_initial_identity_invalid')
                self.identity(row['pid']);(self.out/('state-'+str(number)+'.json')).write_text(json.dumps(row,indent=2)+'\n');return row
            if self.state.get('pid'):self.identity(self.state['pid'])
            time.sleep(.1)
        raise TimeoutError('native_actor_command_timeout_'+self.role+'_'+str(number))
    def ready(self,config):
        row=self.await_state(0)
        if row.get('nonce')!=config['nonce'] or row.get('user')!=config['user'] or row.get('platform')!=config['platform']:raise ValueError('actor_bootstrap_mismatch')
        self.expected={k:row[k] for k in ('role','nonce','pid')};self.state['admission']={k:row[k] for k in ('role','platform','nonce','pid','user','device_id')};self.save();return row
    def command(self,action,**kwargs):
        self.number+=1;self.write('cross-command.json',{'id':self.number,'action':action,**kwargs});row=self.await_state(self.number)
        if row.get('device_id')!=self.state['admission']['device_id']:raise ValueError('actor_device_replaced')
        return row


class AndroidActor(Actor):
    def __init__(self,role,args,private,out,owner,registry,configuration,state,save):
        super().__init__(role,out,state,save);Path(private).mkdir(mode=0o700);self.env=android_environment(private,args)
        self.sim=AndroidSimulator(args,private,out,owner,registry,self.env,configuration,state,save);self.start_ticks=None
    def start(self,apk,origin_port,config):
        self.sim.start();self.sim.adb('install',['install','--no-streaming','-r',str(apk)])
        installed=self.sim.adb('installed-path',['shell','pm','path',PACKAGE]).stdout.decode().strip().removeprefix('package:')
        if not installed.startswith('/data/app/') or not installed.endswith('/base.apk'):raise ValueError('actor_apk_path_invalid')
        installed_hash=installed_apk_hash(self.sim,installed,digest(apk))
        if installed_hash!=digest(apk):raise ValueError('actor_apk_mismatch')
        self.state['apk_sha256']=installed_hash;self.state['installation']=package_installation(self.sim.adb('package',['shell','dumpsys','package',PACKAGE]).stdout.decode())
        self.sim.adb('backend-reverse',['reverse','tcp:'+str(origin_port),'tcp:'+str(origin_port)])
        self.write('cross-input.json',config)
        self.sim.adb('start',['shell','am','start','-W','-a','android.intent.action.MAIN','-c','android.intent.category.LAUNCHER','-n',PACKAGE+'/.MainActivity'])
        text=self.sim.adb('initial-pid',['shell','pidof',PACKAGE]).stdout.decode().strip()
        if not re.fullmatch('[1-9][0-9]*',text):raise ValueError('actor_pid_invalid')
        self.state['pid']=int(text);self.identity(int(text));self.save()
    def write(self,name,data):
        if name not in FILES:raise ValueError('actor_private_filename_invalid')
        path='app_flutter/'+name
        args=[*self.sim.adb_base,'-s',self.sim.serial,'shell','run-as',PACKAGE,'sh','-c',shlex.quote('mkdir -p app_flutter && cat > '+path+'.new && mv '+path+'.new '+path)]
        p=subprocess.run(args,input=json.dumps(data).encode(),cwd=self.sim.private,env=self.env,capture_output=True,timeout=15)
        if p.returncode:raise ValueError('actor_private_write_failed')
    def read(self):
        p=subprocess.run([*self.sim.adb_base,'-s',self.sim.serial,'shell','run-as',PACKAGE,'cat','app_flutter/cross-state.json'],cwd=self.sim.private,env=self.env,capture_output=True,timeout=8)
        if p.returncode:return None
        return json.loads(p.stdout)
    def identity(self,pid):
        p=subprocess.run([*self.sim.adb_base,'-s',self.sim.serial,'shell','pidof',PACKAGE],env=self.env,capture_output=True,timeout=8)
        if p.returncode or p.stdout.strip()!=str(pid).encode():raise ValueError('android_actor_process_changed')
        p=subprocess.run([*self.sim.adb_base,'-s',self.sim.serial,'shell','run-as',PACKAGE,'cat','/proc/'+str(pid)+'/stat'],env=self.env,capture_output=True,timeout=8)
        if p.returncode:raise ValueError('android_actor_start_unproven')
        fields=p.stdout.decode().rsplit(')',1)[-1].split();ticks=fields[19]
        if self.start_ticks is not None and ticks!=self.start_ticks:raise ValueError('android_actor_pid_reused')
        self.start_ticks=ticks;self.state['process_start_ticks']=ticks
    def screenshot(self,label):
        path=self.out/(label+'.png');path.write_bytes(self.sim.adb('screen-'+label,['exec-out','screencap','-p'],binary=True).stdout);validate_png(path.read_bytes())
    def stop_for_restart(self):
        self.identity(self.state['pid']);self.state['restart_before']=dict(self.state['admission'])
        self.state['restart_before']['process_start_ticks']=self.start_ticks
        self.sim.adb('restart-force-stop',['shell','am','force-stop',PACKAGE])
        absent=self.sim.adb('restart-old-absent',['shell','pidof',PACKAGE],required=False)
        if absent.returncode!=1 or absent.stdout.strip():raise ValueError('android_old_process_remains')
        self.state['restart_old_absent']=True;self.save()
    def launch_restart(self,config):
        self.write('cross-input.json',config);self.write('cross-command.json',{'id':0,'action':'wait'});self.write('cross-state.json',{})
        self.state['pid']=None;self.start_ticks=None;self.expected=None;self.number=0
        self.out=self.out/'restart';self.out.mkdir(mode=0o700)
        self.sim.adb('restart-launch',['shell','am','start','-W','-n',PACKAGE+'/.MainActivity'])
        text=self.sim.adb('restart-pid',['shell','pidof',PACKAGE]).stdout.decode().strip()
        if not re.fullmatch('[1-9][0-9]*',text) or int(text)==self.state['restart_before']['pid']:raise ValueError('android_new_process_unproven')
        self.state['pid']=int(text);self.identity(int(text));self.ready(config)
    def close(self):return self.sim.close()


class IOSActor(Actor):
    def __init__(self,role,args,out,owner,state,save,env):
        super().__init__(role,out,state,save);self.args=args;self.owner=owner;self.env=env;self.udid=None;self.fp=None;self.documents=None;self.reserved=False
        self.state.update(name='IM-Cross-'+owner,runtime=args.simulator_runtime,device_type=args.simulator_device_type,commands=[])
    def simctl(self,label,args,timeout=90):
        p=subprocess.run(['/usr/bin/xcrun','simctl',*args],env=self.env,capture_output=True,timeout=timeout)
        (self.out/(label+'.log')).write_bytes(p.stdout);(self.out/(label+'.log.stderr')).write_bytes(p.stderr)
        self.state['commands'].append({'label':label,'argv':['/usr/bin/xcrun','simctl',*args],'actual_exit':p.returncode,'stdout_sha256':hashlib.sha256(p.stdout).hexdigest(),'stderr_sha256':hashlib.sha256(p.stderr).hexdigest()});self.save()
        if p.returncode:raise ValueError('cross_simctl_'+label+'_failed')
        return p.stdout.decode()
    def owned(self):
        inventory=json.loads(self.simctl('inventory-'+str(len(self.state['commands'])),['list','devices','-j']))
        row=owned_simulator(inventory,self.args.simulator_runtime,self.state['name'],self.udid)
        if row and row.get('deviceTypeIdentifier')!=self.args.simulator_device_type:raise ValueError('cross_ios_foreign_device')
        return row
    def start(self,app,config):
        if self.owned() is not None:raise ValueError('cross_ios_name_exists')
        self.reserved=True;self.udid=simulator_id(self.simctl('create',['create',self.state['name'],self.args.simulator_device_type,self.args.simulator_runtime]).strip());self.state['udid']=self.udid
        if self.owned() is None:raise ValueError('cross_ios_identity_missing')
        self.simctl('boot',['boot',self.udid]);self.simctl('bootstatus',['bootstatus',self.udid,'-b'],240)
        self.simctl('install',['install',self.udid,str(app)])
        installed=Path(self.simctl('installed-path',['get_app_container',self.udid,PACKAGE,'app']).strip())
        expected_app=Path.home()/'Library/Developer/CoreSimulator/Devices'/self.udid/'data/Containers/Bundle/Application'
        if installed.name!='Runner.app' or installed.parent.parent!=expected_app or installed.is_symlink() or installed.parent.is_symlink():raise ValueError('cross_ios_app_path_unverified')
        simulator_id(installed.parent.name)
        if bundle_manifest(app)!=bundle_manifest(installed):raise ValueError('cross_ios_installed_manifest_changed')
        self.state['installed_app']=str(installed);self.state['executable_sha256']=digest(installed/'Runner')
        data=Path(self.simctl('data-path',['get_app_container',self.udid,PACKAGE,'data']).strip());expected=Path.home()/'Library/Developer/CoreSimulator/Devices'/self.udid/'data/Containers/Data/Application'
        if data.parent!=expected or data.is_symlink():raise ValueError('cross_ios_data_path_unverified')
        simulator_id(data.name);self.documents=data/'Documents';self.documents.mkdir(mode=0o700,exist_ok=True);self.state['data_path']=str(data)
        self.write('cross-input.json',config)
        pid=launched_pid(self.simctl('launch',['launch','--terminate-running-process',self.udid,PACKAGE]));self.state['pid']=pid;self.identity(pid);self.save()
    def write(self,name,data):
        if name not in FILES:raise ValueError('actor_private_filename_invalid')
        path=self.documents/name;temp=self.documents/(name+'.new');temp.write_text(json.dumps(data));temp.chmod(0o600);temp.replace(path)
    def read(self):
        path=self.documents/'cross-state.json'
        return json.loads(path.read_text()) if path.exists() else None
    def identity(self,pid):
        fp=process_fingerprint(pid)
        if not fp or fp['executable_path']!=str(Path(self.state['installed_app'])/'Runner') or fp['executable_sha256']!=self.state['executable_sha256']:raise ValueError('cross_ios_process_unverified')
        if self.fp and any(fp[k]!=self.fp[k] for k in ('pid','uid','start_time','executable_path','executable_sha256')):raise ValueError('cross_ios_process_changed')
        self.fp=fp;self.state['process_identity']=fp
    def screenshot(self,label):
        path=self.out/(label+'.png');self.simctl('screen-'+label,['io',self.udid,'screenshot','--type=png',str(path)]);validate_png(path.read_bytes())
    def stop_for_restart(self):
        self.identity(self.state['pid']);self.state['restart_before']=dict(self.state['admission'])
        self.state['restart_before']['process_identity']=self.fp
        if self.owned() is None:raise ValueError('ios_restart_vm_missing')
        os.kill(self.state['pid'],9)
        wait_process_absent(self.state['pid'])
        self.state['restart_old_absent']=True;self.save()
    def launch_restart(self,config):
        self.write('cross-input.json',config);self.write('cross-command.json',{'id':0,'action':'wait'});self.write('cross-state.json',{})
        self.expected=None;self.number=0;self.fp=None;self.state['pid']=None
        self.out=self.out/'restart';self.out.mkdir(mode=0o700)
        pid=launched_pid(self.simctl('restart-launch',['launch',self.udid,PACKAGE]))
        if pid==self.state['restart_before']['pid']:raise ValueError('ios_new_process_unproven')
        self.state['pid']=pid;self.identity(pid);self.ready(config)
    def close(self):
        failures=[]
        try:
            if self.reserved:
                row=self.owned()
                if row:
                    self.udid=simulator_id(row['udid'])
                    if row.get('state')=='Booted':self.simctl('shutdown',['shutdown',self.udid])
                    self.simctl('delete',['delete',self.udid])
                    if self.owned() is not None:raise ValueError('cross_ios_device_remains')
                self.state['owned_simulator_removed']=True
        except Exception as exc:failures.append(type(exc).__name__)
        self.state['cleanup_failures']=failures;self.save();return failures
