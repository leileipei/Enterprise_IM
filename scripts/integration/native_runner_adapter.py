"""Derive limited acceptance cases from completed mobile runners; never rebase source."""
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import subprocess
import tarfile
import zipfile
import struct
import re
from .native_results import COMMIT, EvidenceError, require, digest, read_json, _reference
from .native_runner_artifacts import android_artifact, ios_artifact, validate_png

_NO_HASH=object()
SHA256=re.compile(r"[0-9a-f]{64}")


def validate_export_output(repo, out):
    original=Path(repo).absolute();repo=original.resolve();out=Path(out).absolute()
    try:rel=out.relative_to(original)
    except ValueError:
        try:rel=out.relative_to(repo)
        except ValueError:raise EvidenceError('acceptance_output_must_be_inside_repo') from None
    out=repo/rel
    require('..' not in rel.parts and rel.parts,'acceptance_output_must_be_inside_repo')
    current=repo
    for part in rel.parts:
        current=current/part;require(not current.is_symlink(),'linked_runner_output')
    return repo,out


def _snapshot(repo, source, paths):
    try:
        data=subprocess.check_output(['git','-C',str(repo),'archive',source,*paths],stderr=subprocess.DEVNULL,timeout=30)
        result={}
        with tarfile.open(fileobj=io.BytesIO(data)) as archive:
            for member in archive.getmembers():
                if member.isdir():continue
                require(member.isfile(),'source_input_mismatch')
                result[member.name]=hashlib.sha256(archive.extractfile(member).read()).hexdigest()
        return result
    except (OSError,subprocess.SubprocessError,tarfile.TarError):raise EvidenceError('source_input_mismatch') from None


def _derive_inputs(repo, out):
    repo,out=validate_export_output(repo,out)
    def file(name, expected=_NO_HASH):
        rel=(out/name).relative_to(repo).as_posix()
        # Check links before computing a digest from a producer-selected file.
        cursor=repo
        for part in PurePosixPath(rel).parts:
            cursor=cursor/part;require(not cursor.is_symlink(),'linked_runner_input')
        require(cursor.is_file(),'runner_input_missing')
        return _reference(repo,{'path':rel,'sha256':digest(cursor) if expected is _NO_HASH else expected})
    def ref(path):return {'path':Path(path).relative_to(repo).as_posix(),'sha256':digest(path)}
    result_path=file('result.json');result=read_json(result_path);preflight_path=file('preflight.json');preflight=read_json(preflight_path)
    source=result.get('source_commit');require(isinstance(source,str) and COMMIT.fullmatch(source),'runner_source_required')
    require(result.get('target') in ('android','ios-simulator'),'unsupported_mobile_runner')
    platform='android' if result['target']=='android' else 'ios'
    require(result.get('status')=='passed' and type(result.get('runner_exit')) is int and result['runner_exit']==0,'runner_terminal_unverified')
    require(result.get('cleanup')=={'removed':True,'failures':[]} and result.get('private_root_removed') is True,'runner_cleanup_unverified')
    for key in ('source_unchanged','implementation_unchanged','sdk_source_unchanged','tool_inputs_unchanged'):
        require(result.get(key) is True,'runner_inputs_changed')
    if platform=='android':require(result.get('temporary_signing_key_removed') is True,'runner_signing_key_not_removed')
    require(result.get('source_sha256')==_snapshot(repo,source,['clients/enterprise_im']),'source_input_mismatch')
    implementation=result.get('implementation_sha256')
    required={'scripts/build-client-mobile.py','scripts/integration/mobile.py','scripts/integration/registry.py','scripts/integration/commands.py'}
    if platform=='android':
        required.add('scripts/integration/android_build.py')
        if isinstance(result.get('simulator'),dict):required.add('scripts/integration/android_simulator.py')
    require(isinstance(implementation,dict) and required<=set(implementation) and all(isinstance(p,str) and p.startswith('scripts/') and '..' not in PurePosixPath(p).parts and ':' not in p and '\\' not in p for p in implementation),'source_input_mismatch')
    require(implementation==_snapshot(repo,source,sorted(implementation)),'source_input_mismatch')
    try:lock=json.loads(subprocess.check_output(['git','-C',str(repo),'show',source+':docs/verification/p5-02-toolchain-lock.json'],stderr=subprocess.DEVNULL,timeout=10))
    except (OSError,ValueError,subprocess.SubprocessError):raise EvidenceError('locked_sdk_input_missing') from None
    require(preflight.get('status')=='ready' and preflight.get('target')==result['target'] and preflight.get('sdk_commit')==lock.get('framework_commit'),'runner_sdk_mismatch')
    hashes=preflight.get('tool_sha256',{});require(isinstance(hashes,dict),'runner_sdk_mismatch')
    require(all(isinstance(p,str) and Path(p).is_absolute() and isinstance(v,str) and SHA256.fullmatch(v) for p,v in hashes.items()),'runner_tool_digest_missing')
    def tool(suffix):
        matches=[p for p in hashes if p.endswith(suffix)]
        require(len(matches)==1,'runner_tool_digest_missing')
        return matches[0]
    dart=tool('/bin/cache/dart-sdk/bin/dart');snapshot=tool('/bin/cache/flutter_tools.snapshot')
    require(hashes[dart]==lock.get('dart_executable_sha256') and snapshot==dart.removesuffix('/dart-sdk/bin/dart')+'/flutter_tools.snapshot','runner_dart_digest_missing_or_mismatched')
    if platform=='ios':
        engine=preflight.get('ios_engine_sha256');prefix=dart.removesuffix('/dart-sdk/bin/dart')+'/artifacts/engine/ios/'
        require(isinstance(engine,dict) and engine and all(isinstance(p,str) and p.startswith(prefix) and '..' not in Path(p).parts and isinstance(h,str) and SHA256.fullmatch(h) for p,h in engine.items()) and result.get('ios_engine_inputs_unchanged') is True,'ios_engine_inputs_unverified')
    commands=result.get('commands');require(isinstance(commands,list) and commands,'runner_commands_missing')
    by_name={}
    for command in commands:
        require(isinstance(command,dict) and isinstance(command.get('name'),str) and command['name'] not in by_name,'duplicate_or_invalid_runner_command')
        require(type(command.get('actual_exit')) is int and command['actual_exit']==0 and command.get('failures')==[],'runner_command_failed')
        by_name[command['name']]=command
    names=['build','badging','signature'] if platform=='android' else ['build']
    selected=[]
    for name in names:
        command=by_name.get(name);require(isinstance(command,dict) and isinstance(command.get('argv'),list) and command['argv'],'build_command_missing')
        stdout=file(name+'.log',command.get('stdout_sha256'));stderr=file(name+'.log.stderr',command.get('stderr_sha256'))
        selected.append({'argv':command['argv'],'actual_exit':0,'stdout':ref(stdout),'stderr':ref(stderr)})
    build=by_name['build']['argv']
    wanted=['build','apk','--release'] if platform=='android' else ['build','ios','--simulator','--debug','--no-pub']
    require(build==[dart,snapshot,*wanted],'build_mode_or_target_unverified')
    if platform=='android':
        owner=result.get('owner');root=result.get('root')
        require(isinstance(owner,str) and re.fullmatch('[0-9a-f]{32}',owner) and isinstance(root,str) and Path(root).is_absolute() and '..' not in Path(root).parts,'runner_owner_unverified')
        original_apk=str(Path(root)/owner/'client/build/app/outputs/flutter-apk/app-release.apk')
        require(by_name['badging']['argv']==[tool('/build-tools/36.0.0/aapt2'),'dump','badging',original_apk] and by_name['signature']['argv']==[tool('/build-tools/36.0.0/apksigner'),'verify','--verbose','--print-certs',original_apk],'inspection_command_unverified')
    require('Built ' in (out/'build.log').read_text(),'build_completion_marker_missing')
    artifact=android_artifact(out,result,file) if platform=='android' else ios_artifact(out,result,file)
    artifact_ref=ref(artifact['path']);lock_hash=result['source_sha256']['clients/enterprise_im/pubspec.lock']
    require(lock_hash==lock.get('pubspec_lock_sha256'),'pubspec_lock_mismatch')
    checks=['fixed_source','fixed_sdk','runner_terminal','cleanup','artifact_integrity','build_command','artifact_metadata']
    base={'schema':'native_case_evidence_v1','source_commit':source,'platform':platform,'architecture':'arm64-v8a' if platform=='android' else 'arm64','sdk_commit':lock['framework_commit'],'sdk_archive_sha256':lock['sdk_source_archive_sha256'],'pubspec_lock_sha256':lock_hash,'artifact_sha256':artifact_ref['sha256'],'build_mode':artifact['build_mode'],'artifact_target':artifact['target'],'environment':artifact['environment'],'commands':selected,'tests':{'passed':len(checks),'failures':0,'errors':0,'skips':0},'count_basis':'runner_checks','check_names':checks,'cleanup':{'removed':True,'failures':[]},'upstream_result':ref(result_path),'upstream_preflight':ref(preflight_path)}
    cases={'build':dict(base,case='build')}
    # A release launch is distinct from Debug driver lifecycle/storage tests.
    sim=result.get('simulator')
    if platform=='android' and isinstance(sim,dict) and sim.get('release_launch_passed') is True:
        require(sim.get('release_installed_sha256')==artifact_ref['sha256'] and type(sim.get('release_pid')) is int and sim['release_pid']>0 and sim.get('new_release_crash_interval_clean') is True,'release_launch_unverified')
        configuration=sim.get('configuration',{});require(configuration.get('abi')=='arm64-v8a' and configuration.get('api') in (24,36),'release_device_unverified')
        from .android_simulator import validate_device
        require(sim.get('name')=='IM-Mobile-Android-'+result.get('owner',''),'release_device_unverified')
        validate_device(sim.get('properties',{}),sim['name'],configuration)
        # Existing simulator runner records every actual command and retained log.
        observations=[]
        installed=(out/'android-release-path.log').read_text().strip()
        require(re.fullmatch(r'package:/data/app/[^\s]+/base\.apk',installed) is not None,'release_installed_path_unverified')
        installed=installed.removeprefix('package:')
        expected_arguments={'release-install':['install','-r',str(artifact['path'])], 'release-path':['shell','pm','path','com.enterpriseim.client'], 'release-installed-bytes':['exec-out','cat',installed], 'release-launch':['shell','am','start','-W','-a','android.intent.action.MAIN','-c','android.intent.category.LAUNCHER','-f','0x10200000','-n','com.enterpriseim.client/.MainActivity'], 'release-pid':['shell','pidof','com.enterpriseim.client'], 'release-stopped':['shell','pidof','com.enterpriseim.client'], 'release-stop':['shell','am','force-stop','com.enterpriseim.client'], 'release-screenshot':['exec-out','screencap','-p'], 'release-crash-before':['logcat','-b','crash','-d'], 'release-crash-after':['logcat','-b','crash','-d']}
        console=sim.get('console_port');port=sim.get('adb_server_port');serial=sim.get('serial')
        require(type(console) is int and 5554<=console<=5682 and console%2==0 and type(port) is int and 1024<=port<=65535 and port not in (5037,console,console+1) and serial in ('emulator-'+str(console),'127.0.0.1:'+str(console+1)),'release_device_unverified')
        adb=tool('/platform-tools/adb');labels=list(expected_arguments)
        for p,h in configuration.get('tool_sha256',{}).items():require(hashes.get(p)==h,'release_device_tools_unverified')
        for label in labels:
            rows=[r for r in sim.get('commands',[]) if r.get('label')==label]
            require(len(rows)==1 and type(rows[0].get('actual_exit')) is int and rows[0]['actual_exit']==(1 if label=='release-stopped' else 0),'release_launch_unverified')
            require(rows[0].get('arguments')==expected_arguments[label] and rows[0].get('argv')==[adb,'-P',str(port),'-s',serial,*expected_arguments[label]],'release_command_unverified')
            require(all(isinstance(rows[0].get(k),str) and SHA256.fullmatch(rows[0][k]) for k in ('stdout_sha256','stderr_sha256','combined_sha256')),'release_log_hash_mismatch')
            if label not in ('release-installed-bytes','release-screenshot'):
                log=file('android-'+label+'.log')
                require(digest(log)==rows[0].get('combined_sha256'),'release_log_hash_mismatch')
            if label=='release-installed-bytes':require(rows[0].get('stdout_sha256')==artifact_ref['sha256'],'release_installed_hash_mismatch')
            observations.append({'label':label,'actual_exit':rows[0]['actual_exit'],'stdout_sha256':rows[0].get('stdout_sha256')})
        require((out/'android-release-pid.log').read_text().strip()==str(sim['release_pid']),'release_pid_mismatch')
        require('Success' in (out/'android-release-install.log').read_text() and 'Status: ok' in (out/'android-release-launch.log').read_text(),'release_launch_unverified')
        before=(out/'android-release-crash-before.log').read_bytes();after=(out/'android-release-crash-after.log').read_bytes()
        require(after.startswith(before) and b'FATAL EXCEPTION' not in after[len(before):] and b'Fatal signal' not in after[len(before):] and (out/'android-release-stopped.log').read_bytes()==b'','release_launch_unverified')
        screenshot=file('android-release-login.png',sim.get('release_screenshot_sha256'));validate_png(screenshot.read_bytes())
        image_rows=[r for r in sim['commands'] if r['label']=='release-screenshot']
        require(image_rows[0]['stdout_sha256']==digest(screenshot),'release_screenshot_hash_mismatch')
        launch=dict(base,case='launch',release_observations=observations)
        launch['tests']={'passed':len(labels),'failures':0,'errors':0,'skips':0};launch['check_names']=labels;cases['launch']=launch
    record={'architecture':base['architecture'],'artifact':artifact_ref,'build_mode':artifact['build_mode'],'artifact_target':artifact['target'],'signing':'test_only','cases':{}}
    manifest={'schema':'native_acceptance_input_v1','source_commit':source,'backend_source_commit':source,'platforms':{platform:record},'historical_reports':[]}
    return repo,out,manifest,cases


def _derive(repo, out):
    try:return _derive_inputs(repo,out)
    except EvidenceError:raise
    except (OSError,ValueError,TypeError,KeyError,AttributeError,zipfile.BadZipFile,struct.error,NotImplementedError):
        raise EvidenceError('mobile_runner_evidence_invalid') from None


def export_mobile_run(repo, out):
    repo,out=validate_export_output(repo,out);destination=out/'acceptance'
    require(not destination.exists() and not destination.is_symlink(),'acceptance_output_exists')
    repo,out,manifest,cases=_derive(repo,out);platform=next(iter(manifest['platforms']));record=manifest['platforms'][platform]
    planned={}
    for name,proof in cases.items():
        body=(json.dumps(proof,ensure_ascii=False,indent=2)+'\n').encode();filename=platform+'-'+name+'.json';planned[filename]=body
        record['cases'][name]={'path':(destination/filename).relative_to(repo).as_posix(),'sha256':hashlib.sha256(body).hexdigest()}
    planned['manifest.json']=(json.dumps(manifest,ensure_ascii=False,indent=2)+'\n').encode()
    receipt={'schema':'mobile_runner_acceptance_export_v1','status':'exported_partial','source_commit':manifest['source_commit'],'platform':platform,'exported_cases':sorted(cases),'files':{name:hashlib.sha256(body).hexdigest() for name,body in planned.items()},'platform_complete':False,'physical_claim':False}
    planned['receipt.json']=(json.dumps(receipt,ensure_ascii=False,indent=2)+'\n').encode()
    destination.mkdir(mode=0o700);created=[]
    try:
        for name,body in planned.items():
            with (destination/name).open('xb') as stream:
                created.append(destination/name)
                stream.write(body)
    except BaseException:
        for file in reversed(created):file.unlink()
        destination.rmdir();raise
    return receipt


def collect_mobile_exports(repo, outputs, source):
    require(outputs,'runner_outputs_required');platforms={};original_repo=repo
    for out in outputs:
        repo,out,expected,cases=_derive(original_repo,out);require(expected['source_commit']==source,'runner_source_mismatch')
        platform=next(iter(expected['platforms']));require(platform not in platforms,'duplicate_platform')
        root=out/'acceptance';require(root.is_dir() and not root.is_symlink() and not (root/'receipt.json').is_symlink(),'linked_export_input');receipt=read_json(root/'receipt.json')
        require(receipt.get('platform')==platform and receipt.get('platform_complete') is False and receipt.get('physical_claim') is False and isinstance(receipt.get('files'),dict) and set(receipt['files'])==({platform+'-'+name+'.json' for name in cases}|{'manifest.json'}),'export_receipt_invalid')
        require(receipt.get('schema')=='mobile_runner_acceptance_export_v1' and receipt.get('status')=='exported_partial' and receipt.get('source_commit')==source and receipt.get('exported_cases')==sorted(cases),'export_receipt_invalid')
        for name,value in receipt.get('files',{}).items():_reference(repo,{'path':(root/name).relative_to(repo).as_posix(),'sha256':value})
        actual=read_json(root/'manifest.json');record=actual.get('platforms',{}).get(platform)
        require(isinstance(record,dict),'export_manifest_invalid')
        expected_record=expected['platforms'][platform]
        for name,proof in cases.items():
            ref=record.get('cases',{}).get(name);loaded=read_json(_reference(repo,ref));require(loaded==proof,'export_case_not_derived_from_runner')
            expected_record['cases'][name]=ref
        require(actual==expected,'export_manifest_not_derived_from_runner');platforms[platform]=record
    return {'schema':'native_acceptance_input_v1','source_commit':source,'backend_source_commit':source,'platforms':platforms,'historical_reports':[]}


def finish_mobile_run(repo, out, code, enabled):
    if not enabled or code!=0:return code
    try:
        export_mobile_run(repo,out)
        return 0
    except (EvidenceError,OSError,ValueError,TypeError,KeyError,subprocess.SubprocessError,tarfile.TarError):
        path=Path(out)/'result.json';result=read_json(path);result.update(status='failed',runner_exit=1,acceptance_export_error='native_acceptance_export_failed')
        path.write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n');return 1
