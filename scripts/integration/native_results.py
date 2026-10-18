"""Offline evidence contracts: no platform execution or producer attestation."""
from collections import Counter
import hashlib
import json
from pathlib import Path, PurePosixPath, PureWindowsPath
import re
import subprocess

PLATFORMS = {'macos': 'arm64', 'windows': 'x64', 'android': 'arm64-v8a', 'ios': 'arm64'}
TARGETS = {'macos': {'macos_app'}, 'windows': {'windows_directory'}, 'android': {'android_apk'}, 'ios': {'ios_device', 'ios_simulator'}}
CASES = ('build', 'launch', 'oidc', 'text', 'files', 'receipts', 'storage', 'lifecycle', 'recovery', 'groups', 'revocation', 'retention')
BUSINESS = {'oidc', 'text', 'files', 'receipts', 'recovery', 'groups', 'revocation', 'retention', 'physical', 'simulator'}
CROSS_CHECKS = {'send', 'delivered', 'read', 'disconnect', 'restart', 'lost_ack', 'receipt_retry', 'logout', 'revocation', 'permission_change', 'retention_change', 'cross_organization_group'}
SHA = re.compile(r'[a-f0-9]{64}')
COMMIT = re.compile(r'[a-f0-9]{40}')


class EvidenceError(ValueError):
    pass


class RunFailed(EvidenceError):
    pass


def require(condition, code):
    if not condition:
        raise EvidenceError(code)


def _is_hash(value):
    return isinstance(value, str) and SHA.fullmatch(value) is not None


def _pairs(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, 'duplicate_json_key')
        result[key] = value
    return result


def read_json(path):
    try:
        require(Path(path).stat().st_size <= 32 * 1024 * 1024, 'json_too_large')
        value = json.loads(Path(path).read_text(encoding='utf-8'), object_pairs_hook=_pairs,
                           parse_constant=lambda _: (_ for _ in ()).throw(EvidenceError('nonfinite_json')))
        require(isinstance(value, dict), 'json_object_required')
        return value
    except EvidenceError:
        raise
    except (OSError, ValueError, UnicodeError):
        raise EvidenceError('json_unreadable') from None


def digest(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(block)
    return h.hexdigest()


def _reference(repo, ref):
    require(isinstance(ref, dict) and set(ref) == {'path', 'sha256'}, 'file_reference_required')
    name, expected = ref['path'], ref['sha256']
    require(isinstance(name, str) and name and '\\' not in name and not any(ord(c) < 32 for c in name), 'invalid_evidence_path')
    rel = PurePosixPath(name)
    require(not rel.is_absolute() and not PureWindowsPath(name).drive and not PureWindowsPath(name).root
            and rel.as_posix() == name and not any(p in ('..', '.', '') for p in name.split('/'))
            and ':' not in name, 'invalid_evidence_path')
    require(_is_hash(expected), 'invalid_evidence_hash')
    target = repo
    for part in rel.parts:
        target = target / part
        require(not target.is_symlink(), 'linked_evidence_rejected')
    require(target.resolve().is_relative_to(repo.resolve()), 'invalid_evidence_path')
    require(target.is_file(), 'evidence_missing')
    require(digest(target) == expected, 'evidence_hash_mismatch')
    return target


def _safe_summary(proof, ref):
    commands = proof.get('commands')
    exits = [c.get('actual_exit') if isinstance(c, dict) and type(c.get('actual_exit')) is int else None
             for c in commands[:200]] if isinstance(commands, list) else []
    tests = proof.get('tests')
    counts = {k: tests.get(k) if isinstance(tests, dict) and type(tests.get(k)) is int else None
              for k in ('passed', 'failures', 'errors', 'skips')}
    cleanup = proof.get('cleanup')
    removed = cleanup.get('removed') if isinstance(cleanup, dict) and type(cleanup.get('removed')) is bool else None
    failures = cleanup.get('failures') if isinstance(cleanup, dict) else None
    return {'evidence': dict(ref), 'actual_exits': exits, 'test_counts': counts,
            'count_basis': proof.get('count_basis') if proof.get('count_basis') in ('runner_checks', 'framework_tests') else 'unspecified',
            'cleanup': {'removed': removed, 'failure_count': len(failures) if isinstance(failures, list) else None},
            'cleanup_verified': removed is True and failures == []}


def _commands(repo, proof):
    commands = proof.get('commands')
    require(isinstance(commands, list) and 0 < len(commands) <= 200, 'actual_commands_required')
    for command in commands:
        require(isinstance(command, dict), 'command_object_required')
        argv = command.get('argv')
        require(isinstance(argv, list) and argv and all(isinstance(x, str) and x for x in argv), 'argv_required')
        code = command.get('actual_exit')
        require(type(code) is int, 'actual_exit_integer_required')
        _reference(repo, command.get('stdout'))
        _reference(repo, command.get('stderr'))
        if code != 0:
            raise RunFailed('actual_command_failed')
    tests = proof.get('tests')
    require(isinstance(tests, dict), 'test_results_required')
    for field in ('passed', 'failures', 'errors', 'skips'):
        require(type(tests.get(field)) is int and tests[field] >= 0, 'test_count_invalid')
    require(tests['passed'] > 0, 'actual_checks_required')
    if any(tests[f] for f in ('failures', 'errors', 'skips')):
        raise RunFailed('failed_or_skipped_checks')
    cleanup = proof.get('cleanup')
    require(isinstance(cleanup, dict) and cleanup.get('removed') is True and cleanup.get('failures') == [], 'cleanup_unverified')


def _checked(action):
    try:
        value = action()
        return {'status': 'passed', **(value if isinstance(value, dict) else {})}
    except RunFailed as exc:
        return {'status': 'failed', 'reason': str(exc), **getattr(exc, 'details', {})}
    except EvidenceError as exc:
        return {'status': 'invalid', 'reason': str(exc), **getattr(exc, 'details', {})}
    except (OSError, ValueError, TypeError, KeyError):
        return {'status': 'invalid', 'reason': 'malformed_evidence'}


def _with_details(check, details):
    try:
        check()
    except EvidenceError as exc:
        exc.details = details
        raise
    except (OSError, ValueError, TypeError, KeyError):
        exc = EvidenceError('malformed_evidence')
        exc.details = details
        raise exc from None
    return details


def _artifact_binding(repo, record, case):
    require(isinstance(record, dict), 'platform_record_required')
    envelope = {k: record.get(k) for k in ('artifact', 'build_mode', 'artifact_target')}
    overrides = record.get('case_artifacts', {})
    require(isinstance(overrides, dict) and not set(overrides) - set(CASES + ('simulator', 'physical')), 'case_artifacts_invalid')
    if case in overrides:
        override = overrides[case]
        require(isinstance(override, dict) and set(override) == set(envelope), 'case_artifact_binding_required')
        envelope = override
    require(envelope['build_mode'] in ('debug', 'profile', 'release'), 'build_mode_required')
    _reference(repo, envelope['artifact'])
    return envelope


def _case(repo, ref, source, backend_source, sdk, sdk_archive, lock, platform, arch, case, record):
    proof = read_json(_reference(repo, ref))
    details = _safe_summary(proof, ref)
    def check():
        envelope = _artifact_binding(repo, record, case)
        expected = {'schema': 'native_case_evidence_v1', 'source_commit': source, 'sdk_commit': sdk,
                    'sdk_archive_sha256': sdk_archive, 'pubspec_lock_sha256': lock, 'platform': platform,
                    'architecture': arch, 'case': case, 'artifact_sha256': envelope['artifact']['sha256'],
                    'build_mode': envelope['build_mode'], 'artifact_target': envelope['artifact_target']}
        if case in BUSINESS:
            expected['backend_source_commit'] = backend_source
        require(all(proof.get(k) == v for k, v in expected.items()), 'case_input_binding_mismatch')
        require(envelope['artifact_target'] in TARGETS[platform], 'artifact_target_mismatch')
        if case == 'build' and platform in ('macos', 'windows', 'android'):
            require(envelope['build_mode'] == 'release', 'release_build_required')
        environment = proof.get('environment')
        require(environment in ('native', 'simulator', 'physical'), 'native_runtime_required')
        if platform in ('macos', 'windows'):
            require(environment == 'native', 'native_desktop_required')
        if case in ('physical', 'simulator'):
            require(environment == case, 'physical_or_simulator_mismatch')
        if case in BUSINESS:
            require(proof.get('backend') == 'production_api', 'production_backend_required')
        if platform == 'ios':
            require(envelope['artifact_target'] == ('ios_simulator' if environment == 'simulator' else 'ios_device'), 'artifact_target_mismatch')
            other = 'physical' if case == 'simulator' else 'simulator'
            if case in ('simulator', 'physical') and other in record.get('cases', {}):
                other_envelope = _artifact_binding(repo, record, other)
                require(other_envelope['artifact']['sha256'] != envelope['artifact']['sha256'], 'ios_device_simulator_artifacts_must_differ')
        if case == 'physical':
            require(proof.get('hardware') is True, 'physical_hardware_required')
            require(_is_hash(proof.get('physical_device_sha256')), 'physical_identity_required')
            require(set(proof.get('physical_checks', [])) == {'lifecycle', 'receipts', 'storage'}, 'physical_checks_incomplete')
            if platform == 'ios':
                require(proof.get('signed_install_verified') is True, 'ios_signed_install_required')
        if case == 'recovery' and platform in ('macos', 'windows'):
            require(type(proof.get('restart_count')) is int and proof['restart_count'] >= 2, 'two_native_restarts_required')
        _commands(repo, proof)
    return _with_details(check, details)


def _cross(repo, ref, source, backend_source, sdk, sdk_archive, lock, platforms):
    proof = read_json(_reference(repo, ref))
    details = _safe_summary(proof, ref)
    def check():
        require(proof.get('schema') == 'native_cross_device_v1' and proof.get('source_commit') == source
                and proof.get('backend_source_commit') == backend_source, 'cross_source_mismatch')
        require(proof.get('backend') == 'production_api', 'production_backend_required')
        clients = proof.get('clients')
        require(isinstance(clients, list) and len(clients) >= 3, 'alice_and_bob_two_devices_required')
        for client in clients:
            require(isinstance(client, dict) and all(_is_hash(client.get(k)) for k in ('device_sha256', 'user_sha256', 'process_instance_sha256')), 'cross_client_identity_invalid')
            platform = client.get('platform')
            require(isinstance(platform, str) and platform in PLATFORMS and isinstance(platforms.get(platform), dict), 'cross_client_input_mismatch')
            require(client.get('architecture') == PLATFORMS[platform] and client.get('sdk_commit') == sdk
                    and client.get('sdk_archive_sha256') == sdk_archive and client.get('pubspec_lock_sha256') == lock, 'cross_client_input_mismatch')
            record = platforms[platform]
            bindings = [_artifact_binding(repo, record, '')]
            for case in record.get('case_artifacts', {}):
                bindings.append(_artifact_binding(repo, record, case))
            require(any(client.get('artifact_sha256') == b['artifact']['sha256'] and client.get('build_mode') == b['build_mode']
                        and client.get('artifact_target') == b['artifact_target'] and b['artifact_target'] in TARGETS[platform]
                        for b in bindings), 'cross_client_input_mismatch')
        require(len({c['device_sha256'] for c in clients}) == len(clients) and len({c['process_instance_sha256'] for c in clients}) == len(clients), 'distinct_cross_clients_required')
        users = Counter(c['user_sha256'] for c in clients)
        require(len(users) >= 2 and max(users.values()) >= 2, 'same_user_two_devices_required')
        require(all(_is_hash(c.get('organization_sha256')) for c in clients)
                and len({c['organization_sha256'] for c in clients}) >= 2, 'cross_organization_identities_required')
        require(set(proof.get('checks', [])) == CROSS_CHECKS, 'cross_checks_incomplete')
        _commands(repo, proof)
    return _with_details(check, details)


def _historical(repo, ref):
    proof = read_json(_reference(repo, ref))
    raw = proof.get('raw_evidence_sha256', {})
    require(isinstance(raw, dict) and len(raw) <= 20000, 'raw_manifest_invalid')
    for name, value in raw.items():
        _reference(repo, {'path': name, 'sha256': value})
    return {'verified_raw_files': len(raw), 'raw_manifest_present': 'raw_evidence_sha256' in proof,
            'integrity_scope': 'report_and_declared_raw' if raw else 'report_file_only'}


def _commit(repo, source):
    require(isinstance(source, str) and COMMIT.fullmatch(source), 'immutable_source_required')
    resolved = subprocess.check_output(['git', '-C', str(repo), 'rev-parse', '--verify', source+'^{commit}'], stderr=subprocess.DEVNULL, timeout=10).decode().strip()
    require(resolved == source, 'immutable_source_required')


def assess(repo, manifest, source, *, defer_physical=False):
    repo = Path(repo).resolve()
    result = {'schema': 'native_acceptance_results_v1', 'source_commit': source,
              'mode': 'offline_evidence_contract_check_only', 'status': 'invalid',
              'four_platform_complete': False, 'physical_deferred': bool(defer_physical),
              'customer_acceptance': False, 'production_acceptance': False,
              'platforms': {}, 'historical_reports': [],
              'limits': ['Does not run tests or attest evidence producers.', 'Historical evidence never satisfies current-source platform cases.', 'Deferral never removes a required gate.']}
    try:
        _commit(repo, source)
        require(isinstance(manifest, dict) and manifest.get('schema') == 'native_acceptance_input_v1' and manifest.get('source_commit') == source, 'input_source_or_schema_mismatch')
        backend_source = manifest.get('backend_source_commit')
        _commit(repo, backend_source)
        toolchain = json.loads(subprocess.check_output(['git','-C',str(repo),'show',source+':docs/verification/p5-02-toolchain-lock.json'],stderr=subprocess.DEVNULL,timeout=10))
        sdk = toolchain['framework_commit']
        require(isinstance(sdk, str) and COMMIT.fullmatch(sdk), 'sdk_commit_required')
        sdk_archive = toolchain['sdk_source_archive_sha256']
        require(_is_hash(sdk_archive), 'sdk_archive_hash_required')
        lock = hashlib.sha256(subprocess.check_output(['git','-C',str(repo),'show',source+':clients/enterprise_im/pubspec.lock'],stderr=subprocess.DEVNULL,timeout=10)).hexdigest()
        platforms = manifest.get('platforms', {})
        require(isinstance(platforms, dict) and not set(platforms)-set(PLATFORMS), 'platform_set_invalid')
        history = manifest.get('historical_reports', [])
        require(isinstance(history, list) and len(history) <= 100, 'history_list_invalid')
    except (EvidenceError, OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
        result['reason'] = 'input_or_fixed_source_invalid'
        return result
    result.update(sdk_commit=sdk, sdk_archive_sha256=sdk_archive, pubspec_lock_sha256=lock, backend_source_commit=backend_source)
    for ref in history:
        row = _checked(lambda: _historical(repo, ref))
        if row['status'] == 'passed':
            row['status'] = 'integrity_verified_historical_only'
            row['reference'] = dict(ref)
        else:
            row['catalog_entry'] = len(result['historical_reports'])
        result['historical_reports'].append(row)
    for platform, arch in PLATFORMS.items():
        row = {'status': 'blocked', 'architecture': arch, 'signing_declaration': 'not_verified', 'cases': {}}
        record = platforms.get(platform)
        required = CASES + (('simulator', 'physical') if platform in ('android', 'ios') else ())
        if isinstance(record, dict):
            row['signing_declaration'] = record.get('signing') if record.get('signing') in ('test_only', 'development', 'customer_verified') else 'not_verified'
            main = _checked(lambda: _artifact_binding(repo, record, ''))
            row['artifact_sha256'] = record['artifact']['sha256'] if main['status'] == 'passed' else None
        for case in required:
            cases = record.get('cases', {}) if isinstance(record, dict) else {}
            ref = cases.get(case) if isinstance(cases, dict) else None
            if ref is None:
                value = {'status': 'deferred' if case == 'physical' and defer_physical else 'not_run'}
            elif record.get('architecture') != arch:
                value = {'status': 'invalid', 'reason': 'platform_architecture_invalid'}
            else:
                value = _checked(lambda: _case(repo,ref,source,backend_source,sdk,sdk_archive,lock,platform,arch,case,record))
            row['cases'][case] = value
        states = {v['status'] for v in row['cases'].values()}
        row['status'] = 'invalid' if 'invalid' in states else 'failed' if 'failed' in states else 'passed' if states == {'passed'} else 'blocked'
        result['platforms'][platform] = row
    ref = manifest.get('cross_device')
    result['cross_device'] = {'status': 'not_run'} if ref is None else _checked(lambda: _cross(repo,ref,source,backend_source,sdk,sdk_archive,lock,platforms))
    states = {r['status'] for r in result['platforms'].values()} | {result['cross_device']['status']}
    if any(r['status'] == 'invalid' for r in result['historical_reports']):
        states.add('invalid')
    result['status'] = 'invalid' if 'invalid' in states else 'failed' if 'failed' in states else 'passed' if states == {'passed'} else 'incomplete'
    result['four_platform_complete'] = result['status'] == 'passed'
    return result
