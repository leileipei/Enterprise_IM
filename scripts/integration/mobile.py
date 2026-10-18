"""Owned iOS simulator build helpers; no global Xcode or account mutations."""
import hashlib
import os
from pathlib import Path
import shlex
import shutil
import stat


def simulator_choice(inventory, runtime, device_type):
    matches = [row for row in inventory.get('runtimes', []) if row.get('identifier') == runtime and row.get('isAvailable') is True]
    if len(matches) != 1:
        raise ValueError('simulator_runtime_unavailable')
    if not any(row.get('identifier') == device_type for row in matches[0].get('supportedDeviceTypes', [])):
        raise ValueError('simulator_device_type_unavailable')
    return {'runtime': runtime, 'device_type': device_type}


def owned_simulator(inventory, runtime, name, udid=None):
    rows = inventory['devices'].get(runtime, [])
    if udid:
        selected = [row for row in rows if row.get('udid') == udid]
        if selected and selected[0].get('name') != name:
            raise ValueError('simulator_identity_mismatch')
        if not selected and any(row.get('name') == name for row in rows):
            raise ValueError('simulator_identity_mismatch')
    else:
        selected = [row for row in rows if row.get('name') == name]
    if len(selected) > 1:
        raise ValueError('simulator_identity_ambiguous')
    return selected[0] if selected else None


def copy_client(repo, destination, sources):
    repo = Path(repo).resolve()
    destination = Path(destination)
    if destination.exists() or destination.is_symlink():
        raise ValueError('client_destination_reused')
    contents = {}
    for relative, expected in sources.items():
        path = Path(relative)
        try:
            local = path.relative_to('clients/enterprise_im')
        except ValueError:
            raise ValueError('client_source_path_invalid')
        if path.is_absolute() or '..' in path.parts:
            raise ValueError('client_source_path_invalid')
        source = repo / path
        if any(part.is_symlink() for part in [source, *source.parents] if part != repo and part.is_relative_to(repo)) or not stat.S_ISREG(source.lstat().st_mode):
            raise ValueError('client_source_not_regular')
        data = source.read_bytes()
        if hashlib.sha256(data).hexdigest() != expected:
            raise ValueError('client_source_changed')
        contents[local] = data
    if not contents:
        raise ValueError('client_source_empty')
    destination.mkdir(mode=0o700, parents=True)
    for relative, data in contents.items():
        target = destination / relative
        target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        target.write_bytes(data)
        target.chmod(0o600)


def xcrun_environment(private, developer, pub_cache):
    private = Path(private)
    wrapper = private / 'tools'
    wrapper.mkdir(mode=0o700)
    script = wrapper / 'xcrun'
    script.write_text('#!/bin/sh\nexport DEVELOPER_DIR=' + shlex.quote(str(developer)) + '\nexec /usr/bin/xcrun "$@"\n')
    script.chmod(0o700)
    env = {key: os.environ[key] for key in ('HOME', 'LANG', 'TZ') if key in os.environ}
    env.update(PATH=str(wrapper) + ':/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin', DEVELOPER_DIR=str(developer), PUB_CACHE=str(pub_cache), TMPDIR='/private/tmp', CI='true', FLUTTER_SUPPRESS_ANALYTICS='true')
    return env


def ios_tool_inputs(sdk):
    """Hash the executed Flutter tool and the complete Debug iOS engine cache."""
    sdk=Path(sdk);snapshot=sdk/'bin/cache/flutter_tools.snapshot'
    engine=sdk/'bin/cache/artifacts/engine/ios'
    if not snapshot.is_file() or snapshot.is_symlink() or not engine.is_dir():
        raise ValueError('prepared_ios_cache_missing')
    files={}
    for path in sorted(engine.rglob('*')):
        if path.is_symlink():raise ValueError('linked_ios_cache_input')
        if path.is_file():files[str(path)]=hashlib.sha256(path.read_bytes()).hexdigest()
    if not files:raise ValueError('prepared_ios_cache_missing')
    return {'tool_sha256':{str(snapshot):hashlib.sha256(snapshot.read_bytes()).hexdigest()},'ios_engine_sha256':files}


def preflight(repo, args):
    import json
    import platform
    import subprocess
    import sys
    if args.target != 'ios-simulator':
        raise ValueError('mobile_target_not_prepared')
    if any(not path.is_absolute() for path in (args.flutter_sdk, args.pub_cache, args.output, args.developer_dir)):
        raise ValueError('mobile_paths_must_be_absolute')
    if sys.platform != 'darwin' or platform.machine() != 'arm64':
        raise ValueError('ios_builder_requires_macos_arm64')
    lock = json.loads((Path(repo) / 'docs/verification/p5-02-toolchain-lock.json').read_text())
    if lock['ios']['approved_minimum'] != '15':
        raise ValueError('ios_baseline_unapproved')
    head = subprocess.check_output(['git', '-C', str(args.flutter_sdk), 'rev-parse', 'HEAD'], text=True).strip()
    changes = subprocess.check_output(['git', '-C', str(args.flutter_sdk), 'status', '--porcelain', '--untracked-files=no'])
    if head != 'e8113bf45620cbeb8aff64947ee4c93e16adb4cf' or changes:
        raise ValueError('fixed_sdk_source_mismatch')
    dart = args.flutter_sdk / 'bin/cache/dart-sdk/bin/dart'
    if not dart.is_file() or hashlib.sha256(dart.read_bytes()).hexdigest() != lock['dart_executable_sha256']:
        raise ValueError('fixed_dart_binary_mismatch')
    if not args.pub_cache.is_dir() or not (args.flutter_sdk / 'bin/cache/artifacts/engine/ios/Flutter.xcframework').is_dir():
        raise ValueError('prepared_ios_cache_missing')
    if not (args.developer_dir / 'usr/bin/xcodebuild').is_file():
        raise ValueError('prepared_xcode_missing')
    if args.verify_simulator and (not args.simulator_runtime or not args.simulator_device_type):
        raise ValueError('explicit_simulator_runtime_and_type_required')
    env = dict(os.environ, DEVELOPER_DIR=str(args.developer_dir))
    first_launch = subprocess.run(['/usr/bin/xcodebuild', '-checkFirstLaunchStatus'], env=env, capture_output=True, timeout=30)
    if first_launch.returncode:
        raise ValueError('operator_xcode_first_launch_required')
    result = {'status': 'ready', 'target': args.target, 'sdk_commit': head, 'xcode_first_launch_actual_exit': first_launch.returncode, 'fixtures_created': False, 'tool_sha256': {str(dart): hashlib.sha256(dart.read_bytes()).hexdigest()}}
    inputs=ios_tool_inputs(args.flutter_sdk)
    result['tool_sha256'].update(inputs['tool_sha256'])
    result['ios_engine_sha256']=inputs['ios_engine_sha256']
    if args.verify_simulator:
        inventory = subprocess.check_output(['/usr/bin/xcrun', 'simctl', 'list', 'runtimes', '--json'], env=env, text=True, timeout=45)
        result['simulator'] = simulator_choice(json.loads(inventory), args.simulator_runtime, args.simulator_device_type)
    return result


def simulator_id(value):
    import re
    if not re.fullmatch(r'[A-Fa-f0-9]{8}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{12}', value):
        raise ValueError('invalid_created_simulator_id')
    return value.upper()


def archive_ios_bundle(app, archive):
    import subprocess
    return subprocess.run(['/usr/bin/ditto','-c','-k','--norsrc','--noextattr','--noqtn','--noacl','--keepParent',str(app),str(archive)],capture_output=True,timeout=120)
