"""Normal iOS simulator app launch, bound to the archived application files."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import time
from .mobile import owned_simulator, simulator_id
from .registry import process_fingerprint
from .native_results import require, digest, read_json
from .native_runner_artifacts import validate_png

PACKAGE='com.enterpriseim.client'
PS_FORMAT='pid=,lstart=,comm='
ORDER=('install','bundle-path','launch','alive-before','alive-after','screenshot','terminate','stopped')
IDENTITY=('pid','uid','start_time','executable_path','executable_sha256')


def process_observation(pid, env):
    # lstart must agree with registry.process_fingerprint regardless of host TZ.
    return subprocess.run(['/bin/ps','-p',str(pid),'-o',PS_FORMAT],env=dict(env,TZ='UTC',LANG='C',LC_ALL='C'),capture_output=True,timeout=5)


def launched_pid(text):
    match=re.fullmatch(re.escape(PACKAGE)+r': ([1-9][0-9]*)\n?',text)
    if not match:raise ValueError('ios_normal_launch_pid_invalid')
    return int(match[1])


def bundle_manifest(app):
    result={}
    for path in sorted(Path(app).rglob('*')):
        if path.is_symlink():raise ValueError('ios_installed_bundle_link')
        if path.is_file():result[path.relative_to(app).as_posix()]=digest(path)
    if not result:raise ValueError('ios_installed_bundle_empty')
    return result


def _row(inventory, sim):
    row=owned_simulator(inventory,sim['runtime'],sim['name'],sim['udid'])
    require(row is not None and row.get('state')=='Booted' and row.get('deviceTypeIdentifier')==sim['device_type'],'ios_normal_device_unverified')
    return {k:row[k] for k in ('name','udid','deviceTypeIdentifier','state')}


def run(app, out, env, result, save, inventory):
    app=Path(app);out=Path(out);sim=result['simulator'];udid=simulator_id(sim['udid'])
    state={'passed':False,'artifact_sha256':result['archive_sha256'],'commands':[],'native_leaf_os_exit_independently_observed':False,'native_leaf_os_exit':None}
    result['normal_launch']=state;state['ownership_before']=_row(inventory(),sim);save()
    def command(name, args, expected=0):
        p=process_observation(int(args[2]),env) if args[0]=='/bin/ps' else subprocess.run(args,env=env,capture_output=True,timeout=90)
        stdout=out/('ios-normal-'+name+'.log');stderr=out/('ios-normal-'+name+'.log.stderr')
        stdout.write_bytes(p.stdout);stderr.write_bytes(p.stderr)
        state['commands'].append({'name':name,'argv':args,'actual_exit':p.returncode,'stdout_sha256':digest(stdout),'stderr_sha256':digest(stderr)});save()
        require(type(p.returncode) is int and p.returncode==expected,'ios_normal_command_failed')
        return p.stdout.decode()
    def simctl(name, args):return command(name,['/usr/bin/xcrun','simctl',*args])
    simctl('install',['install',udid,str(app)])
    installed=Path(simctl('bundle-path',['get_app_container',udid,PACKAGE,'app']).strip());state['installed_app']=str(installed)
    root=Path.home()/'Library/Developer/CoreSimulator/Devices'/udid/'data/Containers/Bundle/Application'
    require(installed.is_absolute() and installed.parent.parent==root and installed.name=='Runner.app' and '..' not in installed.parts,'ios_normal_installed_path_unverified')
    simulator_id(installed.parent.name)
    manifest=bundle_manifest(installed);require(manifest==read_json(out/'bundle-manifest.json'),'ios_normal_installed_manifest_mismatch')
    file=out/'ios-normal-installed-manifest.json';file.write_text(json.dumps(manifest,indent=2)+'\n');state['installed_manifest_sha256']=digest(file);save()
    pid=launched_pid(simctl('launch',['launch','--terminate-running-process',udid,PACKAGE]));state['pid']=pid;save()
    argv=['/bin/ps','-p',str(pid),'-o',PS_FORMAT]
    command('alive-before',argv);before=process_fingerprint(pid)
    require(isinstance(before,dict) and before['uid']==os.getuid() and before['executable_path']==str(installed/'Runner') and before['executable_sha256']==result['bundle']['executable_sha256'],'ios_normal_process_unverified')
    state['identity_before']=before;save();time.sleep(3)
    command('alive-after',argv);after=process_fingerprint(pid)
    require(isinstance(after,dict) and all(before[k]==after.get(k) for k in IDENTITY),'ios_normal_process_changed')
    state['identity_after']=after
    screenshot=out/'ios-normal-login.png';simctl('screenshot',['io',udid,'screenshot','--type=png',str(screenshot)])
    validate_png(screenshot.read_bytes());state['screenshot_sha256']=digest(screenshot)
    state['ownership_after']=_row(inventory(),sim);save()
    simctl('terminate',['terminate',udid,PACKAGE])
    deadline=time.monotonic()+10
    while True:
        probe=process_observation(pid,env)
        if probe.returncode==1 and not probe.stdout and not probe.stderr:break
        require(probe.returncode==0,'ios_normal_absence_unknown')
        if time.monotonic()>deadline:raise ValueError('ios_normal_app_still_running')
        time.sleep(.1)
    command('stopped',argv,expected=1)
    require(not (out/'ios-normal-stopped.log').read_bytes() and not (out/'ios-normal-stopped.log.stderr').read_bytes(),'ios_normal_absence_unknown')
    require(digest(out/'ios-simulator-debug.zip')==state['artifact_sha256'] and bundle_manifest(app)==manifest,'ios_normal_artifact_changed')
    state.update(host_absence_after_terminate=True,passed=True);save()


def evidence(out, result, preflight, file, ref):
    """Validate retained observations offline; never query a current device."""
    require(result.get('owned_simulator_removed') is True,'ios_normal_simulator_cleanup_unverified')
    state=result.get('normal_launch');sim=result.get('simulator')
    require(isinstance(state,dict) and isinstance(sim,dict) and state.get('passed') is True,'ios_normal_launch_unverified')
    require(state.get('artifact_sha256')==result['archive_sha256'] and state.get('host_absence_after_terminate') is True and state.get('native_leaf_os_exit_independently_observed') is False and state.get('native_leaf_os_exit') is None,'ios_normal_launch_unverified')
    udid=simulator_id(sim['udid']);require(udid==sim['udid'] and sim['name']=='IM-Mobile-'+result['owner'],'ios_normal_device_unverified')
    require(preflight.get('simulator')=={'runtime':sim['runtime'],'device_type':sim['device_type']},'ios_normal_device_unverified')
    for key in ('ownership_before','ownership_after'):
        row=state.get(key);require(row=={'name':sim['name'],'udid':udid,'deviceTypeIdentifier':sim['device_type'],'state':'Booted'},'ios_normal_device_unverified')
    tools=preflight['tool_sha256'];require(all(isinstance(tools.get(p),str) and re.fullmatch('[a-f0-9]{64}',tools[p]) for p in ('/usr/bin/xcrun','/bin/ps')) and any(p.endswith('/usr/bin/simctl') for p in tools),'ios_normal_tools_unverified')
    installed=Path(state.get('installed_app',''));device_root=Path(preflight.get('simulator_device_root',''))
    require(device_root.is_absolute() and '..' not in device_root.parts and device_root.parts[-3:]==('Developer','CoreSimulator','Devices') and installed.is_absolute() and '..' not in installed.parts and installed.parent.parent==device_root/udid/'data/Containers/Bundle/Application' and installed.name=='Runner.app','ios_normal_installed_path_unverified')
    simulator_id(installed.parent.name)
    installed_manifest=file('ios-normal-installed-manifest.json',state.get('installed_manifest_sha256'))
    require(read_json(installed_manifest)==read_json(out/'bundle-manifest.json'),'ios_normal_installed_manifest_mismatch')
    pid=state.get('pid');require(type(pid) is int and pid>0,'ios_normal_process_unverified')
    before=state.get('identity_before');after=state.get('identity_after')
    require(isinstance(before,dict) and isinstance(after,dict) and before.get('pid')==pid and type(before.get('uid')) is int and before.get('uid')==preflight.get('host_uid') and isinstance(before.get('start_time'),str) and before['start_time'] and before.get('executable_path')==str(installed/'Runner') and before.get('executable_sha256')==result['bundle']['executable_sha256'] and all(before.get(k)==after.get(k) for k in IDENTITY),'ios_normal_process_unverified')
    root=result.get('root');owner=result.get('owner');require(isinstance(root,str) and Path(root).is_absolute() and '..' not in Path(root).parts and isinstance(owner,str) and re.fullmatch('[a-f0-9]{32}',owner),'ios_normal_owner_unverified')
    app=str(Path(root)/owner/'client/build/ios/iphonesimulator/Runner.app');image=out/'ios-normal-login.png'
    expected={'install':['/usr/bin/xcrun','simctl','install',udid,app],'bundle-path':['/usr/bin/xcrun','simctl','get_app_container',udid,PACKAGE,'app'],'launch':['/usr/bin/xcrun','simctl','launch','--terminate-running-process',udid,PACKAGE],'screenshot':['/usr/bin/xcrun','simctl','io',udid,'screenshot','--type=png',str(image)],'terminate':['/usr/bin/xcrun','simctl','terminate',udid,PACKAGE]}
    for key in ('alive-before','alive-after','stopped'):expected[key]=['/bin/ps','-p',str(pid),'-o',PS_FORMAT]
    commands=state.get('commands');require(isinstance(commands,list) and len(commands)==len(expected),'ios_normal_commands_unverified');by_name={c.get('name'):c for c in commands if isinstance(c,dict)};require(set(by_name)==set(expected) and [c.get('name') for c in commands]==list(ORDER),'ios_normal_commands_unverified')
    selected=[]
    for name in ORDER:
        args=expected[name]
        row=by_name[name];require(row.get('argv')==args and type(row.get('actual_exit')) is int and row['actual_exit']==(1 if name=='stopped' else 0),'ios_normal_commands_unverified')
        stdout=file('ios-normal-'+name+'.log',row.get('stdout_sha256'));stderr=file('ios-normal-'+name+'.log.stderr',row.get('stderr_sha256'))
        if name!='stopped':selected.append({'argv':args,'actual_exit':0,'stdout':ref(stdout),'stderr':ref(stderr)})
        if name in ('alive-before','alive-after'):
            parts=stdout.read_text().strip().split(None,6);require(len(parts)==7 and parts[0]==str(pid) and ' '.join(parts[1:6])==before['start_time'] and parts[6]==str(installed/'Runner') and not stderr.read_bytes(),'ios_normal_process_log_mismatch')
        if name=='stopped':require(not stdout.read_bytes() and not stderr.read_bytes(),'ios_normal_absence_unknown')
    require((out/'ios-normal-bundle-path.log').read_text().strip()==str(installed) and launched_pid((out/'ios-normal-launch.log').read_text())==pid,'ios_normal_process_log_mismatch')
    validate_png(file('ios-normal-login.png',state.get('screenshot_sha256')).read_bytes())
    return selected,{'artifact_sha256':state['artifact_sha256'],'runtime_pid':pid,'host_absence_after_terminate':True,'native_leaf_os_exit_independently_observed':False,'native_leaf_os_exit':None,'stopped_observation':by_name['stopped'],'screenshot':ref(image),'installed_manifest':ref(installed_manifest)}
