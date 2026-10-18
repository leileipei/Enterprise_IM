"""Acceptance evaluator tests use synthetic evidence, never platform execution."""
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parents[1]
SDK = 'e8113bf45620cbeb8aff64947ee4c93e16adb4cf'
SDK_ARCHIVE = 'f'*64
TARGETS = {'macos':'macos_app','windows':'windows_directory','android':'android_apk','ios':'ios_device'}
PLATFORMS = {'macos': 'arm64', 'windows': 'x64', 'android': 'arm64-v8a', 'ios': 'arm64'}
CASES = ('build', 'launch', 'oidc', 'text', 'files', 'receipts', 'storage', 'lifecycle', 'recovery', 'groups', 'revocation', 'retention')


class NativeResultsTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        def git(*args):
            return subprocess.check_output(['git', '-C', str(self.root), *args], stderr=subprocess.DEVNULL).decode().strip()
        self.git = git
        git('init', '-q')
        for path, body in {'clients/enterprise_im/pubspec.lock': b'fixed lock', 'docs/verification/p5-02-toolchain-lock.json': json.dumps({'framework_commit': SDK, 'sdk_source_archive_sha256':SDK_ARCHIVE}).encode()}.items():
            p = self.root / path; p.parent.mkdir(parents=True, exist_ok=True); p.write_bytes(body)
        git('add', '.')
        git('-c', 'user.name=Fixture', '-c', 'user.email=fixture@invalid.test', 'commit', '-qm', 'synthetic fixture')
        self.source = git('rev-parse', 'HEAD')
        self.lock = hashlib.sha256(b'fixed lock').hexdigest()
        self.manifest = {'schema': 'native_acceptance_input_v1', 'source_commit': self.source, 'backend_source_commit':self.source, 'platforms': {}, 'historical_reports': []}
        for platform, arch in PLATFORMS.items():
            artifact = self.write(platform + '/test-artifact', b'synthetic parser fixture, not an executable')
            record = {'architecture': arch, 'artifact': artifact, 'signing': 'test_only', 'build_mode':'release', 'artifact_target':TARGETS[platform], 'case_artifacts':{}, 'cases': {}}
            names = CASES + (('simulator', 'physical') if platform in ('android', 'ios') else ())
            for case in names:
                case_artifact=artifact; mode='release'; target=TARGETS[platform]
                if platform=='ios' and case=='simulator':
                    case_artifact=self.write('ios/simulator-artifact',b'synthetic simulator binary fixture'); mode='debug'; target='ios_simulator'
                    record['case_artifacts'][case]={'artifact':case_artifact,'build_mode':mode,'artifact_target':target}
                environment = 'physical' if case == 'physical' else 'simulator' if case == 'simulator' else 'native'
                proof = {'schema': 'native_case_evidence_v1', 'source_commit': self.source, 'platform': platform, 'architecture': arch, 'case': case, 'environment': environment, 'sdk_commit': SDK, 'pubspec_lock_sha256': self.lock, 'artifact_sha256': case_artifact['sha256'], 'sdk_archive_sha256':SDK_ARCHIVE, 'build_mode':mode, 'artifact_target':target, 'backend_source_commit':self.source, 'backend': 'production_api', 'commands': [{'argv': ['synthetic-parser-fixture', case], 'actual_exit': 0, 'stdout': self.write(platform+'/'+case+'.log', b'controlled fixture output'), 'stderr': self.write(platform+'/'+case+'.err', b'')}], 'tests': {'passed': 1, 'failures': 0, 'errors': 0, 'skips': 0}, 'cleanup': {'removed': True, 'failures': []}, 'restart_count': 2, 'physical_device_sha256': hashlib.sha256(platform.encode()).hexdigest(), 'physical_checks': ['lifecycle', 'receipts', 'storage'], 'signed_install_verified': True, 'hardware': True}
                record['cases'][case] = self.write(platform + '/' + case + '.json', json.dumps(proof).encode())
            self.manifest['platforms'][platform] = record
        self.manifest['cross_device'] = self.write('cross-device.json', json.dumps({'schema': 'native_cross_device_v1', 'source_commit': self.source, 'backend': 'production_api', 'backend_source_commit':self.source, 'clients': [{'device_sha256': str(i)*64, 'user_sha256': str(1 if i == 1 else 2)*64, 'process_instance_sha256': str(i+3)*64, 'organization_sha256':str(1 if i==1 else 2)*64, 'platform':'android','architecture':'arm64-v8a','sdk_commit':SDK,'sdk_archive_sha256':SDK_ARCHIVE,'pubspec_lock_sha256':self.lock,'artifact_sha256':self.manifest['platforms']['android']['artifact']['sha256'],'build_mode':'release','artifact_target':'android_apk'} for i in (1,2,3)], 'checks': ['send', 'delivered', 'read', 'disconnect', 'restart', 'lost_ack', 'receipt_retry', 'logout', 'revocation', 'permission_change', 'retention_change', 'cross_organization_group'], 'commands': [{'argv': ['synthetic-parser-fixture'], 'actual_exit': 0, 'stdout': self.write('cross.log', b'fixture'), 'stderr': self.write('cross.err', b'')}], 'tests': {'passed': 12, 'failures': 0, 'errors': 0, 'skips': 0}, 'cleanup': {'removed': True, 'failures': []}}).encode())

    def write(self, name, body):
        path = self.root / name; path.parent.mkdir(parents=True, exist_ok=True); path.write_bytes(body)
        return {'path': name, 'sha256': hashlib.sha256(body).hexdigest()}

    def module(self):
        path = SCRIPTS / 'integration/native_results.py'
        self.assertTrue(path.is_file(), 'Task9 evidence evaluator has not been implemented')
        spec = importlib.util.spec_from_file_location('native_results_under_test', path)
        module = importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
        return module

    def assess(self, defer=False):
        return self.module().assess(self.root, self.manifest, self.source, defer_physical=defer)

    def change(self, platform, case, transform):
        ref = self.manifest['platforms'][platform]['cases'][case]
        value = json.loads((self.root/ref['path']).read_text()); transform(value)
        self.manifest['platforms'][platform]['cases'][case] = self.write(ref['path'], json.dumps(value).encode())

    def test_complete_synthetic_contract_is_accepted_but_not_customer_or_production(self):
        r = self.assess(); self.assertTrue(r['four_platform_complete']); self.assertEqual(r['status'], 'passed')
        self.assertFalse(r['customer_acceptance']); self.assertFalse(r['production_acceptance'])

    def test_shared_only_cannot_complete_any_platform(self):
        self.manifest['platforms'] = {}; self.manifest.pop('cross_device'); self.manifest['shared_tests'] = {'passed': 9999}
        r = self.assess(); self.assertFalse(r['four_platform_complete']); self.assertEqual(r['status'], 'incomplete')
        self.assertTrue(all(p['status']=='blocked' for p in r['platforms'].values()))

    def test_deferred_physical_stays_incomplete(self):
        for p in ('android','ios'): self.manifest['platforms'][p]['cases'].pop('physical')
        r = self.assess(True); self.assertFalse(r['four_platform_complete']); self.assertEqual(r['status'], 'incomplete')
        self.assertTrue(r['physical_deferred']); self.assertEqual(r['platforms']['android']['cases']['physical']['status'], 'deferred')

    def test_missing_native_platform_cannot_be_filled_by_android(self):
        self.manifest['platforms'].pop('windows'); self.assertFalse(self.assess()['four_platform_complete'])

    def test_actual_exit_overrides_success_claim_and_boolean_zero_is_invalid(self):
        for value in (1, False, '0', None):
            with self.subTest(value=value):
                self.change('android','text', lambda p: p['commands'][0].update(actual_exit=value))
                self.assertFalse(self.assess()['four_platform_complete'])

    def test_failed_or_skipped_test_cannot_pass(self):
        for field in ('failures','errors','skips'):
            with self.subTest(field=field):
                self.change('windows','text', lambda p: p['tests'].update({field:1}))
                self.assertFalse(self.assess()['four_platform_complete'])
                self.change('windows','text', lambda p: p['tests'].update({field:0}))

    def test_zero_tests_cannot_prove_runtime(self):
        self.change('ios','receipts',lambda p:p['tests'].update(passed=0)); self.assertFalse(self.assess()['four_platform_complete'])

    def test_cleanup_false_or_failure_list_blocks_platform(self):
        for cleanup in ({'removed':False,'failures':[]},{'removed':True,'failures':['owned_resource_alive']}):
            self.change('macos','text',lambda p:p.update(cleanup=cleanup)); self.assertFalse(self.assess()['four_platform_complete'])

    def test_source_platform_architecture_sdk_lock_and_artifact_mismatch_rejected(self):
        for field,value in [('source_commit','a'*40),('platform','ios'),('architecture','x64'),('sdk_commit','b'*40),('pubspec_lock_sha256','c'*64),('artifact_sha256','d'*64)]:
            with self.subTest(field=field):
                before=copy.deepcopy(self.manifest)
                self.change('android','text',lambda p:p.update({field:value})); self.assertEqual(self.assess()['platforms']['android']['cases']['text'].get('reason'),'case_input_binding_mismatch')
                self.manifest=before
                # Restore original proof bytes as well as its reference.
                original={'source_commit':self.source,'platform':'android','architecture':'arm64-v8a','sdk_commit':SDK,'pubspec_lock_sha256':self.lock,'artifact_sha256':before['platforms']['android']['artifact']['sha256']}[field]
                self.change('android','text',lambda p:p.update({field:original}))

    def test_simulator_and_shared_claims_are_not_physical_evidence(self):
        for env in ('simulator','shared'):
            self.change('android','physical',lambda p:p.update(environment=env)); self.assertFalse(self.assess()['four_platform_complete'])

    def test_physical_needs_hardware_checks_and_ios_signed_install(self):
        self.change('android','physical',lambda p:p.update(physical_checks=['storage'])); self.assertFalse(self.assess()['four_platform_complete'])
        self.change('android','physical',lambda p:p.update(physical_checks=['storage','lifecycle','receipts']))
        self.change('ios','physical',lambda p:p.update(signed_install_verified=False)); self.assertFalse(self.assess()['four_platform_complete'])

    def test_backend_mock_is_not_real_backend(self):
        self.change('android','text',lambda p:p.update(backend='mock')); self.assertFalse(self.assess()['four_platform_complete'])

    def test_two_native_restarts_required_on_desktop(self):
        self.change('macos','recovery',lambda p:p.update(restart_count=1)); self.assertFalse(self.assess()['four_platform_complete'])

    def test_modified_missing_or_linked_artifact_is_rejected(self):
        ref=self.manifest['platforms']['android']['artifact']; path=self.root/ref['path']; path.write_text('tampered')
        self.assertFalse(self.assess()['four_platform_complete']); path.unlink()
        self.assertFalse(self.assess()['four_platform_complete']); outside=self.root/'outside'; outside.write_text('foreign'); path.symlink_to(outside)
        self.assertFalse(self.assess()['four_platform_complete'])

    def test_path_traversal_absolute_and_symlink_ancestor_rejected(self):
        for value in ('../foreign','/private/tmp/foreign','android/../cross.log'):
            self.manifest['platforms']['android']['artifact']['path']=value; self.assertFalse(self.assess()['four_platform_complete'])
        (self.root/'linked').symlink_to(self.root/'ios', target_is_directory=True)
        self.manifest['platforms']['android']['artifact']['path']='linked/test-artifact'; self.assertFalse(self.assess()['four_platform_complete'])

    def test_log_hash_mismatch_is_not_accepted(self):
        (self.root/'android/text.log').write_text('changed'); self.assertFalse(self.assess()['four_platform_complete'])

    def test_missing_cross_device_and_duplicate_device_are_incomplete(self):
        original=self.manifest.pop('cross_device'); self.assertFalse(self.assess()['four_platform_complete']); self.manifest['cross_device']=original
        p=json.loads((self.root/original['path']).read_text()); p['clients'][1]=p['clients'][0]
        self.manifest['cross_device']=self.write(original['path'],json.dumps(p).encode()); self.assertFalse(self.assess()['four_platform_complete'])

    def test_old_reports_remain_historical_even_when_hash_valid(self):
        self.manifest['platforms']={}; raw=self.write('old-raw.log',b'old run')
        report=self.write('old-report.json',json.dumps({'tested_source_commit':'a'*40,'status':'passed','raw_evidence_sha256':{raw['path']:raw['sha256']}}).encode())
        self.manifest['historical_reports']=[report]; r=self.assess()
        self.assertFalse(r['four_platform_complete']); self.assertEqual(r['historical_reports'][0]['status'],'integrity_verified_historical_only')

    def test_changed_old_raw_fails_history_integrity(self):
        raw=self.write('old-raw.log',b'original'); self.manifest['historical_reports']=[self.write('old-report.json',json.dumps({'raw_evidence_sha256':{raw['path']:raw['sha256']}}).encode())]
        (self.root/raw['path']).write_text('changed'); self.assertEqual(self.assess()['status'],'invalid')

    def test_output_does_not_copy_credentials_or_raw_error_text(self):
        secret='fixture-secret-token-DO-NOT-PRINT'; self.change('android','text',lambda p:p.update(secret=secret))
        self.assertNotIn(secret,json.dumps(self.assess()))

    def test_input_source_mismatch_rejected(self):
        self.manifest['source_commit']='a'*40; self.assertEqual(self.assess()['status'],'invalid')

    def test_real_cli_writes_incomplete_and_preserves_existing_output(self):
        self.manifest['platforms']={}; self.manifest.pop('cross_device'); manifest=self.root/'input.json'; manifest.write_text(json.dumps(self.manifest)); out=self.root/'out.json'
        argv=[sys.executable,str(SCRIPTS/'test-native-clients.py'),'--repo',str(self.root),'--source',self.source,'--manifest',str(manifest),'--output',str(out),'--defer-physical']
        p=subprocess.run(argv,capture_output=True,text=True); self.assertEqual(p.returncode,2); self.assertTrue(out.exists(),p.stderr)
        first=out.read_bytes(); self.assertFalse(json.loads(first)['four_platform_complete'])
        p=subprocess.run(argv,capture_output=True,text=True); self.assertNotEqual(p.returncode,0); self.assertEqual(out.read_bytes(),first)

    def test_cli_rejects_duplicate_json_keys(self):
        self.module(); manifest=self.root/'input.json'; manifest.write_text('{"schema":"native_acceptance_input_v1","platforms":{},"platforms":{}}'); out=self.root/'out.json'
        p=subprocess.run([sys.executable,str(SCRIPTS/'test-native-clients.py'),'--repo',str(self.root),'--source',self.source,'--manifest',str(manifest),'--output',str(out)],capture_output=True,text=True)
        self.assertEqual(p.returncode,1); self.assertEqual(json.loads(out.read_text())['status'],'invalid')

    def test_summary_retains_verified_input_hashes_counts_and_actual_exits(self):
        r=self.assess(); self.assertIn('sdk_commit',r); self.assertEqual(r['sdk_commit'],SDK); self.assertEqual(r['pubspec_lock_sha256'],self.lock)
        self.assertEqual(r['platforms']['android']['artifact_sha256'],self.manifest['platforms']['android']['artifact']['sha256'])
        self.assertEqual(r['platforms']['android']['cases']['text']['actual_exits'],[0])
        self.assertEqual(r['platforms']['android']['cases']['text']['test_counts'],{'passed':1,'failures':0,'errors':0,'skips':0})

    def test_hardware_field_is_required_beyond_physical_label(self):
        for value in (None, False, 'true'):
            self.change('android','physical',lambda p:p.update(hardware=value))
            self.assertEqual(self.assess()['platforms']['android']['cases']['physical'].get('reason'),'physical_hardware_required')

    def test_cross_device_requires_one_user_with_two_distinct_devices(self):
        ref=self.manifest['cross_device']; p=json.loads((self.root/ref['path']).read_text())
        for i,client in enumerate(p['clients']): client['user_sha256']=str(i+1)*64
        self.manifest['cross_device']=self.write(ref['path'],json.dumps(p).encode())
        self.assertEqual(self.assess()['cross_device'].get('reason'),'same_user_two_devices_required')

    def test_history_records_number_of_verified_raw_references(self):
        raw=self.write('historical.log',b'unchanged'); ref=self.write('historical.json',json.dumps({'raw_evidence_sha256':{raw['path']:raw['sha256']}}).encode())
        self.manifest['historical_reports']=[ref]
        row=self.assess()['historical_reports'][0]; self.assertIn('verified_raw_files',row); self.assertEqual(row['verified_raw_files'],1)

    def test_physical_and_simulator_business_cannot_use_mock_backend(self):
        for case in ('physical','simulator'):
            self.change('android',case,lambda p:p.update(backend='mock'))
            self.assertEqual(self.assess()['platforms']['android']['cases'][case].get('reason'),'production_backend_required')
            self.change('android',case,lambda p:p.update(backend='production_api'))

    def test_missing_or_wrong_sdk_archive_build_mode_and_backend_source(self):
        for field,bad in [('sdk_archive_sha256','a'*64),('build_mode','debug'),('backend_source_commit','b'*40)]:
            ref=self.manifest['platforms']['android']['cases']['text']; original=json.loads((self.root/ref['path']).read_text())
            for value in (None,bad):
                self.change('android','text',lambda p:p.update({field:value})); self.assertEqual(self.assess()['platforms']['android']['cases']['text'].get('reason'),'case_input_binding_mismatch')
            self.manifest['platforms']['android']['cases']['text']=self.write(ref['path'],json.dumps(original).encode())

    def test_cross_clients_must_bind_current_platform_sdk_lock_and_artifact(self):
        ref=self.manifest['cross_device']; original=json.loads((self.root/ref['path']).read_text())
        for field,value in [('platform','unknown'),('architecture','x64'),('sdk_commit','a'*40),('sdk_archive_sha256','b'*64),('pubspec_lock_sha256','c'*64),('artifact_sha256','d'*64),('build_mode','debug'),('artifact_target','ios_device')]:
            with self.subTest(field=field):
                proof=copy.deepcopy(original);proof['clients'][0][field]=value
                self.manifest['cross_device']=self.write(ref['path'],json.dumps(proof).encode())
                self.assertEqual(self.assess()['cross_device'].get('reason'),'cross_client_input_mismatch')

    def test_cross_org_group_and_distinct_organizations_are_required(self):
        ref=self.manifest['cross_device']; original=json.loads((self.root/ref['path']).read_text())
        proof=copy.deepcopy(original); proof['checks'].remove('cross_organization_group')
        self.manifest['cross_device']=self.write(ref['path'],json.dumps(proof).encode());self.assertEqual(self.assess()['cross_device'].get('reason'),'cross_checks_incomplete')
        proof=copy.deepcopy(original)
        for client in proof['clients']:client['organization_sha256']='a'*64
        self.manifest['cross_device']=self.write(ref['path'],json.dumps(proof).encode());self.assertEqual(self.assess()['cross_device'].get('reason'),'cross_organization_identities_required')

    def test_existing_correct_hash_paths_are_rejected_for_the_path_reason(self):
        module=self.module()
        with tempfile.TemporaryDirectory() as external:
            target=Path(external)/'owned-evidence';target.write_bytes(b'correct bytes')
            ref={'path':str(target),'sha256':hashlib.sha256(target.read_bytes()).hexdigest()}
            with self.assertRaisesRegex(module.EvidenceError,'invalid_evidence_path'):module._reference(self.root,ref)
            import os
            ref['path']=os.path.relpath(target,self.root)
            with self.assertRaisesRegex(module.EvidenceError,'invalid_evidence_path'):module._reference(self.root,ref)
            (self.root/'terminal-link').symlink_to(target);ref['path']='terminal-link'
            with self.assertRaisesRegex(module.EvidenceError,'linked_evidence_rejected'):module._reference(self.root,ref)
        for name in ('D:/outside/evidence','D:outside'):
            ref=self.write(name,b'correct bytes')
            with self.assertRaisesRegex(module.EvidenceError,'invalid_evidence_path'):module._reference(self.root,ref)

    def test_failure_summaries_preserve_safe_exits_counts_cleanup_and_evidence_ref(self):
        secret='opaque-private-error-must-not-appear'
        self.change('android','text',lambda p:(p['commands'][0].update(actual_exit=7),p['cleanup'].update(removed=False,failures=[secret])))
        row=self.assess()['platforms']['android']['cases']['text']
        self.assertEqual(row['status'],'failed');self.assertEqual(row.get('actual_exits'),[7])
        self.assertEqual(row.get('test_counts'),{'passed':1,'failures':0,'errors':0,'skips':0})
        self.assertEqual(row.get('cleanup'),{'removed':False,'failure_count':1})
        self.assertEqual(row.get('evidence'),self.manifest['platforms']['android']['cases']['text'])
        self.assertNotIn(secret,json.dumps(row))

    def test_ios_simulator_and_physical_must_bind_distinct_correct_targets(self):
        self.assertTrue(self.assess()['four_platform_complete'])
        self.change('ios','simulator',lambda p:p.update(artifact_target='ios_device'))
        self.assertFalse(self.assess()['four_platform_complete'])

    def test_release_build_required_for_desktop_and_android(self):
        for platform in ('macos','windows','android'):
            self.manifest['platforms'][platform]['build_mode']='debug'
            self.change(platform,'build',lambda p:p.update(build_mode='debug'))
            self.assertEqual(self.assess()['platforms'][platform]['cases']['build'].get('reason'),'release_build_required')

    def test_unused_artifact_override_cannot_smuggle_cross_client_version(self):
        record=self.manifest['platforms']['android']
        record['case_artifacts']['not_a_real_case']={'artifact':record['artifact'],'build_mode':'release','artifact_target':'android_apk'}
        self.assertEqual(self.assess()['platforms']['android']['cases']['text'].get('reason'),'case_artifacts_invalid')
