"""Owned Android APK builds using explicitly prepared, fixed local tools."""
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import secrets
import shutil
import subprocess
import tempfile
import time
import zipfile


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def android_environment(private, args):
    private = Path(private)
    for name in ('home', 'java-user', 'java-user/.android', 'tmp'):
        (private / name).mkdir(mode=0o700, parents=True, exist_ok=True)
    env = {key: os.environ[key] for key in ('LANG', 'TZ') if key in os.environ}
    env.update(HOME=str(private / 'home'), JAVA_HOME=str(args.java_home), JAVA_TOOL_OPTIONS='-Duser.home=' + str(private / 'java-user'), ANDROID_HOME=str(args.android_sdk), ANDROID_SDK_ROOT=str(args.android_sdk), ANDROID_USER_HOME=str(private / 'java-user/.android'), GRADLE_USER_HOME=str(args.gradle_cache), GRADLE_OPTS='-Dorg.gradle.daemon=false -Dorg.gradle.java.installations.auto-download=false', PUB_CACHE=str(args.pub_cache), TMPDIR=str(private / 'tmp'), CI='true', FLUTTER_SUPPRESS_ANALYTICS='true', ADB_MDNS_AUTO_CONNECT='0', ADB_USB='0', ADB_EMU='0', ADB_MDNS='0')
    env['PATH'] = str(args.java_home / 'bin') + ':/usr/bin:/bin:/usr/sbin:/sbin'
    return env


def jdk_release_metadata(text):
    expected = {'IMPLEMENTOR': 'Eclipse Adoptium', 'JAVA_VERSION': '21.0.12.1', 'JAVA_RUNTIME_VERSION': '21.0.12.1+1-LTS', 'OS_ARCH': 'aarch64', 'OS_NAME': 'Darwin'}
    for key, value in expected.items():
        if re.findall(r'^' + key + r'="([^"\n]+)"$', text, re.M) != [value]:
            raise ValueError('fixed_jdk_version_mismatch')
    return {'version': expected['JAVA_RUNTIME_VERSION'], 'architecture': expected['OS_ARCH'], 'vendor': expected['IMPLEMENTOR']}


def preflight(repo, args):
    simulator = None
    if args.verify_simulator:
        from .android_simulator import simulator_preflight
        if not args.simulator_runtime or not args.simulator_device_type:
            raise ValueError('explicit_android_simulator_runtime_and_type_required')
        if args.android_sdk is not None:
            simulator = simulator_preflight(args.android_sdk, args.simulator_runtime, args.simulator_device_type)
    elif args.simulator_runtime or args.simulator_device_type:
        raise ValueError('android_simulator_verification_flag_required')
    paths = (args.android_sdk, args.java_home, args.gradle_cache)
    if any(p is None for p in paths):
        raise ValueError('prepared_android_paths_required')
    if any(not p.is_absolute() for p in (*paths, args.flutter_sdk, args.pub_cache, args.output)):
        raise ValueError('mobile_paths_must_be_absolute')
    if platform.system() != 'Darwin' or platform.machine() != 'arm64':
        raise ValueError('android_builder_requires_macos_arm64')
    lock = json.loads((Path(repo) / 'docs/verification/p5-02-toolchain-lock.json').read_text())
    head = subprocess.check_output(['git', '-C', str(args.flutter_sdk), 'rev-parse', 'HEAD'], text=True).strip()
    changes = subprocess.check_output(['git', '-C', str(args.flutter_sdk), 'status', '--porcelain', '--untracked-files=no'])
    if head != lock['framework_commit'] or changes:
        raise ValueError('fixed_sdk_source_mismatch')
    dart = args.flutter_sdk / 'bin/cache/dart-sdk/bin/dart'
    if not dart.is_file() or digest(dart) != lock['dart_executable_sha256']:
        raise ValueError('fixed_dart_binary_mismatch')
    if not args.pub_cache.is_dir() or not args.gradle_cache.is_dir():
        raise ValueError('prepared_android_caches_missing')
    required = [dart, Path(repo) / 'docs/verification/p5-02-toolchain-lock.json', args.flutter_sdk / 'bin/cache/flutter_tools.snapshot', args.java_home / 'bin/java', args.java_home / 'release', args.android_sdk / 'platforms/android-36/android.jar', args.android_sdk / 'platforms/android-36/source.properties', args.android_sdk / 'build-tools/36.0.0/aapt2', args.android_sdk / 'build-tools/36.0.0/apksigner', args.android_sdk / 'build-tools/36.0.0/source.properties', args.android_sdk / 'ndk/28.2.13676358/source.properties', args.android_sdk / 'licenses/android-sdk-license']
    for abi in ('android-arm-release', 'android-arm64-release', 'android-x64-release'):
        required.append(args.flutter_sdk / 'bin/cache/artifacts/engine' / abi / 'flutter.jar')
    if any(not p.is_file() for p in required):
        raise ValueError('prepared_android_tools_missing')
    jdk = jdk_release_metadata((args.java_home / 'release').read_text())
    for path, expected in [(args.android_sdk / 'build-tools/36.0.0/source.properties', '36.0.0'), (args.android_sdk / 'ndk/28.2.13676358/source.properties', '28.2.13676358')]:
        if re.findall(r'^Pkg.Revision\s*=\s*([^\r\n]+)', path.read_text(), re.M) != [expected]:
            raise ValueError('fixed_android_tool_version_mismatch')
    wrapper = (Path(repo) / 'clients/enterprise_im/android/gradle/wrapper/gradle-wrapper.properties').read_text()
    if 'distributionSha256Sum=' + lock['android']['gradle_distribution_sha256'] not in wrapper:
        raise ValueError('fixed_gradle_checksum_mismatch')
    java = subprocess.run([str(args.java_home / 'bin/java'), '--version'], env={'PATH': '/usr/bin:/bin'}, capture_output=True, text=True, timeout=30)
    if java.returncode or '21.0.12' not in java.stdout:
        raise ValueError('prepared_java_cannot_run')
    if simulator:
        required.extend(Path(p) for p in simulator['tool_sha256'])
    return {'simulator': simulator, 'status': 'ready', 'target': 'android', 'sdk_commit': head, 'fixtures_created': False, 'tool_sha256': {str(p): digest(p) for p in required}, 'java_actual_exit': java.returncode, 'java_version': java.stdout, 'jdk': jdk, 'tools_installed_or_licenses_accepted': False}


def sdk_source_is_fixed(sdk, commit):
    head = subprocess.check_output(['git', '-C', str(sdk), 'rev-parse', 'HEAD'], text=True).strip()
    changes = subprocess.check_output(['git', '-C', str(sdk), 'status', '--porcelain', '--untracked-files=no'])
    return head == commit and not changes


def apk_metadata(badging, signature):
    def field(pattern):
        values = re.findall(pattern, badging, re.M)
        if len(values) != 1:
            raise ValueError('apk_metadata_missing_or_ambiguous')
        return values[0]
    package = field(r"^package: name='([^']+)'")
    minimum = int(field(r"^minSdkVersion:'(\d+)'$"))
    target = int(field(r"^targetSdkVersion:'(\d+)'$"))
    compile_api = int(field(r"\bcompileSdkVersion='(\d+)'"))
    abis = sorted(re.findall(r"'([^']+)'", field(r'^native-code: (.+)$')))
    if package != 'com.enterpriseim.client' or (minimum, target, compile_api) != (24, 36, 36) or abis != ['arm64-v8a', 'armeabi-v7a', 'x86_64'] or 'application-debuggable' in badging:
        raise ValueError('apk_platform_or_baseline_mismatch')
    if not re.search(r'^Verifies$', signature, re.M) or not re.search(r'^Verified using v2 scheme .*: true$', signature, re.M) or not re.search(r'^Number of signers: 1$', signature, re.M):
        raise ValueError('apk_signature_not_verified')
    certificates = re.findall(r'^Signer #1 certificate SHA-256 digest: ([a-fA-F0-9]{64})$', signature, re.M)
    if len(certificates) != 1 or not re.search(r'^Signer #1 certificate DN: .*\bCN=Android Debug(?:,|$)', signature, re.M):
        raise ValueError('apk_unselected_signing_identity')
    return {'package': package, 'minimum_api': minimum, 'target_api': target, 'compile_api': compile_api, 'abis': abis, 'signature_kind': 'temporary_debug_certificate', 'certificate_sha256': certificates[0].lower(), 'distribution_acceptance': False}


def archive_manifest(apk):
    forbidden = (b'Lcom/enterpriseim/client/tests/', b'Lcom/enterpriseim/exportfixture/', b'FaultDocumentsProvider', b'RecoveryInstrumentation', b'ReauthorizationLifecycleInstrumentation', b'SessionInstrumentation')
    with zipfile.ZipFile(apk) as archive:
        names = archive.namelist()
        if len(names) != len(set(names)) or 'AndroidManifest.xml' not in names or not any(re.fullmatch(r'classes\d*\.dex', n) for n in names):
            raise ValueError('apk_archive_incomplete_or_ambiguous')
        manifest = {}
        for name in names:
            if 'owned_backend' in name or 'fixture_credentials' in name or name.endswith(('.jks', '.keystore', '.pem')):
                raise ValueError('unexpected_fixture_or_key_asset')
            data = archive.read(name)
            if re.fullmatch(r'classes\d*\.dex', name) and any(marker in data for marker in forbidden):
                raise ValueError('unexpected_test_fixture_class')
            manifest[name] = {'size': len(data), 'sha256': hashlib.sha256(data).hexdigest()}
    return manifest


def run(repo, args, out, report, source_inventory):
    from .commands import run_command
    from .mobile import copy_client
    from .registry import Registry
    commit = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=repo, text=True).strip()
    owner = secrets.token_hex(16)
    root = Path(tempfile.mkdtemp(prefix='im-mobile-android-', dir='/private/tmp')).resolve()
    root.chmod(0o700)
    registry = Registry(root / 'registry.jsonl', owner, commit, recover_containers=False)
    private = root / owner; client = private / 'client'
    implementation = ('scripts/build-client-mobile.py', 'scripts/integration/android_build.py', 'scripts/integration/android_simulator.py', 'scripts/integration/android_mobile_boundary.py', 'scripts/integration/mobile_resume.py', 'scripts/integration/mobile.py', 'scripts/integration/registry.py', 'scripts/integration/commands.py')
    result = {'status': 'failed', 'target': 'android', 'source_commit': commit, 'owner': owner, 'commands': [], 'platform_acceptance': 'partial', 'scope': 'owned Android Release APK build; optional new owned simulator active-scene, retained two-process vault, normal Release launch; opt-in actual cold/warm OAuth callback and HOME/MAIN boundary; no backend, physical-device or CI execution', 'root': str(root), 'implementation_sha256': {p: digest(repo / p) for p in implementation}, 'signing': 'fresh private Android Debug test certificate; not distribution signing'}
    code = 1; simulator = None
    def save():
        (out / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    try:
        result['source_sha256'] = source_inventory()
        copy_client(repo, client, result['source_sha256'])
        env = android_environment(private, args)
        def step(name, argv, timeout=1200):
            event = run_command(name, 'mobile-android', commit, argv, client, env, out / (name + '.log'), time.monotonic() + timeout, registry)
            result['commands'].append({'name': name, 'argv': argv, 'actual_exit': event.exit_code, 'failures': event.failures, 'stdout_sha256': digest(event.log), 'stderr_sha256': digest(str(event.log) + '.stderr')}); save()
            if event.exit_code or event.failures:
                raise RuntimeError(name + '_failed')
        flutter = [str(args.flutter_sdk / 'bin/cache/dart-sdk/bin/dart'), str(args.flutter_sdk / 'bin/cache/flutter_tools.snapshot')]
        step('pub', flutter + ['pub', 'get', '--offline', '--enforce-lockfile'], 120)
        if digest(client / 'pubspec.lock') != result['source_sha256']['clients/enterprise_im/pubspec.lock']:
            raise ValueError('pubspec_lock_changed')
        step('build', flutter + ['build', 'apk', '--release'], 1800)
        if digest(client / 'pubspec.lock') != result['source_sha256']['clients/enterprise_im/pubspec.lock']:
            raise ValueError('pubspec_lock_changed_during_build')
        apk = client / 'build/app/outputs/flutter-apk/app-release.apk'
        step('badging', [str(args.android_sdk / 'build-tools/36.0.0/aapt2'), 'dump', 'badging', str(apk)], 30)
        step('signature', [str(args.android_sdk / 'build-tools/36.0.0/apksigner'), 'verify', '--verbose', '--print-certs', str(apk)], 30)
        result['apk'] = apk_metadata((out / 'badging.log').read_text(), (out / 'signature.log').read_text())
        manifest = archive_manifest(apk)
        (out / 'apk-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
        destination = out / 'android-release-test.apk'; shutil.copyfile(apk, destination); destination.chmod(0o600)
        result['apk'].update(path=destination.name, sha256=digest(destination), size=destination.stat().st_size, manifest_sha256=digest(out / 'apk-manifest.json'), test_fixture_classes_absent=True)
        if args.verify_simulator:
            from .android_simulator import AndroidSimulator
            step('shared', flutter + ['test', '--no-pub'])
            step('analysis', flutter + ['analyze', '--fatal-infos', '--no-pub'])
            result['simulator'] = {}
            simulator = AndroidSimulator(args, private, out, owner, registry, env, report['simulator'], result['simulator'], save)
            simulator.start(); simulator.verify(flutter, step, client)
            if args.verify_mobile_boundaries:
                from .android_mobile_boundary import run as boundary_run
                boundary_run(simulator, flutter, step, client)
            if getattr(args, 'verify_mobile_resume', False):
                from .mobile_resume import run_android
                run_android(simulator, flutter, step, client)
            simulator.release(destination, result['apk']['sha256'])
            if digest(client / 'pubspec.lock') != result['source_sha256']['clients/enterprise_im/pubspec.lock']:
                raise ValueError('pubspec_lock_changed_during_native_verification')
        result['status'] = 'passed'; code = 0
    except BaseException as exc:
        result['error'] = type(exc).__name__ + ':' + str(exc)
        print(result['error'], flush=True)
    finally:
        failures = simulator.close() if simulator else []
        cleanup = registry.cleanup(time.monotonic() + 45)
        failures += list(cleanup.failures)
        (out / 'registry.jsonl').write_bytes(registry.path.read_bytes())
        audits = {'sdk_source': lambda: sdk_source_is_fixed(args.flutter_sdk, report['sdk_commit']),
                  'source': lambda: result.get('source_sha256') == source_inventory(),
                  'implementation': lambda: all(digest(repo / p) == h for p, h in result['implementation_sha256'].items()),
                  'tool_inputs': lambda: all(Path(p).is_file() and digest(p) == h for p, h in report['tool_sha256'].items())}
        for name, audit in audits.items():
            try:
                result[name + '_unchanged'] = audit()
            except Exception as exc:
                result[name + '_unchanged'] = False
                failures.append(name + '_audit:' + type(exc).__name__)
        for name in ('sdk_source', 'source', 'implementation', 'tool_inputs'):
            if not result[name + '_unchanged']:
                failures.append(name + '_changed')
        result['cleanup'] = {'removed': not failures, 'failures': failures}
        result['temporary_signing_key_removed'] = False
        if not failures:
            shutil.rmtree(root)
            result['private_root_removed'] = not root.exists()
            result['temporary_signing_key_removed'] = result['private_root_removed']
        else:
            result['recovery_root'] = str(root); result['private_root_removed'] = False
        if failures:
            code = 1; result['status'] = 'failed'
        result['runner_exit'] = code; save()
    print(json.dumps({key: result.get(key) for key in ('status', 'runner_exit', 'cleanup', 'private_root_removed')}), flush=True)
    return code
