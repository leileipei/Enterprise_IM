"""Real OS link and HOME/MAIN probes on the caller's newly owned Android AVD."""
import hashlib
import json
from pathlib import Path
import re
import secrets
import shlex
import subprocess
import threading
import time
from urllib.parse import urlsplit

PHASES = ['wrong-state', 'wrong-path', 'duplicate-state', 'warm-valid', 'stale-after-cancel', 'new-valid', 'home-ready', 'background', 'done']


def vm_service(text, pid):
    values = set(re.findall(r'^I/flutter\s*\(\s*' + str(pid) + r'\): The Dart VM service is listening on (\S+)\s*$', text, re.M))
    if len(values) != 1:raise ValueError('owned_vm_service_unverified')
    uri = urlsplit(next(iter(values)))
    if uri.scheme != 'http' or uri.hostname != '127.0.0.1' or uri.username or uri.password or uri.query or uri.fragment or not uri.port or not re.fullmatch(r'/[A-Za-z0-9_=-]+/', uri.path):
        raise ValueError('owned_vm_service_unverified')
    return uri.port, uri.path


def verify_report(report, pid, phases, driver):
    if report.get('status') != 'passed' or report.get('pid') != pid or report.get('cold_initial') is not True or report.get('cancelled') is not True or report.get('lifecycle') != ['paused', 'resumed'] or report.get('background_ms', 0) < 1000 or phases != PHASES or 'All tests passed.' not in driver or 'Failure Details:' in driver:
        raise ValueError('android_mobile_boundary_unverified')


def run(sim, flutter, step, client):
    probe = secrets.token_hex(16)
    target = 'integration_test/android_mobile_boundary_test.dart'
    step('boundary-build', [*flutter, 'build', 'apk', '--debug', '--no-pub', '--target=' + target, '--dart-define=IM_ANDROID_BOUNDARY_PROBE_ID=' + probe])
    sim.adb('boundary-install', ['install', '-r', str(client / 'build/app/outputs/flutter-apk/app-debug.apk')])
    sim.stop_app('boundary-before-cold')
    sim.adb('boundary-log-clear', ['logcat', '-c'])
    def uri(state, code, path='/oauth/callback'):
        return 'com.enterpriseim.client:' + path + '?state=' + state + '-' + probe + '&code=' + code
    def view(link):
        return ['am','start','-W','-a','android.intent.action.VIEW','-c','android.intent.category.BROWSABLE','-d',link]
    def launch_text(value):
        if 'Status: ok' not in value or 'com.enterpriseim.client/.MainActivity' not in value or 'Error:' in value:
            raise ValueError('android_boundary_intent_unverified')
    cold = sim.adb('boundary-cold-view', ['shell', shlex.join(view(uri('cold','cold')))]).stdout.decode()
    sim.adb('boundary-cold-diagnostics', ['logcat','-d','-v','brief'])
    launch_text(cold)
    pid_text = sim.adb('boundary-cold-pid', ['shell','pidof','com.enterpriseim.client']).stdout.decode().strip()
    if not re.fullmatch(r'\d+',pid_text):raise ValueError('android_boundary_pid_unverified')
    pid = int(pid_text)
    end = time.monotonic() + 45
    while True:
        logs = sim.adb('boundary-vm-log', ['logcat','-d','-v','brief','-s','flutter']).stdout.decode(errors='replace')
        try:
            guest, path = vm_service(logs, pid); break
        except ValueError:
            if time.monotonic() >= end:raise
            time.sleep(.2)
    forward = sim.adb('boundary-forward', ['forward','tcp:0','tcp:' + str(guest)]).stdout.decode().strip()
    if not re.fullmatch(r'[1-9]\d{0,4}',forward) or not 0 < int(forward) <= 65535:raise ValueError('android_boundary_forward_unverified')
    result = {'probe':probe,'pid':pid,'phases':[],'commands':[],'acceptance':'OS callback/lifecycle boundary only; synthetic OAuth values; no IdP/backend'}
    stopped = threading.Event()
    # Controller owns only its result/logs. It never mutates the parent's save/state
    # concurrently with run_command; join before attaching its result to evidence.
    def adb(label, arguments, required=True):
        p = subprocess.run([*sim.adb_base,'-s',sim.serial,*arguments], cwd=sim.private, env=sim.env, capture_output=True, timeout=8)
        if label:
            (sim.out / ('boundary-controller-' + label + '.log')).write_bytes(p.stdout + p.stderr)
            result['commands'].append({'label':label,'arguments':arguments,'actual_exit':p.returncode,'stdout_sha256':hashlib.sha256(p.stdout).hexdigest()})
        if required and p.returncode:raise RuntimeError('boundary_controller_command_failed')
        return p
    def same_pid():
        if adb('pid-' + str(len(result['phases'])), ['shell','pidof','com.enterpriseim.client']).stdout.decode().strip() != str(pid):
            raise ValueError('android_boundary_process_changed')
    def controller():
        try:
            marker = 'code_cache/im-boundary-' + probe + '.json'
            for phase in PHASES:
                end = time.monotonic() + 40
                while not stopped.is_set():
                    p = adb(None, ['shell','run-as','com.enterpriseim.client','cat',marker], required=False)
                    try:data = json.loads(p.stdout) if p.returncode == 0 else {}
                    except (ValueError, UnicodeError):data = {}
                    if data.get('status') == 'failed':raise ValueError('android_boundary_native_assertion_failed')
                    if data.get('phase') == phase:break
                    if time.monotonic() >= end:raise TimeoutError('android_boundary_phase_' + phase)
                    stopped.wait(.1)
                else:return
                if data.get('pid') != pid:raise ValueError('android_boundary_marker_pid_mismatch')
                same_pid(); result['phases'].append(phase)
                (sim.out / ('boundary-phase-' + phase + '.json')).write_text(json.dumps(data, indent=2) + '\n')
                if phase == 'done':
                    result['native_report'] = data; return
                if phase == 'home-ready':
                    adb(phase,['shell','input','keyevent','KEYCODE_HOME'])
                elif phase == 'background':
                    launch_text(adb(phase,['shell',shlex.join(['am','start','-W','-a','android.intent.action.MAIN','-c','android.intent.category.LAUNCHER','-n','com.enterpriseim.client/.MainActivity'])]).stdout.decode())
                else:
                    links = {'wrong-state':uri('old','wrong'), 'wrong-path':uri('warm','wrong','/oauth/wrong'), 'duplicate-state':'com.enterpriseim.client:/oauth/callback?state=warm-' + probe + '&state=warm-' + probe + '&code=wrong', 'warm-valid':uri('warm','warm'), 'stale-after-cancel':uri('cancel','stale'), 'new-valid':uri('next','next')}
                    launch_text(adb(phase,['shell',shlex.join(view(links[phase]))]).stdout.decode())
        except Exception as exc:result['error'] = type(exc).__name__ + ':' + str(exc)
    thread = threading.Thread(target=controller, name='owned-android-boundary-controller')
    thread.start()
    try:
        step('boundary-driver', [*flutter,'drive','--driver=test_driver/integration_test.dart','--target=' + target,'--use-existing-app=http://127.0.0.1:' + forward + path,'-d',sim.serial,'--no-pub','--no-dds','--keep-app-running','-v'], 420)
        thread.join(timeout=45)
        if thread.is_alive() or result.get('error'):raise RuntimeError('android_boundary_controller_unverified')
        verify_report(result.get('native_report',{}), pid, result['phases'], (sim.out / 'boundary-driver.log').read_text())
        result['passed'] = True
    finally:
        stopped.set(); thread.join(timeout=12)
        result['controller_joined'] = not thread.is_alive()
        sim.state['mobile_boundaries'] = result; sim.save()
        sim.adb('boundary-forward-remove',['forward','--remove','tcp:' + forward])
        sim.stop_app('boundary-final')
        if thread.is_alive():raise RuntimeError('android_boundary_controller_still_running')
