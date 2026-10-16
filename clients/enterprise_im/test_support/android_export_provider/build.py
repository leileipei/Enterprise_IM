"""Build the standalone controlled provider with an existing SDK/JDK; accepts no licenses."""
from pathlib import Path
import argparse, os, subprocess, zipfile
p=argparse.ArgumentParser();p.add_argument('--sdk',type=Path,required=True);p.add_argument('--java-home',type=Path,required=True);p.add_argument('--out',type=Path,required=True);a=p.parse_args();a.out.mkdir(parents=True,exist_ok=True);source=Path(__file__).resolve().parent;tools=a.sdk/'build-tools/36.0.0';android=a.sdk/'platforms/android-36/android.jar';env=dict(os.environ,JAVA_HOME=str(a.java_home));env['PATH']=str(a.java_home/'bin')+':'+env.get('PATH','')
def run(args):subprocess.run([str(n) for n in args],env=env,check=True)
classes=a.out/'classes';classes.mkdir(exist_ok=True);dex=a.out/'dex';dex.mkdir(exist_ok=True)
run([a.java_home/'bin/javac','-source','17','-target','17','-classpath',android,'-d',classes,source/'FaultDocumentsProvider.java'])
run([tools/'d8','--lib',android,'--min-api','26','--output',dex,*sorted(classes.rglob('*.class'))])
unsigned=a.out/'unsigned.apk';run([tools/'aapt2','link','--manifest',source/'AndroidManifest.xml','-I',android,'-o',unsigned])
with zipfile.ZipFile(unsigned,'a') as z:z.write(dex/'classes.dex','classes.dex')
aligned=a.out/'aligned.apk';run([tools/'zipalign','-f','4',unsigned,aligned]);key=a.out/'fixture.jks'
if not key.exists():run([a.java_home/'bin/keytool','-genkeypair','-keystore',key,'-storepass','fixture-test-only','-keypass','fixture-test-only','-alias','fixture','-dname','CN=Enterprise IM Test Fixture','-keyalg','RSA','-validity','7'])
run([tools/'apksigner','sign','--ks',key,'--ks-pass','pass:fixture-test-only','--key-pass','pass:fixture-test-only','--out',a.out/'provider.apk',aligned]);run([tools/'apksigner','verify','--verbose',a.out/'provider.apk'])
