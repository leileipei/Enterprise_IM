"""Synthetic runner-output fixtures test conversion, not actual platform builds."""
import copy
import os
import hashlib
import json
from pathlib import Path
import plistlib
import struct
import subprocess
import sys
import tempfile
import unittest
import zipfile
import zlib

SCRIPTS=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(SCRIPTS))
from integration.native_results import assess
SDK='e8113bf45620cbeb8aff64947ee4c93e16adb4cf'
DART='/owned/sdk/bin/cache/dart-sdk/bin/dart'
SNAPSHOT='/owned/sdk/bin/cache/flutter_tools.snapshot'
AAPT='/owned/android/build-tools/36.0.0/aapt2'
SIGNER='/owned/android/build-tools/36.0.0/apksigner'
ADB='/owned/android/platform-tools/adb'
TOOLS={DART:'d'*64,SNAPSHOT:'e'*64,AAPT:'a'*64,SIGNER:'b'*64,ADB:'c'*64}

def sha(data):return hashlib.sha256(data).hexdigest()


class NativeRunnerAdapterTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup);self.repo=Path(self.temp.name).resolve()
        def git(*args):return subprocess.check_output(['git','-C',str(self.repo),*args],stderr=subprocess.DEVNULL).decode().strip()
        self.git=git;git('init','-q')
        self.sources={'clients/enterprise_im/pubspec.lock':b'fixed pub lock','clients/enterprise_im/lib/main.dart':b'normal main'}
        self.impl={p:b'controlled committed producer' for p in ['scripts/build-client-mobile.py','scripts/integration/android_build.py','scripts/integration/android_simulator.py','scripts/integration/mobile.py','scripts/integration/registry.py','scripts/integration/commands.py']}
        lock={'framework_commit':SDK,'sdk_source_archive_sha256':'f'*64,'dart_executable_sha256':'d'*64,'pubspec_lock_sha256':sha(self.sources['clients/enterprise_im/pubspec.lock'])}
        for name,body in {**self.sources,**self.impl,'docs/verification/p5-02-toolchain-lock.json':json.dumps(lock).encode()}.items():
            p=self.repo/name;p.parent.mkdir(parents=True,exist_ok=True);p.write_bytes(body)
        git('add','.');git('-c','user.name=Fixture','-c','user.email=fixture@invalid.test','commit','-qm','synthetic producer source');self.source=git('rev-parse','HEAD')
        self.out=self.repo/'run';self.out.mkdir();self.result={}
        self.android()

    def module(self):
        path=SCRIPTS/'integration/native_runner_adapter.py'
        self.assertTrue(path.exists(),'mobile runner acceptance adapter is missing')
        import importlib.util
        spec=importlib.util.spec_from_file_location('integration.adapter_under_test',path);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m);return m

    def save(self):
        (self.out/'result.json').write_text(json.dumps(self.result))

    def command(self,name,argv,body):
        (self.out/(name+'.log')).write_text(body);(self.out/(name+'.log.stderr')).write_bytes(b'')
        return {'name':name,'argv':argv,'actual_exit':0,'failures':[],'stdout_sha256':sha(body.encode()),'stderr_sha256':sha(b'')}

    def android(self):
        with zipfile.ZipFile(self.out/'android-release-test.apk','w') as z:z.writestr('AndroidManifest.xml',b'controlled manifest');z.writestr('classes.dex',b'normal dex')
        apk=self.out/'android-release-test.apk'
        manifest={'AndroidManifest.xml':{'size':19,'sha256':sha(b'controlled manifest')},'classes.dex':{'size':10,'sha256':sha(b'normal dex')}}
        (self.out/'apk-manifest.json').write_text(json.dumps(manifest))
        self.badging="package: name='com.enterpriseim.client' compileSdkVersion='36'\nminSdkVersion:'24'\ntargetSdkVersion:'36'\nnative-code: 'armeabi-v7a' 'arm64-v8a' 'x86_64'\n"
        self.signature='Verifies\nVerified using v2 scheme (APK Signature Scheme v2): true\nNumber of signers: 1\nSigner #1 certificate DN: CN=Android Debug\nSigner #1 certificate SHA-256 digest: '+'a'*64+'\n'
        commands=[self.command('build',[DART,SNAPSHOT,'build','apk','--release'],'Built build/app/outputs/flutter-apk/app-release.apk\n'),self.command('badging',[AAPT,'dump','badging','/private/tmp/im-owned/'+ 'a'*32 +'/client/build/app/outputs/flutter-apk/app-release.apk'],self.badging),self.command('signature',[SIGNER,'verify','--verbose','--print-certs','/private/tmp/im-owned/'+ 'a'*32 +'/client/build/app/outputs/flutter-apk/app-release.apk'],self.signature)]
        self.result={'status':'passed','target':'android','source_commit':self.source,'owner':'a'*32,'root':'/private/tmp/im-owned','runner_exit':0,'platform_acceptance':'partial','commands':commands,'source_sha256':{k:sha(v) for k,v in self.sources.items()},'implementation_sha256':{k:sha(v) for k,v in self.impl.items()},'source_unchanged':True,'implementation_unchanged':True,'sdk_source_unchanged':True,'tool_inputs_unchanged':True,'cleanup':{'removed':True,'failures':[]},'private_root_removed':True,'temporary_signing_key_removed':True,'apk':{'path':apk.name,'sha256':sha(apk.read_bytes()),'manifest_sha256':sha((self.out/'apk-manifest.json').read_bytes()),'package':'com.enterpriseim.client','minimum_api':24,'target_api':36,'compile_api':36,'abis':['arm64-v8a','armeabi-v7a','x86_64'],'signature_kind':'temporary_debug_certificate','certificate_sha256':'a'*64,'distribution_acceptance':False,'test_fixture_classes_absent':True}}
        (self.out/'preflight.json').write_text(json.dumps({'status':'ready','target':'android','sdk_commit':SDK,'tool_sha256':TOOLS}));self.save()

    def ios(self):
        executable=struct.pack('<IiiIIIII',0xfeedfacf,0x100000c,0,2,1,24,0,0)+struct.pack('<IIIIII',0x32,24,7,15<<16,27<<16,0)
        info={'CFBundleIdentifier':'com.enterpriseim.client','CFBundleExecutable':'Runner','DTPlatformName':'iphonesimulator','MinimumOSVersion':'15.0'}
        files={'Info.plist':plistlib.dumps(info),'Runner':executable}
        with zipfile.ZipFile(self.out/'ios-simulator-debug.zip','w') as z:
            for name,body in files.items():z.writestr('Runner.app/'+name,body)
        manifest={k:sha(v) for k,v in files.items()};(self.out/'bundle-manifest.json').write_text(json.dumps(manifest))
        r=self.result;r.update(target='ios-simulator',sdk_commit=SDK,archive_actual_exit=0,archive_sha256=sha((self.out/'ios-simulator-debug.zip').read_bytes()),bundle={'file_count':2,'manifest_sha256':sha((self.out/'bundle-manifest.json').read_bytes()),'executable_sha256':sha(executable),'minimum_os_version':'15.0','codesign_actual_exit':0,'signature':'Signature=adhoc\n'},commands=[self.command('build',[DART,SNAPSHOT,'build','ios','--simulator','--debug','--no-pub'],'Built build/ios/iphonesimulator/Runner.app\n')]);r.pop('apk');r['ios_engine_inputs_unchanged']=True
        (self.out/'preflight.json').write_text(json.dumps({'status':'ready','target':'ios-simulator','sdk_commit':SDK,'tool_sha256':TOOLS,'ios_engine_sha256':{'/owned/sdk/bin/cache/artifacts/engine/ios/Flutter.xcframework/Info.plist':'f'*64}}));self.save()

    def exported(self):
        return self.module().export_mobile_run(self.repo,self.out)

    def test_real_adapter_exports_build_with_runner_checks_and_leaves_runtime_cases_unrun(self):
        m=self.module();receipt=self.exported();manifest=json.loads((self.out/'acceptance/manifest.json').read_text())
        r=assess(self.repo,manifest,self.source,defer_physical=True)
        self.assertEqual(receipt['status'],'exported_partial');self.assertEqual(r['status'],'incomplete')
        self.assertEqual(r['platforms']['android']['cases']['build']['status'],'passed')
        self.assertEqual(r['platforms']['android']['cases']['build']['count_basis'],'runner_checks')
        self.assertEqual(r['platforms']['android']['cases']['receipts']['status'],'not_run')
        self.assertEqual(r['platforms']['android']['cases']['physical']['status'],'deferred')
        self.assertFalse(r['four_platform_complete'])

    def test_sdk_client_or_implementation_candidate_overlay_is_not_rebased(self):
        m=self.module()
        for field,key in [('source_sha256','clients/enterprise_im/lib/main.dart'),('implementation_sha256','scripts/build-client-mobile.py')]:
            original=copy.deepcopy(self.result);self.result[field][key]='b'*64;self.save()
            with self.assertRaisesRegex(ValueError,'source_input_mismatch'):m.export_mobile_run(self.repo,self.out)
            self.assertFalse((self.out/'acceptance').exists());self.result=original
        self.result['source_sha256'].pop('clients/enterprise_im/lib/main.dart');self.save()
        with self.assertRaisesRegex(ValueError,'source_input_mismatch'):m.export_mobile_run(self.repo,self.out)

    def test_nonzero_boolean_exit_cleanup_or_changed_inputs_never_export(self):
        m=self.module();original=copy.deepcopy(self.result)
        for key,value in [('runner_exit',1),('runner_exit',False),('private_root_removed',False),('temporary_signing_key_removed',False),('source_unchanged',False),('cleanup',{'removed':True,'failures':['owned-child-alive']})]:
            self.result=copy.deepcopy(original);self.result[key]=value;self.save()
            with self.assertRaises(ValueError):m.export_mobile_run(self.repo,self.out)
            self.assertFalse((self.out/'acceptance').exists())

    def test_changed_log_or_artifact_and_missing_signature_metadata_are_rejected(self):
        m=self.module();original=(self.out/'build.log').read_bytes();(self.out/'build.log').write_bytes(b'changed')
        with self.assertRaises(ValueError):m.export_mobile_run(self.repo,self.out)
        (self.out/'build.log').write_bytes(original);self.result['apk']['certificate_sha256']='c'*64;self.save()
        with self.assertRaises(ValueError):m.export_mobile_run(self.repo,self.out)
        self.result['apk']['certificate_sha256']='a'*64;self.save();(self.out/'android-release-test.apk').write_bytes(b'tampered')
        with self.assertRaises(ValueError):m.export_mobile_run(self.repo,self.out)

    def test_existing_export_and_links_are_preserved(self):
        m=self.module();dest=self.out/'acceptance';dest.mkdir();marker=dest/'foreign';marker.write_text('keep')
        with self.assertRaises(ValueError):m.export_mobile_run(self.repo,self.out)
        self.assertEqual(marker.read_text(),'keep');marker.unlink();dest.rmdir()
        with tempfile.TemporaryDirectory() as tmp:
            target=Path(tmp)/'artifact';artifact=self.out/'android-release-test.apk';target.write_bytes(artifact.read_bytes());artifact.unlink();artifact.symlink_to(target)
            with self.assertRaises(ValueError):m.export_mobile_run(self.repo,self.out)
            self.assertEqual(target.read_bytes()[:2],b'PK')

    def test_export_result_hashes_are_anchored_and_collection_rejects_wrong_source(self):
        m=self.module();self.exported();manifest=m.collect_mobile_exports(self.repo,[self.out],self.source)
        self.assertEqual(set(manifest['platforms']),{'android'})
        with self.assertRaisesRegex(ValueError,'source'):m.collect_mobile_exports(self.repo,[self.out],'b'*40)
        self.result['runner_exit']=1;self.save()
        with self.assertRaises(ValueError):m.collect_mobile_exports(self.repo,[self.out],self.source)

    def test_ios_simulator_archive_is_exported_as_debug_simulator_not_device(self):
        self.ios();self.exported();manifest=json.loads((self.out/'acceptance/manifest.json').read_text());r=assess(self.repo,manifest,self.source,defer_physical=True)
        self.assertEqual(r['platforms']['ios']['cases']['build']['status'],'passed')
        self.assertEqual(manifest['platforms']['ios']['artifact_target'],'ios_simulator')
        self.assertEqual(manifest['platforms']['ios']['build_mode'],'debug');self.assertEqual(r['platforms']['ios']['cases']['physical']['status'],'deferred')

    def test_ios_archive_bytes_and_macho_platform_must_match_simulator(self):
        self.ios();m=self.module();archive=self.out/'ios-simulator-debug.zip';archive.write_bytes(b'changed')
        with self.assertRaises(ValueError):m.export_mobile_run(self.repo,self.out)

    def test_duplicate_platform_collection_never_selects_a_winner(self):
        m=self.module();self.exported()
        with self.assertRaisesRegex(ValueError,'duplicate_platform'):m.collect_mobile_exports(self.repo,[self.out,self.out],self.source)

    def test_finish_hook_is_opt_in_and_failure_is_nonzero_without_success_sidecar(self):
        m=self.module();self.assertEqual(m.finish_mobile_run(self.repo,self.out,0,False),0);self.assertFalse((self.out/'acceptance').exists())
        self.result['source_sha256'].clear();self.save();self.assertEqual(m.finish_mobile_run(self.repo,self.out,0,True),1)
        updated=json.loads((self.out/'result.json').read_text());self.assertEqual(updated['runner_exit'],1);self.assertEqual(updated['status'],'failed');self.assertEqual(updated['acceptance_export_error'],'native_acceptance_export_failed');self.assertFalse((self.out/'acceptance').exists())

    def test_finish_hook_does_not_export_failed_upstream(self):
        m=self.module();self.assertEqual(m.finish_mobile_run(self.repo,self.out,1,True),1);self.assertFalse((self.out/'acceptance').exists())

    def test_read_only_cli_accepts_runner_outputs_and_reports_incomplete(self):
        self.exported();out=self.repo/'assessed.json'
        p=subprocess.run([sys.executable,str(SCRIPTS/'test-native-clients.py'),'--repo',str(self.repo),'--source',self.source,'--runner-output',str(self.out),'--output',str(out),'--defer-physical'],capture_output=True,text=True)
        self.assertEqual(p.returncode,2);self.assertTrue(out.exists(),p.stderr);r=json.loads(out.read_text());self.assertEqual(r['platforms']['android']['cases']['build']['status'],'passed');self.assertFalse(r['four_platform_complete'])

    def test_build_cli_flag_is_known_and_preserves_real_preflight_failure(self):
        out=self.repo/'blocked'
        p=subprocess.run([sys.executable,str(SCRIPTS/'build-client-mobile.py'),'--target','android','--flutter-sdk',str(self.repo/'absent-sdk'),'--pub-cache',str(self.repo/'pub'),'--output',str(out),'--export-acceptance'],capture_output=True,text=True)
        self.assertEqual(p.returncode,2);self.assertTrue((out/'preflight.json').exists(),p.stderr);self.assertFalse((out/'acceptance').exists())

    def release_args(self,label):
        return {'release-install':['install','-r',str(self.out/'android-release-test.apk')], 'release-path':['shell','pm','path','com.enterpriseim.client'], 'release-installed-bytes':['exec-out','cat','/data/app/owned/base.apk'], 'release-launch':['shell','am','start','-W','-a','android.intent.action.MAIN','-c','android.intent.category.LAUNCHER','-f','0x10200000','-n','com.enterpriseim.client/.MainActivity'], 'release-pid':['shell','pidof','com.enterpriseim.client'], 'release-stopped':['shell','pidof','com.enterpriseim.client'], 'release-stop':['shell','am','force-stop','com.enterpriseim.client'], 'release-screenshot':['exec-out','screencap','-p'], 'release-crash-before':['logcat','-b','crash','-d'], 'release-crash-after':['logcat','-b','crash','-d']}[label]

    def launch_fixture(self):
        artifact=self.result['apk']['sha256'];pid=2468;name='IM-Mobile-Android-'+'a'*32
        texts={'release-install':'Performing Streamed Install\nSuccess\n','release-path':'package:/data/app/owned/base.apk\n','release-launch':'Status: ok\nActivity: com.enterpriseim.client/.MainActivity\n','release-pid':str(pid)+'\n','release-crash-before':'','release-crash-after':'','release-stopped':'','release-stop':''}
        rows=[]
        for label,text in texts.items():
            (self.out/('android-'+label+'.log')).write_text(text)
            rows.append({'label':label,'arguments':self.release_args(label), 'actual_exit':1 if label=='release-stopped' else 0,'stdout_sha256':sha(text.encode()),'stderr_sha256':sha(b''),'combined_sha256':sha(text.encode())})
        rows.append({'label':'release-installed-bytes','arguments':['exec-out','cat','/data/app/owned/base.apk'],'actual_exit':0,'stdout_sha256':artifact,'stderr_sha256':sha(b''),'combined_sha256':artifact})
        chunk=lambda kind,body:struct.pack('>I',len(body))+kind+body+struct.pack('>I',zlib.crc32(kind+body));screenshot=b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR',struct.pack('>IIBBBBB',1,1,8,6,0,0,0))+chunk(b'IDAT',zlib.compress(b'\0\xff\0\0\xff'))+chunk(b'IEND',b'');(self.out/'android-release-login.png').write_bytes(screenshot)
        rows.append({'label':'release-screenshot','arguments':self.release_args('release-screenshot'),'actual_exit':0,'stdout_sha256':sha(screenshot),'stderr_sha256':sha(b''),'combined_sha256':sha(screenshot)})
        for row in rows:row['argv']=[ADB,'-P','23457','-s','emulator-5600',*row['arguments']]
        self.result['simulator']={'name':name,'serial':'emulator-5600','console_port':5600,'adb_server_port':23457,'configuration':{'api':36,'abi':'arm64-v8a'},'properties':{'ro.boot.qemu.avd_name':name,'ro.build.version.sdk':'36','ro.product.cpu.abi':'arm64-v8a','ro.kernel.qemu':'1'},'commands':rows,'release_launch_passed':True,'release_installed_sha256':artifact,'release_pid':pid,'release_screenshot_sha256':sha(screenshot),'new_release_crash_interval_clean':True};self.save()

    def test_release_launch_is_bound_to_installed_artifact_and_retained_observations(self):
        self.launch_fixture();self.exported();r=assess(self.repo,json.loads((self.out/'acceptance/manifest.json').read_text()),self.source,defer_physical=True)
        self.assertEqual(r['platforms']['android']['cases']['launch']['status'],'passed')
        self.assertEqual(r['platforms']['android']['cases']['storage']['status'],'not_run');self.assertEqual(r['platforms']['android']['cases']['receipts']['status'],'not_run')

    def test_release_launch_changed_log_is_not_hidden_by_true_flags(self):
        self.launch_fixture();(self.out/'android-release-launch.log').write_text('Error: Activity not started')
        with self.assertRaisesRegex(ValueError,'release_log_hash_mismatch'):self.exported()

    def test_release_pid_and_qemu_facts_are_independently_checked(self):
        self.launch_fixture();self.result['simulator']['release_pid']=999;self.save()
        with self.assertRaisesRegex(ValueError,'release_pid_mismatch'):self.exported()
        self.result['simulator']['release_pid']=2468;self.result['simulator']['properties']['ro.kernel.qemu']='0';self.save()
        with self.assertRaises(ValueError):self.exported()

    def test_partial_export_write_is_removed_and_upstream_failure_is_recorded(self):
        m=self.module();from unittest.mock import patch
        original=Path.open;calls=[]
        class BrokenWrite:
            def __init__(self,file):self.file=file
            def __enter__(self):return self
            def write(self,data):self.file.write(data[:7]);self.file.flush();calls.append(True);raise OSError('controlled disk failure')
            def __exit__(self,*args):self.file.close()
        def opened(path,*args,**kwargs):
            file=original(path,*args,**kwargs)
            return BrokenWrite(file) if path.name=='android-build.json' and args and args[0]=='xb' else file
        with patch.object(Path,'open',new=opened):code=m.finish_mobile_run(self.repo,self.out,0,True)
        self.assertEqual(calls,[True]);self.assertEqual(code,1);self.assertFalse((self.out/'acceptance').exists())
        self.assertEqual(json.loads((self.out/'result.json').read_text())['runner_exit'],1)

    def test_missing_receipt_file_hashes_cannot_be_silently_accepted(self):
        m=self.module();self.exported();p=self.out/'acceptance/receipt.json';r=json.loads(p.read_text());r['files']={};p.write_text(json.dumps(r))
        with self.assertRaisesRegex(ValueError,'export_receipt_invalid'):m.collect_mobile_exports(self.repo,[self.out],self.source)

    def test_malformed_archive_with_correct_digest_returns_structured_cli_invalid(self):
        self.exported();artifact=self.out/'android-release-test.apk';artifact.write_bytes(b'not a zip')
        self.result['apk']['sha256']=sha(artifact.read_bytes());self.save();out=self.repo/'invalid.json'
        p=subprocess.run([sys.executable,str(SCRIPTS/'test-native-clients.py'),'--repo',str(self.repo),'--source',self.source,'--runner-output',str(self.out),'--output',str(out)],capture_output=True,text=True)
        self.assertEqual(p.returncode,1);self.assertTrue(out.exists(),p.stderr);self.assertEqual(json.loads(out.read_text())['status'],'invalid')

    def test_ios_device_macho_with_consistent_hashes_is_not_a_simulator(self):
        self.ios();archive=self.out/'ios-simulator-debug.zip'
        with zipfile.ZipFile(archive) as z:files={name:z.read(name) for name in z.namelist()}
        exe=bytearray(files['Runner.app/Runner']);struct.pack_into('<I',exe,40,2);files['Runner.app/Runner']=bytes(exe)
        with zipfile.ZipFile(archive,'w') as z:
            for name,body in files.items():z.writestr(name,body)
        manifest={name.removeprefix('Runner.app/'):sha(body) for name,body in files.items()};(self.out/'bundle-manifest.json').write_text(json.dumps(manifest))
        self.result['archive_sha256']=sha(archive.read_bytes());self.result['bundle']['manifest_sha256']=sha((self.out/'bundle-manifest.json').read_bytes());self.result['bundle']['executable_sha256']=sha(bytes(exe));self.save()
        with self.assertRaisesRegex(ValueError,'ios_simulator_minimum_platform_mismatch'):self.exported()

    def test_review_required_upstream_hashes_missing_null_or_invalid_are_rejected(self):
        original=copy.deepcopy(self.result)
        for key in ('sha256','manifest_sha256'):
            for value in (None,'bad'):
                self.result=copy.deepcopy(original);self.result['apk'][key]=value;self.save()
                with self.assertRaises(ValueError):self.exported()
        for key in ('stdout_sha256','stderr_sha256'):
            self.result=copy.deepcopy(original);self.result['commands'][0].pop(key);self.save()
            with self.assertRaises(ValueError):self.exported()

    def test_review_build_and_inspection_commands_cannot_be_unrelated_programs(self):
        original=copy.deepcopy(self.result)
        for index in range(3):
            self.result=copy.deepcopy(original);self.result['commands'][index]['argv'][0]='echo';self.save()
            with self.assertRaises(ValueError):self.exported()
        self.result=copy.deepcopy(original);self.result['commands'][1]['argv'][-1]='/different.apk';self.save()
        with self.assertRaises(ValueError):self.exported()

    def test_review_ios_snapshot_and_engine_inputs_are_mandatory(self):
        self.ios();pre=self.out/'preflight.json';original=json.loads(pre.read_text())
        for key in ('tool_sha256','ios_engine_sha256'):
            value=copy.deepcopy(original)
            if key=='tool_sha256':value[key].pop(SNAPSHOT)
            else:value.pop(key)
            pre.write_text(json.dumps(value))
            with self.assertRaises(ValueError):self.exported()

    def test_review_all_release_arguments_and_device_argv_are_bound(self):
        self.launch_fixture();original=copy.deepcopy(self.result)
        for index in range(len(original['simulator']['commands'])):
            self.result=copy.deepcopy(original);self.result['simulator']['commands'][index]['arguments']=['echo'];self.save()
            with self.assertRaises(ValueError):self.exported()
        self.result=copy.deepcopy(original);self.result['simulator']['commands'][0]['argv'][4]='emulator-9999';self.save()
        with self.assertRaises(ValueError):self.exported()

    def test_review_release_stop_and_screenshot_commands_are_required(self):
        self.launch_fixture();original=copy.deepcopy(self.result)
        for label in ('release-stop','release-screenshot'):
            self.result=copy.deepcopy(original);self.result['simulator']['commands']=[r for r in self.result['simulator']['commands'] if r['label']!=label];self.save()
            with self.assertRaises(ValueError):self.exported()

    def test_review_truncated_png_with_updated_hashes_cannot_prove_launch(self):
        self.launch_fixture();image=self.out/'android-release-login.png';image.write_bytes(b'\x89PNG\r\n\x1a\n');value=sha(image.read_bytes());self.result['simulator']['release_screenshot_sha256']=value
        for r in self.result['simulator']['commands']:
            if r['label']=='release-screenshot':r.update(stdout_sha256=value,combined_sha256=value)
        self.save()
        with self.assertRaises(ValueError):self.exported()

    def test_review_ios_engine_inventory_detects_changed_snapshot_and_engine_files(self):
        from integration.mobile import ios_tool_inputs
        sdk=self.repo/'sdk';snapshot=sdk/'bin/cache/flutter_tools.snapshot';snapshot.parent.mkdir(parents=True);snapshot.write_bytes(b'tool');engine=sdk/'bin/cache/artifacts/engine/ios/Flutter.xcframework/Info.plist';engine.parent.mkdir(parents=True);engine.write_bytes(b'engine')
        first=ios_tool_inputs(sdk);snapshot.write_bytes(b'changed');self.assertNotEqual(first,ios_tool_inputs(sdk));snapshot.write_bytes(b'tool');engine.write_bytes(b'changed');self.assertNotEqual(first,ios_tool_inputs(sdk));engine.write_bytes(b'engine');(engine.parent/'new').write_bytes(b'added');self.assertNotEqual(first,ios_tool_inputs(sdk))

    def test_review_unsupported_zip_compression_returns_structured_invalid(self):
        archive=self.out/'android-release-test.apk';data=bytearray(archive.read_bytes())
        offset=0
        while True:
            offset=data.find(b'PK\x03\x04',offset)
            if offset<0:break
            struct.pack_into('<H',data,offset+8,99);offset+=4
        offset=0
        while True:
            offset=data.find(b'PK\x01\x02',offset)
            if offset<0:break
            struct.pack_into('<H',data,offset+10,99);offset+=4
        archive.write_bytes(data);self.result['apk']['sha256']=sha(data);self.save();out=self.repo/'invalid-compression.json'
        p=subprocess.run([sys.executable,str(SCRIPTS/'test-native-clients.py'),'--repo',str(self.repo),'--source',self.source,'--runner-output',str(self.out),'--output',str(out)],capture_output=True,text=True)
        self.assertEqual(p.returncode,1);self.assertTrue(out.exists(),p.stderr);self.assertEqual(json.loads(out.read_text())['status'],'invalid')

    @unittest.skipUnless(sys.platform=='darwin','actual macOS bundle archiver regression')
    def test_actual_ios_archive_has_exact_files_without_resource_fork_sidecars(self):
        from integration.mobile import archive_ios_bundle
        app=self.repo/'Runner.app';app.mkdir();data=app/'data';data.write_bytes(b'normal app content');subprocess.run(['/usr/bin/xattr','-w','com.apple.ResourceFork','controlled resource metadata',str(data)],check=True,capture_output=True)
        self.assertEqual(subprocess.check_output(['/usr/bin/xattr','-p','com.apple.ResourceFork',str(data)]).strip(),b'controlled resource metadata')
        archive=self.repo/'bundle.zip';p=archive_ios_bundle(app,archive);self.assertEqual(p.returncode,0,p.stderr)
        with zipfile.ZipFile(archive) as z:
            self.assertEqual([n for n in z.namelist() if not n.endswith('/')],['Runner.app/data']);self.assertEqual(z.read('Runner.app/data'),b'normal app content')

    def ios_normal_fixture(self):
        self.ios();udid='11111111-2222-3333-4444-555555555555';runtime='com.apple.CoreSimulator.SimRuntime.iOS-26-5';device='com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro'
        app='/private/tmp/im-owned/'+'a'*32+'/client/build/ios/iphonesimulator/Runner.app';installed='/Users/test/Library/Developer/CoreSimulator/Devices/'+udid+'/data/Containers/Bundle/Application/AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE/Runner.app';pid=2468
        row={'name':'IM-Mobile-'+'a'*32,'udid':udid,'deviceTypeIdentifier':device,'state':'Booted'}
        pre=self.out/'preflight.json';value=json.loads(pre.read_text());value.update(simulator={'runtime':runtime,'device_type':device},simulator_device_root='/Users/test/Library/Developer/CoreSimulator/Devices',host_uid=501);value['tool_sha256'].update({'/usr/bin/xcrun':'1'*64,'/bin/ps':'2'*64,'/Applications/Xcode.app/Contents/Developer/usr/bin/simctl':'3'*64});pre.write_text(json.dumps(value))
        manifest=json.loads((self.out/'bundle-manifest.json').read_text());(self.out/'ios-normal-installed-manifest.json').write_text(json.dumps(manifest))
        self.impl['scripts/integration/ios_app_launch.py']=b'controlled normal launch implementation';path=self.repo/'scripts/integration/ios_app_launch.py';path.write_bytes(self.impl['scripts/integration/ios_app_launch.py']);self.git('add','.');self.git('-c','user.name=Fixture','-c','user.email=fixture@invalid.test','commit','-qm','normal launch producer');self.source=self.git('rev-parse','HEAD');self.result['source_commit']=self.source;self.result['implementation_sha256']={k:sha(v) for k,v in self.impl.items()}
        chunk=lambda kind,body:struct.pack('>I',len(body))+kind+body+struct.pack('>I',zlib.crc32(kind+body));image=b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR',struct.pack('>IIBBBBB',1,1,8,6,0,0,0))+chunk(b'IDAT',zlib.compress(b'\0\xff\0\0\xff'))+chunk(b'IEND',b'');(self.out/'ios-normal-login.png').write_bytes(image)
        commands=[]
        args={'install':['install',udid,app],'bundle-path':['get_app_container',udid,'com.enterpriseim.client','app'],'launch':['launch','--terminate-running-process',udid,'com.enterpriseim.client'],'screenshot':['io',udid,'screenshot','--type=png',str(self.out/'ios-normal-login.png')],'terminate':['terminate',udid,'com.enterpriseim.client']}
        for name in ['install','bundle-path','launch','alive-before','alive-after','screenshot','terminate','stopped']:
            argv=['/bin/ps','-p',str(pid),'-o','pid=,lstart=,comm='] if name in ['alive-before','alive-after','stopped'] else ['/usr/bin/xcrun','simctl',*args[name]]
            text={'bundle-path':installed+'\n','launch':'com.enterpriseim.client: 2468\n','alive-before':'2468 Sun Oct 18 12:00:00 2026 '+installed+'/Runner\n','alive-after':'2468 Sun Oct 18 12:00:00 2026 '+installed+'/Runner\n'}.get(name,'')
            name_log='ios-normal-'+name;(self.out/(name_log+'.log')).write_text(text);(self.out/(name_log+'.log.stderr')).write_bytes(b'');commands.append({'name':name,'argv':argv,'actual_exit':1 if name=='stopped' else 0,'stdout_sha256':sha(text.encode()),'stderr_sha256':sha(b'')})
        identity={'pid':pid,'uid':501,'start_time':'Sun Oct 18 12:00:00 2026','executable_path':installed+'/Runner','executable_sha256':self.result['bundle']['executable_sha256'],'workdir':'/','pgid':1234}
        self.result['simulator']={'name':row['name'],'udid':udid,'runtime':runtime,'device_type':device}
        self.result['owned_simulator_removed']=True
        self.result['normal_launch']={'passed':True,'artifact_sha256':self.result['archive_sha256'],'installed_manifest_sha256':sha((self.out/'ios-normal-installed-manifest.json').read_bytes()),'installed_app':installed,'pid':pid,'identity_before':identity,'identity_after':copy.deepcopy(identity),'ownership_before':row,'ownership_after':copy.deepcopy(row),'commands':commands,'screenshot_sha256':sha(image),'host_absence_after_terminate':True,'native_leaf_os_exit_independently_observed':False,'native_leaf_os_exit':None}
        self.save()

    def test_ios_ordinary_same_archive_launch_exports_without_promoting_debug_driver_cases(self):
        self.ios_normal_fixture();self.exported();manifest=json.loads((self.out/'acceptance/manifest.json').read_text());r=assess(self.repo,manifest,self.source,defer_physical=True)
        self.assertEqual(r['platforms']['ios']['cases']['launch']['status'],'passed');self.assertEqual(r['platforms']['ios']['cases']['storage']['status'],'not_run');self.assertFalse(r['four_platform_complete'])

    def test_ios_launch_false_flags_or_different_artifact_cannot_export(self):
        self.ios_normal_fixture();original=copy.deepcopy(self.result)
        for key,value in [('passed',False),('artifact_sha256','b'*64),('host_absence_after_terminate',False),('pid',True),('native_leaf_os_exit_independently_observed',True),('native_leaf_os_exit',0)]:
            self.result=copy.deepcopy(original);self.result['normal_launch'][key]=value;self.save()
            with self.assertRaises(ValueError):self.exported()

    def test_ios_launch_wrong_device_or_changed_identity_is_rejected(self):
        self.ios_normal_fixture();original=copy.deepcopy(self.result)
        for field,key,value in [('ownership_before','name','foreign'),('ownership_after','state','Shutdown'),('identity_before','executable_sha256','b'*64),('identity_after','start_time','changed'),('identity_after','uid',999)]:
            self.result=copy.deepcopy(original);self.result['normal_launch'][field][key]=value;self.save()
            with self.assertRaises(ValueError):self.exported()

    def test_ios_launch_missing_hash_or_unrelated_command_is_rejected(self):
        self.ios_normal_fixture();original=copy.deepcopy(self.result)
        for index in range(8):
            self.result=copy.deepcopy(original);self.result['normal_launch']['commands'][index]['argv'][0]='echo';self.save()
            with self.assertRaises(ValueError):self.exported()
        self.result=copy.deepcopy(original);self.result['normal_launch']['commands'][0].pop('stdout_sha256');self.save()
        with self.assertRaises(ValueError):self.exported()

    def test_ios_launch_unknown_stop_exit_or_nonempty_absence_log_is_rejected(self):
        self.ios_normal_fixture();self.result['normal_launch']['commands'][-1]['actual_exit']=2;self.save()
        with self.assertRaises(ValueError):self.exported()
        self.result['normal_launch']['commands'][-1]['actual_exit']=1;(self.out/'ios-normal-stopped.log').write_text('still alive');self.result['normal_launch']['commands'][-1]['stdout_sha256']=sha(b'still alive');self.save()
        with self.assertRaises(ValueError):self.exported()

    def test_ios_launch_changed_installed_manifest_or_truncated_screenshot_is_rejected(self):
        self.ios_normal_fixture();manifest=self.out/'ios-normal-installed-manifest.json';manifest.write_text('{}');self.result['normal_launch']['installed_manifest_sha256']=sha(manifest.read_bytes());self.save()
        with self.assertRaises(ValueError):self.exported()
        manifest.write_text((self.out/'bundle-manifest.json').read_text());self.result['normal_launch']['installed_manifest_sha256']=sha(manifest.read_bytes());image=self.out/'ios-normal-login.png';image.write_bytes(b'\x89PNG\r\n\x1a\n');self.result['normal_launch']['screenshot_sha256']=sha(image.read_bytes());self.save()
        with self.assertRaises(ValueError):self.exported()

    def test_ios_launch_parser_rejects_foreign_pid_output(self):
        path=SCRIPTS/'integration/ios_app_launch.py';self.assertTrue(path.exists(),'normal iOS launch helper missing')
        from integration.ios_app_launch import launched_pid
        self.assertEqual(launched_pid('com.enterpriseim.client: 2468\n'),2468)
        for text in ['foreign: 2468','com.enterpriseim.client: 0','com.enterpriseim.client: true','com.enterpriseim.client: 2468\nextra']:
            with self.assertRaises(ValueError):launched_pid(text)

    def test_ios_process_observation_uses_same_utc_time_as_fingerprint(self):
        from integration import ios_app_launch as module
        self.assertTrue(hasattr(module,'process_observation'),'canonical process observation is missing')
        argv=['/bin/ps','-p',str(os.getpid()),'-o','pid=,lstart=,comm='];reference=subprocess.run(argv,env={'TZ':'UTC','LANG':'C','LC_ALL':'C'},capture_output=True)
        actual=module.process_observation(os.getpid(),{'TZ':'Asia/Shanghai','LANG':'fr_FR.UTF-8','LC_ALL':'fr_FR.UTF-8'})
        self.assertEqual(reference.returncode,0);self.assertEqual(actual.returncode,0);self.assertEqual(actual.stdout,reference.stdout);self.assertEqual(actual.stderr,b'')

    def test_ios_normal_launch_requires_explicit_owned_simulator_cleanup(self):
        self.ios_normal_fixture()
        for value in (None,False):
            self.result['owned_simulator_removed']=value;self.save()
            with self.assertRaisesRegex(ValueError,'ios_normal_simulator_cleanup_unverified'):self.exported()
        self.result.pop('owned_simulator_removed');self.save()
        with self.assertRaisesRegex(ValueError,'ios_normal_simulator_cleanup_unverified'):self.exported()

    def test_ios_normal_launch_rejects_reordered_commands_and_preserves_chronology(self):
        self.ios_normal_fixture();commands=self.result['normal_launch']['commands'];self.result['normal_launch']['commands']=list(reversed(commands));self.save()
        with self.assertRaisesRegex(ValueError,'ios_normal_commands_unverified'):self.exported()
        self.result['normal_launch']['commands']=commands;self.save();self.exported();proof=json.loads((self.out/'acceptance/ios-launch.json').read_text());self.assertEqual([row['stdout']['path'].rsplit('/',1)[-1] for row in proof['commands']],['ios-normal-install.log','ios-normal-bundle-path.log','ios-normal-launch.log','ios-normal-alive-before.log','ios-normal-alive-after.log','ios-normal-screenshot.log','ios-normal-terminate.log'])
        self.assertEqual(proof['normal_launch_observations']['stopped_observation']['actual_exit'],1)
