#!/usr/bin/env python3
"""Opt-in independent Android/iOS messaging; owned emulators only, no hardware."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
from urllib.parse import urlparse
import uuid
import zipfile
sys.dont_write_bytecode=True
from integration import android_build, mobile
from integration.commands import run_command
from integration.registry import Registry
from integration.model import ResourceRef
from integration.mobile_backend import Backend, preflight as backend_preflight
from integration.cross_device import actor_config, validate_participants, checkpoint, network_rule, verify_integrity, confirm_recovery_sender, validate_offline_observation
from integration.cross_native import AndroidActor, IOSActor, digest

class CrossBackend(Backend):
    fixture_test='TestCrossDeviceBackendFixture'


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    for name in ('flutter-sdk','pub-cache','android-sdk','java-home','gradle-cache','go','developer-dir','output'):
        parser.add_argument('--'+name,type=Path,required=True)
    args=parser.parse_args()
    repo=Path(__file__).resolve().parents[1]
    sha=subprocess.check_output(['git','rev-parse','HEAD'],cwd=repo,text=True).strip()
    if subprocess.check_output(['git','status','--porcelain','--untracked-files=all'],cwd=repo):raise ValueError('freeze_clean_source_before_native_run')
    args.output.mkdir(mode=0o700)
    out=args.output
    aa=SimpleNamespace(**vars(args),target='android',verify_simulator=True,simulator_runtime='system-images;android-36;default;arm64-v8a',simulator_device_type='pixel_7')
    ia=SimpleNamespace(**vars(args),target='ios-simulator',verify_simulator=True,simulator_runtime='com.apple.CoreSimulator.SimRuntime.iOS-26-5',simulator_device_type='com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro')
    pre={'android':android_build.preflight(repo,aa),'ios':mobile.preflight(repo,ia),'backend':backend_preflight(args.go)}
    owner=secrets.token_hex(16);root=Path(tempfile.mkdtemp(prefix='im-cross-',dir='/private/tmp')).resolve();root.chmod(0o700)
    reg=Registry(root/'registry.jsonl',owner,sha);private=root/owner;client=private/'client'
    result={'status':'failed','source_commit':sha,'owner':owner,'root':str(root),'preflight':pre,'commands':[],'actors':{},'backend':{},'checkpoints':[], 'scope':'two owned Android API36 and one iOS26.5 native process; production API and workers; synthetic platform-bound refresh IdP; no physical hardware or interactive customer SSO'}
    actors={};backend=None;blocked=None
    def save():(out/'result.json').write_text(json.dumps(result,indent=2)+'\n')
    def inventory():
        paths=subprocess.check_output(['git','ls-files','-z'],cwd=repo).split(b'\0')
        return {os.fsdecode(p):digest(repo/os.fsdecode(p)) for p in paths if p}
    sources=inventory();result['source_sha256']=sources;save()
    def step(name,argv,env,timeout=1200):
        e=run_command(name,'cross-native',sha,argv,client,env,out/(name+'.log'),time.monotonic()+timeout,reg)
        result['commands'].append({'name':name,'actual_exit':e.exit_code,'failures':e.failures,'stdout_sha256':digest(e.log),'stderr_sha256':digest(str(e.log)+'.stderr')});save()
        if e.exit_code!=0 or e.failures:raise ValueError('cross_'+name+'_failed')
    def dbphase(phase):
        schema=backend.config['schema']
        if not re.fullmatch('im_policy_[a-zA-Z0-9_]+',schema):raise ValueError('cross_schema_unverified')
        role_sql=[]
        for role,actor in actors.items():
            dev=str(uuid.UUID(actor.state['admission']['device_id']))
            role_sql.append("'%s',json_build_object('delivered',(SELECT count(*) FROM message_device_receipts WHERE device_id='%s'),'read',(SELECT count(*) FROM message_device_receipts WHERE device_id='%s' AND read_at IS NOT NULL))"%(role,dev,dev))
        alice=str(uuid.UUID(backend.config['actors']['alice']['user']));bob=str(uuid.UUID(backend.config['actors']['bob_ios']['user']))
        sql="SET search_path TO "+schema+",public; SELECT json_build_object('messages',(SELECT count(*) FROM messages),'devices',(SELECT count(*) FROM user_devices),'roles',json_build_object("+','.join(role_sql)+"),'users',json_build_object('alice',(SELECT count(*) FROM message_user_receipts WHERE user_id='"+alice+"' AND read_at IS NOT NULL),'bob',(SELECT count(*) FROM message_user_receipts WHERE user_id='"+bob+"' AND read_at IS NOT NULL)));"
        end=time.monotonic()+45
        while True:
            facts=json.loads(backend.docker(['exec','-i',result['backend']['services']['postgres']['id'],'psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','enterprise_im_p5'],input=sql))
            try:checkpoint(phase,facts);break
            except ValueError:
                if time.monotonic()>end:
                    (out/('failed-checkpoint-'+phase+'.json')).write_text(json.dumps(facts,indent=2)+'\n');raise
                time.sleep(.2)
        (out/('checkpoint-'+phase+'.json')).write_text(json.dumps(facts,indent=2)+'\n')
        result['checkpoints'].append(phase);save()
    try:
        mobile.copy_client(repo,client,{p:h for p,h in sources.items() if p.startswith('clients/enterprise_im/')})
        build=private/'android-build';build.mkdir(mode=0o700);env=android_build.android_environment(build,aa)
        flutter=[str(args.flutter_sdk/'bin/cache/dart-sdk/bin/dart'),str(args.flutter_sdk/'bin/cache/flutter_tools.snapshot')]
        target='integration_test/cross_device_test.dart'
        step('pub-android',flutter+['pub','get','--offline','--enforce-lockfile'],env,120)
        step('shared',flutter+['test','--no-pub'],env)
        step('analysis',flutter+['analyze','--fatal-infos','--no-pub'],env)
        step('build-android',flutter+['build','apk','--debug','--no-pub','--target='+target],env,1800)
        apk=out/'cross-debug.apk';shutil.copyfile(client/'build/app/outputs/flutter-apk/app-debug.apk',apk);apk.chmod(0o600)
        iosbuild=private/'ios-build';iosbuild.mkdir(mode=0o700);ienv=mobile.xcrun_environment(iosbuild,args.developer_dir,args.pub_cache)
        step('pub-ios',flutter+['pub','get','--offline','--enforce-lockfile'],ienv,120)
        step('build-ios',flutter+['build','ios','--simulator','--debug','--no-pub','--target='+target],ienv,1800)
        app=client/'build/ios/iphonesimulator/Runner.app';archive=out/'cross-ios-debug.zip';p=mobile.archive_ios_bundle(app,archive)
        if p.returncode:raise ValueError('cross_ios_archive_failed')
        for binary in (apk,archive):
            with zipfile.ZipFile(binary) as z:
                if any(Path(n).name in ('cross-input.json','owned_backend.json') for n in z.namelist()):raise ValueError('runtime_credentials_bundled')
        if digest(client/'pubspec.lock')!=sources['clients/enterprise_im/pubspec.lock']:raise ValueError('cross_lock_changed')
        result['binaries']={'android':{'file':apk.name,'sha256':digest(apk)},'ios':{'file':archive.name,'sha256':digest(archive)},'credentials_created_after_build':True};save()
        backend=CrossBackend(repo,args,private,out,reg,'cross',result['backend'],save);config=backend.start();port=urlparse(config['origin']).port
        for role in ('alice','bob_ios','bob_android'):
            actorout=out/role;actorout.mkdir(mode=0o700);state={};result['actors'][role]=state
            if role=='bob_ios':actor=IOSActor(role,ia,actorout,secrets.token_hex(16),state,save,ienv);binary=app
            else:actor=AndroidActor(role,aa,private/role,actorout,secrets.token_hex(16),reg,pre['android']['simulator'],state,save);binary=apk
            actors[role]=actor;bootstrap=actor_config(config,role,secrets.token_hex(16))
            if role=='bob_ios':actor.start(binary,bootstrap)
            else:actor.start(binary,port,bootstrap)
            actor.ready(bootstrap)
        validate_participants({role:a.state['admission'] for role,a in actors.items()})
        if actors['alice'].sim.name==actors['bob_android'].sim.name or actors['alice'].sim.console==actors['bob_android'].sim.console:raise ValueError('independent_android_vms_unproven')
        dbphase('registered');alice=actors['alice'];ios=actors['bob_ios'];android=actors['bob_android']
        alice.command('send',text='独立进程消息一',seq='1');dbphase('first_sent')
        ios.command('open');dbphase('bob_delivered')
        android.command('open');dbphase('both_delivered');alice.command('summary',seq='1',status='delivered')
        ios.command('read',seqs=['1']);dbphase('bob_read');alice.command('summary',seq='1',status='read');ios.screenshot('first-read')
        uid=int(android.state['installation']['uid']);comment='IMCROSS_'+secrets.token_hex(6).upper()
        # Only the verified owned guest's application UID is affected.
        android.identity(android.state['pid']);android.sim.adb('network-cut',network_rule(uid,comment,'insert'));blocked=(android,uid,comment)
        validate_offline_observation(android.command('probe_offline'));alice.command('send',text='断线期间消息二',seq='2');dbphase('offline_sent')
        time.sleep(3);android.command('offline_stable');dbphase('offline_stable')
        counters=android.sim.adb('network-counters',['shell','su','0','iptables','-L','OUTPUT','-n','-v','-x']).stdout.decode()
        matching=[line for line in counters.splitlines() if comment in line]
        if len(matching)!=1 or int(matching[0].split()[0])<=0:raise ValueError('cross_network_cut_not_observed')
        result['network']={'uid':uid,'comment':comment,'packets':int(matching[0].split()[0]),'offline_seconds':3};save()
        android.sim.adb('network-restore',network_rule(uid,comment,'delete'));blocked=None
        absent=android.sim.adb('network-absent',['shell','su','0','iptables','-C','OUTPUT',*network_rule(uid,comment,'delete')[6:]],required=False)
        if absent.returncode!=1:raise ValueError('cross_network_rule_remains')
        result['network']['rule_removed']=True
        recovered=android.command('auto_catchup')
        if recovered['details'].get('automatic_catchup') is not True or recovered['details'].get('cursor')!='2':raise ValueError('cross_automatic_catchup_unproven')
        dbphase('recovered_delivered');android.command('read',seqs=['1','2']);dbphase('recovered_read');android.screenshot('recovered-read');confirm_recovery_sender(alice)
        ios.command('send',text='跨端回复三',seq='3');alice.command('received_read',seq='3');ios.command('summary',seq='3',status='read');dbphase('reply_read')
        alice.screenshot('reply-read');ios.screenshot('reply-sent')
        for actor in actors.values():
            final=actor.command('done')
            if final.get('cleanup') is not True:raise ValueError('cross_actor_cleanup_unproven')
        dbphase('done');(backend.root/'scenario-done').touch(mode=0o600);result['scenario_passed']=True
    except Exception as exc:
        result['failure_type']=type(exc).__name__;result['failure_reason']=str(exc);save()
    finally:
        cleanup=[]
        if blocked:
            try:blocked[0].sim.adb('network-finally-restore',network_rule(blocked[1],blocked[2],'delete'))
            except Exception as exc:cleanup.append('network:'+type(exc).__name__)
        for actor in reversed(list(actors.values())):
            try:cleanup.extend(actor.close())
            except Exception as exc:cleanup.append(actor.role+':'+type(exc).__name__)
        if backend:
            try:backend.close()
            except Exception as exc:cleanup.append('backend:'+type(exc).__name__)
        result.update(verify_integrity(repo,sources,{p:h for platform in ('android','ios') for p,h in pre[platform]['tool_sha256'].items()}))
        if not cleanup:
            st=private.stat();reg.add(ResourceRef('directory',owner,str(private),{'device':st.st_dev,'inode':st.st_ino}))
        cleaned=reg.cleanup(time.monotonic()+90);cleanup.extend(cleaned.failures)
        (out/'registry.jsonl').write_bytes(reg.path.read_bytes())
        if not cleanup and cleaned.removed and not private.exists():reg.path.unlink();root.rmdir()
        result['cleanup']={'failures':cleanup,'private_removed':not private.exists(),'root_removed':not root.exists()}
        back=result['backend']
        result['status']='passed' if result.get('scenario_passed') is True and back.get('actual_exit')==0 and not back.get('command_failures') and back.get('product_children',{}).get('lifecycle_verified') is True and back.get('source_unchanged') is True and back.get('go_unchanged') is True and back.get('thread_joined') is True and result['source_unchanged'] and result['tools_unchanged'] and not cleanup and not root.exists() else 'failed'
        save()
    print(json.dumps({k:result.get(k) for k in ('status','source_commit','checkpoints','failure_reason','cleanup')},ensure_ascii=False))
    return 0 if result['status']=='passed' else 1

if __name__=='__main__':sys.exit(main())
