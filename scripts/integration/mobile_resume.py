"""Real mobile OS pause/resume around controlled device-bound API contracts."""
import hashlib
import json
from pathlib import Path
import re
import secrets
import shlex
import subprocess
import threading
import time
from .android_mobile_boundary import vm_service

PHASES = ['pause-read','resume-fresh','pause-revoke','resume-revoke','pause-terminal','resume-terminal','done']
TARGET = 'integration_test/mobile_resume_test.dart'
PACKAGE = 'com.enterpriseim.client'


def verify_report(report, pid, probe, phases, driver):
    expected = {'status':'passed','pid':pid,'probe':probe,'lifecycle':['paused','resumed']*3,'old_read_ack_ignored':True,'old_page_redacted':True,'pending_resume_reads':0,'fresh_after':['0','0','0'],'fresh_device_headers':True,'catchup_cursor':'2','read_seqs':['2'],'device_denial':'invalid_device','received_state_cleared':True,'terminal_no_restart':True,'socket_closed_on_pause':True}
    durations = report.get('background_ms', [])
    if any(report.get(k) != v for k,v in expected.items()) or len(durations) != 3 or any(not isinstance(v,int) or v < 1000 for v in durations) or phases != PHASES or 'All tests passed.' not in driver or 'Failure Details:' in driver:
        raise ValueError('mobile_resume_unverified')


def control(result, out, probe, stopped, read, identity, home, front):
    try:
        for phase in PHASES:
            end = time.monotonic() + 100
            while not stopped.is_set():
                data = read()
                if stopped.is_set():return
                if data.get('status') == 'failed':
                    (out / 'resume-native-failed.json').write_text(json.dumps(data,indent=2)+'\n')
                    raise ValueError('mobile_resume_native_assertion_failed')
                if data.get('phase') == phase:break
                if time.monotonic() >= end:raise TimeoutError('mobile_resume_phase_' + phase)
                stopped.wait(.1)
            else:return
            pid = data.get('pid')
            if not isinstance(pid,int) or pid <= 0 or data.get('probe') != probe:raise ValueError('mobile_resume_marker_identity')
            if result.get('pid',pid) != pid:raise ValueError('mobile_resume_process_changed')
            if stopped.is_set():return
            identity(pid)
            if stopped.is_set():return
            result['pid'] = pid
            result['phases'].append(phase)
            (out / ('resume-phase-' + phase + '.json')).write_text(json.dumps(data,indent=2) + '\n')
            if phase == 'done':result['native_report'] = data;return
            if stopped.is_set():return
            if phase.startswith('resume-') and stopped.wait(1.15):return
            if stopped.is_set():return
            (home if phase.startswith('pause-') else front)(phase)
    except Exception as exc:result['error'] = type(exc).__name__ + ':' + str(exc)


def runner(result, out, probe, read, identity, home, front, drive):
    stopped = threading.Event()
    thread = threading.Thread(target=control,args=(result,out,probe,stopped,read,identity,home,front),name='owned-mobile-resume-controller')
    thread.start()
    try:
        drive()
        thread.join(timeout=105)
        if thread.is_alive() or result.get('error'):raise RuntimeError('mobile_resume_controller_unverified')
        verify_report(result.get('native_report',{}), result['pid'], probe, result['phases'], (out / 'resume-driver.log').read_text())
        result['passed'] = True
    finally:
        stopped.set();thread.join(timeout=20)
        result['controller_joined'] = not thread.is_alive()
        if thread.is_alive():raise RuntimeError('mobile_resume_controller_still_running')


def read_json(raw):
    try:
        value = json.loads(raw)
        return value if isinstance(value,dict) else {}
    except (ValueError,UnicodeError):return {}


def run_android(sim, flutter, step, client):
    probe = secrets.token_hex(16)
    step('resume-storage',[*flutter,'drive','--driver=test_driver/integration_test.dart','--target=integration_test/native_storage_test.dart','-d',sim.serial,'--no-pub','--no-dds','--keep-app-running','-v'])
    if 'All tests passed.' not in (sim.out / 'resume-storage.log').read_text():raise ValueError('native_storage_unverified')
    sim.stop_app('resume-storage')
    step('resume-build', [*flutter,'build','apk','--debug','--no-pub','--target=' + TARGET,'--dart-define=IM_MOBILE_RESUME_PROBE_ID=' + probe])
    sim.adb('resume-install',['install','-r',str(client / 'build/app/outputs/flutter-apk/app-debug.apk')])
    sim.stop_app('resume-before');sim.adb('resume-clear-log',['logcat','-c'])
    launch = ['am','start','-W','-a','android.intent.action.MAIN','-c','android.intent.category.LAUNCHER','-n',PACKAGE + '/.MainActivity']
    def checked_launch(raw):
        if 'Status: ok' not in raw or PACKAGE + '/.MainActivity' not in raw or 'Error:' in raw:raise ValueError('resume_android_launch_unverified')
    checked_launch(sim.adb('resume-start',['shell',shlex.join(launch)]).stdout.decode())
    pid = int(sim.adb('resume-pid',['shell','pidof',PACKAGE]).stdout.decode().strip())
    end = time.monotonic()+45
    while True:
        logs = sim.adb('resume-vm',['logcat','-d','-v','brief','-s','flutter']).stdout.decode(errors='replace')
        try:guest,path = vm_service(logs,pid);break
        except ValueError:
            if time.monotonic() >= end:raise
            time.sleep(.2)
    forward = sim.adb('resume-forward',['forward','tcp:0','tcp:' + str(guest)]).stdout.decode().strip()
    if not re.fullmatch(r'[1-9]\d{0,4}',forward) or int(forward)>65535:raise ValueError('resume_android_forward_unverified')
    result = {'probe':probe,'pid':pid,'phases':[],'commands':[],'scope':'actual OS events; controlled API/socket; production sync/receipts/encrypted SQLite; no backend/IdP'}
    def adb(label,argv,required=True):
        p = subprocess.run([*sim.adb_base,'-s',sim.serial,*argv],cwd=sim.private,env=sim.env,capture_output=True,timeout=8)
        if label:
            (sim.out / ('resume-controller-' + label + '.log')).write_bytes(p.stdout+p.stderr)
            result['commands'].append({'label':label,'arguments':argv,'actual_exit':p.returncode,'stdout_sha256':hashlib.sha256(p.stdout).hexdigest()})
        if required and p.returncode:raise RuntimeError('resume_adb_failed')
        return p
    def read():return read_json(adb(None,['shell','run-as',PACKAGE,'cat','code_cache/im-resume-' + probe + '.json'],False).stdout)
    def identity(value):
        if adb('pid-' + str(len(result['phases'])),['shell','pidof',PACKAGE]).stdout.decode().strip()!=str(value):raise ValueError('resume_android_pid_changed')
    try:
        runner(result,sim.out,probe,read,identity,lambda phase:adb(phase,['shell','input','keyevent','KEYCODE_HOME']),lambda phase:checked_launch(adb(phase,['shell',shlex.join(launch)]).stdout.decode()),lambda:step('resume-driver',[*flutter,'drive','--driver=test_driver/integration_test.dart','--target=' + TARGET,'--use-existing-app=http://127.0.0.1:' + forward + path,'-d',sim.serial,'--no-pub','--no-dds','--keep-app-running','-v'],600))
    finally:
        sim.state['mobile_resume'] = result;sim.save()
        sim.adb('resume-forward-remove',['forward','--remove','tcp:' + forward]);sim.stop_app('resume-final')


def run_ios(udid, private, out, env, step, state, save):
    probe = secrets.token_hex(16)
    result = {'probe':probe,'phases':[],'commands':[],'scope':'actual OS events; controlled API/socket; production sync/receipts/encrypted SQLite; no backend/IdP'}
    def simctl(label,argv,required=True):
        p = subprocess.run(['/usr/bin/xcrun','simctl',*argv],cwd=private,env=env,capture_output=True,timeout=12)
        if label:
            (out / ('resume-controller-' + label + '.log')).write_bytes(p.stdout+p.stderr)
            result['commands'].append({'label':label,'arguments':argv,'actual_exit':p.returncode,'stdout_sha256':hashlib.sha256(p.stdout).hexdigest()})
        if required and p.returncode:raise RuntimeError('resume_simctl_failed')
        return p
    container = None
    def read():
        nonlocal container
        if container is None:
            p = simctl(None,['get_app_container',udid,PACKAGE,'data'],False)
            if p.returncode:return {}
            candidate = Path(p.stdout.decode().strip())
            if not candidate.is_absolute() or not re.fullmatch(r'[A-Fa-f0-9-]{36}',candidate.name) or candidate.parent.name!='Application':raise ValueError('resume_ios_container_unverified')
            container = candidate
        file = container / 'tmp' / ('im-resume-' + probe + '.json')
        return read_json(file.read_bytes()) if file.is_file() else {}
    def identity(pid):
        value = simctl('pid-' + str(len(result['phases'])),['spawn',udid,'launchctl','list']).stdout.decode()
        rows = [line.split() for line in value.splitlines() if 'UIKitApplication:' + PACKAGE + '[' in line]
        if len(rows)!=1 or rows[0][0]!=str(pid):raise ValueError('resume_ios_pid_changed')
    try:
        step('resume-storage',['test','integration_test/native_storage_test.dart','-d',udid,'--no-pub'])
        if 'All tests passed!' not in (out / 'resume-storage.log').read_text():raise ValueError('native_storage_unverified')
        # One standard driver owns install/launch. Controller waits for its own
        # nonce file, so no synthetic lifecycle or synthetic focus is injected.
        runner(result,out,probe,read,identity,lambda phase:simctl(phase,['launch',udid,'com.apple.mobilesafari']),lambda phase:simctl(phase,['launch',udid,PACKAGE]),lambda:step('resume-driver',['drive','--driver=test_driver/integration_test.dart','--target=' + TARGET,'-d',udid,'--no-pub','--no-dds','--keep-app-running','--dart-define=IM_MOBILE_RESUME_PROBE_ID=' + probe,'-v'],timeout=900))
    finally:
        state['mobile_resume'] = result;save()
        simctl('stop',['terminate',udid,PACKAGE],False)
