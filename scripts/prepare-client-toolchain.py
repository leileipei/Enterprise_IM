#!/usr/bin/env python3
"""Verify an isolated, existing fixed Flutter SDK and actual client lockfile.

This records preparation only. It never accepts licenses, installs platform
SDKs, changes PATH/xcode-select, or claims a native build passed.
"""
import argparse
import hashlib
import json
import re
import subprocess
from pathlib import Path

FRAMEWORK = 'e8113bf45620cbeb8aff64947ee4c93e16adb4cf'
ENGINE = '06a2e2a110089dff50fe635cffd2a61e1b24fbcd'
SOURCE_ARCHIVE = '46a3e5203e855ff74fd4fcf1f5f511552881e07fc097a980775991183326a66b'
GRADLE_SHA256 = '17f277867f6914d61b1aa02efab1ba7bb439ad652ca485cd8ca6842fccec6e43'
PINS = {
 'sqlite3': ('3.7.0','cc62d72dbf16bb1f6ae8954b238016de1473e53df060ed0c332eed0a41dd1848'),
 'cryptography': ('2.9.0','3eda3029d34ec9095a27a198ac9785630fe525c0eb6a49f3d575272f8e792ef0'),
 'flutter_secure_storage': ('11.2.0','d4e1fb6b2cb524868929e78dc0282fa000554b22060fb53789dc481c9fc95bb8'),
 'app_links': ('7.2.2','08ba88bf19d9edea33130cb71c8ced2cb982b4e00b82efef4a434d1357bc69c3'),
 'path_provider': ('2.1.6','a7f4874f987173da295a61c181b8ee71dab59b332a486b391babf26a1b884825'),
 'url_launcher': ('6.3.3','e8cc955c97e06df9879140a1fe301f3d493f01b7c62dec7f46834be31d7e9fda'),
 'file_selector': ('1.1.0','bd15e43e9268db636b53eeaca9f56324d1622af30e5c34d6e267649758c84d9a'),
 'flutter_local_notifications': ('22.3.1','357be7ff996dc272e2037c69c1bdd5d1bd21172219d55f8bd2e11aeea602e6d3'),
}

def digest(path): return hashlib.sha256(path.read_bytes()).hexdigest()
def checked(argv, cwd): return subprocess.check_output(argv,cwd=cwd,text=True,stderr=subprocess.PIPE,timeout=60).strip()
def dependency_pins(pubspec,lock):
 for section,expected in [('dependencies',set(PINS)|{'flutter'}),('dev_dependencies',{'flutter_test','integration_test'})]:
  found=re.search(r'^'+section+r':\n(.*?)(?=^[a-zA-Z_]|\Z)',pubspec,re.M|re.S)
  names=set(re.findall(r'^  ([a-zA-Z0-9_]+):',found.group(1),re.M)) if found else set()
  if names!=expected:raise ValueError('unapproved direct dependency set: '+section)
 for name in ['flutter','flutter_test','integration_test']:
  if not re.search(r'^  '+name+r':\n    sdk: flutter$',pubspec,re.M):raise ValueError('Flutter SDK dependency mismatch: '+name)
 actual={}
 for name,(version,sha) in PINS.items():
  if not re.search(r'^  '+re.escape(name)+r': '+re.escape(version)+r'$',pubspec,re.M): raise ValueError('direct version mismatch: '+name)
  found=re.findall(r'^  '+re.escape(name)+r':\n(.*?)(?=^  [A-Za-z0-9_]+:\n|^sdks:)',lock,re.M|re.S)
  if len(found)!=1: raise ValueError('lock package mismatch: '+name)
  block=found[0]
  if 'dependency: "direct main"' not in block or 'source: hosted' not in block:raise ValueError('package source mismatch: '+name)
  if not re.search(r'^    version: "'+re.escape(version)+r'"$',block,re.M) or not re.search(r'^      sha256: "?'+sha+r'"?$',block,re.M):raise ValueError('package archive mismatch: '+name)
  actual[name]={'version':version,'pub_archive_sha256':sha}
 return actual

def gradle_distribution(text):
 url=re.search(r'^distributionUrl=(.+)$',text,re.M)
 sha=re.search(r'^distributionSha256Sum=([a-f0-9]{64})$',text,re.M)
 if not url or url.group(1).replace('\\:',':')!='https://services.gradle.org/distributions/gradle-9.3.1-all.zip':raise ValueError('Gradle template changed')
 if not sha or sha.group(1)!=GRADLE_SHA256:raise ValueError('Gradle checksum mismatch')
 return url.group(1).replace('\\:',':'),sha.group(1)

def main():
 parser=argparse.ArgumentParser(description=__doc__)
 parser.add_argument('--sdk-root',required=True,type=Path);parser.add_argument('--sdk-source-archive',required=True,type=Path)
 parser.add_argument('--client-root',type=Path,default=Path('clients/enterprise_im'));parser.add_argument('--output',type=Path,default=Path('docs/verification/p5-02-toolchain-lock.json'))
 args=parser.parse_args();sdk=args.sdk_root.resolve();client=args.client_root.resolve()
 if checked(['/usr/bin/git','rev-parse','HEAD'],sdk)!=FRAMEWORK or checked(['/usr/bin/git','status','--porcelain','--untracked-files=no'],sdk):raise ValueError('fixed SDK tracked source mismatch')
 if digest(args.sdk_source_archive)!=SOURCE_ARCHIVE:raise ValueError('SDK source archive mismatch')
 version=json.loads(checked([str(sdk/'bin/flutter'),'--version','--machine'],client))
 for key,expected in [('frameworkVersion','3.47.3'),('frameworkRevision',FRAMEWORK),('engineRevision',ENGINE),('dartSdkVersion','3.13.3')]:
  if version.get(key)!=expected:raise ValueError('actual SDK version mismatch: '+key)
 pins=dependency_pins((client/'pubspec.yaml').read_text(),(client/'pubspec.lock').read_text())
 gradle=(client/'android/gradle/wrapper/gradle-wrapper.properties').read_text()
 gradle_url,gradle_checksum=gradle_distribution(gradle)
 settings=(client/'android/settings.gradle.kts').read_text()
 for name,value in [('com.android.application','9.1.0'),('org.jetbrains.kotlin.android','2.4.0')]:
  if f'id("{name}") version "{value}"' not in settings:raise ValueError('Android plugin template changed')
 data={'schema_version':'p502_toolchain_lock_v1','status':'source_and_dependency_preparation','framework_version':'3.47.3','framework_commit':FRAMEWORK,'engine_commit':ENGINE,'dart_version':'3.13.3','sdk_source_archive_sha256':SOURCE_ARCHIVE,'dart_executable_sha256':digest(sdk/'bin/cache/dart-sdk/bin/dart'),'pubspec_lock_sha256':digest(client/'pubspec.lock'),'direct_dependencies':pins,'android':{'gradle_version':'9.3.1','gradle_distribution_url':gradle_url,'gradle_distribution_sha256':gradle_checksum,'agp_version':'9.1.0','kotlin_version':'2.4.0','jdk_validation':'not_executed','native_build':'not_executed'},'generated_runners':[p for p in ['windows','macos','android','ios'] if (client/p).is_dir()],'ios':{'approved_minimum':'13','observed_fixed_engine_minimum':'15.0','baseline_decision':'awaiting_user'},'platform_acceptance':{p:'not_executed' for p in ['windows','macos','android','ios']},'customer_acceptance':'not_executed','production_acceptance':'not_executed'}
 if args.output.is_symlink():raise ValueError('output cannot be symlink')
 args.output.parent.mkdir(parents=True,exist_ok=True);args.output.write_text(json.dumps(data,ensure_ascii=False,indent=2)+'\n')
 print('fixed SDK/source/direct dependency preparation verified; native builds not executed')

if __name__=='__main__':main()
