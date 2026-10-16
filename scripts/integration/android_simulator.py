"""Scoped Android simulator checks. Only create and stop this run's empty AVD."""
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import time
from .model import ResourceRef
from .registry import process_fingerprint

IMAGES = {'system-images;android-24;google_apis;arm64-v8a': (24, 'google_apis', '29'), 'system-images;android-36;default;arm64-v8a': (36, 'default', '2')}


def simulator_preflight(sdk, runtime, device_type):
    if not runtime or not device_type:
        raise ValueError('explicit_android_simulator_runtime_and_type_required')
    if runtime not in IMAGES or device_type != 'pixel_7':
        raise ValueError('unapproved_android_simulator_choice')
    api, tag, revision = IMAGES[runtime]; sdk = Path(sdk); image = sdk.joinpath(*runtime.split(';'))
    files = [image / n for n in ('source.properties', 'system.img', 'ramdisk.img', 'kernel-ranchu')]
    tools = [sdk / n for n in ('emulator/emulator', 'emulator/source.properties', 'platform-tools/adb', 'platform-tools/source.properties', 'cmdline-tools/23.0/bin/avdmanager')]
    if any(not p.is_file() for p in files + tools):
        raise ValueError('prepared_android_simulator_missing')
    def props(path):
        rows = {}
        for line in path.read_text().splitlines():
            if '=' in line:
                key, value = line.split('=', 1)
                if key.strip() in rows:raise ValueError('ambiguous_simulator_properties')
                rows[key.strip()] = value.strip()
        return rows
    expected = {'AndroidVersion.ApiLevel': str(api), 'SystemImage.Abi': 'arm64-v8a', 'SystemImage.TagId': tag, 'Pkg.Revision': revision}
    if any(props(image / 'source.properties').get(k) != v for k, v in expected.items()) or props(sdk / 'emulator/source.properties').get('Pkg.Revision') != '37.2.12' or props(sdk / 'platform-tools/source.properties').get('Pkg.Revision') != '37.0.1':
        raise ValueError('fixed_android_simulator_mismatch')
    return {'package': runtime, 'api': api, 'abi': 'arm64-v8a', 'revision': revision, 'device_type': device_type, 'tool_sha256': {str(p): hashlib.sha256(p.read_bytes()).hexdigest() for p in tools + [image / 'source.properties']}, 'image_archive_hash_claimed': False}


def validate_device(properties, name, configuration):
    if (properties.get('ro.boot.qemu.avd_name') or properties.get('ro.kernel.qemu.avd_name')) != name or properties.get('ro.build.version.sdk') != str(configuration['api']) or properties.get('ro.product.cpu.abi') != configuration['abi'] or properties.get('ro.kernel.qemu') != '1':
        raise ValueError('owned_android_device_identity_mismatch')


def vault_phase(text, phase):
    pids = set(re.findall(r'NATIVE_VAULT_PROBE phase=' + re.escape(phase) + r' runtime_pid=(\d+)', text))
    if len(pids) != 1 or 'All tests passed.' not in text or 'Failure Details:' in text:
        raise ValueError('native_vault_phase_unverified')
    return {'phase': phase, 'runtime_pid': int(next(iter(pids)))}


def verify_vault_pair(write, verify):
    if write['runtime_pid'] == verify['runtime_pid'] or not write.get('uid') or write.get('uid') != verify.get('uid') or not write.get('first_install') or write.get('first_install') != verify.get('first_install'):
        raise ValueError('native_vault_process_or_installation_mismatch')


def package_installation(text):
    block = re.search(r'(?ms)^  Package \[com\.enterpriseim\.client\] \([^\n]+\):\n(.*?)(?=^  Package \[|^\S|\Z)', text)
    if block is None:raise ValueError('android_installation_unverified')
    uid = set(re.findall(r'^\s*(?:userId|appId)=(\d+)\s*$', block.group(1), re.M))
    installed = re.findall(r'^\s*firstInstallTime=(.+)$', block.group(1), re.M)
    if len(uid)!=1 or len(installed)!=1:raise ValueError('android_installation_unverified')
    return {'uid':next(iter(uid)), 'first_install':installed[0].strip()}


def available_ports():
    for port in range(5554, 5684, 2):
        first, second = socket.socket(), socket.socket()
        try:
            first.bind(('127.0.0.1', port)); second.bind(('127.0.0.1', port + 1))
            break
        except OSError:continue
        finally:first.close(); second.close()
    else:raise RuntimeError('no_available_android_console_pair')
    with socket.socket() as server:
        server.bind(('127.0.0.1', 0)); adb = server.getsockname()[1]
    if adb in (5037, port, port + 1):return available_ports()
    return port, adb


def boot_probe(argv, env, cwd):
    try:
        return subprocess.run(argv, cwd=cwd, env=env, capture_output=True, timeout=8)
    except subprocess.TimeoutExpired as exc:
        return subprocess.CompletedProcess(argv, 124, stdout=exc.stdout or b'', stderr=b'owned boot transport timeout')


class AndroidSimulator:
    def __init__(self, args, private, out, owner, registry, env, configuration, state, save):
        self.args, self.private, self.out, self.registry = args, Path(private), Path(out), registry
        self.env, self.configuration, self.state, self.save = env, configuration, state, save
        self.name = 'IM-Mobile-Android-' + owner
        self.services = []; self.serial = None
        self.state.update(name=self.name, configuration=configuration, commands=[], service_exits=[], boot_poll=[])
        for name in ('avds', 'emulator-home', 'emulator-tmp'):(self.private / name).mkdir(mode=0o700)
        self.env.update(ANDROID_AVD_HOME=str(self.private / 'avds'), ANDROID_EMULATOR_HOME=str(self.private / 'emulator-home'), ANDROID_TMP=str(self.private / 'emulator-tmp'))
        self.console, self.port = available_ports()
        self.env.update(ANDROID_ADB_SERVER_PORT=str(self.port), ADB_SERVER_SOCKET='tcp:127.0.0.1:' + str(self.port))
        self.state.update(console_port=self.console, adb_server_port=self.port)
        self.adb_base = [str(args.android_sdk / 'platform-tools/adb'), '-P', str(self.port)]
        self.save()

    def adb(self, label, arguments, required=True, binary=False):
        if self.serial is None:raise RuntimeError('owned_android_serial_not_ready')
        p = subprocess.run(self.adb_base + ['-s', self.serial, *arguments], cwd=self.private, env=self.env, capture_output=True, timeout=90)
        self.state['commands'].append({'label': label, 'arguments': arguments, 'actual_exit': p.returncode, 'stdout_sha256': hashlib.sha256(p.stdout).hexdigest()})
        if not binary:(self.out / ('android-' + label + '.log')).write_bytes(p.stdout + p.stderr)
        self.save()
        if required and p.returncode:raise RuntimeError('android_' + label + '_failed')
        return p

    def _launch(self, name, argv, input_data=None):
        stream = (self.out / ('android-' + name + '.log')).open('wb')
        p = subprocess.Popen(argv, cwd=self.private, env=self.env, stdin=subprocess.PIPE if input_data is not None else subprocess.DEVNULL, stdout=stream, stderr=subprocess.STDOUT, start_new_session=True)
        fp = process_fingerprint(p.pid)
        entry = {'name':name, 'process':p, 'fingerprint':fp, 'stream':stream}; self.services.append(entry)
        if fp:self.registry.add(ResourceRef('process', self.registry.owner, str(p.pid), fp))
        self.state.setdefault('launched', []).append({'name':name,'pid':p.pid,'fingerprint':fp}); self.save()
        if input_data is not None:
            p.communicate(input_data, timeout=90)
            self.state['avd_create_actual_exit'] = p.returncode; self.save()
            if fp:self.registry.lifecycle(str(p.pid), 'exited', {'exit_code':p.returncode,'gate':name})
            if p.returncode:raise RuntimeError('android_avd_create_failed')
        elif p.poll() is not None:raise RuntimeError('android_' + name + '_early_exit')
        return entry

    def _refresh(self, entry):
        p, old = entry['process'], entry['fingerprint']
        if p.poll() is not None:return None
        current = process_fingerprint(p.pid)
        if old is None or current is None or any(current[k] != old[k] for k in ('pid','uid','start_time','pgid','workdir')) or not Path(current['workdir']).is_relative_to(self.private) or not Path(current['executable_path']).is_relative_to(self.args.android_sdk):
            raise RuntimeError('android_service_identity_unproven')
        if current != old:
            self.registry.add(ResourceRef('process', self.registry.owner, str(p.pid), current)); entry['fingerprint'] = current
            self.state.setdefault('service_fingerprint_transitions', []).append({'name':entry['name'],'from':old,'to':current}); self.save()
        return current

    def start(self):
        avd = self.args.android_sdk / 'cmdline-tools/23.0/bin/avdmanager'
        self._launch('avd-create', [str(avd),'create','avd','--name',self.name,'--package',self.configuration['package'],'--path',str(self.private / 'avds/device.avd'),'--device',self.configuration['device_type']], b'no\n')
        self._launch('adb-server', [*self.adb_base[:1],'--one-device','emulator-' + str(self.console),'-L','tcp:' + str(self.port),'server','nodaemon'])
        self._launch('emulator', [str(self.args.android_sdk / 'emulator/emulator'),'-avd',self.name,'-port',str(self.console),'-no-window','-no-metrics','-no-snapshot','-no-audio','-no-boot-anim','-gpu','software','-memory','2048','-cores','2'])
        end = time.monotonic() + 300; candidate = 'emulator-' + str(self.console)
        while time.monotonic() < end:
            if any(e['process'].poll() is not None for e in self.services if e['name'] in ('emulator','adb-server')):raise RuntimeError('android_boot_service_exited')
            def raw(args):return boot_probe(self.adb_base + args, self.env, self.private)
            listing = raw(['devices']); connected = None
            if candidate.encode() + b'\tdevice' not in listing.stdout:
                connected = raw(['connect','127.0.0.1:' + str(self.console + 1)]); candidate = '127.0.0.1:' + str(self.console + 1)
            ready = raw(['-s',candidate,'shell','getprop','sys.boot_completed'])
            self.state['boot_poll'].append({'devices_exit':listing.returncode,'connect_exit':connected.returncode if connected else None,'ready_exit':ready.returncode,'boot_completed':ready.stdout.decode(errors='replace').strip(),'serial':candidate}); self.save()
            if ready.returncode == 0 and ready.stdout.strip() == b'1':break
            time.sleep(1)
        else:raise RuntimeError('owned_android_boot_timeout')
        self.serial = candidate
        keys = ('ro.boot.qemu.avd_name','ro.kernel.qemu.avd_name','ro.build.version.sdk','ro.product.cpu.abi','ro.kernel.qemu')
        props = {k:self.adb('identity-' + k,['shell','getprop',k]).stdout.decode().strip() for k in keys}
        validate_device(props, self.name, self.configuration)
        if self.serial != 'emulator-' + str(self.console):
            payload = ('host:emulator:' + str(self.console + 1)).encode()
            with socket.create_connection(('127.0.0.1',self.port),timeout=5) as sock:sock.sendall(('%04x' % len(payload)).encode() + payload)
            self.serial = 'emulator-' + str(self.console)
            canonical = {k:self.adb('canonical-' + k,['shell','getprop',k]).stdout.decode().strip() for k in keys}
            validate_device(canonical,self.name,self.configuration)
            raw(['disconnect',candidate])
        end = time.monotonic() + 45; users = ''
        while time.monotonic() < end:
            users = self.adb('user-unlocked',['shell','dumpsys','user']).stdout.decode()
            if 'RUNNING_UNLOCKED' in users or (self.configuration['api']==24 and 'Started users state: {0=3}' in users):break
            time.sleep(.5)
        else:raise RuntimeError('owned_android_user_unlock_timeout')
        self.state.update(serial=self.serial, properties=props, user_unlocked=True, boot_id=self.adb('boot-id',['shell','cat','/proc/sys/kernel/random/boot_id']).stdout.decode().strip())
        self.save()

    def stop_app(self, label):
        self.adb(label + '-stop',['shell','am','force-stop','com.enterpriseim.client'])
        p = self.adb(label + '-stopped',['shell','pidof','com.enterpriseim.client'],required=False)
        if p.returncode != 1 or p.stdout.strip():raise RuntimeError('owned_android_app_not_stopped')

    def installation(self, label):
        text = self.adb(label + '-package',['shell','dumpsys','package','com.enterpriseim.client']).stdout.decode()
        return package_installation(text)

    def verify(self, flutter, step, client):
        step('debug-pub', [*flutter, 'pub', 'get', '--offline', '--enforce-lockfile'], 120)
        def arguments(entry):return [*flutter,'drive','--driver=test_driver/integration_test.dart','--target=integration_test/' + entry,'-d',self.serial,'--no-pub','--no-dds','--keep-app-running','-v']
        step('activity',arguments('mobile_lifecycle_test.dart'))
        if 'All tests passed.' not in (self.out / 'activity.log').read_text():raise RuntimeError('android_activity_report_missing')
        self.stop_app('activity'); self.state['activity_passed'] = True
        namespace = __import__('secrets').token_hex(32); self.state['vault_namespace'] = namespace; phases = []
        for phase in ('write','verify'):
            label = 'vault-' + phase
            try:
                step(label,arguments('secure_storage_test.dart') + ['--dart-define=IM_VAULT_PROBE_ID=' + namespace,'--dart-define=IM_VAULT_PROBE_PHASE=' + phase])
                data = vault_phase((self.out / (label + '.log')).read_text(),phase); data.update(self.installation(label)); phases.append(data)
            except Exception as exc:
                self.state.setdefault('vault_errors',[]).append({'phase':phase,'error':type(exc).__name__ + ':' + str(exc)})
            finally:self.stop_app(label); self.save()
        if len(phases)!=2 or self.state.get('vault_errors'):raise RuntimeError('android_vault_phases_unverified')
        verify_vault_pair(*phases); self.state.update(vault_phases=phases, vault_cleanup_verified=True, vault_distinct_processes=True); self.save()

    def release(self, apk, expected):
        before = self.adb('release-crash-before',['logcat','-b','crash','-d']).stdout
        self.adb('release-install',['install','-r',str(apk)])
        path = self.adb('release-path',['shell','pm','path','com.enterpriseim.client']).stdout.decode().strip().removeprefix('package:')
        if not path.startswith('/data/app/') or not path.endswith('/base.apk'):raise RuntimeError('android_release_path_unverified')
        installed = self.adb('release-installed-bytes',['exec-out','cat',path],binary=True).stdout
        if hashlib.sha256(installed).hexdigest()!=expected:raise RuntimeError('android_release_installed_hash_mismatch')
        self.adb('release-launch',['shell','am','start','-W','-a','android.intent.action.MAIN','-c','android.intent.category.LAUNCHER','-f','0x10200000','-n','com.enterpriseim.client/.MainActivity'])
        pid = self.adb('release-pid',['shell','pidof','com.enterpriseim.client']).stdout.decode().strip()
        if not re.fullmatch(r'\d+',pid):raise RuntimeError('android_release_pid_unverified')
        time.sleep(3)
        (self.out / 'android-release-login.png').write_bytes(self.adb('release-screenshot',['exec-out','screencap','-p'],binary=True).stdout)
        after = self.adb('release-crash-after',['logcat','-b','crash','-d']).stdout
        if not after.startswith(before) or b'FATAL EXCEPTION' in after[len(before):] or b'Fatal signal' in after[len(before):]:raise RuntimeError('android_release_crash_interval')
        self.stop_app('release'); self.state.update(release_installed_sha256=expected,release_pid=int(pid),release_launch_passed=True,new_release_crash_interval_clean=True); self.save()

    def close(self):
        failures=[]
        try:
            emulators=[e for e in self.services if e['name']=='emulator']
            if emulators and emulators[0]['process'].poll() is None and self.serial:
                self._refresh(emulators[0])
                if self.adb('cleanup-owned-avd',['emu','avd','name']).stdout.decode().splitlines()[0]!=self.name:raise RuntimeError('android_cleanup_foreign_avd')
                self.adb('cleanup-uninstall',['uninstall','com.enterpriseim.client'],required=False)
                absent=self.adb('cleanup-package-absent',['shell','pm','list','packages','com.enterpriseim.client']).stdout.strip()
                if absent:raise RuntimeError('android_test_package_remains')
                self.adb('cleanup-emulator-stop',['emu','kill']); emulators[0]['process'].wait(timeout=40)
                if emulators[0]['process'].returncode!=0:raise RuntimeError('android_emulator_nonzero_graceful_exit')
        except Exception as exc:failures.append(type(exc).__name__ + ':' + str(exc))
        for e in reversed(self.services):
            p=e['process']
            try:
                if p.poll() is None:
                    self._refresh(e); os.kill(p.pid,signal.SIGTERM)
                    try:p.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        self._refresh(e); os.kill(p.pid,signal.SIGKILL); p.wait(timeout=5)
                if e['fingerprint']:self.registry.lifecycle(str(p.pid),'exited',{'exit_code':p.returncode,'gate':e['name'],'test_teardown':True})
                self.state['service_exits'].append({'name':e['name'],'pid':p.pid,'actual_exit':p.returncode})
            except Exception as exc:failures.append(e['name'] + ':' + type(exc).__name__ + ':' + str(exc))
            finally:e['stream'].close()
        self.state['cleanup_failures']=failures; self.save(); return failures
