#!/usr/bin/env python3
"""Build an owned iOS simulator or Android APK copy; platform acceptance remains partial."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import plistlib
import re
import secrets
import shutil
import subprocess
import sys
import tempfile
import time

repo = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(repo / 'scripts'))
from integration.commands import run_command
from integration.mobile import preflight, copy_client, xcrun_environment, owned_simulator, simulator_id
from integration.registry import Registry


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def client_sources():
    paths = subprocess.check_output(['git', 'ls-files', '-z', '--', 'clients/enterprise_im'], cwd=repo).split(b'\0')
    paths += subprocess.check_output(['git', 'ls-files', '--others', '--exclude-standard', '-z', '--', 'clients/enterprise_im'], cwd=repo).split(b'\0')
    return {os.fsdecode(p): digest(repo / os.fsdecode(p)) for p in sorted(set(paths)) if p}


class GateFailure(RuntimeError):
    pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--target', default='ios-simulator')
    for name in ('flutter-sdk', 'pub-cache', 'output'):
        parser.add_argument('--' + name, type=Path, required=True)
    for name in ('android-sdk', 'java-home', 'gradle-cache'):
        parser.add_argument('--' + name, type=Path)
    parser.add_argument('--developer-dir', type=Path, default=Path('/Applications/Xcode.app/Contents/Developer'))
    parser.add_argument('--verify-simulator', action='store_true')
    parser.add_argument('--verify-mobile-boundaries', action='store_true')
    parser.add_argument('--verify-mobile-resume', action='store_true')
    parser.add_argument('--simulator-runtime')
    parser.add_argument('--simulator-device-type')
    args = parser.parse_args()
    if not args.output.is_absolute():
        parser.error('output must be absolute')
    args.output.mkdir(mode=0o700, parents=False, exist_ok=False)
    out = args.output
    try:
        if args.verify_mobile_resume and not args.verify_simulator:
            raise ValueError('mobile_resume_requires_simulator')
        if args.verify_mobile_boundaries and (args.target != 'android' or not args.verify_simulator):
            raise ValueError('mobile_boundaries_require_android_simulator')
        if args.target == 'android':
            from integration.android_build import preflight as android_preflight
            report = android_preflight(repo, args)
        else:
            report = preflight(repo, args)
    except Exception as exc:
        report = {'status': 'not_run', 'target': args.target, 'fixtures_created': False, 'blocked_reason': str(exc) if isinstance(exc, ValueError) else type(exc).__name__}
    (out / 'preflight.json').write_text(json.dumps(report, indent=2) + '\n')
    if report['status'] != 'ready':
        print(json.dumps(report)); return 2
    if args.target == 'android':
        from integration.android_build import run as android_run
        return android_run(repo, args, out, report, client_sources)
    commit = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=repo, text=True).strip()
    owner = secrets.token_hex(16)
    root = Path(tempfile.mkdtemp(prefix='im-mobile-', dir='/private/tmp')).resolve()
    root.chmod(0o700)
    registry = Registry(root / 'registry.jsonl', owner, commit, recover_containers=False)
    private = root / owner
    client = private / 'client'
    result = {'status': 'failed', 'target': args.target, 'source_commit': commit, 'owner': owner, 'commands': [], 'platform_acceptance': 'partial', 'scope': 'owned Debug iOS simulator build; optional active-scene and two-process Keychain checks only', 'native_leaf_os_exit_independently_observed': False, 'implementation_sha256': {path: digest(repo / path) for path in ('scripts/build-client-mobile.py', 'scripts/integration/mobile.py', 'scripts/integration/mobile_resume.py', 'scripts/integration/registry.py', 'scripts/integration/commands.py')}}
    env = None; simulator = None; runtime = args.simulator_runtime
    simulator_name = 'IM-Mobile-' + owner
    simulator_reserved = False
    code = 1

    def save():
        (out / 'result.json').write_text(json.dumps(result, ensure_ascii=False, indent=2) + '\n')

    def simctl(arguments, timeout=60):
        proc = subprocess.run(['/usr/bin/xcrun', 'simctl', *arguments], env=env, capture_output=True, text=True, timeout=timeout)
        result.setdefault('simctl_commands', []).append({'arguments': arguments, 'actual_exit': proc.returncode})
        save()
        if proc.returncode:
            raise GateFailure('simctl_' + arguments[0] + '_failed')
        return proc.stdout

    def inventory():
        return json.loads(simctl(['list', 'devices', '--json']))

    def step(name, arguments, required=True, timeout=600):
        command = [str(args.flutter_sdk / 'bin/cache/dart-sdk/bin/dart'), str(args.flutter_sdk / 'bin/cache/flutter_tools.snapshot'), *arguments]
        event = run_command(name, 'mobile', commit, command, client, env, out / (name + '.log'), time.monotonic() + timeout, registry)
        item = {'name': name, 'argv': command, 'actual_exit': event.exit_code, 'failures': event.failures, 'stdout_sha256': digest(event.log) if event.log.exists() else None, 'stderr_sha256': digest(str(event.log) + '.stderr') if Path(str(event.log) + '.stderr').exists() else None}
        result['commands'].append(item); save()
        if required and (event.exit_code or event.failures):
            raise GateFailure(name + '_failed')
        return item

    try:
        result['source_sha256'] = client_sources()
        copy_client(repo, client, result['source_sha256'])
        env = xcrun_environment(private, args.developer_dir, args.pub_cache)
        result['wrapper_sha256'] = digest(private / 'tools/xcrun')
        result['sdk_commit'] = report['sdk_commit']
        step('pub', ['pub', 'get', '--offline', '--enforce-lockfile'], timeout=120)
        if digest(client / 'pubspec.lock') != result['source_sha256']['clients/enterprise_im/pubspec.lock']:
            raise GateFailure('pubspec_lock_changed')
        if args.verify_simulator:
            step('shared', ['test', '--no-pub'])
            step('analysis', ['analyze', '--fatal-infos', '--no-pub'])
            if owned_simulator(inventory(), runtime, simulator_name) is not None:
                raise GateFailure('reserved_simulator_name_exists')
            simulator_reserved = True
            result['simulator'] = {'name': simulator_name, 'runtime': runtime, 'device_type': args.simulator_device_type, 'udid': None}
            save()
            simulator = simulator_id(simctl(['create', simulator_name, args.simulator_device_type, runtime]).strip())
            result['simulator']['udid'] = simulator; save()
            row = owned_simulator(inventory(), runtime, simulator_name, simulator)
            if not row or row.get('deviceTypeIdentifier') != args.simulator_device_type:
                raise GateFailure('created_simulator_identity_mismatch')
            simctl(['boot', simulator])
            simctl(['bootstatus', simulator, '-b'], timeout=240)
            activity = step('activity', ['test', 'integration_test/mobile_lifecycle_test.dart', '-d', simulator, '--no-pub'])
            if 'All tests passed!' not in (out / 'activity.log').read_text():
                raise GateFailure('activity_test_report_missing')
            namespace = secrets.token_hex(32)
            result['vault_namespace'] = namespace; save()
            phases = []
            for phase in ('write', 'verify'):
                item = step('vault-' + phase, ['test', 'integration_test/secure_storage_test.dart', '-d', simulator, '--no-pub', '--dart-define=IM_VAULT_PROBE_ID=' + namespace, '--dart-define=IM_VAULT_PROBE_PHASE=' + phase], required=False)
                content = (out / ('vault-' + phase + '.log')).read_text()
                pids = re.findall(r'NATIVE_VAULT_PROBE phase=' + phase + r' runtime_pid=(\d+)', content)
                item['runtime_pids'] = pids
                phases.append(item)
            result['vault_distinct_processes'] = len({pid for item in phases for pid in item['runtime_pids']}) == 2
            result['vault_cleanup_verified'] = all(item['actual_exit'] == 0 and not item['failures'] and len(item['runtime_pids']) == 1 for item in phases) and result['vault_distinct_processes']
            if not result['vault_cleanup_verified']:
                raise GateFailure('vault_restart_or_cleanup_unverified')
        if args.verify_mobile_resume:
            from integration.mobile_resume import run_ios
            run_ios(simulator, private, out, env, step, result, save)
        step('build', ['build', 'ios', '--simulator', '--debug', '--no-pub'])
        app = client / 'build/ios/iphonesimulator/Runner.app'
        info = plistlib.loads((app / 'Info.plist').read_bytes())
        if info.get('CFBundleIdentifier') != 'com.enterpriseim.client' or info.get('MinimumOSVersion') != '15.0' or info.get('DTPlatformName') != 'iphonesimulator':
            raise GateFailure('built_app_platform_or_baseline_mismatch')
        manifest = {str(path.relative_to(app)): digest(path) for path in sorted(app.rglob('*')) if path.is_file()}
        if any('owned_backend.json' in name for name in manifest):
            raise GateFailure('unexpected_fixture_credentials_asset')
        (out / 'bundle-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
        signing = subprocess.run(['/usr/bin/codesign', '-dvv', str(app)], capture_output=True, text=True)
        if signing.returncode:
            raise GateFailure('signature_inspection_failed')
        result['bundle'] = {'file_count': len(manifest), 'manifest_sha256': digest(out / 'bundle-manifest.json'), 'executable_sha256': digest(app / info['CFBundleExecutable']), 'minimum_os_version': info['MinimumOSVersion'], 'codesign_actual_exit': signing.returncode, 'signature': signing.stderr}
        archive = out / 'ios-simulator-debug.zip'
        archived = subprocess.run(['/usr/bin/ditto', '-c', '-k', '--keepParent', str(app), str(archive)], capture_output=True, timeout=120)
        result['archive_actual_exit'] = archived.returncode
        if archived.returncode:
            raise GateFailure('archive_failed')
        result['archive_sha256'] = digest(archive)
        result['status'] = 'passed'; code = 0
    except BaseException as exc:
        result['error'] = type(exc).__name__ + ':' + str(exc)
        print(result['error'], flush=True)
    finally:
        failures = []
        if simulator_reserved:
            try:
                row = owned_simulator(inventory(), runtime, simulator_name, simulator)
                if row:
                    if row.get('deviceTypeIdentifier') != args.simulator_device_type:
                        raise GateFailure('cleanup_simulator_type_mismatch')
                    simulator = row['udid']
                    result.setdefault('simulator', {})['udid'] = simulator
                    if row.get('state') == 'Booted':
                        simctl(['shutdown', simulator])
                    simctl(['delete', simulator])
                    if owned_simulator(inventory(), runtime, simulator_name, simulator) is not None:
                        raise GateFailure('simulator_still_present')
                result['owned_simulator_removed'] = True
            except BaseException as exc:
                failures.append('simulator:' + type(exc).__name__ + ':' + str(exc))
        process_cleanup = registry.cleanup(time.monotonic() + 45)
        failures.extend(process_cleanup.failures)
        (out / 'registry.jsonl').write_bytes(registry.path.read_bytes())
        if 'source_sha256' in result:
            result['source_unchanged'] = result['source_sha256'] == client_sources()
            if not result['source_unchanged']:
                failures.append('source_changed')
        result['implementation_unchanged'] = all(digest(repo / path) == value for path, value in result['implementation_sha256'].items())
        if not result['implementation_unchanged']:
            failures.append('implementation_changed')
        result['cleanup'] = {'removed': not failures, 'failures': failures}
        if not failures:
            shutil.rmtree(root); result['private_root_removed'] = not root.exists()
        else:
            result['recovery_root'] = str(root); result['private_root_removed'] = False
            result['status'] = 'failed'; code = 1
        result['runner_exit'] = code
        save()
    print(json.dumps({key: result.get(key) for key in ('status', 'runner_exit', 'cleanup', 'private_root_removed', 'owned_simulator_removed')}), flush=True)
    return code


if __name__ == '__main__':
    sys.exit(main())
